package hostsdk

import "github.com/pulumi/pulumi/sdk/v3/go/pulumi"

type DownloadArgs struct {
	URL     pulumi.StringInput `pulumi:"url"`
	Path    pulumi.StringInput `pulumi:"path"`
	Mode    pulumi.StringInput `pulumi:"mode"`
	Retries pulumi.IntPtrInput `pulumi:"retries,optional"`
}

type Download struct {
	pulumi.CustomResourceState

	URL              pulumi.StringOutput      `pulumi:"url"`
	Path             pulumi.StringOutput      `pulumi:"path"`
	Mode             pulumi.StringOutput      `pulumi:"mode"`
	Retries          pulumi.IntOutput         `pulumi:"retries"`
	Checksum         pulumi.StringOutput      `pulumi:"checksum"`
	ObservedExists   pulumi.BoolOutput        `pulumi:"observedExists"`
	ObservedMode     pulumi.StringOutput      `pulumi:"observedMode"`
	ObservedChecksum pulumi.StringOutput      `pulumi:"observedChecksum"`
	Drifted          pulumi.BoolOutput        `pulumi:"drifted"`
	DriftReasons     pulumi.StringArrayOutput `pulumi:"driftReasons"`
}

func NewDownload(ctx *pulumi.Context, name string, args *DownloadArgs, opts ...pulumi.ResourceOption) (*Download, error) {
	if args == nil {
		args = &DownloadArgs{}
	}
	res := &Download{}
	inputs := pulumi.Map{"url": args.URL, "path": args.Path, "mode": args.Mode}
	if args.Retries != nil {
		inputs["retries"] = args.Retries
	}
	if err := ctx.RegisterResource("magnumhost:index:Download", name, inputs, res, withDefaults(opts)...); err != nil {
		return nil, err
	}
	return res, nil
}
