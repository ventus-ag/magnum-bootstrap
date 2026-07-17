package etcdconfig

import (
	"context"
	"strings"
	"testing"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

func TestRunSkipsActiveCARotation(t *testing.T) {
	cfg := config.Config{
		Shared: config.SharedConfig{
			KubeTag: "v1.24.0",
		},
		Master: &config.MasterConfig{},
		Trigger: config.TriggerConfig{
			CARotationID: "rotate-123",
		},
	}

	res, err := (Module{}).Run(context.Background(), cfg, moduleapi.Request{Apply: true})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(res.Changes) != 0 {
		t.Fatalf("expected no changes during active CA rotation, got %d", len(res.Changes))
	}
	if got := res.Outputs["etcdTag"]; got != "3.6.10-0" {
		t.Fatalf("expected etcd tag output, got %q", got)
	}
}

func TestSkipMembershipReconcileIgnoresAlreadyAppliedCARotation(t *testing.T) {
	cfg := config.Config{
		Shared: config.SharedConfig{
			KubeTag: "v1.24.0",
		},
		Master: &config.MasterConfig{},
		Trigger: config.TriggerConfig{
			CARotationID:        "rotate-123",
			AppliedCARotationID: "rotate-123",
		},
	}

	if skipMembershipReconcile(cfg) {
		t.Fatalf("expected normal etcd membership reconciliation after applied CA rotation")
	}
}

func TestStaticInitialClusterMembers(t *testing.T) {
	master := func(instance, initial string, n int) config.Config {
		return config.Config{
			Shared: config.SharedConfig{InstanceName: instance, NodegroupRole: "master"},
			Master: &config.MasterConfig{NumberOfMasters: n, InitialCluster: initial},
		}
	}
	tests := []struct {
		name     string
		cfg      config.Config
		wantOK   bool
		wantList string
	}{
		{
			name:     "explicit ETCD_INITIAL_CLUSTER wins",
			cfg:      master("kube-x-master-1", "a=https://10.0.0.1:2380,b=https://10.0.0.2:2380", 3),
			wantOK:   true,
			wantList: "a=https://10.0.0.1:2380,b=https://10.0.0.2:2380",
		},
		{
			name:     "single master self-bootstraps",
			cfg:      master("kube-x-master-0", "", 1),
			wantOK:   true,
			wantList: "kube-x-master-0=https://10.9.9.9:2380",
		},
		{
			name:     "first master (master-0) self-bootstraps in multi-master",
			cfg:      master("kube-x-master-0", "", 3),
			wantOK:   true,
			wantList: "kube-x-master-0=https://10.9.9.9:2380",
		},
		{
			name:   "non-first master without a list cannot bootstrap",
			cfg:    master("kube-x-master-2", "", 3),
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := staticInitialClusterMembers(tt.cfg, "10.9.9.9", "https")
			if ok != tt.wantOK {
				t.Fatalf("ok = %t, want %t (list=%q)", ok, tt.wantOK, got)
			}
			if ok && got != tt.wantList {
				t.Fatalf("list = %q, want %q", got, tt.wantList)
			}
		})
	}
}

func TestBuildConfigNewStatic(t *testing.T) {
	cfg := config.Config{
		Shared: config.SharedConfig{InstanceName: "kube-x-master-0", ClusterUUID: "uuid-123", TLSDisabled: true},
		Master: &config.MasterConfig{NumberOfMasters: 1},
	}
	members := "kube-x-master-0=http://10.9.9.9:2380"
	conf := buildConfig(cfg, "10.9.9.9", "http", "new-static", members)

	for _, want := range []string{
		"initial-cluster: \"" + members + "\"",
		"initial-cluster-state: \"new\"",
		"initial-cluster-token: \"etcd-uuid-123\"",
	} {
		if !strings.Contains(conf, want) {
			t.Fatalf("new-static config missing %q\n---\n%s", want, conf)
		}
	}
	if strings.Contains(conf, "discovery:") {
		t.Fatalf("new-static config must not contain a discovery URL\n---\n%s", conf)
	}
}

func TestEtcdClusterToken(t *testing.T) {
	withUUID := config.Config{Shared: config.SharedConfig{ClusterUUID: "abc"}}
	if got := etcdClusterToken(withUUID); got != "etcd-abc" {
		t.Fatalf("token with uuid = %q, want etcd-abc", got)
	}
	none := config.Config{}
	if got := etcdClusterToken(none); got != "magnum-etcd-cluster" {
		t.Fatalf("token without uuid = %q, want magnum-etcd-cluster", got)
	}
}

func TestNeedsSeedWait(t *testing.T) {
	base := func() config.Config {
		return config.Config{
			Shared: config.SharedConfig{InstanceName: "c-xyz-master-1", NodegroupRole: "master"},
			Master: &config.MasterConfig{NumberOfMasters: 3},
		}
	}

	// Non-first master, nothing reachable, no static list → must wait.
	if !needsSeedWait(base(), false, false, false) {
		t.Fatal("non-first master with no LB/local/member should wait for seed")
	}
	// First master never waits — it is the seed.
	first := base()
	first.Shared.InstanceName = "c-xyz-master-0"
	if needsSeedWait(first, false, false, false) {
		t.Fatal("first master must not wait (it bootstraps the seed)")
	}
	// LB already healthy → no wait, join directly.
	if needsSeedWait(base(), true, false, false) {
		t.Fatal("must not wait once the LB seed is reachable")
	}
	// Already a member → no wait.
	if needsSeedWait(base(), false, false, true) {
		t.Fatal("existing member must not wait")
	}
	// Local etcd healthy → no wait.
	if needsSeedWait(base(), false, true, false) {
		t.Fatal("node with healthy local etcd must not wait")
	}
	// Static ETCD_INITIAL_CLUSTER present → bootstrap statically, no wait.
	static := base()
	static.Master.InitialCluster = "m0=https://10.0.0.1:2380,m1=https://10.0.0.2:2380"
	if needsSeedWait(static, false, false, false) {
		t.Fatal("static initial-cluster must bootstrap without waiting")
	}
}

func TestIsTransientMemberAddErr(t *testing.T) {
	// The error string carries etcd's stderr (see host.RunCapture), so we match
	// on the real wire message that wedged a create.
	transient := []string{
		"/usr/local/bin/etcdctl member add: exit status 1 (stderr: etcdserver: re-configuration failed due to not enough started members)",
		"member add: exit status 1 (stderr: etcdserver: unhealthy cluster)",
		"member add: exit status 1 (stderr: context deadline exceeded)",
		"member add: exit status 1 (stderr: etcdserver: request timed out)",
		"member add: exit status 1 (stderr: rpc error: code = Unavailable: no leader)",
	}
	for _, m := range transient {
		if !isTransientMemberAddErr(errString(m)) {
			t.Errorf("expected transient (retryable): %q", m)
		}
	}
	fatal := []string{
		"member add: exit status 1 (stderr: etcdserver: bad peer url)",
		"member add: exit status 1 (stderr: invalid certificate)",
	}
	for _, m := range fatal {
		if isTransientMemberAddErr(errString(m)) {
			t.Errorf("expected fatal (not retryable): %q", m)
		}
	}
	if isTransientMemberAddErr(nil) {
		t.Error("nil error must not be transient")
	}
}

func TestIsAlreadyMemberErr(t *testing.T) {
	yes := []string{
		"member add: exit status 1 (stderr: Error: Peer URLs already exists)",
		"member add: exit status 1 (stderr: etcdserver: member already exists)",
	}
	for _, m := range yes {
		if !isAlreadyMemberErr(errString(m)) {
			t.Errorf("expected already-exists: %q", m)
		}
	}
	if isAlreadyMemberErr(errString("not enough started members")) {
		t.Error("transient quorum error is not an already-exists error")
	}
}

func TestParseMemberList(t *testing.T) {
	out := strings.Join([]string{
		" e89abea5794bf78, started, kube-x-master-0, https://10.0.0.93:2380, https://10.0.0.93:2379, false",
		"f78f5dfdf2353870, started, kube-x-master-2, https://10.0.0.188:2380, https://10.0.0.188:2379, false",
		"aaaa, unstarted, , https://10.0.0.50:2380, , false",                                   // added-but-not-started: empty name
		"bbbb, started, kube-x-master-1, https://10.0.0.77:2380, https://10.0.0.77:2379, true", // learner mid-join
		"garbage line with no commas",
	}, "\n")
	ms := parseMemberList(out)
	if len(ms) != 4 {
		t.Fatalf("expected 4 parsed members (garbage skipped), got %d: %+v", len(ms), ms)
	}
	if ms[0].name != "kube-x-master-0" || ms[0].peerURL != "https://10.0.0.93:2380" {
		t.Fatalf("member[0] mis-parsed: %+v", ms[0])
	}
	if !ms[0].started || ms[0].isLearner {
		t.Fatalf("member[0] should be started voter, got started=%t isLearner=%t", ms[0].started, ms[0].isLearner)
	}
	if ms[2].name != "" || ms[2].started {
		t.Fatalf("expected empty-name unstarted member, got name=%q started=%t", ms[2].name, ms[2].started)
	}
	if !ms[3].isLearner || !ms[3].started {
		t.Fatalf("member[3] should be a started learner, got started=%t isLearner=%t", ms[3].started, ms[3].isLearner)
	}
}

func TestParseMemberListShortRowDefaults(t *testing.T) {
	// A 5-field row (no trailing isLearner) must not panic and must default to a
	// non-learner started voter rather than dropping the member.
	ms := parseMemberList("cccc, started, kube-x-master-3, https://10.0.0.9:2380, https://10.0.0.9:2379")
	if len(ms) != 1 {
		t.Fatalf("expected 1 member from a 5-field row, got %d", len(ms))
	}
	if ms[0].isLearner {
		t.Fatalf("missing isLearner field must default to false, got true")
	}
}

func TestMemberAddArgsLearnerFlag(t *testing.T) {
	base := []string{"--endpoints=https://lb:2379"}
	voter := memberAddArgs(base, "kube-x-master-1", "https://10.0.0.1:2380", false)
	if got := strings.Join(voter, " "); !strings.Contains(got, "member add kube-x-master-1 --peer-urls=https://10.0.0.1:2380") || strings.Contains(got, "--learner") {
		t.Fatalf("voter add args wrong: %q", got)
	}
	learner := memberAddArgs(base, "kube-x-master-1", "https://10.0.0.1:2380", true)
	if got := strings.Join(learner, " "); !strings.HasSuffix(got, "--learner") {
		t.Fatalf("learner add args must end with --learner: %q", got)
	}
}

func TestClassifyPromoteErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want promoteErrClass
	}{
		{"nil is success", nil, promoteAlreadyVoter},
		{"not in sync retries", errString("etcdserver: can only promote a learner member which is in sync with leader"), promoteRetry},
		{"bare not-a-learner is already voter", errString("etcdserver: can only promote a learner member"), promoteAlreadyVoter},
		{"member not found is success", errString("etcdserver: member not found"), promoteAlreadyVoter},
		{"transient no leader retries", errString("rpc error: code = Unavailable: no leader"), promoteRetry},
		{"context deadline retries", errString("context deadline exceeded"), promoteRetry},
		{"routed onto learner retries", errString("etcdserver: rpc not supported for learner"), promoteRetry},
		{"cert error is fatal", errString("etcdserver: invalid certificate"), promoteFatal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyPromoteErr(c.err); got != c.want {
				t.Fatalf("classifyPromoteErr(%v) = %d, want %d", c.err, got, c.want)
			}
		})
	}
}

func TestParseMemberListClientURL(t *testing.T) {
	ms := parseMemberList("aaaa, started, kube-x-master-0, https://10.0.0.1:2380, https://10.0.0.1:2379, false")
	if len(ms) != 1 || ms[0].clientURL != "https://10.0.0.1:2379" {
		t.Fatalf("clientURL mis-parsed: %+v", ms)
	}
	// 4-field row: no clientURL, must not panic.
	ms = parseMemberList("bbbb, unstarted, , https://10.0.0.2:2380")
	if len(ms) != 1 || ms[0].clientURL != "" {
		t.Fatalf("missing clientURL must default to empty: %+v", ms)
	}
}

func TestVoterClientEndpoint(t *testing.T) {
	members := []etcdMember{
		{id: "self", name: "m2", clientURL: "https://10.0.0.3:2379", started: true, isLearner: true},
		{id: "unstarted", name: "", clientURL: "https://10.0.0.4:2379", started: false},
		{id: "learner2", name: "m3", clientURL: "https://10.0.0.5:2379", started: true, isLearner: true},
		{id: "voter", name: "m0", clientURL: "https://10.0.0.1:2379", started: true},
	}
	// Must skip self, unstarted and learners — only the started voter qualifies.
	if got := voterClientEndpoint(members, "self"); got != "https://10.0.0.1:2379" {
		t.Fatalf("voterClientEndpoint = %q, want the started voter", got)
	}
	// No voter at all → empty (caller falls back to the LB).
	if got := voterClientEndpoint(members[:3], "self"); got != "" {
		t.Fatalf("expected empty endpoint with no voters, got %q", got)
	}
}

func TestSelectSelfAndPromoteAction(t *testing.T) {
	members := []etcdMember{
		{id: "1", name: "kube-x-master-0", peerURL: "https://10.0.0.1:2380", isLearner: false},
		{id: "2", name: "kube-x-master-1", peerURL: "https://10.0.0.2:2380", isLearner: true},
		{id: "3", name: "kube-x-master-1", peerURL: "https://10.9.9.9:2380", isLearner: false}, // stale same-name, different IP
	}

	// Exact name+peer match picks the learner row, not the stale same-name entry.
	self, ok := selectSelf(members, "kube-x-master-1", "https://10.0.0.2:2380")
	if !ok || self.id != "2" {
		t.Fatalf("selectSelf picked wrong row: %+v ok=%t", self, ok)
	}
	if promoteAction(self, ok) != promoteDo {
		t.Fatal("a learner self must be promoted")
	}

	// A voter self is a no-op.
	voter, ok := selectSelf(members, "kube-x-master-0", "https://10.0.0.1:2380")
	if !ok || promoteAction(voter, ok) != promoteNoop {
		t.Fatalf("a voter self must be a no-op: %+v ok=%t", voter, ok)
	}

	// Not a member → no-op.
	if _, ok := selectSelf(members, "kube-x-master-9", "https://10.0.0.9:2380"); ok {
		t.Fatal("absent node must not match")
	}
	if promoteAction(etcdMember{}, false) != promoteNoop {
		t.Fatal("not-found must be a no-op")
	}
}

func TestIsTransientMemberAddErrLearnerLimit(t *testing.T) {
	if !isTransientMemberAddErr(errString("member add: exit status 1 (stderr: etcdserver: too many learner members in cluster)")) {
		t.Fatal("too-many-learners must be transient so concurrent joiners serialize")
	}
}

func TestMemberClientEndpoint(t *testing.T) {
	cases := []struct{ peer, want string }{
		{"https://10.0.0.207:2380", "https://10.0.0.207:2379"},
		{"http://10.0.0.5:2380", "http://10.0.0.5:2379"},
		{"", ""},
	}
	for _, c := range cases {
		if got := memberClientEndpoint(c.peer); got != c.want {
			t.Errorf("memberClientEndpoint(%q) = %q, want %q", c.peer, got, c.want)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestMemberMatchesSelfExactness(t *testing.T) {
	cases := []struct {
		name         string
		member       etcdMember
		instanceName string
		nodeIP       string
		want         bool
	}{
		{"exact name", etcdMember{name: "c1-master-1"}, "c1-master-1", "", true},
		{"name prefix collision", etcdMember{name: "c1-master-10"}, "c1-master-1", "", false},
		{"exact peer IP", etcdMember{peerURL: "https://10.0.0.5:2380"}, "", "10.0.0.5", true},
		{"peer IP prefix collision", etcdMember{peerURL: "https://10.0.0.57:2380"}, "", "10.0.0.5", false},
		{"unstarted member by peer URL", etcdMember{name: "", peerURL: "https://10.0.0.5:2380"}, "c1-master-1", "10.0.0.5", true},
		{"empty identity never matches", etcdMember{name: "", peerURL: ""}, "", "", false},
		{"http peer on tls-disabled", etcdMember{peerURL: "http://10.0.0.5:2380"}, "", "10.0.0.5", true},
	}
	for _, tc := range cases {
		if got := memberMatchesSelf(tc.member, tc.instanceName, tc.nodeIP); got != tc.want {
			t.Errorf("%s: memberMatchesSelf=%v want %v", tc.name, got, tc.want)
		}
	}
}

func TestEtcdTagMajorMinor(t *testing.T) {
	if maj, min, ok := etcdTagMajorMinor("3.6.10-0"); !ok || maj != 3 || min != 6 {
		t.Fatalf("got %d.%d ok=%v", maj, min, ok)
	}
	if _, _, ok := etcdTagMajorMinor("garbage"); ok {
		t.Fatal("garbage must not parse")
	}
}

func TestEtcdUnitTagRegex(t *testing.T) {
	unit := `ExecStart=/bin/podman run \
    registry.k8s.io/etcd:3.4.13-0 \
    /usr/local/bin/etcd`
	m := etcdUnitTagRe.FindStringSubmatch(unit)
	if m == nil || m[1] != "3.4.13-0" {
		t.Fatalf("got %v", m)
	}
}
