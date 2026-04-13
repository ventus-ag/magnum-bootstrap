package hostsdk

import "github.com/pulumi/pulumi/sdk/v3/go/pulumi"

type SysctlArgs struct {
	Path      pulumi.StringInput      `pulumi:"path"`
	Content   pulumi.StringInput      `pulumi:"content"`
	Mode      pulumi.StringInput      `pulumi:"mode"`
	ReloadArg pulumi.StringArrayInput `pulumi:"reloadArg,optional"`
}

type Sysctl struct {
	pulumi.CustomResourceState

	Path                  pulumi.StringOutput      `pulumi:"path"`
	Mode                  pulumi.StringOutput      `pulumi:"mode"`
	ContentSHA256         pulumi.StringOutput      `pulumi:"contentSha256"`
	ReloadArg             pulumi.StringArrayOutput `pulumi:"reloadArg"`
	ObservedExists        pulumi.BoolOutput        `pulumi:"observedExists"`
	ObservedMode          pulumi.StringOutput      `pulumi:"observedMode"`
	ObservedContentSHA256 pulumi.StringOutput      `pulumi:"observedContentSha256"`
	Drifted               pulumi.BoolOutput        `pulumi:"drifted"`
	DriftReasons          pulumi.StringArrayOutput `pulumi:"driftReasons"`
}

func NewSysctl(ctx *pulumi.Context, name string, args *SysctlArgs, opts ...pulumi.ResourceOption) (*Sysctl, error) {
	if args == nil {
		args = &SysctlArgs{}
	}
	res := &Sysctl{}
	inputs := pulumi.Map{"path": args.Path, "content": args.Content, "mode": args.Mode}
	if args.ReloadArg != nil {
		inputs["reloadArg"] = args.ReloadArg
	}
	if err := ctx.RegisterResource("magnumhost:index:Sysctl", name, inputs, res, withDefaults(opts)...); err != nil {
		return nil, err
	}
	return res, nil
}
