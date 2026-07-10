# hcp_archiver

A standalone tool that archives an HCP Terraform (formerly Terraform Cloud)
organization to plain files on disk for **long-term reference**, not for
restoration back into HCP Terraform.

The archive is a predictable tree of JSON documents and raw blobs: diff-able and
readable years from now with no dependency on HCP or on this tool still existing.
The JSON metadata stays grep-able on disk; the heavy audit-only artifacts (plan
and apply logs, plan JSON, and raw state) are packed into per-workspace `zip`
bundles, so grepping those means unzipping the one bundle a sidecar points at.

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
go install ./cmd/hcp_archiver
```

## Usage

Authentication reads `HCP_TOKEN` from the environment, falling back to
`TFC_TOKEN` and then `TFE_TOKEN` for compatibility.

```bash
export HCP_TOKEN=...    # an HCP Terraform user, team, or organization token

hcp_archiver version
```

### Configuration

What and how to archive lives in a YAML configuration file; only per-run and
secret settings are flags or environment variables. Point `--config` (`-c`) at
the file or set `HCP_ARCHIVER_CONFIG`; with neither, the built-in defaults apply
(every visible organization, default surfaces only, concurrency 4).

```yaml
# yaml-language-server: $schema=./config/config.schema.json

# HCP Terraform API address (the default).
address: https://app.terraform.io

# Organizations to archive; omit or leave empty for every visible org.
organizations:
  - my-org

# Workspaces archived at once, within the per-token rate limit.
concurrency: 4

# Heavy or org-specific surfaces, each off by default.
scope:
  stacks: true
  hyok: true
  registryDetail: true
  auditTrail: true
```

Every key is optional and defaults as shown. The `yaml-language-server`
directive gives editors completion and validation from the same schema embedded
in the binary, so a malformed file is reported with the offending line
highlighted before any network call. A ready-to-copy
[`hcp_archiver.example.yaml`](hcp_archiver.example.yaml) sits at the repository
root.

### Archiving an organization

The only required flag is the output directory. Point `--output` (`-o`) at the
archive root and `--config` at the file:

```bash
hcp_archiver --config hcp_archiver.yaml --output ./archive
```

With no configuration file, or one that names no organizations, every
organization the token can see is archived, each into its own `./archive/<org>/`
subtree:

```bash
hcp_archiver --output ./archive
```

### Browsing an archive

`view` opens an archive in an interactive terminal UI that mirrors the HCP
interface: an organization opens into its projects, a project into its
workspaces, and a workspace into its runs, state versions, and variables, with
any archived document (run summaries, plan and apply logs, raw state, the whole
file tree) readable in a scrolling viewer. It needs no token and no network.

```bash
hcp_archiver view ./archive          # the archive root, or one org's directory
```

Navigation descends with `enter`, returns with `esc`, filters any list with
`/`, and quits with `q`. The browser reads the archive's physical forms
transparently, so an object displays the same whether it is still a loose file
or has been sealed into an NDJSON roll-up or a zip bundle.

### Resuming and re-running

The output directory is the unit of resume: run the same command again and the
durable [manifest](#behavior) drives an incremental pass that skips what is done
or permanently gone, retries what errored, appends new runs and state versions,
and refreshes mutable metadata, without re-downloading immutable blobs. An
interrupted run (Ctrl-C or `SIGTERM`) exits cleanly, and the next invocation
continues from where it stopped. There is no separate resume command; it is the
second run against the same directory.

### Combining tokens for a superset

Coverage is bounded by the archiving identity, so no single token necessarily
sees everything. An object the token may not read (an HTTP 403, such as the
GitHub App installations list, which rejects team and organization tokens) is
recorded as `forbidden` and, unlike a permanent absence, is **not** settled: a
later run under a differently scoped token retries it. Point several tokens at
the same output directory in turn to accumulate the union of what each can read:

```bash
HCP_TOKEN=$team_token hcp_archiver -c hcp_archiver.yaml -o ./archive
HCP_TOKEN=$user_token hcp_archiver -c hcp_archiver.yaml -o ./archive
```

Each pass keeps everything already captured and fills in only what its token can
now reach.

### Progress and logging

Progress is written to stderr; `--progress` selects the form:

- `auto` (default) — the human line on a TTY, silent when redirected.
- `human` — force the human line even off a TTY.
- `json` — one JSON object per line, for CI or a watcher.
- `quiet` — no progress output.

`--progress-interval` (default `5s`) sets the cadence. Run logs are separate
from progress and have their own knobs, `--log-level` (`error|warn|info|debug`)
and `--log-format` (`text|logfmt|json`), so a warning about a skipped collection
stays legible next to `--progress=json`.

### Reading the run summary

A run ends with a per-status summary line; the counts are the resume model made
visible:

- **done** — fetched and written.
- **absent** — permanently gone (a 404/410), re-probed only with
  `--recheck-absent`.
- **forbidden** — the token may not read it (a 403); retried on the next run, so
  a broader token can still capture it (see above). Counted apart from an error.
- **errored** — a transient or unclassified failure, retried next run; a healthy
  archive ends with `errored=0`.
- **skipped** / **n/a** — intentionally deferred or not applicable to this
  archive; settled, and never mistaken for a gap.

A non-zero `errored` is the count to investigate; `forbidden`, `absent`,
`skipped`, and `n/a` are recorded gaps, not failures.

### Configuration surface

Settings are grouped by how much they vary; see
[Configuration](#configuration) for the file format.

- **Environment**: `HCP_TOKEN` / `TFC_TOKEN` / `TFE_TOKEN` (required, first
  non-empty wins) is the archiving identity's API token, and
  `HCP_ARCHIVER_CONFIG` is the default configuration file path.
- **Flags**: `--config` / `-c` points at the YAML configuration file;
  `--output` / `-o` is the archive root (required; resume and incremental
  re-run are implied when it already holds an archive); `--progress` selects the
  progress format (`auto|human|json|quiet`, default `auto`: human on a TTY,
  quiet off one) with a `--progress-interval` knob; and `--recheck-absent`
  re-probes objects previously recorded as permanently gone.
- **Configuration file** (every key optional, defaulted per field): `address`
  (the API endpoint, default `https://app.terraform.io`), `organizations` (a
  list; empty or omitted archives every organization the token can see in turn),
  `concurrency` (size of the worker pool over workspaces), and a `scope` block
  of toggles for the heavy or optional surfaces (`stacks`, `hyok`,
  `registryDetail`, `auditTrail`), each off by default.

## Output layout

```
archive/<org>/
  org.json                          organization metadata
  .ledger/                          sharded per-object ledger + run records & watermarks

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
- **On disk, frozen heavy artifacts seal and small metadata coalesces.** The tree
  above is the logical namespace: every object's stable path. A collected
  workspace's plan/apply logs, plan JSON, and raw + JSON state pack into
  per-workspace `zip` bundles under `bundles/` (each with a `.sidecar.ndjson`
  index), and the immutable run children and state-version metadata coalesce into
  NDJSON roll-ups under `rollups/`; `run.json` stays loose. Every object keeps its
  stable path as the key, so resume and incremental re-run are unaffected.

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
