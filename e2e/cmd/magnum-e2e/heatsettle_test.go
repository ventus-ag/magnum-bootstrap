package main

import (
	"reflect"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/orchestration/v1/stackresources"
)

func TestHeatProgress(t *testing.T) {
	group := stackresources.Resource{Name: "kube_masters", Type: "OS::Heat::ResourceGroup", PhysicalID: "g", Status: "UPDATE_COMPLETE"}
	member := stackresources.Resource{Name: "0", Type: "kubemaster-abc.yaml", PhysicalID: "m0", Status: "UPDATE_COMPLETE"}
	deploy := stackresources.Resource{Name: "master_config_deployment", Type: "OS::Heat::SoftwareDeployment", PhysicalID: "d", Status: "UPDATE_COMPLETE"}

	busy, members := heatProgress([]stackresources.Resource{group, member, deploy})
	if busy != "" || !reflect.DeepEqual(members, []string{"m0"}) {
		t.Fatalf("settled tree: busy=%q members=%v", busy, members)
	}

	deploy.Status = "UPDATE_IN_PROGRESS"
	if busy, _ := heatProgress([]stackresources.Resource{group, member, deploy}); busy != "master_config_deployment is UPDATE_IN_PROGRESS" {
		t.Fatalf("in-progress deployment not reported: %q", busy)
	}

	pending := stackresources.Resource{Name: "1", Type: "kubeminion.yaml", Status: "INIT_COMPLETE"}
	if _, members := heatProgress([]stackresources.Resource{pending}); members != nil {
		t.Fatalf("member without a stack id listed: %v", members)
	}
}
