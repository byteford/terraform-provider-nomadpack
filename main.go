// Terraform Provider for nomad-pack.
//
// This provider does not render or submit Nomad jobs itself. It shells out
// to the `nomad-pack` CLI binary (must be on PATH, or pointed at via the
// provider's binary_path attribute) and drives its own plan/run/destroy
// lifecycle, so nomad-pack's injected job metadata remains the single
// source of truth for what's deployed on the cluster.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/byteford/terraform-provider-nomadpack/internal/provider"
)

// version is set via -ldflags "-X main.version=..." by goreleaser.
// See .goreleaser.yml.
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "run the provider with support for debuggers like delve")
	flag.Parse()

	opts := providerserver.ServeOpts{
		Address: "registry.terraform.io/byteford/nomadpack",
		Debug:   debug,
	}

	err := providerserver.Serve(context.Background(), provider.New(version), opts)
	if err != nil {
		log.Fatal(err.Error())
	}
}
