package clustercoredns

import (
	"context"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
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

	_, err := clusterhelm.DeployHelmRelease(ctx, name+"-chart", clusterhelm.HelmReleaseArgs{
		ReleaseName: "coredns",
		Namespace:   "kube-system",
		Chart:       "coredns",
		Version:     "1.22.0",
		RepoURL:     "https://coredns.github.io/helm",
		Values: map[string]interface{}{
			"replicaCount": 2,
			"image": map[string]interface{}{
				"repository": prefix + "coredns",
				"tag":        cfg.Shared.CorednsTag,
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
						map[string]interface{}{
							"name":       "cache",
							"parameters": "30",
						},
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
		"corednsTag":   pulumi.String(cfg.Shared.CorednsTag),
		"dnsServiceIp": pulumi.String(dnsServiceIP),
	}); err != nil {
		return nil, err
	}
	return res, nil
}
