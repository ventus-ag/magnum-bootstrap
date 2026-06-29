package zincati

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
	"github.com/ventus-ag/magnum-bootstrap/internal/logging"
)

// fedoraGPGKeyDir is the ostree fedora remote's gpgkeypath directory. ostree
// loads every keyring file in this directory, regardless of filename, so a key
// dropped here becomes a trusted signer for rpm-ostree commit verification.
const fedoraGPGKeyDir = "/etc/pki/rpm-gpg"

// fedoraKeyURLTemplate is the canonical, HTTPS-only source for a Fedora release
// signing key. src.fedoraproject.org is Fedora's official dist-git; TLS to this
// host authenticates that the key really came from Fedora — the same trust class
// as the reconciler's other release-binary downloads. The "rawhide" ref always
// carries the full historical set of per-release key files.
const fedoraKeyURLTemplate = "https://src.fedoraproject.org/rpms/fedora-repos/raw/rawhide/f/RPM-GPG-KEY-fedora-%d-primary"

// fedoraKeyProbeSpan bounds how many releases past the node's own we probe for
// (Fedora release keys are sequential and gap-free).
const fedoraKeyProbeSpan = 16

// pgpPublicKeyHeader marks an ASCII-armored public key block; used to reject any
// response that is not actually a key (error page, redirect body, etc.).
const pgpPublicKeyHeader = "-----BEGIN PGP PUBLIC KEY BLOCK-----"

// installFedoraGPGKeys dynamically fetches and installs the Fedora release
// signing keys the node is missing, so an old Fedora CoreOS node can verify the
// signatures on current FCoS stable ostree commits and walk the zincati upgrade
// graph forward.
//
// Why: Fedora rotates the ostree/package signing key every release, and FCoS
// stable commits are (re)signed with recent release keys. A node booted from an
// old image (e.g. FCoS 34, glibc 2.33) only ships keys up to its own release, so
// every zincati upgrade fails to stage with "Can't check signature: public key
// not found". The first upgrade hop (into Fedora 35) already brings glibc 2.34,
// which is what the dynamically linked containerd 2.x binaries need.
//
// Dynamic by design: we probe only releases NEWER than the node's own Fedora
// version, so a current node installs nothing (all probes 404) and an old node
// installs exactly the keys it lacks — no hardcoded key material, no rebuild when
// Fedora cuts a new release. Best-effort: any download failure is logged and
// skipped, never failing the reconcile (a missing key only means the OS upgrade
// can't proceed yet, same as before).
func installFedoraGPGKeys(ctx context.Context, executor *host.Executor, logger *logging.Logger) ([]host.Change, error) {
	major := currentFedoraMajor()
	if major == 0 {
		return nil, nil // not Fedora / unparseable — nothing to do
	}

	var changes []host.Change
	installed := 0
	consecutiveMisses := 0
	client := &http.Client{Timeout: 20 * time.Second}

	for rel := major + 1; rel <= major+fedoraKeyProbeSpan; rel++ {
		url := fmt.Sprintf(fedoraKeyURLTemplate, rel)
		content, found, err := fetchFedoraKey(ctx, client, url)
		if err != nil {
			// Transient (network/5xx): log and keep probing; do not count as a
			// definitive miss, since releases are sequential.
			if logger != nil {
				logger.Warnf("zincati: fetch fedora %d key failed (non-fatal): %v", rel, err)
			}
			continue
		}
		if !found {
			consecutiveMisses++
			if consecutiveMisses >= 2 {
				break // two real 404s in a row => past the newest release
			}
			continue
		}
		consecutiveMisses = 0

		// magnum-namespaced filename so we never overwrite a distro-shipped key.
		dst := fmt.Sprintf("%s/RPM-GPG-KEY-fedora-%d-magnum", fedoraGPGKeyDir, rel)
		res, err := (hostresource.FileSpec{Path: dst, Content: content, Mode: 0o644}).Apply(executor)
		if err != nil {
			return changes, fmt.Errorf("install fedora %d gpg key: %w", rel, err)
		}
		changes = append(changes, res.Changes...)
		if res.Changed {
			installed++
		}
	}

	if installed > 0 && logger != nil {
		logger.Infof("zincati: installed %d newer Fedora signing key(s) for ostree upgrade verification (node is Fedora %d)", installed, major)
	}
	return changes, nil
}

// fetchFedoraKey GETs an armored Fedora key. found=false on HTTP 404 or a body
// that is not an armored public key; err is returned only for transient
// failures worth distinguishing from a definitive 404.
func fetchFedoraKey(ctx context.Context, client *http.Client, url string) (content []byte, found bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("unexpected status %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // keys are ~2 KiB; cap at 1 MiB
	if err != nil {
		return nil, false, err
	}
	if !strings.Contains(string(body), pgpPublicKeyHeader) {
		return nil, false, nil // not a key (error page / redirect body)
	}
	return body, true, nil
}

// currentFedoraMajor parses the running OS major version from /etc/os-release,
// returning 0 for non-Fedora systems or when the version cannot be parsed.
func currentFedoraMajor() int {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return 0
	}
	return parseFedoraMajor(string(data))
}

// parseFedoraMajor extracts the Fedora major release from os-release contents.
// Returns 0 unless ID=fedora and VERSION_ID parses to an integer major.
func parseFedoraMajor(osRelease string) int {
	var id, versionID string
	for _, line := range strings.Split(osRelease, "\n") {
		switch {
		case strings.HasPrefix(line, "ID="):
			id = strings.Trim(strings.TrimPrefix(line, "ID="), "\"")
		case strings.HasPrefix(line, "VERSION_ID="):
			versionID = strings.Trim(strings.TrimPrefix(line, "VERSION_ID="), "\"")
		}
	}
	if id != "fedora" {
		return 0
	}
	if dot := strings.IndexByte(versionID, '.'); dot >= 0 {
		versionID = versionID[:dot]
	}
	n, err := strconv.Atoi(strings.TrimSpace(versionID))
	if err != nil {
		return 0
	}
	return n
}
