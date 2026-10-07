package kubecommon

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
	"github.com/ventus-ag/magnum-bootstrap/provider/hostsdk"
)

// KubeFilesDir receives one file per kube_file_<name> cluster label, so any
// component option can reference it (e.g. kubeapi_options
// --oidc-ca-file=/etc/kubernetes/files/oidc_ca). The control-plane containers
// mount /etc/kubernetes.
const KubeFilesDir = "/etc/kubernetes/files"

var kubeFilesDir = KubeFilesDir

var kubeFileName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// DecodeKubeFiles unpacks KUBE_FILES: base64 of a JSON {name: content}
// object, which keeps multi-line content intact through Heat and heat-params.
func DecodeKubeFiles(encoded string) (map[string]string, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("KUBE_FILES: not base64: %w", err)
	}
	var files map[string]string
	if err := json.Unmarshal(raw, &files); err != nil {
		return nil, fmt.Errorf("KUBE_FILES: not a JSON object of strings: %w", err)
	}
	for name := range files {
		if !kubeFileName.MatchString(name) {
			return nil, fmt.Errorf("KUBE_FILES: invalid file name %q", name)
		}
	}
	return files, nil
}

// KubeFilePaths returns the absolute paths a KUBE_FILES value provisions.
func KubeFilePaths(files map[string]string) map[string]bool {
	paths := make(map[string]bool, len(files))
	for name := range files {
		paths[filepath.Join(kubeFilesDir, name)] = true
	}
	return paths
}

func kubeFileSpecs(files map[string]string) []hostresource.FileSpec {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	specs := make([]hostresource.FileSpec, 0, len(names))
	for _, name := range names {
		// 0600: contents may be credentials; every consumer runs as root.
		specs = append(specs, hostresource.FileSpec{Path: filepath.Join(kubeFilesDir, name), Content: []byte(files[name]), Mode: 0o600})
	}
	return specs
}

// kubeFilesManifest lists the names EnsureKubeFiles wrote, so a removed label
// deletes its file and nothing placed in the directory by hand.
const kubeFilesManifest = ".magnum-managed"

// EnsureKubeFiles converges the files from KUBE_FILES: files whose label was
// removed are deleted.
func EnsureKubeFiles(executor *host.Executor, files map[string]string) ([]host.Change, error) {
	var changes []host.Change
	for _, spec := range kubeFileSpecs(files) {
		result, err := spec.Apply(executor)
		if err != nil {
			return nil, err
		}
		changes = append(changes, result.Changes...)
	}
	manifestPath := filepath.Join(kubeFilesDir, kubeFilesManifest)
	previous, _ := os.ReadFile(manifestPath)
	for _, name := range strings.Fields(string(previous)) {
		if _, keep := files[name]; keep || !kubeFileName.MatchString(name) {
			continue
		}
		change, err := executor.EnsureAbsent(filepath.Join(kubeFilesDir, name))
		if err != nil {
			return nil, err
		}
		if change != nil {
			changes = append(changes, *change)
		}
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	manifest := hostresource.FileSpec{Path: manifestPath, Content: []byte(strings.Join(names, "\n") + "\n"), Mode: 0o600, Absent: len(names) == 0}
	if _, err := manifest.Apply(executor); err != nil {
		return nil, err
	}
	return changes, nil
}

// RegisterKubeFiles records the provisioned files in Pulumi state. Decode
// errors are left to Run, which reports them.
func RegisterKubeFiles(ctx *pulumi.Context, name, encoded string, opts ...pulumi.ResourceOption) error {
	files, err := DecodeKubeFiles(encoded)
	if err != nil {
		return nil
	}
	for _, spec := range kubeFileSpecs(files) {
		if _, err := hostsdk.RegisterFileSpec(ctx, name+"-kube-file-"+filepath.Base(spec.Path), spec, opts...); err != nil {
			return err
		}
	}
	return nil
}

// outputPathFlags name files a component writes, so they need not exist.
var outputPathFlags = map[string]bool{
	"log-file":  true,
	"lock-file": true,
}

func isInputPathFlag(name string) bool {
	if outputPathFlags[name] {
		return false
	}
	switch name {
	case "config", "kubeconfig":
		return true
	}
	// Not "-path": those are outputs or directories the component creates
	// (--audit-log-path, --pod-manifest-path).
	return strings.HasSuffix(name, "-file") || strings.HasSuffix(name, "-config")
}

// MissingFileFlags returns the options in opts that name an input file that
// does not exist, as "--flag=path". Such an option makes the component exit
// on start, so it is caught before the args are written.
func MissingFileFlags(opts string, exists func(string) bool) []string {
	fields := strings.Fields(opts)
	var missing []string
	for i := 0; i < len(fields); i++ {
		field := fields[i]
		if !strings.HasPrefix(field, "--") {
			continue
		}
		name, value, hasValue := strings.Cut(field[2:], "=")
		if !isInputPathFlag(name) {
			continue
		}
		if !hasValue {
			if i+1 >= len(fields) || strings.HasPrefix(fields[i+1], "-") {
				continue
			}
			i++
			value = fields[i]
		}
		value = strings.Trim(value, `"'`)
		if !strings.HasPrefix(value, "/") || exists(value) {
			continue
		}
		missing = append(missing, "--"+name+"="+value)
	}
	return missing
}

// CheckOptionFiles fails when any of the named option strings references a
// missing input file. provisioned paths (written later in this run) count as
// present, so a dry run and the first run behave the same.
func CheckOptionFiles(provisioned map[string]bool, options map[string]string) error {
	exists := func(path string) bool {
		if provisioned[path] {
			return true
		}
		_, err := os.Stat(path)
		return err == nil
	}
	keys := make([]string, 0, len(options))
	for key := range options {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var problems []string
	for _, key := range keys {
		if missing := MissingFileFlags(options[key], exists); len(missing) > 0 {
			problems = append(problems, fmt.Sprintf("%s: %s", key, strings.Join(missing, ", ")))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("component options reference files that do not exist on this node (%s); "+
			"ship them with a kube_file_<name> cluster label (written to %s/<name>) or fix the path — "+
			"the current args were left in place", strings.Join(problems, "; "), KubeFilesDir)
	}
	return nil
}
