package hostsdk

import "github.com/pulumi/pulumi/sdk/v3/go/pulumi"

type DirectoryArgs struct {
	Path pulumi.StringInput `pulumi:"path"`
	Mode pulumi.StringInput `pulumi:"mode"`
}

type Directory struct {
	pulumi.CustomResourceState

	Path           pulumi.StringOutput      `pulumi:"path"`
	Mode           pulumi.StringOutput      `pulumi:"mode"`
	ObservedExists pulumi.BoolOutput        `pulumi:"observedExists"`
	ObservedIsDir  pulumi.BoolOutput        `pulumi:"observedIsDir"`
	ObservedMode   pulumi.StringOutput      `pulumi:"observedMode"`
	Drifted        pulumi.BoolOutput        `pulumi:"drifted"`
	DriftReasons   pulumi.StringArrayOutput `pulumi:"driftReasons"`
}

func NewDirectory(ctx *pulumi.Context, name string, args *DirectoryArgs, opts ...pulumi.ResourceOption) (*Directory, error) {
	if args == nil {
		args = &DirectoryArgs{}
	}
	res := &Directory{}
	inputs := pulumi.Map{
		"path": args.Path,
		"mode": args.Mode,
	}
	if err := ctx.RegisterResource("magnumhost:index:Directory", name, inputs, res, withDefaults(opts)...); err != nil {
		return nil, err
	}
	return res, nil
}
