package hostsdk

import "github.com/pulumi/pulumi/sdk/v3/go/pulumi"

type OwnershipArgs struct {
	Path          pulumi.StringInput  `pulumi:"path"`
	Owner         pulumi.StringInput  `pulumi:"owner"`
	Group         pulumi.StringInput  `pulumi:"group"`
	Recursive     pulumi.BoolPtrInput `pulumi:"recursive,optional"`
	SkipIfMissing pulumi.BoolPtrInput `pulumi:"skipIfMissing,optional"`
}

type Ownership struct {
	pulumi.CustomResourceState

	Path                   pulumi.StringOutput      `pulumi:"path"`
	Owner                  pulumi.StringOutput      `pulumi:"owner"`
	Group                  pulumi.StringOutput      `pulumi:"group"`
	Recursive              pulumi.BoolOutput        `pulumi:"recursive"`
	SkipIfMissing          pulumi.BoolOutput        `pulumi:"skipIfMissing"`
	ObservedExists         pulumi.BoolOutput        `pulumi:"observedExists"`
	ObservedOwner          pulumi.StringOutput      `pulumi:"observedOwner"`
	ObservedGroup          pulumi.StringOutput      `pulumi:"observedGroup"`
	ObservedRecursiveMatch pulumi.BoolOutput        `pulumi:"observedRecursiveMatch"`
	Drifted                pulumi.BoolOutput        `pulumi:"drifted"`
	DriftReasons           pulumi.StringArrayOutput `pulumi:"driftReasons"`
}

func NewOwnership(ctx *pulumi.Context, name string, args *OwnershipArgs, opts ...pulumi.ResourceOption) (*Ownership, error) {
	if args == nil {
		args = &OwnershipArgs{}
	}
	res := &Ownership{}
	inputs := pulumi.Map{"path": args.Path, "owner": args.Owner, "group": args.Group}
	if args.Recursive != nil {
		inputs["recursive"] = args.Recursive
	}
	if args.SkipIfMissing != nil {
		inputs["skipIfMissing"] = args.SkipIfMissing
	}
	if err := ctx.RegisterResource("magnumhost:index:Ownership", name, inputs, res, withDefaults(opts)...); err != nil {
		return nil, err
	}
	return res, nil
}
