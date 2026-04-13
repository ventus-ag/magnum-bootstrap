package hostsdk

import "github.com/pulumi/pulumi/sdk/v3/go/pulumi"

type SystemdServiceArgs struct {
	Unit            pulumi.StringInput    `pulumi:"unit"`
	SkipIfMissing   pulumi.BoolPtrInput   `pulumi:"skipIfMissing,optional"`
	Enabled         pulumi.BoolPtrInput   `pulumi:"enabled,optional"`
	Active          pulumi.BoolPtrInput   `pulumi:"active,optional"`
	Masked          pulumi.BoolPtrInput   `pulumi:"masked,optional"`
	Restart         pulumi.BoolPtrInput   `pulumi:"restart,optional"`
	RestartReason   pulumi.StringPtrInput `pulumi:"restartReason,optional"`
	DaemonReload    pulumi.BoolPtrInput   `pulumi:"daemonReload,optional"`
	RestartOnChange pulumi.BoolPtrInput   `pulumi:"restartOnChange,optional"`
	RestartToken    pulumi.StringPtrInput `pulumi:"restartToken,optional"`
}

type SystemdService struct {
	pulumi.CustomResourceState

	Unit            pulumi.StringOutput      `pulumi:"unit"`
	SkipIfMissing   pulumi.BoolOutput        `pulumi:"skipIfMissing"`
	Enabled         pulumi.BoolPtrOutput     `pulumi:"enabled"`
	Active          pulumi.BoolPtrOutput     `pulumi:"active"`
	Masked          pulumi.BoolPtrOutput     `pulumi:"masked"`
	Restart         pulumi.BoolOutput        `pulumi:"restart"`
	RestartReason   pulumi.StringOutput      `pulumi:"restartReason"`
	DaemonReload    pulumi.BoolOutput        `pulumi:"daemonReload"`
	RestartOnChange pulumi.BoolOutput        `pulumi:"restartOnChange"`
	RestartToken    pulumi.StringOutput      `pulumi:"restartToken"`
	ObservedExists  pulumi.BoolOutput        `pulumi:"observedExists"`
	ObservedEnabled pulumi.BoolOutput        `pulumi:"observedEnabled"`
	ObservedActive  pulumi.BoolOutput        `pulumi:"observedActive"`
	ObservedMasked  pulumi.BoolOutput        `pulumi:"observedMasked"`
	Drifted         pulumi.BoolOutput        `pulumi:"drifted"`
	DriftReasons    pulumi.StringArrayOutput `pulumi:"driftReasons"`
}

func NewSystemdService(ctx *pulumi.Context, name string, args *SystemdServiceArgs, opts ...pulumi.ResourceOption) (*SystemdService, error) {
	if args == nil {
		args = &SystemdServiceArgs{}
	}
	res := &SystemdService{}
	inputs := pulumi.Map{
		"unit": args.Unit,
	}
	if args.SkipIfMissing != nil {
		inputs["skipIfMissing"] = args.SkipIfMissing
	}
	if args.Enabled != nil {
		inputs["enabled"] = args.Enabled
	}
	if args.Active != nil {
		inputs["active"] = args.Active
	}
	if args.Masked != nil {
		inputs["masked"] = args.Masked
	}
	if args.Restart != nil {
		inputs["restart"] = args.Restart
	}
	if args.RestartReason != nil {
		inputs["restartReason"] = args.RestartReason
	}
	if args.DaemonReload != nil {
		inputs["daemonReload"] = args.DaemonReload
	}
	if args.RestartOnChange != nil {
		inputs["restartOnChange"] = args.RestartOnChange
	}
	if args.RestartToken != nil {
		inputs["restartToken"] = args.RestartToken
	}
	if err := ctx.RegisterResource("magnumhost:index:SystemdService", name, inputs, res, withDefaults(opts)...); err != nil {
		return nil, err
	}
	return res, nil
}
