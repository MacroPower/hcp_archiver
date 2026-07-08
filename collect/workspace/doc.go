// Package workspace archives the project-scoped core of an organization: each
// project and its settings, then every workspace beneath it together with the
// append-mostly history of state versions and runs. Work within a single
// workspace is sequential, while the orchestrator runs workspaces concurrently.
//
// # Projects and workspace settings
//
// For each project it captures the project record (default execution mode,
// agent pool, auto-destroy, tag bindings), its project-scoped notification
// configurations, and its team access. For each workspace it captures the full
// settings (including the project reference and the global-remote-state flag),
// the variables (read through the all-vars endpoint so variable-set-inherited
// variables are included, with a value redacted when the variable is
// sensitive), the readme, the tag and effective-tag bindings, team access,
// notification configurations, inbound run triggers, per-workspace run-task
// bindings, and, only when global remote state is disabled, the remote-state
// consumers.
//
// # State versions
//
// State versions are listed by organization and workspace name and ordered by
// creation time. Each is stored as the raw state pulled from its hosted download
// URL, the JSON-format state when one is available, and a metadata sidecar with
// the serial, creation time, originating run, size, and VCS commit SHA. The
// state-version outputs endpoint is intentionally skipped: it redacts
// sensitive outputs and so is only a lossy subset of the raw state already
// captured.
//
// # Runs and their children
//
// Runs are listed newest-first and hydrated with their plan, apply,
// configuration-version, created-by, and cost-estimate relations. Beneath each
// run go the run summary; the configuration-version record, kept as its id plus
// ingress attributes (commit SHA, branch, PR) so the join survives even after
// the tarball itself has expired; the plan log and, when the Terraform version
// is recent enough to offer it, the structured plan JSON; the apply log; the
// cost estimate, kept as its own attributes (the monthly cost deltas and matched
// and unmatched resource counts) plus its human-readable log rather than
// flattened to a bare id; the run comments; the actor-attributed run events; the
// policy checks with their logs; the task stages resolved into task results and
// policy evaluations down to policy-set outcomes; and native Terraform policy-
// evaluation outcomes. Comments come from their own list endpoint because the
// run's comment relation is not sideloadable through an include.
//
// Configuration-version tarballs are deduplicated org-wide by their globally
// unique id, so workspaces that share one stay race-free; a run references its
// tarball by id and never by the expiring signed upload URL a hydrated relation
// would otherwise nest.
//
// Two workspace-adjacent surfaces are deliberately omitted as derivable
// duplicates: the flat workspace resource list, and the legacy organization tag
// list, since each workspace's tags are already recorded on the workspace itself.
package workspace
