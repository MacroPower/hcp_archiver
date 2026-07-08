# tfc_archiver

A standalone tool that archives an HCP Terraform (formerly Terraform Cloud)
organization to plain files on disk for **long-term reference**, not for
restoration back into HCP Terraform.

The archive is a predictable tree of JSON documents and raw blobs: grep-able,
diff-able, and readable years from now with no dependency on HCP or on this tool
still existing.

## Goal & non-goals

- **Goal**: capture as much fidelity as possible so the archive can answer
  "what existed, and what changed" long after the fact (state over time, run
  history, the configuration that produced each run, and the surrounding
  org-level metadata).
- **Non-goal**: recreating an organization inside HCP Terraform. There is no
  import graph, no dependency ordering, and no attempt to reconstruct objects.
- **Fidelity**: everything the API will still return. That means all state
  versions, all runs, all plan/apply logs, all still-retained configuration
  versions, per-run governance and task results, and org-level metadata
  (projects, teams and access, VCS connections, policy and variable sets, run
  tasks and triggers, notifications, agent pools, the private registry, and the
  audit trail). The heaviest or most org-specific of these (Stacks, HYOK, the
  deeper registry detail, and the audit trail) are opt-in (off by default).

## Install

```bash
task build          # cross-compile snapshot binaries to ./dist via Dagger
go install ./cmd/tfc_archiver
```

## Usage

Authentication reads `TFC_TOKEN` or `TFE_TOKEN` from the environment.

```bash
export TFC_TOKEN=...    # an HCP Terraform user, team, or organization token

tfc_archiver version
```

### Configuration surface

- `TFC_TOKEN` / `TFE_TOKEN` (required): the archiving identity's API token.
- **address**: the API endpoint, defaulting to `https://app.terraform.io`.
- **organization**: optional; when omitted, every organization the token can
  see is archived in turn.
- **output directory**: the archive root. Resume and incremental re-run are
  implied when the directory already holds an archive.
- **workspace concurrency**: size of the worker pool over workspaces.
- **scope toggles** for the heavy or optional surfaces (Stacks, HYOK, and the
  registry version/platform/binary detail) and the audit trail.
- **`--progress=auto|human|json|quiet`**: progress format, defaulting to
  `auto` (human-readable on a TTY, quiet off one), plus a progress-interval
  knob.
- **`--recheck-absent`**: re-probe objects previously recorded as permanently
  gone, for when an operator suspects one has been restored.

## Output layout

```
archive/<org>/
  org.json                          organization metadata
  manifest.json                     per-object ledger + run records & watermarks

  # org-level objects (not scoped to a single project)
  teams/<id>/
    team.json                       definition + access matrix, members, SSO/SCIM
    notification-configs.json       team-scoped alerting (Token redacted)
  memberships.json                  org roster: email, status, user + team refs
  oauth-clients/<id>.json           VCS connection + tokens (Secret redacted)
  github-app-installations.json     GitHub App installs (metadata only)
  variable-sets/<id>/               set metadata + variables (values redacted)
  policy-sets/<id>/                 set metadata + parameters (values redacted)
  policies/<id>.json                policy metadata
  policies/<id>.<ext>               Sentinel/OPA source
  run-tasks.json                    org run-task definitions (HMACKey redacted)
  agent-pools/<id>.json             pool config + allowed/excluded scopes
  token-ttl-policies.json           org token max-TTL governance
  audit-trails/                     audit config + windowed who-did-what pages
  reserved-tag-keys.json            org tag governance
  hyok-configurations/<id>.json     HYOK encryption config (optional)
  registry/                         modules, no-code modules, providers, GPG keys
  config-versions/<cv-id>.tar.gz    deduped org-wide; runs reference by id

  # project-scoped objects nest beneath the owning project
  projects/<project-name>/
    project.json                    defaults, tag bindings, team access
    notification-configs.json       project-scoped alerting (Token redacted)
    workspaces/<ws-name>/
      workspace.json                full settings + project ref
      variables.json                values redacted when sensitive
      readme.md, tags.json, team-access.json, notification-configs.json
      run-triggers.json, run-tasks.json, remote-state-consumers.json
      state-versions/               raw + JSON state blobs, per-version metadata
      runs/<run-id>/                run summary, config version, plan/apply logs,
                                    plan json, cost estimate, comments, events,
                                    policy checks, task stages, TF policy outcomes
    stacks/<name>/                  stack config, deployment groups, runs, states
```

Layout rules worth knowing:

- **Everything a project owns nests under `projects/<project-name>/`.** Both
  workspaces and stacks carry a project relation, so the tree groups by project
  rather than scattering `workspaces/` and `stacks/` at the top level. A
  workspace with no explicit project lands in the organization's default
  project. Only genuinely org-scoped objects stay at the root.
- **Projects key on name; the id lives inside `project.json`.** Project names
  are unique within an organization, so the directory is unambiguous, while
  RBAC joins still resolve by the recorded id.
- **State versions order by creation time, not serial.** A serial is not
  zero-padded (so it sorts wrong lexically) and is not unique (a rollback can
  repeat one); ordering and "latest" logic key on the creation timestamp, and
  the id keeps filenames unique.
- **Configuration version tarballs are deduped org-wide.** Their ids are
  globally unique, so each tarball is stored once and runs reference it by id;
  a run keeps the ingress attributes (commit sha, branch, PR) even when the
  tarball itself has expired.

## Limitations

Some data cannot be archived at full fidelity, or at all. The archive records
what it can and marks the rest so gaps are never mistaken for missing objects.

- **Secrets are metadata-only.** Write-only fields return blank on read, so
  sensitive variable, variable-set, and policy-set-parameter values, every
  token secret, OAuth client secrets, run-task HMAC keys, and notification
  tokens are recorded as `[REDACTED]`. SSH private keys never come back at all.
- **Raw state is sensitive and stored in cleartext.** State blobs embed
  sensitive variable, output, and resource values; the API only redacts them
  through an endpoint that returns a lossy subset. **Treat the archive as
  secret at rest.**
- **Retention- and version-gated artifacts are best-effort.** Plan and apply
  logs expire; structured plan JSON exists only for recent Terraform versions;
  many VCS-driven configuration versions have no downloadable tarball; audit
  trails cover only HCP's retention window and need an elevated token. The
  archive grabs whatever is still downloadable and records the rest as absent.
- **A few endpoints are not enumerable** and are reachable only when another
  object references them (plan exports, HYOK data keys, OIDC configs, some
  registry types); completeness there depends on the referencing objects.
- **Billing, entitlements, and subscription tier** are platform-managed with no
  export, and native soft-delete recovery is out of scope.

The archive is **not point-in-time consistent**: a long run against a live
organization sees new runs and state versions appear mid-walk. It captures each
collection's delta as of when it reaches that collection, which is acceptable
for a best-effort snapshot.

## Behavior

- **Best-effort, not fail-fast.** A `404`/`410` on one object is recorded and
  the archive continues; one missing log never aborts the whole run.
- **Resumable and re-runnable.** A durable manifest records per-object status,
  content signatures, and per-collection high-water marks. Re-invoking against
  an existing archive skips what is done or permanently gone, retries what
  errored, appends what is new, and refreshes mutable metadata, without
  re-downloading immutable blobs.
- **Live progress.** The run reports forward motion to stderr, in a
  human-readable form on a TTY and as one JSON object per line for CI or a
  watcher, with a final per-status summary.

## Implementation notes

- Built on the official **`hashicorp/go-tfe` v1 client**, pinned to `v1.109.0`.
  v1 is frozen but complete and the highest-fidelity read surface; v2 (a
  separate, beta, nightly-regenerated module) omits roughly twenty operations
  this archive needs today (the entire Stacks and private-registry families
  among them), so it is disqualifying here. The two modules have distinct import
  paths, so v2 can be adopted additively later for anything v1 lacks.
- Serialization marshals the go-tfe response structs through their vendored
  jsonapi tags (kebab-case, matching the public API docs), redacts sensitive
  values by mutation, drops ephemeral signed URLs, and flattens hydrated
  relations to ids.

## Development

```bash
devbox install      # provision the toolchain (or run `direnv allow`)
task check          # local gate: lint + test
task check:all      # everything CI runs (adds the Dagger-backed gates)
```

This is a self-contained Go module so the `go-tfe` dependency tree stays
isolated. CI runs through a local Dagger toolchain (`dagger call ci <task>`),
composing shared toolchains from [go.jacobcolvin.com/x][x] (devbox, goreleaser,
security, zizmor).

[x]: https://github.com/MacroPower/x
[fang]: https://github.com/charmbracelet/fang
