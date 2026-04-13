package hostsdk

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/buildinfo"
	"github.com/ventus-ag/magnum-bootstrap/provider/hostplugin"
)

const (
	EnvProviderPath = "MAGNUM_HOST_PROVIDER_PATH"
	EnvProviderURL  = "MAGNUM_HOST_PROVIDER_URL"
	EnvProviderMode = "MAGNUM_USE_HOST_PROVIDER"
)

func Enabled() bool {
	if mode := os.Getenv(EnvProviderMode); mode != "" {
		if strings.EqualFold(mode, "0") || strings.EqualFold(mode, "false") {
			return false
		}
		if strings.EqualFold(mode, "1") || strings.EqualFold(mode, "true") {
			return true
		}
	}
	return os.Getenv(EnvProviderPath) != "" || ProviderURL() != ""
}

func ProviderDir() string {
	path := os.Getenv(EnvProviderPath)
	if path == "" {
		return ""
	}
	return filepath.Dir(path)
}

func ProviderURL() string {
	if value := os.Getenv(EnvProviderURL); value != "" {
		return value
	}
	return buildinfo.DefaultHostProviderURL()
}

func withDefaults(opts []pulumi.ResourceOption) []pulumi.ResourceOption {
	result := append([]pulumi.ResourceOption{}, opts...)
	result = append(result, pulumi.Version(hostplugin.ProviderVersion))
	return result
}
