package moduleapi

import (
	"context"
	"sync"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/logging"
	"github.com/ventus-ag/magnum-bootstrap/internal/paths"
)

// Request carries the runtime context for a module's Run() call.
type Request struct {
	Apply        bool
	AllowPartial bool
	Logger       *logging.Logger
	Paths        paths.Paths

	// PreviousKubeTag is the KUBE_TAG from the last successful reconcile run.
	// Used by stop-services/start-services to detect actual upgrades (KUBE_TAG
	// changed) vs stale IS_UPGRADE=true that never resets in heat-params.
	PreviousKubeTag string

	// Restarts is a shared tracker that modules use to signal which systemd
	// services need a restart.  Config-writing modules call Restarts.Add()
	// when they change a file that affects a running service.  The "services"
	// module reads the tracker and only restarts what actually changed.
	Restarts *RestartTracker
}

// RestartTracker is a thread-safe set of systemd service names that need
// restarting.  It is shared across all modules within a single reconcile run.
type RestartTracker struct {
	mu       sync.Mutex
	services map[string]string // service name → reason
}

// NewRestartTracker creates an empty restart tracker.
func NewRestartTracker() *RestartTracker {
	return &RestartTracker{services: make(map[string]string)}
}

// Add marks a service as needing restart, with a human-readable reason.
func (rt *RestartTracker) Add(service, reason string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.services[service] = reason
}

// NeedsRestart returns true if the given service has been marked for restart.
func (rt *RestartTracker) NeedsRestart(service string) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	_, ok := rt.services[service]
	return ok
}

// All returns a copy of all services that need restart with their reasons.
func (rt *RestartTracker) All() map[string]string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	copy := make(map[string]string, len(rt.services))
	for k, v := range rt.services {
		copy[k] = v
	}
	return copy
}

// Empty returns true if no services need restart.
func (rt *RestartTracker) Empty() bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return len(rt.services) == 0
}

// Result is returned by a module's Run() call.
type Result struct {
	Changes  []host.Change
	Outputs  map[string]string
	Warnings []string
}

// HeatParamsComponent is a Pulumi component resource that wraps the parsed
// heat-params config and makes it the root dependency for all module resources.
//
// Modules receive *HeatParamsComponent in Register() instead of a plain
// config.Config. They access config values via heat.Cfg and wire Pulumi
// dependencies by using heat as their parent resource. The component also
// publishes the relevant config values as Pulumi resource outputs so Pulumi can
// diff the desired state that drove each run between executions.
type HeatParamsComponent struct {
	pulumi.ResourceState

	// Cfg is the parsed heat-params config, accessible as a plain Go value
	// in Register() implementations without unwrapping Pulumi output futures.
	Cfg config.Config
}

// NewHeatParamsComponent registers a HeatParams Pulumi component resource and
// exposes all key config fields as Pulumi outputs for state tracking and diff
// rendering.
func NewHeatParamsComponent(ctx *pulumi.Context, name string, cfg config.Config, opts ...pulumi.ResourceOption) (*HeatParamsComponent, error) {
	heat := &HeatParamsComponent{Cfg: cfg}
	if err := ctx.RegisterComponentResource("magnum:index:HeatParams", name, heat, opts...); err != nil {
		return nil, err
	}

	outputs := pulumi.Map{
		// Identity
		"role":          pulumi.String(cfg.Role().String()),
		"operation":     pulumi.String(cfg.Operation().String()),
		"inputChecksum": pulumi.String(cfg.InputChecksum),

		// Node identity
		"instanceName":  pulumi.String(cfg.Shared.InstanceName),
		"nodegroupRole": pulumi.String(cfg.Shared.NodegroupRole),
		"nodegroupName": pulumi.String(cfg.Shared.NodegroupName),

		// Kubernetes version
		"kubeTag":     pulumi.String(cfg.Shared.KubeTag),
		"kubeVersion": pulumi.String(cfg.Shared.KubeVersion),
		"arch":        pulumi.String(cfg.Shared.Arch),

		// Runtime and network
		"containerRuntime": pulumi.String(cfg.Shared.ContainerRuntime),
		"networkDriver":    pulumi.String(cfg.Shared.NetworkDriver),
		"selinuxMode":      pulumi.String(cfg.Shared.SELinuxMode),

		// Cluster identity
		"clusterUuid": pulumi.String(cfg.Shared.ClusterUUID),
		"kubeApiPort": pulumi.Int(cfg.Shared.KubeAPIPort),
		"tlsDisabled": pulumi.Bool(cfg.Shared.TLSDisabled),

		// OpenStack auth
		"authUrl":       pulumi.String(cfg.Shared.AuthURL),
		"trusteeUserId": pulumi.String(cfg.Shared.TrusteeUserID),
		"trustId":       pulumi.String(cfg.Shared.TrustID),

		// OpenStack networking
		"clusterSubnet":      pulumi.String(cfg.Shared.ClusterSubnet),
		"externalNetworkId":  pulumi.String(cfg.Shared.ExternalNetworkID),
		"clusterNetworkName": pulumi.String(cfg.Shared.ClusterNetworkName),
		"octaviaEnabled":     pulumi.Bool(cfg.Shared.OctaviaEnabled),

		// Proxy
		"httpProxy":  pulumi.String(cfg.Shared.HTTPProxy),
		"httpsProxy": pulumi.String(cfg.Shared.HTTPSProxy),
		"noProxy":    pulumi.String(cfg.Shared.NoProxy),

		// Reconciler metadata
		"reconcilerVersion": pulumi.String(cfg.Shared.ReconcilerVersion),

		// Trigger tokens
		"caRotationId": pulumi.String(cfg.Trigger.CARotationID),
	}

	// Role-specific fields
	if cfg.Master != nil {
		outputs["numberOfMasters"] = pulumi.Int(cfg.Master.NumberOfMasters)
		outputs["kubeApiPublicAddress"] = pulumi.String(cfg.Master.KubeAPIPublicAddress)
		outputs["kubeApiPrivateAddress"] = pulumi.String(cfg.Master.KubeAPIPrivateAddress)
		outputs["etcdDiscoveryUrl"] = pulumi.String(cfg.Master.EtcdDiscoveryURL)
		outputs["etcdTag"] = pulumi.String(cfg.Master.EtcdTag)
	}
	if cfg.Worker != nil {
		outputs["kubeMasterIp"] = pulumi.String(cfg.Worker.KubeMasterIP)
		outputs["etcdServerIp"] = pulumi.String(cfg.Worker.EtcdServerIP)
		outputs["registryEnabled"] = pulumi.Bool(cfg.Worker.RegistryEnabled)
		outputs["registryPort"] = pulumi.Int(cfg.Worker.RegistryPort)
	}

	if err := ctx.RegisterResourceOutputs(heat, outputs); err != nil {
		return nil, err
	}
	return heat, nil
}

// Module is the interface every reconcile phase must implement.
type Module interface {
	// PhaseID returns the stable phase identifier (e.g. "client-tools").
	PhaseID() string

	// Dependencies returns the phase IDs this module must run after.
	// Modules with no dependencies return nil — they can run in parallel
	// with other dependency-free modules.  The program builder constructs
	// a DAG from these declarations and runs independent branches in parallel.
	Dependencies() []string

	// Run executes the module's imperative host operations.
	// req.Apply gates whether changes are actually written; when false the
	// module performs a dry-run and returns planned changes only.
	Run(ctx context.Context, cfg config.Config, req Request) (Result, error)

	// Register declares the module as a Pulumi component resource whose
	// parent is heat. Using heat as the parent creates a proper Pulumi
	// dependency edge so the state JSON records which config inputs drove
	// each phase and Pulumi can diff them between runs.
	Register(ctx *pulumi.Context, name string, heat *HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error)
}
