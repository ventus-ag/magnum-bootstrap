package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"golang.org/x/crypto/ssh"
)

// nodeLogCmd is the on-host diagnostic bundle run over SSH on every cluster node
// when an op fails. It pulls the reconciler's own log + last-run result, the
// heat-container-agent journal (the agent that ran the bootstrap deployment), and
// the kubernetes/runtime service journals + states — the on-node context the
// Kubernetes API and Heat outputs cannot show. All best-effort (2>/dev/null) so a
// missing unit never aborts the bundle.
const nodeLogCmd = `set +e
echo '### uname / os'; uname -a; cat /etc/os-release 2>/dev/null | grep -E '^(PRETTY_NAME|VERSION)='
echo '### /etc/sysconfig/heat-params (KUBE_TAG/NUMBER_OF_MASTERS/role)'; sudo grep -E '^(KUBE_TAG|KUBE_VERSION|NUMBER_OF_MASTERS|NODEGROUP_ROLE|MASTER_INDEX|CA_ROTATION_ID)=' /etc/sysconfig/heat-params 2>/dev/null
echo '### reconciler-last-run.json'; sudo cat /var/lib/magnum/reconciler-last-run.json 2>/dev/null
# Decision-critical reconcile lines, grepped from the WHOLE log so they survive
# regardless of tail position. A multi-master etcd join/promotion trail sits in
# the early etcd phase and scrolls out of a plain tail once the later phases
# spam kubectl retries — this is what a learner-never-promoted wedge needs.
echo '### magnum-reconcile.log (etcd/join/promotion/phase-failure trail)'; sudo grep -aE 'etcd:|member (add|list|promote)|promoted learner|learner .* (not yet in sync|did not become)|too many learner|not enough started|hasData=|isMember=|selfIsLearner|rpc not supported|running phase=|phase .* failed|reconcile failed|apiserver not ready|advertise-address' /var/log/magnum-reconcile.log 2>/dev/null | tail -n 400
echo '### magnum-reconcile.log (tail 500)'; sudo tail -n 500 /var/log/magnum-reconcile.log 2>/dev/null
echo '### journalctl heat-container-agent (tail 200)'; sudo journalctl -u heat-container-agent --no-pager -n 200 2>/dev/null
echo '### journalctl magnum-reconcile* (tail 200)'; sudo journalctl -u 'magnum-reconcile*' --no-pager -n 200 2>/dev/null
echo '### systemctl is-active core services'; sudo systemctl is-active containerd kubelet etcd kube-apiserver kube-controller-manager kube-scheduler kube-proxy 2>/dev/null
echo '### journalctl kubelet (tail 200)'; sudo journalctl -u kubelet --no-pager -n 200 2>/dev/null
echo '### journalctl containerd (tail 120)'; sudo journalctl -u containerd --no-pager -n 120 2>/dev/null
echo '### journalctl etcd/apiserver/controller (tail 150)'; sudo journalctl -u etcd -u kube-apiserver -u kube-controller-manager --no-pager -n 150 2>/dev/null
echo '### crictl pods'; sudo crictl pods 2>/dev/null
`

// collectNodeLogs SSHes to every cluster node and captures the on-host diagnostic
// bundle (nodeLogCmd). It needs a private key: the ephemeral keypair's generated
// key (captured at create) or -ssh-key for a named KEYPAIR. With no key it logs a
// note and skips. Best-effort: never returns an error, never blocks teardown.
func (r *runner) collectNodeLogs(ctx context.Context, reason string) {
	signer, err := r.sshSigner()
	if err != nil {
		r.log("node-logs: skipping SSH log collection (%v)", err)
		return
	}
	nodes, err := r.clusterNodeIPs(ctx)
	if err != nil {
		r.err("node-logs: cannot list cluster servers: %v", err)
		return
	}
	if len(nodes) == 0 {
		r.log("node-logs: no cluster servers found for %q", r.cfg.clusterName)
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "==== e2e node logs (SSH) ====\nreason:   %s\ncluster:  %s\nuser:     %s\ntime:     %s\nnodes:    %d\n",
		reason, r.cfg.clusterName, r.cfg.sshUser, time.Now().UTC().Format("2006-01-02T15:04:05Z"), len(nodes))

	for _, n := range nodes {
		fmt.Fprintf(&b, "\n========================================\n### node %s @ %s\n========================================\n", n.name, n.ip)
		out, serr := sshRun(ctx, n.ip, r.cfg.sshUser, signer, nodeLogCmd)
		if serr != nil {
			fmt.Fprintf(&b, "(ssh failed: %v)\n", serr)
		}
		b.WriteString(out)
	}

	r.writeDiagFile("nodelogs-"+reason, b.String())
	r.log("node-logs: captured on-host logs from %d node(s) for %q", len(nodes), reason)
}

// sshSigner builds an ssh.Signer from the ephemeral keypair (captured at create)
// or, for a named KEYPAIR, from -ssh-key / SSH_PRIVATE_KEY.
func (r *runner) sshSigner() (ssh.Signer, error) {
	key := r.sshKey
	if len(key) == 0 {
		if r.cfg.sshKeyPath == "" {
			return nil, fmt.Errorf("no SSH key (named KEYPAIR set without -ssh-key/SSH_PRIVATE_KEY)")
		}
		b, err := os.ReadFile(r.cfg.sshKeyPath)
		if err != nil {
			return nil, fmt.Errorf("read ssh key %q: %w", r.cfg.sshKeyPath, err)
		}
		key = b
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("parse ssh key: %w", err)
	}
	return signer, nil
}

type nodeAddr struct {
	name string
	ip   string
}

// clusterNodeIPs lists every Nova server for the cluster (master + minion) and
// returns a reachable IP per node, preferring a floating address. Like the manual
// runbook, this reads Nova directly rather than the cluster's reported addresses,
// so a node that exists but is not yet registered (e.g. a failed config deploy)
// is still captured.
func (r *runner) clusterNodeIPs(ctx context.Context) ([]nodeAddr, error) {
	nova, err := r.computeClient()
	if err != nil {
		return nil, err
	}
	// Nova server names are "<stackname>-master-N" / "<stackname>-node-N", and the
	// stack name is the truncated cluster name + a stack short-id — NOT the cluster
	// name — so filter by the resolved stack name. Fall back to the cluster name.
	nameFilter := r.cfg.clusterName
	if c, cerr := r.getCluster(ctx); cerr == nil && c.StackID != "" {
		if sn, serr := r.resolveStackName(ctx, c.StackID); serr == nil {
			nameFilter = sn
		}
	}
	pages, err := servers.List(nova, servers.ListOpts{Name: nameFilter}).AllPages(ctx)
	if err != nil {
		return nil, err
	}
	srvs, err := servers.ExtractServers(pages)
	if err != nil {
		return nil, err
	}
	var out []nodeAddr
	for _, s := range srvs {
		if ip := pickServerIP(s.Addresses); ip != "" {
			out = append(out, nodeAddr{name: s.Name, ip: ip})
		}
	}
	return out, nil
}

// pickServerIP extracts a reachable IP from a Nova server's Addresses map,
// preferring a floating address and falling back to the first fixed one.
func pickServerIP(addresses map[string]any) string {
	var fixed string
	for _, v := range addresses {
		list, ok := v.([]any)
		if !ok {
			continue
		}
		for _, a := range list {
			m, ok := a.(map[string]any)
			if !ok {
				continue
			}
			addr := asString(m["addr"])
			if addr == "" {
				continue
			}
			switch asString(m["OS-EXT-IPS:type"]) {
			case "floating":
				return addr
			case "fixed":
				if fixed == "" {
					fixed = addr
				}
			default:
				if fixed == "" {
					fixed = addr
				}
			}
		}
	}
	return fixed
}

// sshRun opens a one-shot SSH session to addr and runs cmd, returning combined
// stdout+stderr. Host keys are not verified: nodes are ephemeral e2e VMs reached
// over the cloud's own network, and this path runs only to gather diagnostics
// from an already-failed run.
func sshRun(ctx context.Context, addr, user string, signer ssh.Signer, cmd string) (string, error) {
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	}
	d := net.Dialer{Timeout: 15 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(addr, "22"))
	if err != nil {
		return "", fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	c, chans, reqs, err := ssh.NewClientConn(conn, net.JoinHostPort(addr, "22"), cfg)
	if err != nil {
		return "", fmt.Errorf("handshake: %w", err)
	}
	client := ssh.NewClient(c, chans, reqs)
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("session: %w", err)
	}
	defer sess.Close()

	out, err := sess.CombinedOutput(cmd)
	return string(out), err
}
