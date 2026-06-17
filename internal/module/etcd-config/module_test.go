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
