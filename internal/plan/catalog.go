package plan

func masterCreatePhases() []Phase {
	return []Phase{
		newPhase("prereq-validation", "validate desired master input and prerequisites", false),
		newPhase("container-runtime", "install and reconcile the container runtime", true),
		newPhase("client-tools", "install kubernetes and cluster client helpers", false),
		newPhase("master-certificates", "render or refresh master certificate material", true),
		newPhase("cert-api-manager", "reconcile certificate API manager bootstrap", true),
		newPhase("etcd", "reconcile etcd service, membership, and config", true),
		newPhase("kube-os-config", "render common kubernetes OS-level config files", false),
		newPhase("admin-kubeconfig", "render admin kubeconfig and exported access files", false),
		newPhase("kube-master-config", "render control-plane config and units", true),
		newPhase("storage", "reconcile container storage configuration", true),
		newPhase("services", "enable and start master services", true),
		newPhase("proxy-env", "reconcile environment proxy settings", false),
		newPhase("health", "verify master health after reconciliation", false),
		// Cluster-level addons — only execute on master-0, skip on other masters.
		newPhase("cluster-rbac", "create RBAC roles, secrets, and pod security for cluster", false),
		newPhase("cluster-flannel", "deploy flannel CNI via Helm", false),
		newPhase("cluster-coredns", "deploy CoreDNS via Helm", false),
		newPhase("cluster-occm", "deploy OpenStack Cloud Controller Manager via Helm", false),
		newPhase("cluster-cinder-csi", "deploy Cinder CSI driver via Helm", false),
		newPhase("cluster-manila-csi", "deploy Manila CSI driver via Helm", false),
		newPhase("cluster-metrics-server", "deploy metrics server via Helm", false),
		newPhase("cluster-auto-healer", "deploy node problem detector and auto-healer", false),
		newPhase("cluster-autoscaler", "deploy cluster autoscaler via Helm", false),
	}
}

func masterReconcilePhases(includeCARotation bool) []Phase {
	phases := []Phase{
		newPhase("prereq-validation", "validate desired master input and prerequisites", false),
	}
	if includeCARotation {
		phases = append(phases, newPhase("ca-rotation", "rotate cluster CA material on master", true))
	}
	phases = append(phases,
		newPhase("etcd", "reconcile etcd service, membership, and config", true),
		newPhase("admin-kubeconfig", "render admin kubeconfig and exported access files", false),
		newPhase("stop-services", "stop or quiesce master services before disruptive changes", true),
		newPhase("client-tools", "install kubernetes and cluster client helpers", false),
		newPhase("container-runtime", "install and reconcile the container runtime", true),
		newPhase("kube-master-config", "render control-plane config and units", true),
		newPhase("start-services", "start master services after reconciliation", true),
		newPhase("health", "verify master health after reconciliation", false),
		// Cluster-level addons — reconcile on master-0, skip on other masters.
		newPhase("cluster-rbac", "create RBAC roles, secrets, and pod security for cluster", false),
		newPhase("cluster-flannel", "deploy flannel CNI via Helm", false),
		newPhase("cluster-coredns", "deploy CoreDNS via Helm", false),
		newPhase("cluster-occm", "deploy OpenStack Cloud Controller Manager via Helm", false),
		newPhase("cluster-cinder-csi", "deploy Cinder CSI driver via Helm", false),
		newPhase("cluster-manila-csi", "deploy Manila CSI driver via Helm", false),
		newPhase("cluster-metrics-server", "deploy metrics server via Helm", false),
		newPhase("cluster-auto-healer", "deploy node problem detector and auto-healer", false),
		newPhase("cluster-autoscaler", "deploy cluster autoscaler via Helm", false),
	)
	return phases
}

func workerCreatePhases() []Phase {
	return []Phase{
		newPhase("prereq-validation", "validate desired worker input and prerequisites", false),
		newPhase("container-runtime", "install and reconcile the container runtime", true),
		newPhase("client-tools", "install kubernetes and cluster client helpers", false),
		newPhase("kube-os-config", "render common kubernetes OS-level config files", false),
		newPhase("worker-certificates", "render or refresh worker certificate material", true),
		newPhase("registry", "reconcile container registry integration", false),
		newPhase("admin-kubeconfig", "render admin kubeconfig and exported access files", false),
		newPhase("kube-worker-config", "render kubelet and kube-proxy config and units", true),
		newPhase("proxy-env", "reconcile environment proxy settings", false),
		newPhase("storage", "reconcile container storage configuration", true),
		newPhase("services", "enable and start worker services", true),
		newPhase("health", "verify worker health after reconciliation", false),
	}
}

func workerReconcilePhases(includeCARotation bool) []Phase {
	phases := []Phase{
		newPhase("prereq-validation", "validate desired worker input and prerequisites", false),
	}
	if includeCARotation {
		phases = append(phases, newPhase("ca-rotation", "rotate cluster CA material on worker", true))
	}
	phases = append(phases,
		newPhase("admin-kubeconfig", "render admin kubeconfig and exported access files", false),
		newPhase("stop-services", "stop or quiesce worker services before disruptive changes", true),
		newPhase("client-tools", "install kubernetes and cluster client helpers", false),
		newPhase("container-runtime", "install and reconcile the container runtime", true),
		newPhase("kube-worker-config", "render kubelet and kube-proxy config and units", true),
		newPhase("start-services", "start worker services after reconciliation", true),
		newPhase("health", "verify worker health after reconciliation", false),
	)
	return phases
}
