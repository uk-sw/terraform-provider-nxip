package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/uk-sw/terraform-provider-nxip/internal/provider"
)

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "set to true to run the provider with support for debuggers")
	flag.Parse()

	opts := providerserver.ServeOpts{
		Address: "registry.terraform.io/uk-sw/nxip",
	}

	err := providerserver.Serve(context.Background(), provider.New("1.0.0"), opts)
	if err != nil {
		log.Fatal(err.Error())
	}
}
