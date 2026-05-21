// Command mock-heat is a stand-in for the Heat orchestration/signal endpoints
// that the real heat-container-agent talks to — nothing more. It carries NO
// bootstrap logic: it serves the SoftwareDeployment metadata the agent fetches
// and records the deployment signal the agent POSTs back. The metadata it serves
// (the four real bootstrap scripts + ~90 inputs) is produced by scenario-gen.
//
// It is the os-collect-config `request` collector's metadata source plus the
// HEAT_SIGNAL sink:
//
//	GET  /md/{node}     -> contents of <dir>/{node}.md.json (or {"deployments":[]})
//	POST /signal/{id}   -> writes the body to <dir>/{id}.signal.json
//	GET  /healthz       -> ok
//
// The harness drives it via files in -dir: it writes <node>.md.json (a new
// deployment id per trigger) and then reads <id>.signal.json for the result.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:9512", "host:port to listen on")
	dir := flag.String("dir", ".", "state dir holding <node>.md.json (served) and <id>.signal.json (written)")
	verbose := flag.Bool("v", false, "log every request")
	flag.Parse()

	if err := os.MkdirAll(*dir, 0o755); err != nil {
		log.Fatalf("mock-heat: mkdir %s: %v", *dir, err)
	}
	s := &server{dir: *dir, verbose: *verbose}
	log.Printf("mock-heat: listening on %s, state dir %s", *listen, *dir)
	if err := http.ListenAndServe(*listen, http.HandlerFunc(s.route)); err != nil {
		log.Fatalf("mock-heat: %v", err)
	}
}

type server struct {
	dir     string
	verbose bool
}

func (s *server) route(w http.ResponseWriter, r *http.Request) {
	if s.verbose {
		log.Printf("mock-heat: %s %s", r.Method, r.URL.Path)
	}
	p := strings.Trim(r.URL.Path, "/")
	switch {
	case p == "healthz":
		io.WriteString(w, "ok")
	case strings.HasPrefix(p, "md/") && r.Method == http.MethodGet:
		s.serveMetadata(w, strings.TrimPrefix(p, "md/"))
	case strings.HasPrefix(p, "signal/") && (r.Method == http.MethodPost || r.Method == http.MethodPut):
		s.recordSignal(w, r, strings.TrimPrefix(p, "signal/"))
	default:
		http.NotFound(w, r)
	}
}

// serveMetadata returns the node's current deployment metadata. A missing file
// means "no deployment yet" — return an empty set so the agent polls happily
// until the harness writes one.
func (s *server) serveMetadata(w http.ResponseWriter, node string) {
	if !safeName(node) {
		http.Error(w, "bad node", http.StatusBadRequest)
		return
	}
	b, err := os.ReadFile(filepath.Join(s.dir, node+".md.json"))
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		io.WriteString(w, `{"deployments":[]}`)
		return
	}
	w.Write(b)
}

// recordSignal stores the deployment signal (the script hook's outputs +
// deploy_status_code/stdout/stderr) for the harness to assert on.
func (s *server) recordSignal(w http.ResponseWriter, r *http.Request, id string) {
	if !safeName(id) {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := os.WriteFile(filepath.Join(s.dir, id+".signal.json"), body, 0o600); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("mock-heat: signal for %s (%d bytes)", id, len(body))
	fmt.Fprintln(w, "ok")
}

// safeName rejects path traversal in the node/id path segments.
func safeName(s string) bool {
	if s == "" || strings.ContainsAny(s, "/\\") || strings.Contains(s, "..") {
		return false
	}
	return true
}
