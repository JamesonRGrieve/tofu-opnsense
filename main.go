// SPDX-License-Identifier: AGPL-3.0-or-later

// Command tofu-opnsense is the OpenTofu/Terraform provider plugin entrypoint
// for OPNsense firewalls via the REST API.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/JamesonRGrieve/tofu-opnsense/internal/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
)

// version is overridden at build time via -ldflags.
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "run with support for debuggers like delve")
	flag.Parse()

	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: "registry.terraform.io/jamesonrgrieve/opnsense",
		Debug:   debug,
	})
	if err != nil {
		log.Fatal(err.Error())
	}
}
