package clustercoredns

import (
	"context"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/kubeletconfig"
	"github.com/ventus-ag/magnum-bootstrap/internal/module/cluster-helm"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string { return "cluster-coredns" }
func (Module) Dependencies() []string { return []string{"cluster-rbac"} }

func (Module) Run(ctx context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	return clusterhelm.RunNoop(ctx, cfg, req, cfg.IsFirstMaster(), "coredns", "kube-system")
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	cfg := heat.Cfg
	if !cfg.IsFirstMaster() {
		return clusterhelm.RegisterSkipped(ctx, "magnum:cluster:CoreDNS", name, opts...)
	}

	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:cluster:CoreDNS", name, res, opts...); err != nil {
		return nil, err
	}
	childOpts := append(opts, pulumi.Parent(res))

	prefix := cfg.Shared.ContainerInfraPrefix
	if prefix == "" {
		prefix = "registry.k8s.io/coredns/"
	}

	dnsServiceIP := cfg.Shared.DNSServiceIP
	clusterDomain := cfg.Shared.DNSClusterDomain
	if clusterDomain == "" {
		clusterDomain = "cluster.local"
	}

	portalCIDR := cfg.Shared.PortalNetworkCIDR
	podsCIDR := cfg.Shared.PodsNetworkCIDR

	// kubernetes plugin parameters: cluster domain + reverse zones.
	kubeParams := clusterDomain + " in-addr.arpa " + portalCIDR + " " + podsCIDR

	chartVersion := corednsChartDefault(cfg.Shared.KubeTag)
	imageTag := corednsImageDefault(cfg.Shared.KubeTag)

	_, err := clusterhelm.DeployHelmRelease(ctx, name+"-chart", clusterhelm.HelmReleaseArgs{
		ReleaseName: "coredns",
		Namespace:   "kube-system",
		Chart:       "coredns",
		Version:     chartVersion,
		RepoURL:     "https://coredns.github.io/helm",
		Values: map[string]interface{}{
			"replicaCount": 2,
			"image": map[string]interface{}{
				"repository": prefix + "coredns",
				"tag":        imageTag,
			},
			"resources": map[string]interface{}{
				"limits": map[string]interface{}{
					"cpu":    "100m",
					"memory": "128Mi",
				},
				"requests": map[string]interface{}{
					"cpu":    "100m",
					"memory": "128Mi",
				},
			},
			"isClusterService":  true,
			"priorityClassName": "system-cluster-critical",
			"autoscaling": map[string]interface{}{
				"minReplicas": 2,
				"maxReplicas": 10,
				"metrics": []interface{}{
					map[string]interface{}{
						"type": "Resource",
						"resource": map[string]interface{}{
							"name":                     "cpu",
							"targetAverageUtilization": 60,
						},
					},
					map[string]interface{}{
						"type": "Resource",
						"resource": map[string]interface{}{
							"name":                     "memory",
							"targetAverageUtilization": 60,
						},
					},
				},
			},
			"rollingUpdate": map[string]interface{}{
				"maxUnavailable": 1,
				"maxSurge":       "25%",
			},
			"prometheus": map[string]interface{}{
				"service": map[string]interface{}{
					"enabled": true,
					"annotations": map[string]interface{}{
						"prometheus.io/scrape": "true",
						"prometheus.io/port":   "9153",
					},
				},
				"monitor": map[string]interface{}{
					"enabled":          false,
					"additionalLabels": map[string]interface{}{},
					"namespace":        "",
				},
			},
			"service": map[string]interface{}{
				"clusterIP": dnsServiceIP,
				"name":      "kube-dns",
			},
			"nodeSelector": map[string]interface{}{
				"kubernetes.io/os": "linux",
			},
			"securityContext": map[string]interface{}{
				"capabilities": map[string]interface{}{
					"add": []interface{}{"NET_BIND_SERVICE"},
				},
			},
			"tolerations": []interface{}{
				map[string]interface{}{
					"effect":   "NoSchedule",
					"operator": "Exists",
				},
				map[string]interface{}{
					"key":      "CriticalAddonsOnly",
					"operator": "Exists",
				},
				map[string]interface{}{
					"effect":   "NoExecute",
					"operator": "Exists",
				},
			},
			"servers": []interface{}{
				map[string]interface{}{
					"zones": []interface{}{
						map[string]interface{}{
							"zone": ".",
						},
					},
					"port": 53,
					"plugins": []interface{}{
						map[string]interface{}{
							"name": "errors",
						},
						map[string]interface{}{
							"name": "log",
						},
						map[string]interface{}{
							"name":       "autopath",
							"parameters": "@kubernetes",
						},
						map[string]interface{}{
							"name":        "health",
							"configBlock": "lameduck 5s",
						},
						map[string]interface{}{
							"name": "ready",
						},
						map[string]interface{}{
							"name":        "kubernetes",
							"parameters":  kubeParams,
							"configBlock": "pods verified\nfallthrough in-addr.arpa\nttl 30",
						},
						map[string]interface{}{
							"name":       "prometheus",
							"parameters": "0.0.0.0:9153",
						},
						map[string]interface{}{
							"name":       "forward",
							"parameters": ". 1.1.1.1 1.0.0.1 /etc/resolv.conf",
						},
						corednsCachePlugin(cfg.Shared.KubeTag),
						map[string]interface{}{
							"name": "loop",
						},
						map[string]interface{}{
							"name": "reload",
						},
						map[string]interface{}{
							"name": "loadbalance",
						},
					},
				},
			},
			"deployment": map[string]interface{}{
				"enabled": true,
				"name":    "coredns",
			},
		},
	}, childOpts...)
	if err != nil {
		return nil, err
	}

	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"corednsTag":   pulumi.String(imageTag),
		"chartVersion": pulumi.String(chartVersion),
		"dnsServiceIp": pulumi.String(dnsServiceIP),
	}); err != nil {
		return nil, err
	}
	return res, nil
}

// corednsChartVersions maps K8s minor version to a compatible CoreDNS Helm
// chart version. Chart versions are chosen so the bundled CoreDNS appVersion
// matches what kubeadm ships for that K8s release.
// Source: helm search repo coredns/coredns --versions
//
//	chart 1.15.1 → CoreDNS 1.8.0   (K8s 1.20-1.21)
//	chart 1.16.4 → CoreDNS 1.8.4   (K8s 1.22)
//	chart 1.16.6 → CoreDNS 1.8.6   (K8s 1.23-1.24)
//	chart 1.19.6 → CoreDNS 1.9.3   (K8s 1.25-1.26)
//	chart 1.24.5 → CoreDNS 1.10.1  (K8s 1.27-1.28)
//	chart 1.31.0 → CoreDNS 1.11.1  (K8s 1.29-1.30)
//	chart 1.36.1 → CoreDNS 1.11.3  (K8s 1.31-1.32)
//	chart 1.42.2 → CoreDNS 1.12.0  (K8s 1.33)
//	chart 1.44.3 → CoreDNS 1.12.3  (K8s 1.34)
//	chart 1.45.2 → CoreDNS 1.13.1  (K8s 1.35)
var corednsChartVersions = map[string]string{
	"1.35": "1.45.2",
	"1.34": "1.44.3",
	"1.33": "1.42.2",
	"1.32": "1.36.1",
	"1.31": "1.36.1",
	"1.30": "1.31.0",
	"1.29": "1.31.0",
	"1.28": "1.24.5",
	"1.27": "1.24.5",
	"1.26": "1.19.6",
	"1.25": "1.19.6",
	"1.24": "1.16.6",
	"1.23": "1.16.6",
	"1.22": "1.16.4",
	"1.21": "1.15.1",
	"1.20": "1.15.1",
}

// corednsImageTags maps K8s minor version to the CoreDNS image tag that
// kubeadm bundles. Tags use "v" prefix matching registry.k8s.io/coredns/coredns.
// Source: kubernetes/kubernetes cmd/kubeadm/app/constants/constants.go
var corednsImageTags = map[string]string{
	"1.35": "v1.13.1",
	"1.34": "v1.12.1",
	"1.33": "v1.12.0",
	"1.32": "v1.11.3",
	"1.31": "v1.11.3",
	"1.30": "v1.11.1",
	"1.29": "v1.11.1",
	"1.28": "v1.10.1",
	"1.27": "v1.10.1",
	"1.26": "v1.9.3",
	"1.25": "v1.9.3",
	"1.24": "v1.8.6",
	"1.23": "v1.8.6",
	"1.22": "v1.8.4",
	"1.21": "v1.8.0",
	"1.20": "v1.7.0",
}

func corednsChartDefault(kubeTag string) string {
	return config.LookupByKubeVersion(corednsChartVersions, kubeTag)
}

func corednsImageDefault(kubeTag string) string {
	return config.LookupByKubeVersion(corednsImageTags, kubeTag)
}

// corednsCachePlugin returns the cache plugin entry for the CoreDNS Corefile.
// K8s >= 1.32 (kubeadm) adds "disable success" and "disable denial" to prevent
// conflicting cached responses during rolling updates.
func corednsCachePlugin(kubeTag string) map[string]interface{} {
	if kubeletconfig.KubeMinorAtLeast(kubeTag, 32) {
		return map[string]interface{}{
			"name":        "cache",
			"parameters":  "30",
			"configBlock": "disable success\ndisable denial",
		}
	}
	return map[string]interface{}{
		"name":       "cache",
		"parameters": "30",
	}
}
