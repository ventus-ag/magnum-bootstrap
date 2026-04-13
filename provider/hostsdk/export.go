package hostsdk

import "github.com/pulumi/pulumi/sdk/v3/go/pulumi"

type ExportArgs struct {
	Path    pulumi.StringInput `pulumi:"path"`
	VarName pulumi.StringInput `pulumi:"varName"`
	Value   pulumi.StringInput `pulumi:"value"`
	Mode    pulumi.StringInput `pulumi:"mode"`
}

type Export struct {
	pulumi.CustomResourceState

	Path               pulumi.StringOutput      `pulumi:"path"`
	VarName            pulumi.StringOutput      `pulumi:"varName"`
	Mode               pulumi.StringOutput      `pulumi:"mode"`
	HasValue           pulumi.BoolOutput        `pulumi:"hasValue"`
	ValueSHA256        pulumi.StringOutput      `pulumi:"valueSha256"`
	ObservedExists     pulumi.BoolOutput        `pulumi:"observedExists"`
	ObservedMode       pulumi.StringOutput      `pulumi:"observedMode"`
	ObservedHasDesired pulumi.BoolOutput        `pulumi:"observedHasDesiredValue"`
	Drifted            pulumi.BoolOutput        `pulumi:"drifted"`
	DriftReasons       pulumi.StringArrayOutput `pulumi:"driftReasons"`
}

func NewExport(ctx *pulumi.Context, name string, args *ExportArgs, opts ...pulumi.ResourceOption) (*Export, error) {
	if args == nil {
		args = &ExportArgs{}
	}
	res := &Export{}
	inputs := pulumi.Map{
		"path":    args.Path,
		"varName": args.VarName,
		"value":   args.Value,
		"mode":    args.Mode,
	}
	if err := ctx.RegisterResource("magnumhost:index:Export", name, inputs, res, withDefaults(opts)...); err != nil {
		return nil, err
	}
	return res, nil
}
