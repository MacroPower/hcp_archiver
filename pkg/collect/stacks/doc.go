// Package stacks archives the stacks deployment model, a project-scoped
// alternative to workspaces that an organization enables only when it uses one.
// It is gathered only when its scope toggle is on, and then only for the
// projects the configured allow-list admits.
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
// Every path a stack writes nests under its project's display name, which the
// stack listing does not carry, so the name is read per project and cached. Only
// an answer later runs repeat may key that path: a project with no name of its
// own, or one that is gone, stands in with its id, while a name that merely
// failed to read leaves the stack skipped whole with its surface dropped. The
// skip is what keeps a blip from materializing an id-keyed copy of the stack
// beside the name-keyed one a later run writes, which nothing would ever
// reconcile, since directory claims detect a rename only among siblings and the
// two trees hang off different projects.
//
// The configuration and run walks freeze terminal elements and stop revisiting
// them, so every child enumeration beneath one (a configuration's deployment
// groups, a group's runs walk, a terminal run's steps) runs under a persisted
// obligation marker ([manifest.Obligation]): opened before the enumeration,
// failed on a drop, settled on success. For the nested runs walk it settles
// only once the nested collection itself settled, so a deployment run still
// executing under a terminal configuration holds the configurations walk open
// until its final state and steps land. An open or failed marker keeps the
// enclosing walks re-paging across runs; a marker is a ledger entry only,
// never a file in the archive.
package stacks
