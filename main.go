package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/uk-sw/terraform-provider-nxip/internal/provider"
)

// version is overridden at build time by GoReleaser via
// -ldflags="-X main.version={{.Version}}", so a release binary always
// reports the exact tag it was built from - nothing to remember to bump
// by hand in source before cutting a release.
var version string = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "set to true to run the provider with support for debuggers")
	flag.Parse()

	opts := providerserver.ServeOpts{
		Address: "registry.terraform.io/uk-sw/nxip",
	}

	err := providerserver.Serve(context.Background(), provider.New(version), opts)
	if err != nil {
		log.Fatal(err.Error())
	}
}
