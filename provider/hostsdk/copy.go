package hostsdk

import "github.com/pulumi/pulumi/sdk/v3/go/pulumi"

type CopyArgs struct {
	Source pulumi.StringInput `pulumi:"source"`
	Path   pulumi.StringInput `pulumi:"path"`
	Mode   pulumi.StringInput `pulumi:"mode"`
}

type Copy struct {
	pulumi.CustomResourceState

	Source              pulumi.StringOutput      `pulumi:"source"`
	Path                pulumi.StringOutput      `pulumi:"path"`
	Mode                pulumi.StringOutput      `pulumi:"mode"`
	SourceSHA256        pulumi.StringOutput      `pulumi:"sourceSha256"`
	ObservedExists      pulumi.BoolOutput        `pulumi:"observedExists"`
	ObservedMode        pulumi.StringOutput      `pulumi:"observedMode"`
	ObservedMatchesCopy pulumi.BoolOutput        `pulumi:"observedMatchesSource"`
	Drifted             pulumi.BoolOutput        `pulumi:"drifted"`
	DriftReasons        pulumi.StringArrayOutput `pulumi:"driftReasons"`
}

func NewCopy(ctx *pulumi.Context, name string, args *CopyArgs, opts ...pulumi.ResourceOption) (*Copy, error) {
	if args == nil {
		args = &CopyArgs{}
	}
	res := &Copy{}
	inputs := pulumi.Map{"source": args.Source, "path": args.Path, "mode": args.Mode}
	if err := ctx.RegisterResource("magnumhost:index:Copy", name, inputs, res, withDefaults(opts)...); err != nil {
		return nil, err
	}
	return res, nil
}
