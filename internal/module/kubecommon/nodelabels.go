package kubecommon

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/kubeletconfig"
)

type nodeDocument struct {
	Metadata struct {
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
}

func EnsureNodeLabels(cfg config.Config, executor *host.Executor, kubectl, kubeconfig string, apply bool, retries int, interval time.Duration) ([]host.Change, error) {
	labels, err := fetchNodeLabelsWithRetry(cfg, executor, kubectl, kubeconfig, apply, retries, interval)
	if err != nil {
		return nil, err
	}
	desired, remove := desiredNodeLabels(cfg)
	var changes []host.Change
	for key, value := range desired {
		current, ok := labels[key]
		if ok && current == value {
			continue
		}
		changes = append(changes, host.Change{Action: host.ActionUpdate, Summary: fmt.Sprintf("set node label %s=%q on %s", key, value, cfg.Shared.InstanceName)})
		if apply {
			if err := executor.Run(kubectl, "--kubeconfig="+kubeconfig, "label", "node", cfg.Shared.InstanceName, fmt.Sprintf("%s=%s", key, value), "--overwrite"); err != nil {
				return changes, err
			}
		}
	}
	for _, key := range remove {
		if _, ok := labels[key]; !ok {
			continue
		}
		changes = append(changes, host.Change{Action: host.ActionUpdate, Summary: fmt.Sprintf("remove node label %s from %s", key, cfg.Shared.InstanceName)})
		if apply {
			if err := executor.Run(kubectl, "--kubeconfig="+kubeconfig, "label", "node", cfg.Shared.InstanceName, key+"-"); err != nil {
				return changes, err
			}
		}
	}
	return changes, nil
}

func fetchNodeLabelsWithRetry(cfg config.Config, executor *host.Executor, kubectl, kubeconfig string, apply bool, retries int, interval time.Duration) (map[string]string, error) {
	if retries < 1 {
		retries = 1
	}
	var lastErr error
	for attempt := 1; attempt <= retries; attempt++ {
		labels, err := fetchNodeLabels(cfg, executor, kubectl, kubeconfig)
		if err == nil {
			return labels, nil
		}
		lastErr = err
		if !apply || attempt == retries {
			break
		}
		time.Sleep(interval)
	}
	return nil, lastErr
}

func fetchNodeLabels(cfg config.Config, executor *host.Executor, kubectl, kubeconfig string) (map[string]string, error) {
	out, err := executor.RunCapture(kubectl, "--kubeconfig="+kubeconfig, "get", "node", cfg.Shared.InstanceName, "-o", "json")
	if err != nil {
		return nil, err
	}
	var node nodeDocument
	if err := json.Unmarshal([]byte(out), &node); err != nil {
		return nil, err
	}
	if node.Metadata.Labels == nil {
		return map[string]string{}, nil
	}
	return node.Metadata.Labels, nil
}

func desiredNodeLabels(cfg config.Config) (map[string]string, []string) {
	desired := map[string]string{
		"magnum.openstack.org/role": cfg.Shared.NodegroupRole,
	}
	if cfg.Shared.NodegroupName != "" {
		desired["magnum.openstack.org/nodegroup"] = cfg.Shared.NodegroupName
	}
	remove := make([]string, 0, 2)
	if cfg.Role() == config.RoleMaster {
		if kubeletconfig.KubeMinorAtLeast(cfg.Shared.KubeTag, 25) {
			desired["node-role.kubernetes.io/control-plane"] = ""
			remove = append(remove, "node-role.kubernetes.io/master")
		} else if kubeletconfig.KubeMinorAtLeast(cfg.Shared.KubeTag, 20) {
			desired["node-role.kubernetes.io/master"] = ""
			desired["node-role.kubernetes.io/control-plane"] = ""
		} else {
			desired["node-role.kubernetes.io/master"] = ""
			remove = append(remove, "node-role.kubernetes.io/control-plane")
		}
	}
	return desired, remove
}
