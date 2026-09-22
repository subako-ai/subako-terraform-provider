// Command terraform-provider-subako serves the Subako Terraform provider.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/subako-ai/terraform-provider-subako/internal/provider"
)

// version is stamped at release build time with -ldflags.
var version = "dev"

// Address is the registry address configurations name the provider by.
//
// CODESYNC(terraform-provider-address)
const Address = "registry.terraform.io/subako-ai/subako"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "run the provider with support for debuggers like delve")
	flag.Parse()

	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: Address,
		Debug:   debug,
	})
	if err != nil {
		log.Fatal(err.Error())
	}
}
