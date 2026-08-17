// Command hcp_archiver archives an HCP Terraform (formerly Terraform Cloud)
// organization to local disk for long-term reference. It captures state
// history, run history, the configuration that produced each run, and the
// surrounding org-level metadata as plain files. Read-only subcommands browse
// an existing archive (view) and list, print, or extract its objects (list,
// show, extract). It does not restore anything back into HCP Terraform.
//
// It is a thin entrypoint; the command-line interface lives in the internal
// cli package.
package main

import (
	"context"
	"os"

	"charm.land/fang/v2"
	"go.jacobcolvin.com/niceyaml/fangs"
	"go.jacobcolvin.com/x/version"

	"go.jacobcolvin.com/hcp_archiver/internal/cli"
)

func main() {
	err := fang.Execute(
		context.Background(),
		cli.NewRootCmd(),
		fang.WithVersion(version.GetVersion()),
		// Preserve the multi-line, source-annotated formatting of a niceyaml
		// configuration error instead of collapsing it into a single styled block.
		fang.WithErrorHandler(fangs.ErrorHandler),
	)
	if err != nil {
		os.Exit(1)
	}
}
