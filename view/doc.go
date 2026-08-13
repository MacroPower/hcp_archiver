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
// sealed one identically, and needs no ledger: everything it knows, it learns
// from the tree. Only reading a member of a bundle whose zip was evicted to a
// remote store touches the network.
//
// [Browse] starts the browser; [OpenArchive] exposes the read layer on its
// own, and [Archive] layers org-prefixed addressing over it for logical
// listing, single-object reads, and unsealing any scope back into loose
// files.
package view
