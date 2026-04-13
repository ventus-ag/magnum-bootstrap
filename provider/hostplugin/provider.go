package hostplugin

import (
	providerpkg "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/infer"
)

func NewProvider() (providerpkg.Provider, error) {
	return infer.NewProviderBuilder().
		WithDisplayName("Magnum Host Provider").
		WithDescription("Host-local file and directory resources for magnum-bootstrap.").
		WithRepository("https://github.com/ventus-ag/magnum-bootstrap").
		WithGoImportPath("github.com/ventus-ag/magnum-bootstrap/provider/hostplugin").
		WithKeywords("pulumi", "magnum", "host", "system").
		WithResources(
			infer.Resource(&File{}),
			infer.Resource(&Directory{}),
			infer.Resource(&Line{}),
			infer.Resource(&Export{}),
			infer.Resource(&Copy{}),
			infer.Resource(&Download{}),
			infer.Resource(&SystemdService{}),
			infer.Resource(&Mode{}),
			infer.Resource(&Ownership{}),
			infer.Resource(&Sysctl{}),
			infer.Resource(&ModuleLoad{}),
			infer.Resource(&ExtractTar{}),
		).
		Build()
}
