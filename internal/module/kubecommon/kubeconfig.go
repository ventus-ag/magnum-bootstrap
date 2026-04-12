package kubecommon

import "fmt"

// BuildKubeconfig renders a kubeconfig YAML with TLS client certificate auth.
func BuildKubeconfig(user, certFile, keyFile, caFile, server string) string {
	return fmt.Sprintf(`apiVersion: v1
clusters:
- cluster:
    certificate-authority: %s
    server: %s
  name: kubernetes
contexts:
- context:
    cluster: kubernetes
    user: %s
  name: default
current-context: default
kind: Config
preferences: {}
users:
- name: %s
  user:
    as-user-extra: {}
    client-certificate: %s
    client-key: %s
`, caFile, server, user, user, certFile, keyFile)
}

// BuildInsecureKubeconfig renders a kubeconfig YAML without TLS verification.
func BuildInsecureKubeconfig(user, server string) string {
	return fmt.Sprintf(`apiVersion: v1
clusters:
- cluster:
    server: %s
  name: kubernetes
contexts:
- context:
    cluster: kubernetes
    user: %s
  name: default
current-context: default
kind: Config
preferences: {}
users:
- name: %s
  user:
    as-user-extra: {}
`, server, user, user)
}
