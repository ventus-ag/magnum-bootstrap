package main

import (
	"context"
	"fmt"
	"os"

	"github.com/ventus-ag/magnum-bootstrap/provider/hostplugin"
)

func main() {
	provider, err := hostplugin.NewProvider()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := provider.Run(context.Background(), hostplugin.ProviderName, hostplugin.ProviderVersion); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
