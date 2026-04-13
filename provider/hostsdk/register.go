package hostsdk

import (
	"fmt"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
)

func RegisterDirectorySpec(ctx *pulumi.Context, name string, spec hostresource.DirectorySpec, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	if Enabled() {
		return NewDirectory(ctx, name, &DirectoryArgs{Path: pulumi.String(spec.Path), Mode: pulumi.String(fmt.Sprintf("%04o", spec.Mode.Perm()))}, opts...)
	}
	return spec.Register(ctx, name, opts...)
}

func RegisterFileSpec(ctx *pulumi.Context, name string, spec hostresource.FileSpec, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	if Enabled() {
		args := &FileArgs{Path: pulumi.String(spec.Path), Content: pulumi.String(string(spec.Content)), Mode: pulumi.String(fmt.Sprintf("%04o", spec.Mode.Perm()))}
		if spec.Absent {
			args.Absent = pulumi.BoolPtr(true)
		}
		return NewFile(ctx, name, args, opts...)
	}
	return spec.Register(ctx, name, opts...)
}

func RegisterLineSpec(ctx *pulumi.Context, name string, spec hostresource.LineSpec, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	if Enabled() {
		return NewLine(ctx, name, &LineArgs{Path: pulumi.String(spec.Path), Line: pulumi.String(spec.Line), Mode: pulumi.String(fmt.Sprintf("%04o", spec.Mode.Perm()))}, opts...)
	}
	return spec.Register(ctx, name, opts...)
}

func RegisterExportSpec(ctx *pulumi.Context, name string, spec hostresource.ExportSpec, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	if Enabled() {
		return NewExport(ctx, name, &ExportArgs{Path: pulumi.String(spec.Path), VarName: pulumi.String(spec.VarName), Value: pulumi.String(spec.Value), Mode: pulumi.String(fmt.Sprintf("%04o", spec.Mode.Perm()))}, opts...)
	}
	return spec.Register(ctx, name, opts...)
}

func RegisterCopySpec(ctx *pulumi.Context, name string, spec hostresource.CopySpec, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	if Enabled() {
		return NewCopy(ctx, name, &CopyArgs{Source: pulumi.String(spec.Source), Path: pulumi.String(spec.Path), Mode: pulumi.String(fmt.Sprintf("%04o", spec.Mode.Perm()))}, opts...)
	}
	return spec.Register(ctx, name, opts...)
}

func RegisterDownloadSpec(ctx *pulumi.Context, name string, spec hostresource.DownloadSpec, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	if Enabled() {
		args := &DownloadArgs{URL: pulumi.String(spec.URL), Path: pulumi.String(spec.Path), Mode: pulumi.String(fmt.Sprintf("%04o", spec.Mode.Perm()))}
		if spec.Retries > 0 {
			args.Retries = pulumi.IntPtr(spec.Retries)
		}
		return NewDownload(ctx, name, args, opts...)
	}
	return spec.Register(ctx, name, opts...)
}

func RegisterSystemdServiceSpec(ctx *pulumi.Context, name string, spec hostresource.SystemdServiceSpec, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	if Enabled() {
		args := &SystemdServiceArgs{Unit: pulumi.String(spec.Unit)}
		if spec.SkipIfMissing {
			args.SkipIfMissing = pulumi.BoolPtr(true)
		}
		if spec.Enabled != nil {
			args.Enabled = pulumi.BoolPtr(*spec.Enabled)
		}
		if spec.Active != nil {
			args.Active = pulumi.BoolPtr(*spec.Active)
		}
		if spec.Masked != nil {
			args.Masked = pulumi.BoolPtr(*spec.Masked)
		}
		if spec.Restart {
			args.Restart = pulumi.BoolPtr(true)
		}
		if spec.RestartReason != "" {
			args.RestartReason = pulumi.StringPtr(spec.RestartReason)
		}
		if spec.DaemonReload {
			args.DaemonReload = pulumi.BoolPtr(true)
		}
		if spec.RestartOnChange {
			args.RestartOnChange = pulumi.BoolPtr(true)
		}
		if spec.RestartToken != "" {
			args.RestartToken = pulumi.StringPtr(spec.RestartToken)
		}
		return NewSystemdService(ctx, name, args, opts...)
	}
	return spec.Register(ctx, name, opts...)
}

func RegisterModeSpec(ctx *pulumi.Context, name string, spec hostresource.ModeSpec, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	if Enabled() {
		args := &ModeArgs{Path: pulumi.String(spec.Path), Mode: pulumi.String(fmt.Sprintf("%04o", spec.Mode.Perm()))}
		if spec.SkipIfMissing {
			args.SkipIfMissing = pulumi.BoolPtr(true)
		}
		return NewMode(ctx, name, args, opts...)
	}
	return spec.Register(ctx, name, opts...)
}

func RegisterOwnershipSpec(ctx *pulumi.Context, name string, spec hostresource.OwnershipSpec, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	if Enabled() {
		args := &OwnershipArgs{Path: pulumi.String(spec.Path), Owner: pulumi.String(spec.Owner), Group: pulumi.String(spec.Group)}
		if spec.Recursive {
			args.Recursive = pulumi.BoolPtr(true)
		}
		if spec.SkipIfMissing {
			args.SkipIfMissing = pulumi.BoolPtr(true)
		}
		return NewOwnership(ctx, name, args, opts...)
	}
	return spec.Register(ctx, name, opts...)
}

func RegisterSysctlSpec(ctx *pulumi.Context, name string, spec hostresource.SysctlSpec, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	if Enabled() {
		args := &SysctlArgs{Path: pulumi.String(spec.Path), Content: pulumi.String(string(spec.Content)), Mode: pulumi.String(fmt.Sprintf("%04o", spec.Mode.Perm()))}
		if len(spec.ReloadArg) > 0 {
			reloadArgs := make(pulumi.StringArray, 0, len(spec.ReloadArg))
			for _, arg := range spec.ReloadArg {
				reloadArgs = append(reloadArgs, pulumi.String(arg))
			}
			args.ReloadArg = reloadArgs
		}
		return NewSysctl(ctx, name, args, opts...)
	}
	return spec.Register(ctx, name, opts...)
}

func RegisterModuleLoadSpec(ctx *pulumi.Context, name string, spec hostresource.ModuleLoadSpec, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	if Enabled() {
		modules := make(pulumi.StringArray, 0, len(spec.Modules))
		for _, module := range spec.Modules {
			modules = append(modules, pulumi.String(module))
		}
		return NewModuleLoad(ctx, name, &ModuleLoadArgs{Path: pulumi.String(spec.Path), Modules: modules, Mode: pulumi.String(fmt.Sprintf("%04o", spec.Mode.Perm()))}, opts...)
	}
	return spec.Register(ctx, name, opts...)
}

func RegisterExtractTarSpec(ctx *pulumi.Context, name string, spec hostresource.ExtractTarSpec, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	if Enabled() {
		checkPaths := make(pulumi.StringArray, 0, len(spec.CheckPaths))
		for _, path := range spec.CheckPaths {
			checkPaths = append(checkPaths, pulumi.String(path))
		}
		args := &ExtractTarArgs{ArchivePath: pulumi.String(spec.ArchivePath), Destination: pulumi.String(spec.Destination), CheckPaths: checkPaths}
		if spec.ChmodExecutables {
			args.ChmodExecutables = pulumi.BoolPtr(true)
		}
		return NewExtractTar(ctx, name, args, opts...)
	}
	return spec.Register(ctx, name, opts...)
}
