package hostsdk

import "github.com/pulumi/pulumi/sdk/v3/go/pulumi"

type ModuleLoadArgs struct {
	Path    pulumi.StringInput      `pulumi:"path"`
	Modules pulumi.StringArrayInput `pulumi:"modules"`
	Mode    pulumi.StringInput      `pulumi:"mode"`
}

type ModuleLoad struct {
	pulumi.CustomResourceState

	Path                  pulumi.StringOutput      `pulumi:"path"`
	Modules               pulumi.StringArrayOutput `pulumi:"modules"`
	Mode                  pulumi.StringOutput      `pulumi:"mode"`
	ContentSHA256         pulumi.StringOutput      `pulumi:"contentSha256"`
	ObservedExists        pulumi.BoolOutput        `pulumi:"observedExists"`
	ObservedMode          pulumi.StringOutput      `pulumi:"observedMode"`
	ObservedContentSHA256 pulumi.StringOutput      `pulumi:"observedContentSha256"`
	Drifted               pulumi.BoolOutput        `pulumi:"drifted"`
	DriftReasons          pulumi.StringArrayOutput `pulumi:"driftReasons"`
}

func NewModuleLoad(ctx *pulumi.Context, name string, args *ModuleLoadArgs, opts ...pulumi.ResourceOption) (*ModuleLoad, error) {
	if args == nil {
		args = &ModuleLoadArgs{}
	}
	res := &ModuleLoad{}
	inputs := pulumi.Map{"path": args.Path, "modules": args.Modules, "mode": args.Mode}
	if err := ctx.RegisterResource("magnumhost:index:ModuleLoad", name, inputs, res, withDefaults(opts)...); err != nil {
		return nil, err
	}
	return res, nil
}
