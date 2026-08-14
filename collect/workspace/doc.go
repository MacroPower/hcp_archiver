// Package workspace archives the project-scoped core of an organization: each
// project and its settings, then every workspace beneath it together with the
// append-mostly history of state versions and runs. The orchestrator runs
// workspaces concurrently, and work within a single workspace fans out too:
// the state-version and run walks proceed side by side and each page's items
// archive concurrently, with the shared client's request gates bounding the
// run's real parallelism. The run walk's list pages draw on the client's
// separate runs-list gate, since the server meters that endpoint far more
// tightly; everything else, run children included, draws on the general gate.
//
// Both histories are paged by page number over a listing that stays live while
// the walk archives it, so an element deleted upstream between two page fetches
// shifts every later element up a slot and would drop the one that had been
// about to lead the next page out of the walk entirely, silently and for good.
// Both walks therefore page through a stable pager, which watches the listing's
// reported total for a drop, re-lists from the boundary the shift could have
// reached back to, and serves each element at most once.
//
// # Projects and workspace settings
//
// For each project it captures the project record (default execution mode,
// agent pool, auto-destroy, tag bindings), its resolved effective tag bindings
// (hydrated on the project read and kept as their own record rather than dropped
// to bare id refs), its project-scoped notification configurations, and its team
// access. For each workspace it captures the full
// settings (including the project reference and the global-remote-state flag),
// the variables (read through the all-vars endpoint so variable-set-inherited
// variables are included; a sensitive value reads back empty from the API and
// is stored as returned), the readme, the tag and effective-tag bindings, team access,
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
// configuration-version, created-by, and cost-estimate relations. An optional
// run-history limit ([WithRunHistoryLimit]) bounds each workspace's run walk
// to its newest runs by count and/or creation age, keeping whichever window
// admits more history; unbounded by default. Each of those
// hydrated relations would otherwise collapse to a bare id ref on the run
// summary, so it is archived as its own record: the configuration-version record
// and, split out beside it, its ingress attributes (commit SHA, branch, PR) so
// the VCS join survives even after the tarball has expired; the plan and apply
// summaries (resource counts and change flags) alongside their logs; and the
// user that created the run. Beneath each run also go the run summary itself; the
// structured plan JSON when the Terraform version is recent enough to offer it;
// the cost estimate, kept as its own attributes (the monthly cost deltas and
// matched and unmatched resource counts) plus its human-readable log; the run
// comments; the actor-attributed run events, whose actors are archived as their
// own user records; the policy checks with their logs; the task stages as
// listed, whose task-result and policy-evaluation relations stay bare id refs;
// and the native Terraform policy evaluations with their set outcomes. Comments
// come from their own list endpoint because the run's comment relation is not
// sideloadable through an include.
//
// A run's created-by and each run event's actor are hydrated users, and go-tfe
// exposes no user listing, so these sub-objects are the only capture of who
// queued or confirmed a run; each is archived to the org-level users directory,
// shared org-wide so a user who acts across workspaces is stored once.
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
