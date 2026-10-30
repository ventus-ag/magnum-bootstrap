package clusteroccm

import (
	"testing"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
)

func TestCloudConfigValuesSetsClusterSubnet(t *testing.T) {
	cfg := config.Config{}
	cfg.Shared.ClusterSubnet = "c807b82d-cb98-47d7-86a5-4559594cf810"

	lb, ok := cloudConfigValues(cfg)["loadBalancer"].(map[string]interface{})
	if !ok {
		t.Fatal("loadBalancer section missing")
	}
	if got := lb["subnet-id"]; got != cfg.Shared.ClusterSubnet {
		t.Fatalf("subnet-id = %v, want %s", got, cfg.Shared.ClusterSubnet)
	}
}

func TestCloudConfigValuesOmitsEmptySubnet(t *testing.T) {
	values := cloudConfigValues(config.Config{})
	if _, ok := values["loadBalancer"]; ok {
		t.Fatal("loadBalancer section rendered without a cluster subnet")
	}
	if _, ok := values["global"]; !ok {
		t.Fatal("global section missing")
	}
}
