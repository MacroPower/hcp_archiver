// Package store owns the on-disk archive tree: both where every object lands and
// how its bytes are committed. It maps a logical object (an organization,
// project, workspace, state version, run, or one of their children) to a stable
// relative path under the archive root, and writes JSON payloads and raw blob
// streams to those paths atomically. Mutable metadata is overwritten in place
// only when its content changes, and a caller that opts in (see [WithHistory])
// has the outgoing content appended to an append-only history sidecar beside
// the object (variables.json gains variables.history.ndjson) before the
// overwrite, so no version the archive ever held is lost to a refresh. Callers
// reach the filesystem only
// through this package, so the layout is the archive's single naming authority,
// and the relative path it computes doubles as the opaque key the ledger records
// an object under.
//
// # Layout rules
//
// Everything a project owns nests beneath projects/<project-name>/. Both
// workspaces and stacks carry a project relation, so the tree groups by project
// rather than scattering workspaces and stacks as org-level siblings; a
// workspace with no explicit project lands in the organization's default
// project. Only genuinely org-scoped objects (teams, VCS clients, policy and
// variable sets, agent pools, the registry, audit trails, and the org-wide
// configuration versions) stay at the top level.
//
// Projects key on name, which is unique within an organization and so makes the
// directory unambiguous, while the id recorded inside project.json is what RBAC
// joins resolve against. State versions order by creation time, not by serial: a
// serial is an int64 that is neither zero-padded (so it sorts wrong lexically)
// nor unique (a rollback or re-upload can repeat one), so filenames carry the
// creation timestamp for ordering and the id for identity, with the serial kept
// in the per-version metadata. Configuration version tarballs use globally
// unique ids and are deduplicated org-wide: stored once and referenced by id
// from each run, so the shared directory stays race-free even as concurrent
// workspaces write into it.
//
// Notification configurations are polymorphic across workspace, project, and
// team scope, so they are archived at all three levels rather than only under
// workspaces.
//
// # Sensitivity
//
// Everything is stored exactly as the API returned it: the raw state blobs
// embed sensitive variable, output, and resource values in cleartext, and the
// serialized objects keep whatever secret material the API chose to return.
// The whole archive should therefore be treated as secret at rest; every file
// is written owner-only (0600).
package store
