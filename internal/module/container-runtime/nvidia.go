package containerruntime

import (
	"fmt"
	"os"
	"regexp"
	"strconv"

	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
)

const nvidiaContainerdConfigPath = "/etc/containerd/conf.d/99-nvidia.toml"

var nvidiaCgroupOption = regexp.MustCompile(`(?m)^([\t ]*SystemdCgroup[\t ]*=[\t ]*)(?:true|false)([\t ]*(?:#[^\r\n]*)?\r?)$`)

// nvidiaContainerdConfig keeps the toolkit's generated runtime settings and
// comments, changing only cgroup options that would override the root config.
// The toolkit owns creating this file; non-GPU nodes get no additional file.
func nvidiaContainerdConfig(path string, systemdCgroup bool) (*hostresource.FileSpec, error) {
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read NVIDIA containerd config: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat NVIDIA containerd config: %w", err)
	}
	content = nvidiaCgroupOption.ReplaceAll(content, []byte("${1}"+strconv.FormatBool(systemdCgroup)+"${2}"))
	return &hostresource.FileSpec{Path: path, Content: content, Mode: info.Mode().Perm()}, nil
}
