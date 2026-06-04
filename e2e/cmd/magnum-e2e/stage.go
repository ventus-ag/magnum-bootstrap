package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/objectstorage/v1/containers"
	"github.com/gophercloud/gophercloud/v2/openstack/objectstorage/v1/objects"
)

// objectClient returns a lazily-built, cached Swift (object-store v1) client.
func (r *runner) objectClient() (*gophercloud.ServiceClient, error) {
	if r.swift != nil {
		return r.swift, nil
	}
	c, err := openstack.NewObjectStorageV1(r.provider, gophercloud.EndpointOpts{Region: r.cfg.region})
	if err != nil {
		return nil, fmt.Errorf("locating object-store (Swift) endpoint: %w", err)
	}
	r.swift = c
	return c, nil
}

// stageContainer is the deterministic per-run container name, so a separate
// -teardown can clean it up too.
func (r *runner) stageContainer() string { return "magnum-e2e-" + r.cfg.clusterName }

// stageBootstrap uploads the locally-built reconciler binary (+ sha256 sidecar)
// to a public-read Swift container and points reconciler_binary_url at it, so
// the cluster nodes fetch the EXACT current build over their in-cloud network.
// No-op when -bootstrap-binary is unset (then RECONCILER_VERSION/URL stand).
//
// The container is anonymous-read (.r:*): the node's launcher curls the object
// with no Keystone token. The binary is the reconciler, not a secret, and the
// container is uniquely named per run and deleted at teardown.
func (r *runner) stageBootstrap(ctx context.Context) error {
	if r.cfg.bootstrapBinary == "" {
		return nil
	}
	data, err := os.ReadFile(r.cfg.bootstrapBinary)
	if err != nil {
		return fmt.Errorf("read bootstrap binary %q: %w", r.cfg.bootstrapBinary, err)
	}
	sum := sha256.Sum256(data)
	hexsum := hex.EncodeToString(sum[:])

	swift, err := r.objectClient()
	if err != nil {
		return err
	}
	cont := r.stageContainer()
	if res := containers.Create(ctx, swift, cont, containers.CreateOpts{ContainerRead: ".r:*"}); res.Err != nil {
		return fmt.Errorf("create staging container %q: %w", cont, res.Err)
	}
	r.log("staging reconciler binary (%d bytes, sha256 %s…) -> swift container %q", len(data), hexsum[:12], cont)
	if res := objects.Create(ctx, swift, cont, "bootstrap", objects.CreateOpts{
		Content:       bytes.NewReader(data),
		ContentType:   "application/octet-stream",
		ContentLength: int64(len(data)),
	}); res.Err != nil {
		return fmt.Errorf("upload bootstrap: %w", res.Err)
	}
	if res := objects.Create(ctx, swift, cont, "bootstrap.sha256", objects.CreateOpts{
		Content:     strings.NewReader(hexsum + "  bootstrap\n"),
		ContentType: "text/plain",
	}); res.Err != nil {
		return fmt.Errorf("upload checksum: %w", res.Err)
	}

	url := strings.TrimRight(swift.Endpoint, "/") + "/" + cont + "/bootstrap"
	r.cfg.reconcilerURL = url
	if r.cfg.reconcilerVersion == "" {
		// Non-empty version is required by the launcher and names the on-node
		// cache dir; key it to the content so a changed binary busts the cache.
		r.cfg.reconcilerVersion = "e2e-" + hexsum[:12]
	}
	r.log("staged: reconciler_binary_url=%s reconciler_version=%s", url, r.cfg.reconcilerVersion)
	return nil
}

// unstageBootstrap best-effort removes the staged objects + container. The
// container name is deterministic, so this also cleans up after a separate
// -teardown invocation. Safe no-op if nothing was staged / no Swift.
func (r *runner) unstageBootstrap(ctx context.Context) {
	if r.cfg.bootstrapBinary == "" {
		return
	}
	swift, err := r.objectClient()
	if err != nil {
		return
	}
	cont := r.stageContainer()
	objects.Delete(ctx, swift, cont, "bootstrap", nil)
	objects.Delete(ctx, swift, cont, "bootstrap.sha256", nil)
	if res := containers.Delete(ctx, swift, cont); res.Err == nil {
		r.log("removed staging container %q", cont)
	}
}

// stageSelfTest stages the binary, fetches it back anonymously over HTTP (the
// way a node would), verifies the bytes + checksum, then unstages. A cheap,
// reversible live proof of the Swift delivery path — no cluster involved.
func (r *runner) stageSelfTest(ctx context.Context) error {
	if r.cfg.bootstrapBinary == "" {
		return fmt.Errorf("-bootstrap-binary (or BOOTSTRAP_BINARY) is required for -stage-selftest")
	}
	if err := r.stageBootstrap(ctx); err != nil {
		return err
	}
	defer r.unstageBootstrap(ctx)

	want, err := os.ReadFile(r.cfg.bootstrapBinary)
	if err != nil {
		return err
	}
	wantSum := sha256.Sum256(want)

	r.log("anonymous GET %s", r.cfg.reconcilerURL)
	got, err := httpGet(ctx, r.cfg.reconcilerURL)
	if err != nil {
		return fmt.Errorf("anonymous fetch (node would hit this too): %w", err)
	}
	if gotSum := sha256.Sum256(got); gotSum != wantSum {
		return fmt.Errorf("fetched bytes differ from source (%d vs %d bytes)", len(got), len(want))
	}
	chk, err := httpGet(ctx, r.cfg.reconcilerURL+".sha256")
	if err != nil {
		return fmt.Errorf("fetch checksum sidecar: %w", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(chk)), hex.EncodeToString(wantSum[:])) {
		return fmt.Errorf("checksum sidecar mismatch: %q", string(chk))
	}
	r.log("stage self-test OK ✅ — uploaded %d bytes, fetched back anonymously, sha256 + sidecar match", len(want))
	return nil
}

func httpGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	cl := &http.Client{Timeout: 120 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
