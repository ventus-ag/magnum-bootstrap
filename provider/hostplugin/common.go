package hostplugin

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/ventus-ag/magnum-bootstrap/internal/buildinfo"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
)

const (
	ProviderName   = "magnumhost"
	ProviderBinary = buildinfo.HostProviderAsset
)

var ProviderVersion = buildinfo.ProviderSemver()

func newExecutor(apply bool) *host.Executor {
	return host.NewExecutor(apply, nil)
}

func parseMode(value string) (os.FileMode, error) {
	trimmed := strings.TrimSpace(value)
	trimmed = strings.TrimPrefix(trimmed, "0o")
	trimmed = strings.TrimPrefix(trimmed, "0O")
	if trimmed == "" {
		return 0, fmt.Errorf("mode is required")
	}
	parsed, err := strconv.ParseUint(trimmed, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("parse mode %q: %w", value, err)
	}
	return os.FileMode(parsed), nil
}

func modeString(mode os.FileMode) string {
	return fmt.Sprintf("%04o", mode.Perm())
}
