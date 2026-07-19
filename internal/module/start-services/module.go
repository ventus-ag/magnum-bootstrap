package startservices

import (
	"context"
	"fmt"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
	"github.com/ventus-ag/magnum-bootstrap/internal/module/kubecommon"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
	"github.com/ventus-ag/magnum-bootstrap/provider/hostsdk"
)

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string        { return "start-services" }
func (Module) Dependencies() []string { return []string{"services"} }

func (Module) Run(_ context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	executor := host.NewExecutor(req.Apply, req.Logger)
	var changes []host.Change
	var warnings []string
	disruptiveCycle := moduleapi.DisruptiveServiceCycleNeeded(cfg, req)

	if disruptiveCycle {
		// Reload systemd and restart services.
		if err := executor.Run("systemctl", "daemon-reload"); err != nil {
			return moduleapi.Result{}, err
		}

		var serviceList []string
		if cfg.Role() == config.RoleMaster {
			serviceList = []string{"kube-apiserver", "kube-controller-manager", "kube-scheduler", "kubelet", "kube-proxy"}
		} else {
			serviceList = []string{"kubelet", "kube-proxy"}
		}

		// Only start services that aren't already running.
		for _, svc := range serviceList {
			if !executor.SystemctlIsActive(svc) {
				result, err := (hostresource.SystemdServiceSpec{Unit: svc, Active: hostresource.BoolPtr(true)}).Apply(executor)
				if err != nil {
					return moduleapi.Result{}, fmt.Errorf("start %s: %w", svc, err)
				}
				if req.Apply && !executor.WaitForSystemctlActive(svc, serviceReadyTimeout(svc), 2*time.Second) {
					return moduleapi.Result{}, fmt.Errorf("service %s did not become active after start", svc)
				}
				changes = append(changes, result.Changes...)
			}
		}
	}

	wasCordoned := false
	if req.Apply {
		kubeconfig := "/etc/kubernetes/admin.conf"
		kubectl := "/srv/magnum/bin/kubectl"

		cordonState, uncordonChanges, uncordonWarnings := reconcileNodeSchedulable(cfg.Shared.InstanceName, executor, kubectl, kubeconfig)
		wasCordoned = cordonState
		changes = append(changes, uncordonChanges...)
		warnings = append(warnings, uncordonWarnings...)

		if disruptiveCycle || wasCordoned {
			// Create uncordon service for reboot resilience.
			uncordonService := buildUncordonService(cfg)
			change, err := (hostresource.FileSpec{Path: "/etc/systemd/system/uncordon.service", Content: []byte(uncordonService), Mode: 0o644}).Apply(executor)
			if err != nil {
				return moduleapi.Result{}, err
			}
			changes = append(changes, change.Changes...)
			serviceResult, err := (hostresource.SystemdServiceSpec{Unit: "uncordon.service", DaemonReload: change.Changed, Enabled: hostresource.BoolPtr(true)}).Apply(executor)
			if err != nil {
				return moduleapi.Result{}, err
			}
			changes = append(changes, serviceResult.Changes...)
		}

		labelChanges, err := kubecommon.EnsureNodeMetadata(cfg, executor, kubectl, kubecommon.MetadataKubeconfig(cfg), true, 30, 5*time.Second)
		changes = append(changes, labelChanges...)
		if err != nil && req.Logger != nil {
			req.Logger.Warnf("start-services: failed to reconcile node metadata for %s: %v", cfg.Shared.InstanceName, err)
		}
	}

	return moduleapi.Result{
		Changes:  changes,
		Warnings: warnings,
		Outputs:  map[string]string{"operation": cfg.Operation().String()},
	}, nil
}

func reconcileNodeSchedulable(nodeName string, executor *host.Executor, kubectl, kubeconfig string) (bool, []host.Change, []string) {
	cordoned, err := nodeIsCordoned(nodeName, executor, kubectl, kubeconfig)
	if err != nil {
		return false, nil, []string{fmt.Sprintf("start-services: failed to determine cordon state for %s: %v", nodeName, err)}
	}
	if !cordoned {
		return false, nil, nil
	}

	var lastErr error
	for i := 0; i < 30; i++ {
		if _, err := executor.RunCapture(kubectl, "--kubeconfig="+kubeconfig, "uncordon", nodeName); err == nil {
			return true, []host.Change{{Action: host.ActionOther, Summary: fmt.Sprintf("uncordon node %s", nodeName)}}, nil
		} else {
			lastErr = err
		}
		time.Sleep(5 * time.Second)
	}

	return true, nil, []string{fmt.Sprintf("start-services: node %s remains cordoned after uncordon retries: %v", nodeName, lastErr)}
}

func nodeIsCordoned(nodeName string, executor *host.Executor, kubectl, kubeconfig string) (bool, error) {
	out, err := executor.RunCapture(kubectl, "--kubeconfig="+kubeconfig,
		"get", "node", nodeName, "-o", "jsonpath={.spec.unschedulable}")
	if err != nil {
		return false, err
	}
	return out == "true", nil
}

func buildUncordonService(cfg config.Config) string {
	kubectl := "/srv/magnum/bin/kubectl"
	kubeconfig := "/etc/kubernetes/admin.conf"
	// Reboot-resilience for the upgrade window only: retry until the API
	// accepts the uncordon, then disable the unit. The legacy bash unit had
	// Restart=always and stayed enabled — it re-uncordoned the node every 10s
	// forever, silently defeating any operator `kubectl cordon` and racing
	// the next upgrade's own drain window.
	return fmt.Sprintf(`[Unit]
Description=magnum-uncordon
After=network.target kubelet.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh -c 'for i in $(seq 1 120); do %s --kubeconfig=%s uncordon %s && exit 0; sleep 10; done; exit 1'
ExecStartPost=/usr/bin/systemctl disable uncordon.service

[Install]
WantedBy=multi-user.target
`, kubectl, kubeconfig, cfg.Shared.InstanceName)
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:module:StartServices", name, res, opts...); err != nil {
		return nil, err
	}
	childOpts := hostresource.ChildResourceOptions(res, opts...)
	fileRes, err := hostsdk.RegisterFileSpec(ctx, name+"-uncordon-file", hostresource.FileSpec{Path: "/etc/systemd/system/uncordon.service", Content: []byte(buildUncordonService(heat.Cfg)), Mode: 0o644}, childOpts...)
	if err != nil {
		return nil, err
	}
	// Deliberately no SystemdService(Enabled) registration for uncordon: the
	// unit disables itself after a successful uncordon, and Run() only
	// enables it during a disruptive cycle. A standing Enabled=true resource
	// would re-enable it on every reconcile and bring back the perpetual
	// boot-time uncordon this unit design removes.
	_ = fileRes
	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"operation": pulumi.String(heat.Cfg.Operation().String()),
	}); err != nil {
		return nil, err
	}
	return res, nil
}

func serviceReadyTimeout(service string) time.Duration {
	switch service {
	case "kube-apiserver", "kube-controller-manager", "kube-scheduler", "kubelet":
		return 60 * time.Second
	default:
		return 30 * time.Second
	}
}
