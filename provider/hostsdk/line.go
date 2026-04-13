package hostsdk

import "github.com/pulumi/pulumi/sdk/v3/go/pulumi"

type LineArgs struct {
	Path pulumi.StringInput `pulumi:"path"`
	Line pulumi.StringInput `pulumi:"line"`
	Mode pulumi.StringInput `pulumi:"mode"`
}

type Line struct {
	pulumi.CustomResourceState

	Path                 pulumi.StringOutput      `pulumi:"path"`
	Line                 pulumi.StringOutput      `pulumi:"line"`
	Mode                 pulumi.StringOutput      `pulumi:"mode"`
	ObservedExists       pulumi.BoolOutput        `pulumi:"observedExists"`
	ObservedMode         pulumi.StringOutput      `pulumi:"observedMode"`
	ObservedContainsLine pulumi.BoolOutput        `pulumi:"observedContainsLine"`
	Drifted              pulumi.BoolOutput        `pulumi:"drifted"`
	DriftReasons         pulumi.StringArrayOutput `pulumi:"driftReasons"`
}

func NewLine(ctx *pulumi.Context, name string, args *LineArgs, opts ...pulumi.ResourceOption) (*Line, error) {
	if args == nil {
		args = &LineArgs{}
	}
	res := &Line{}
	inputs := pulumi.Map{
		"path": args.Path,
		"line": args.Line,
		"mode": args.Mode,
	}
	if err := ctx.RegisterResource("magnumhost:index:Line", name, inputs, res, withDefaults(opts)...); err != nil {
		return nil, err
	}
	return res, nil
}
