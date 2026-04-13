package hostsdk

import "github.com/pulumi/pulumi/sdk/v3/go/pulumi"

type ModeArgs struct {
	Path          pulumi.StringInput  `pulumi:"path"`
	Mode          pulumi.StringInput  `pulumi:"mode"`
	SkipIfMissing pulumi.BoolPtrInput `pulumi:"skipIfMissing,optional"`
}

type Mode struct {
	pulumi.CustomResourceState

	Path           pulumi.StringOutput      `pulumi:"path"`
	Mode           pulumi.StringOutput      `pulumi:"mode"`
	SkipIfMissing  pulumi.BoolOutput        `pulumi:"skipIfMissing"`
	ObservedExists pulumi.BoolOutput        `pulumi:"observedExists"`
	ObservedMode   pulumi.StringOutput      `pulumi:"observedMode"`
	Drifted        pulumi.BoolOutput        `pulumi:"drifted"`
	DriftReasons   pulumi.StringArrayOutput `pulumi:"driftReasons"`
}

func NewMode(ctx *pulumi.Context, name string, args *ModeArgs, opts ...pulumi.ResourceOption) (*Mode, error) {
	if args == nil {
		args = &ModeArgs{}
	}
	res := &Mode{}
	inputs := pulumi.Map{"path": args.Path, "mode": args.Mode}
	if args.SkipIfMissing != nil {
		inputs["skipIfMissing"] = args.SkipIfMissing
	}
	if err := ctx.RegisterResource("magnumhost:index:Mode", name, inputs, res, withDefaults(opts)...); err != nil {
		return nil, err
	}
	return res, nil
}
