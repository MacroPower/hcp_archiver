// Package view browses an archive interactively in the terminal.
//
// The browser mirrors the HCP Terraform interface: organizations open into
// projects, projects into workspaces, and workspaces into their runs, state
// versions, and variables, with any archived file readable in a scrolling
// viewer. Navigation descends with enter and returns with esc, and every list
// filters with /.
//
// The package reads the archive in its physical forms transparently. An object
// is looked up first as a loose file under the organization root, then in the
// per-workspace NDJSON roll-ups, then in the sealed zip bundles via their
// sidecar indexes, all keyed by the same archive-relative path the ledger
// records. The browser therefore renders a freshly-collected tree and a fully
// sealed one identically, and needs no network access and no ledger.
//
// [Browse] starts the browser; [OpenArchive] exposes the read layer on its
// own.
package view
