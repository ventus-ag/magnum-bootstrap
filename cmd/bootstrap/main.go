package main

import (
	"context"
	"os"

	"github.com/ventus-ag/magnum-bootstrap/internal/app"
)

func main() {
	os.Exit(app.Main(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
