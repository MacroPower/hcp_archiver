// Command hcp_archiver archives an HCP Terraform (formerly Terraform Cloud)
// organization to local disk for long-term reference. It captures state
// history, run history, the configuration that produced each run, and the
// surrounding org-level metadata as plain files. Read-only subcommands browse
// an existing archive (view) and list, print, or extract its objects (list,
// show, extract). It does not restore anything back into HCP Terraform.
//
// It is a thin entrypoint. It builds the command-line interface, binds flags
// and environment variables into a configuration, constructs the archiver, runs
// it under a signal-aware context, and maps the outcome to an exit code. It
// carries no archiving logic itself; every behavior lives in the packages it
// wires together.
package main
