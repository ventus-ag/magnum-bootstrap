package plan

// masterPhases returns the unified phase list for master nodes.
// Every operation (create, upgrade, resize, ca-rotate, periodic) uses the
// same list.  Each module internally checks desired vs current state and
// only acts when something actually needs changing.
//
// The "stop-services" and "start-services" phases are included but only
// perform drain/uncordon when an upgrade or resize is active.
// The "ca-rotation" phase is always present but is a no-op unless the
// CA_ROTATION_ID changed since the last run.
func masterPhases() []Phase {
	return []Phase{
		newPhase("prereq-validation", "validate desired master input and prerequisites", false),
		newPhase("ca-rotation", "rotate cluster CA material if rotation ID changed", true),
		newPhase("container-runtime", "reconcile the container runtime to desired state", true),
		newPhase("client-tools", "reconcile kubernetes client binaries", false),
		newPhase("master-certificates", "reconcile master certificate material", true),
		newPhase("cert-api-manager", "reconcile certificate API manager bootstrap", true),
		newPhase("etcd", "reconcile etcd service, membership, and config", true),
		newPhase("kube-os-config", "reconcile kubernetes OS-level config files", false),
		newPhase("admin-kubeconfig", "reconcile admin kubeconfig and exported access files", false),
		newPhase("stop-services", "drain node before disruptive changes (upgrade/resize only)", true),
		newPhase("kube-master-config", "reconcile control-plane config and units", true),
		newPhase("storage", "reconcile container storage configuration", true),
		newPhase("proxy-env", "reconcile environment proxy settings", false),
		newPhase("services", "reconcile master services to desired state", true),
		newPhase("start-services", "uncordon node after disruptive changes (upgrade/resize only)", true),
		newPhase("health", "verify master health after reconciliation", false),
		// Cluster-level addons — only execute on master-0, skip on other masters.
		newPhase("cluster-rbac", "reconcile RBAC roles, secrets, and pod security for cluster", false),
		newPhase("cluster-cleanup-deprecated", "clean up deprecated pre-Helm cluster addon resources", false),
		newPhase("cluster-flannel", "reconcile flannel CNI via Helm", false),
		newPhase("cluster-coredns", "reconcile CoreDNS via Helm", false),
		newPhase("cluster-occm", "reconcile OpenStack Cloud Controller Manager via Helm", false),
		newPhase("cluster-cinder-csi", "reconcile Cinder CSI driver via Helm", false),
		newPhase("cluster-manila-csi", "reconcile Manila CSI driver via Helm", false),
		newPhase("cluster-metrics-server", "reconcile metrics server via Helm", false),
		newPhase("cluster-dashboard", "reconcile Kubernetes Dashboard via Helm", false),
		newPhase("cluster-auto-healer", "reconcile node problem detector and auto-healer", false),
		newPhase("cluster-autoscaler", "reconcile cluster autoscaler via Helm", false),
		newPhase("cluster-gpu-operator", "reconcile NVIDIA GPU Operator via Helm", false),
		newPhase("cluster-health", "verify cluster pods are healthy, restart crashlooping pods", false),
		newPhase("zincati", "reconcile OS auto-upgrade (Zincati) settings", false),
	}
}

// workerPhases returns the unified phase list for worker nodes.
func workerPhases() []Phase {
	return []Phase{
		newPhase("prereq-validation", "validate desired worker input and prerequisites", false),
		newPhase("ca-rotation", "rotate cluster CA material if rotation ID changed", true),
		newPhase("container-runtime", "reconcile the container runtime to desired state", true),
		newPhase("client-tools", "reconcile kubernetes client binaries", false),
		newPhase("kube-os-config", "reconcile kubernetes OS-level config files", false),
		newPhase("worker-certificates", "reconcile worker certificate material", true),
		newPhase("registry", "reconcile container registry integration", false),
		newPhase("admin-kubeconfig", "reconcile admin kubeconfig and exported access files", false),
		newPhase("stop-services", "drain node before disruptive changes (upgrade/resize only)", true),
		newPhase("kube-worker-config", "reconcile kubelet and kube-proxy config and units", true),
		newPhase("storage", "reconcile container storage configuration", true),
		newPhase("proxy-env", "reconcile environment proxy settings", false),
		newPhase("services", "reconcile worker services to desired state", true),
		newPhase("start-services", "uncordon node after disruptive changes (upgrade/resize only)", true),
		newPhase("health", "verify worker health after reconciliation", false),
		newPhase("zincati", "reconcile OS auto-upgrade (Zincati) settings", false),
	}
}
