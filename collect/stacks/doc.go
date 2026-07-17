// Package stacks archives the stacks deployment model, a project-scoped
// alternative to workspaces that an organization enables only when it uses one.
// It is gathered only when its scope toggle is on.
//
// A stack record carries its name, VCS repository, project reference, and latest
// configuration. Beneath it the package walks each configuration with its JSON
// schemas and diagnostics, and then the deployment groups under that
// configuration. Runs hang off deployment groups rather than off named
// deployments: each group's deployment runs resolve into per-step artifacts
// (the plan and apply descriptions) and step diagnostics. Named deployments are
// captured for their own metadata and latest-run reference (the API exposes no
// full run list for them), and each stack's states are captured as the full
// state description per generation.
//
// This traversal pivots on deployment groups, unlike the workspace-and-run
// model, which is why the package stands apart from the core project walk.
//
// The configuration and run walks freeze terminal elements and stop revisiting
// them, so a child enumeration that fails beneath one (a configuration's
// deployment groups, a group's runs, a terminal run's steps) records a
// persisted listing marker in the ledger. The errored marker holds the
// enclosing walks open across runs until a later pass re-fetches the missing
// children; a marker is a ledger entry only, never a file in the archive.
package stacks
