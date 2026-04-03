package module

import (
	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	adminkubeconfig "github.com/ventus-ag/magnum-bootstrap/internal/module/admin-kubeconfig"
	carotation "github.com/ventus-ag/magnum-bootstrap/internal/module/ca-rotation"
	certapimanager "github.com/ventus-ag/magnum-bootstrap/internal/module/cert-api-manager"
	clienttools "github.com/ventus-ag/magnum-bootstrap/internal/module/client-tools"
	clusterautohealer "github.com/ventus-ag/magnum-bootstrap/internal/module/cluster-auto-healer"
	clusterautoscaler "github.com/ventus-ag/magnum-bootstrap/internal/module/cluster-autoscaler"
	clustercindercsi "github.com/ventus-ag/magnum-bootstrap/internal/module/cluster-cinder-csi"
	clustercoredns "github.com/ventus-ag/magnum-bootstrap/internal/module/cluster-coredns"
	clusterflannel "github.com/ventus-ag/magnum-bootstrap/internal/module/cluster-flannel"
	clustermanilacsi "github.com/ventus-ag/magnum-bootstrap/internal/module/cluster-manila-csi"
	clustermetricsserver "github.com/ventus-ag/magnum-bootstrap/internal/module/cluster-metrics-server"
	clusteroccm "github.com/ventus-ag/magnum-bootstrap/internal/module/cluster-occm"
	clusterrbac "github.com/ventus-ag/magnum-bootstrap/internal/module/cluster-rbac"
	containerruntime "github.com/ventus-ag/magnum-bootstrap/internal/module/container-runtime"
	dockerregistry "github.com/ventus-ag/magnum-bootstrap/internal/module/docker-registry"
	etcdconfig "github.com/ventus-ag/magnum-bootstrap/internal/module/etcd-config"
	"github.com/ventus-ag/magnum-bootstrap/internal/module/health"
	kubemasterconfig "github.com/ventus-ag/magnum-bootstrap/internal/module/kube-master-config"
	kubeosconfig "github.com/ventus-ag/magnum-bootstrap/internal/module/kube-os-config"
	kubeworkerconfig "github.com/ventus-ag/magnum-bootstrap/internal/module/kube-worker-config"
	mastercerts "github.com/ventus-ag/magnum-bootstrap/internal/module/master-certs"
	prereqvalidation "github.com/ventus-ag/magnum-bootstrap/internal/module/prereq-validation"
	"github.com/ventus-ag/magnum-bootstrap/internal/module/proxy"
	"github.com/ventus-ag/magnum-bootstrap/internal/module/services"
	startservices "github.com/ventus-ag/magnum-bootstrap/internal/module/start-services"
	stopservices "github.com/ventus-ag/magnum-bootstrap/internal/module/stop-services"
	"github.com/ventus-ag/magnum-bootstrap/internal/module/storage"
	workercerts "github.com/ventus-ag/magnum-bootstrap/internal/module/worker-certs"
	"github.com/ventus-ag/magnum-bootstrap/internal/plan"
)

func BuildRegistry(_ config.Config) map[string]Module {
	return map[string]Module{
		"prereq-validation":   prereqvalidation.Module{},
		"container-runtime":   containerruntime.Module{},
		"client-tools":        clienttools.Module{},
		"master-certificates": mastercerts.Module{},
		"worker-certificates": workercerts.Module{},
		"cert-api-manager":    certapimanager.Module{},
		"etcd":                etcdconfig.Module{},
		"kube-os-config":      kubeosconfig.Module{},
		"admin-kubeconfig":    adminkubeconfig.Module{},
		"kube-master-config":  kubemasterconfig.Module{},
		"kube-worker-config":  kubeworkerconfig.Module{},
		"registry":            dockerregistry.Module{},
		"storage":             storage.Module{},
		"services":            services.Module{},
		"stop-services":       stopservices.Module{},
		"start-services":      startservices.Module{},
		"proxy-env":           proxy.Module{},
		"health":              health.Module{},
		"ca-rotation":         carotation.Module{},
		// Cluster-level addons (master-0 only).
		"cluster-rbac":           clusterrbac.Module{},
		"cluster-flannel":        clusterflannel.Module{},
		"cluster-coredns":        clustercoredns.Module{},
		"cluster-occm":           clusteroccm.Module{},
		"cluster-cinder-csi":     clustercindercsi.Module{},
		"cluster-manila-csi":     clustermanilacsi.Module{},
		"cluster-metrics-server": clustermetricsserver.Module{},
		"cluster-auto-healer":    clusterautohealer.Module{},
		"cluster-autoscaler":     clusterautoscaler.Module{},
	}
}

func MissingPhases(registry map[string]Module, reconcilePlan plan.Plan) []string {
	missing := make([]string, 0)
	for _, phase := range reconcilePlan.Phases {
		if _, ok := registry[phase.ID]; !ok {
			missing = append(missing, phase.ID)
		}
	}
	return missing
}
