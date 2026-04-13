package hostsdk

import "github.com/pulumi/pulumi/sdk/v3/go/pulumi"

type ExtractTarArgs struct {
	ArchivePath      pulumi.StringInput      `pulumi:"archivePath"`
	Destination      pulumi.StringInput      `pulumi:"destination"`
	CheckPaths       pulumi.StringArrayInput `pulumi:"checkPaths"`
	ChmodExecutables pulumi.BoolPtrInput     `pulumi:"chmodExecutables,optional"`
}

type ExtractTar struct {
	pulumi.CustomResourceState

	ArchivePath       pulumi.StringOutput      `pulumi:"archivePath"`
	Destination       pulumi.StringOutput      `pulumi:"destination"`
	CheckPaths        pulumi.StringArrayOutput `pulumi:"checkPaths"`
	ChmodExecutables  pulumi.BoolOutput        `pulumi:"chmodExecutables"`
	ObservedSatisfied pulumi.BoolOutput        `pulumi:"observedSatisfied"`
	MissingCheckPaths pulumi.StringArrayOutput `pulumi:"missingCheckPaths"`
	Drifted           pulumi.BoolOutput        `pulumi:"drifted"`
	DriftReasons      pulumi.StringArrayOutput `pulumi:"driftReasons"`
}

func NewExtractTar(ctx *pulumi.Context, name string, args *ExtractTarArgs, opts ...pulumi.ResourceOption) (*ExtractTar, error) {
	if args == nil {
		args = &ExtractTarArgs{}
	}
	res := &ExtractTar{}
	inputs := pulumi.Map{"archivePath": args.ArchivePath, "destination": args.Destination, "checkPaths": args.CheckPaths}
	if args.ChmodExecutables != nil {
		inputs["chmodExecutables"] = args.ChmodExecutables
	}
	if err := ctx.RegisterResource("magnumhost:index:ExtractTar", name, inputs, res, withDefaults(opts)...); err != nil {
		return nil, err
	}
	return res, nil
}
