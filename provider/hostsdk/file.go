package hostsdk

import "github.com/pulumi/pulumi/sdk/v3/go/pulumi"

type FileArgs struct {
	Path    pulumi.StringInput  `pulumi:"path"`
	Content pulumi.StringInput  `pulumi:"content"`
	Mode    pulumi.StringInput  `pulumi:"mode"`
	Absent  pulumi.BoolPtrInput `pulumi:"absent,optional"`
}

type File struct {
	pulumi.CustomResourceState

	Path                  pulumi.StringOutput      `pulumi:"path"`
	Mode                  pulumi.StringOutput      `pulumi:"mode"`
	Absent                pulumi.BoolOutput        `pulumi:"absent"`
	ContentSHA256         pulumi.StringOutput      `pulumi:"contentSha256"`
	ObservedExists        pulumi.BoolOutput        `pulumi:"observedExists"`
	ObservedIsRegularFile pulumi.BoolOutput        `pulumi:"observedIsRegularFile"`
	ObservedMode          pulumi.StringOutput      `pulumi:"observedMode"`
	ObservedContentSHA256 pulumi.StringOutput      `pulumi:"observedContentSha256"`
	Drifted               pulumi.BoolOutput        `pulumi:"drifted"`
	DriftReasons          pulumi.StringArrayOutput `pulumi:"driftReasons"`
}

func NewFile(ctx *pulumi.Context, name string, args *FileArgs, opts ...pulumi.ResourceOption) (*File, error) {
	if args == nil {
		args = &FileArgs{}
	}
	res := &File{}
	inputs := pulumi.Map{
		"path":    args.Path,
		"content": args.Content,
		"mode":    args.Mode,
	}
	if args.Absent != nil {
		inputs["absent"] = args.Absent
	}
	if err := ctx.RegisterResource("magnumhost:index:File", name, inputs, res, withDefaults(opts)...); err != nil {
		return nil, err
	}
	return res, nil
}
