# HCP Archiver: Design Notes

A standalone tool to fully archive an HCP Terraform (formerly Terraform Cloud)
account to disk for **long-term reference**, not restoration back into TFC.

## Goal & non-goals

- **Goal**: capture as much fidelity as possible so future-you can browse and
  grep what existed: state over time, run history, the config that produced it,
  and surrounding metadata.
- **Non-goal**: restoring an org back into TFC. No `tfe`-provider import graph,
  no dependency ordering, no attempt to recreate objects.
- **Output philosophy**: plain files (JSON + raw blobs) in a predictable tree.
  Grep-able, diff-able, readable in ten years with no dependency on HCP or on
  this tool still existing.

## Why a custom tool

No existing community tool does a complete high-fidelity archive:

- S3 state-backup tools (`Olgoetz/terraform-aws-tfc-state-backup`,
  `Quentral/tfc-state-dump`) capture state only, no runs, logs, or config
  versions.
- `holtkampricardo/hcp-terraform-org-backup-restore` captures org metadata but
  is restore-oriented and skips run logs / full state history.

So: script the API directly. Chosen stack is **Go with the official
`hashicorp/go-tfe` client** (handles pagination and the download-URL dance
cleanly). Runs once top-to-bottom; tar the result.

## Hard limits of the TFC API (things that CANNOT be archived)

These fall into three general classes, plus platform data.

- **Full fidelity, no local redaction.** Every payload is stored exactly as
  the API returned it. Most secret fields are write-only upstream and come back
  blank on read (sensitive variable, variable-set, and policy-set-parameter
  values, and the team / org / agent / user token secrets), so the archive
  records their existence and metadata with whatever value — usually empty —
  the server sent; anything the API does return (for example a
  `NotificationConfiguration.Token`) is kept verbatim, which is part of why
  the archive is secret at rest. `SSHKey` is a related but distinct case:
  its read model exposes only `ID`/`Name` (the private key lives solely on the
  write-only create options), so key material is never returned at all; there
  is no field to blank.
- **Non-enumerable endpoints**: no `List`, so objects are reachable only when
  another object references their id: `PlanExports`, `HYOKEncryptedDataKeys`,
  the four OIDC config types (AWS/GCP/Azure/Vault, reached via
  `HYOKConfigurations` with `include=oidc_configuration`), and two registry
  types: `ProviderSets` (experimental/internal) and `RegistryComponents`
  (API-only; only its tag bindings enumerate, and only for a known component id).
- **Retention / version-gated artifacts**, grab whatever is still
  downloadable:
  - Config version tarballs are retention-limited, and _structurally_
    speculative and many VCS-driven config versions have no downloadable
    tarball at all (only ingress attributes / a VCS ref). So metadata is often
    the only recoverable artifact regardless of age.
  - Plan/apply logs expire; `Plans.ReadJSONOutput` exists only for
    sufficiently recent Terraform versions (older runs return 404).
  - Audit trails cover only HCP's retention window and need an elevated token
    (org owner / audit token).
  - Registry module tarballs and provider binaries have no typed download
    method (only a source URL / opaque `Links` map). Capture metadata +
    shasums, best-effort on the blobs.
- **Billing / entitlements / subscription tier**: platform-managed, no export.
- Native 30-day soft-delete/recovery UI is out of scope.

## Fidelity level chosen: EVERYTHING

All state versions, all runs, all plan/apply logs, all still-retained config
versions, per-run governance/task results, and the surrounding org-level
metadata (projects, teams and access, VCS connections, policy/variable sets,
run tasks/triggers, notifications, agent pools, registry, audit trail). The
heavy or org-specific surfaces among these (Stacks, HYOK, the deeper registry
version/platform/binary detail, and the audit trail) default off and are
captured only when a run opts in (the audit trail also needs an elevated token);
everything else is captured on every run.

## Output layout

```
archive/<org>/
  org.json                          # organization metadata
  .ledger/                          # sharded per-object ledger + run records &
                                     #   watermarks driving resume / re-run
                                     #   (see Behavior decisions)
  .remote.json                      # only with remote offload configured: the
                                     #   read-relevant backend settings (bucket,
                                     #   prefix, endpoint, region, path style)

  # --- org-level objects (not scoped to a single project) ---
  teams/<id>/
    team.json                       # def + OrganizationAccess matrix, members,
                                     #   SSO/SCIM linkage
    notification-configs.json       # team-scoped alerting
  memberships.json                  # org roster: email, status, user + team refs
  users/<id>.json                   # hydrated user records; no user-list API, so
                                     #   sourced from run/event/team/membership refs
  oauth-clients/<id>/
    oauth-client.json               # VCS connection def
    tokens/<token-id>.json          # token metadata (uid, ssh-key flag; no secret)
  github-app-installations.json     # GitHub App VCS installs (metadata only;
                                     #   user/token-scoped, not org-scoped)
  variable-sets/<id>/
    variable-set.json               # name, global/priority, applied scopes
    variables.json                  # sensitive values read back empty upstream
  policy-sets/<id>/
    policy-set.json                 # kind, scope, current/newest version refs
    current-version.json            # current version source/status/timestamps
    newest-version.json             # newest version source/status/timestamps
    parameters.json                 # sensitive values read back empty upstream
  policies/<id>.json                # metadata
  policies/<id>.<ext>               # Sentinel/OPA source (Policies.Download)
  run-tasks.json                    # org run-task definitions
  agent-pools/<id>.json             # pool config + allowed & excluded ws, allowed projects
  token-ttl-policies.json           # org max-TTL-per-token-type governance
  audit-trails/
    config.json                     # whether/how auditing is on (elevated token)
    <since>-p<page>.json            # who-did-what-when (elevated token; pages
                                     #   named by window stamp + page number)
  reserved-tag-keys.json            # org tag governance (optional)
  hyok-configurations/<id>/         # HYOK encryption config (optional)
    hyok-configuration.json         # KEK id, KMS options, status
    oidc-configuration.json         # concrete AWS/GCP/Azure/Vault OIDC config
    key-versions/<kv-id>.json       # per customer-key-version status
  registry/
    modules/<ns>/<name>/<provider>/               # nested dirs, not hyphen-joined:
      module.json                                 #   all three segments can
      commits.json                                #   themselves contain hyphens
      version-<version>.json                      # per-version detail (registryDetail)
    no-code-modules/<id>.json                     # version pin (BETA API; via
                                                   #   module include)
    no-code-module-variables/<id>.json            # per-version variable options
    providers/<ns>/<name>/
      provider.json
      version-<version>.json                      # + platforms-<version>.json,
      platforms-<version>.json                    #   shasums (registryDetail)
    gpg-keys/<namespace>/<key-id>.json            # ascii-armor public signing keys
                                                   #   (namespaced; ListPrivate)
  config-versions/<cv-id>.tar.gz    # deduped org-wide; runs reference by id

  # --- project-scoped objects: everything a project owns nests beneath it ---
  projects/<project-name>/
    project.json                    # id, defaults (exec mode, agent pool,
                                     #   auto-destroy), tag bindings
    team-access.json                # project RBAC (TeamProjectAccess)
    effective-tag-bindings.json     # resolved bindings, including inherited
    notification-configs.json       # project-scoped alerting

    workspaces/<ws-name>/
      workspace.json                # full settings + Project ref, GlobalRemoteState
      variables.json                # sensitive values read back empty upstream
      readme.md                     # workspace README (Workspaces.Readme)
      tags.json                     # tag + effective-tag bindings
      team-access.json              # workspace RBAC (which team, what access)
      notification-configs.json     # alerting wiring
      run-triggers.json             # inbound cross-workspace trigger sources
      run-tasks.json                # per-workspace run-task bindings
      remote-state-consumers.json   # only when GlobalRemoteState=false
      state-versions/
        <created-at>-<id>.tfstate.json  # raw state (hosted-state-download-url)
        <created-at>-<id>.json          # JSON-format state, if available
        <created-at>-<id>.meta.json     # serial, created-at, run ref, size, vcs sha
      runs/<run-id>/
        run.json                    # summary, status, timestamps, trigger, message
        config-version.json         # cv record (ingress relation is a bare ref here)
        config-version-ingress.json # commit sha/branch/PR (survives tarball expiry)
        plan-summary.json           # plan resource counts + change flags
        plan.log                    # plan log output
        plan.json                   # structured plan (ReadJSONOutput), if available
        apply-summary.json          # apply resource counts
        apply.log                   # apply log output
        cost-estimate.json          # monthly cost deltas + resource counts, if any
        cost-estimate.log           # human-readable cost breakdown (Logs)
        comments.json               # run comments
        run-events.json             # actor-attributed timeline (confirm/discard)
        policy-checks.json          # Sentinel results (+ policy-check-<id>.log)
        task-stages.json            # task stages -> task results + policy evals
        tf-policy-evaluations.json  # native TF policy evaluation metadata, if any
        tf-policy-outcomes.json     # native TF policy set outcomes, if any

    # stacks are a parallel deployment model, also project-scoped; present only
    # if the project uses them. runs live under deployment GROUPS, not named
    # deployments (see Stacks in the go-tfe API surface below)
    stacks/<name>/
      stack.json                    # name, VCS repo, project ref, latest config
      configurations/<id>/
        configuration.json
        json-schemas.json           # once the configuration is terminal
        diagnostics.json
        deployment-groups/<gid>/
          group.json
          runs/<run-id>/
            run.json
            steps/<step-id>/        # step.json, plan-description.json,
                                     #   apply-description.json, diagnostics.json
      deployments/<name>/
        deployment.json             # named-deployment metadata + latest-run ref
      states/<deployment>-<generation>.json  # StackStates Description() = full
                                     #   stack state; bare <generation> when the
                                     #   state names no deployment
```

Notes:

- **Everything a project owns nests under `projects/<project-name>/`.** Both
  workspaces and stacks carry a `Project` relation (a workspace with no explicit
  project lands in the org's Default Project), so the tree groups by project
  instead of scattering `workspaces/` and `stacks/` as org-level siblings. Only
  objects that are genuinely org-scoped (teams, VCS clients, policy/variable
  sets, agent pools, registry, audit trails, org-wide config versions) stay at
  the top level.
- **Projects key on name for readability; the id lives in `project.json`.**
  Project names are unique within an org, so the directory is unambiguous, while
  RBAC joins (`TeamProjectAccess`, a workspace's `Project` ref) still resolve by
  the id recorded inside the file.
- **State versions order by `CreatedAt`, not by `serial`.** `Serial` is an
  int64, is not zero-padded in filenames (so a lexical sort orders `10-` before
  `2-`), and is not unique (rollback / re-upload can repeat a serial). The
  `-<id>` suffix keeps filenames unique; ordering and "latest" logic key on
  `CreatedAt`, and `id` is the identity. `serial` still lives in the `.meta.json`.
- Config version tarballs use globally-unique ids and are deduped org-wide
  (store once). A run references its cv by **id** plus a relative path. The
  hydrated ingress relation is split into `config-version-ingress.json` (commit
  sha/branch/PR), which survives even after the tarball is no longer
  downloadable, while `config-version.json` holds the record itself.
- Several relations are hydrated by a list/read `include` but would serialize as
  a bare `{type, id}` on their parent (the serializer renders relations as id
  refs). Each is instead archived as its own primary object so the sideloaded
  attributes are not discarded: HYOK OIDC configs and key versions, policy-set
  current/newest versions, OAuth tokens, run plan/apply summaries, native TF
  policy evaluations, project effective tag bindings, and the users a run,
  event, team, or membership references. Users are special: go-tfe has no user
  list (only `ReadCurrent`), so the hydrated sub-object is the only way to
  capture who ran or confirmed a run or who is on a team.
- Runs record only the cv **id** as the join key, never the expiring signed
  `upload-url` that a `RunConfigVer` include would otherwise nest in run.json (a
  `ConfigurationVersion` carries only `upload-url`; the `hosted-*-url` fields are
  a `StateVersion` concern).
- Any org-level object with a write-only secret stores metadata only
  (see Hard limits).
- **The archive is secret at rest.** Nothing is redacted locally: the
  `.tfstate.json` pulled from `hosted-state-download-url` embeds sensitive
  variable / output / resource values in cleartext, and serialized objects keep
  whatever the API returned. Every file is written owner-only (0600).
- **Notification configs are polymorphic** across workspace / project / team
  scope, so they are archived at all three levels: in each
  `projects/<name>/workspaces/<ws>/`, in `projects/<name>/`, and in `teams/<id>/`,
  not just workspaces.
- **The tree above is the logical namespace**: every object's stable
  archive-relative path. Physically that namespace is stored as per-workspace
  ledger shards, coalesced NDJSON roll-ups, and sealed `zip` bundles, all keyed
  under the same path (see Storage at scale).

## Behavior decisions

- **Best-effort, not fail-fast**: a 404/410 on one object (e.g. an expired
  config version or missing log) records the outcome in the ledger and
  continues. One bad object must not abort the whole archive.
- **Resumable via a real ledger, not file-existence** (required). A
  permanently-gone object (404/410) and a not-yet-fetched one both leave no
  file, so existence alone cannot drive resume. The ledger records
  per-object status (`done` / `absent` / `forbidden` / `skipped` /
  `errored` / `not-applicable`, with counts, timestamps, and the failing
  error), and resume consults it. `skipped` and `not-applicable` are settled
  like `done` (never re-requested), so a deferred or intentionally-skipped
  object is not mistaken for a gap. `forbidden` (a 403) is counted apart from
  `errored` but retried the same way, so a later run under a differently
  scoped token captures the union of what each token can see.
  Downloads write to a temp path and atomic-rename on success, so an
  interrupted run never leaves a truncated `.tfstate.json` / `.tar.gz` that
  looks complete. The manifest is flushed durably (append-only log lines, with
  atomic-renamed snapshots on compaction; see Sharded ledger) at a bounded
  cadence and on shutdown, so a kill -9 loses at most the last in-flight batch,
  never the ledger itself.
  - **Restart semantics**: a re-invocation against a non-empty output dir loads
    the existing manifest and, per object, _skips_ `done` and `absent`,
    _retries_ `errored`, `forbidden`, and anything absent from the ledger.
    `absent` is sticky (a 404 is not re-requested every run); a
    `--retry-absent` toggle forces re-probing when the operator suspects a
    spurious 404 or a since-restored object. The toggle also counts a
    recorded absence as unsettled for the append-mostly walks' early stop:
    most absences sit below a settled collection's newest-first boundary
    (expired plan logs of old runs), and a boundary gated on the normal
    settled predicate would halt before reaching any of them, leaving the
    flag inert for exactly the entries it exists for. A retry-absent run
    whose absences persist leaves those collections recorded unsettled, so
    the following normal run re-pages them once (immutables skip; near
    free) and settles them again. Resume and a clean first run are
    the same code path: a first run just starts from an empty ledger.
  - **Interrupted vs. failed** are the same to resume: both leave objects in
    `errored`/absent, and both are picked up on the next invocation. Transient
    errors (429, 5xx, timeouts, context cancellation) are recorded distinctly
    from terminal ones (404) so resume never mistakes a rate-limit blip for an
    absence.
  - **A 404 is confirmed before it settles**: the API can answer 404 out of
    eventual consistency for an object it listed moments earlier, and `absent`
    is sticky, so one response is never trusted alone. The archive layer
    re-probes once in-run after a short delay; only a repeated 404 records
    `absent`. A genuinely gone object costs one extra request, once. The
    confirmation lives entirely inside the run — see Cross-run state below for
    why it is not spread across two runs.
  - **Cross-run state is avoided**: an object's recorded outcome is a function
    of its most recent attempt alone, never of how the current run relates to a
    prior one. (An earlier design settled `absent` only when two _different_
    runs observed the 404, pairing a persisted observation timestamp with the
    current run's start time; the escalation depended on run boundaries, made
    one run's outcome unreproducible from its own inputs, and surfaced
    first-sight 404s as `errored` noise. The in-run confirm replaced it.)
    The durable state that legitimately spans runs is exactly the state resume
    exists to provide, and each piece is a plain last-known value, not a
    relation between runs:
    - the per-object entries themselves (status, content signature,
      `fetchedAt`) — resume _is_ reading these back; without them every run
      starts from zero;
    - the per-collection high-water marks (newest `CreatedAt` archived; the
      audit trail's `Since` cursor) — incremental re-run cannot know where the
      last walk ended without them;
    - the collection completed/settled flags that gate the seal phase — whether
      a collection's tail was ever fully walked is inherently a fact about a
      past run;
    - the run summary records (`lastRunAt`, `runCount`, per-status totals) —
      informational only; nothing consults them to decide a fetch.
      Anything else that needs more than one observation to decide (today, only
      the 404 confirm) must gather those observations within a single run.
- **Re-runnable to capture updates since the last run** (required). Re-invoking
  a completed archive must fetch only what changed, not re-download everything.
  Two object classes, two strategies:
  - **Append-mostly, ordered by `CreatedAt`** (state versions, runs, per-run
    children, config versions): the manifest stores a per-collection high-water
    mark (the newest `CreatedAt` archived) and a re-run lists newest-first,
    stopping the page walk once it reaches an already-archived object that has
    frozen into a terminal state. New objects append; existing immutable
    artifacts (logs, plan json, state blobs, tarballs) are never re-fetched. A
    known but still-non-terminal run is revisited so its mutable tail (status
    flips from `planning` to `applied`, late comments) is refreshed until it
    reaches a terminal state, then frozen. Audit-trail pages are the one variant:
    they carry no per-entry id to stop at, so their high-water mark is a `Since`
    time cursor and the re-run walks forward from it, not newest-first (see the
    Audit collector).
  - **Mutable, re-fetched and overwritten** (org/project/workspace settings,
    variables, team access, tag bindings, notification configs, registry
    metadata): these have no natural watermark, so a re-run re-reads them and
    atomic-overwrites the JSON when the payload differs, recording a new
    `fetchedAt` in the manifest. Cheap metadata reads are always refreshed;
    heavy blob downloads never are.
  - The manifest records a top-level `lastRunAt` / `runCount` and, per object,
    `fetchedAt` + a content signature (size or hash) so a re-run can tell
    "unchanged" from "updated" without diffing files. Not point-in-time
    consistent still holds: a re-run captures the delta as of when it walks each
    collection, not a single instant.
- **Progress reporting is required, not optional.** A full-org archive is a
  long, mostly-I/O job, so it must surface live progress rather than sitting
  silent until done. The archiver emits, to stderr:
  - on an interactive terminal, a Bubble Tea panel pinned to the bottom of the
    screen: a spinner and the current phase/target, colored per-status counts
    (done / errored / forbidden) with bytes, rate, and elapsed, and a progress
    bar while the phase is determinate. Log output routes through a slog sink
    into the panel so log lines scroll above it rather than corrupting it, and
    ctrl+c/q cancels the whole run under raw mode;
  - off a terminal (a pipe, a redirect, CI), `--progress=human` carries the
    same signal as a periodic plain line (phase, current target, counts,
    `completed=x/y` while determinate, bytes, elapsed, a rough rate); the
    default `auto` stays quiet off a TTY;
  - the same signal in a machine-readable form (`--progress=json`, one JSON
    object per line: phase, counts, current target, and `phaseTotal` /
    `phaseCompleted` while determinate) for wrapping in CI or a watcher,
    defaulting to the panel on a TTY and quiet/log-only when not on one.

  The bar tracks a per-phase weighted unit count the archiver drives, distinct
  from the object tally: the true object count is discovered only by walking, so
  it cannot seed a bar. During the workspaces phase each workspace is weighted by
  one plus its probed run and state-version totals (one `PageSize`-1 list probe
  of each collection before the workers start, since the advertised `RunsCount`
  omits speculative runs and can go stale), a weight that tracks real work far
  better than a flat workspaces-done bar and, as numerator and denominator share
  the weight, reaches exactly 100% when the last workspace finishes; phases
  with no cheap pre-count (org-scope, registry, stacks, audit) show a spinner.
  The per-status counts are derived from the same in-memory counters that back
  the manifest (a single mutex-guarded tally), so they and the ledger's own
  tally never disagree (the on-disk copy trails only by the last unflushed
  batch). Those counts are cumulative across runs (every entry
  counted by its current status), so a resumed run opens with the objects a prior
  run already settled rather than climbing from zero, and is tagged `resumed`; the
  bytes and rate stay per-run. A final summary (totals per status class, wall
  time, any orgs/workspaces that errored) prints on completion and is also written
  to the manifest as the run record, which stays per-run.

- **Concurrency and rate**: a fixed gate of 16 in-flight API requests, shared
  by the whole run, bounds "how many at once"; "how fast" belongs to
  per-bucket adaptive rate governors at the HTTP transport, so the server's
  feedback moves the rate, never the concurrency. The server meters most
  endpoints from one general bucket (30 requests per second), but the two runs
  list endpoints (`/workspaces/:id/runs`, `/organizations/:name/runs`) from
  their own bucket of 30 requests per _minute_, documented only on the runs API
  page, so each bucket gets its own governor and a 429 in one never pauses or
  halves the other. The runs governor paces just under the documented budget
  (29/min) and the run walk spends it carefully: pages are fetched at the
  maximum size (100) and the per-workspace count probe reads the workspace's
  advertised `RunsCount` instead of the listing. Every physical attempt pays a
  token from its endpoint's governor before launching. A 429 halves that
  bucket's rate (once per cooldown window, floored), drains the bucket, and
  pauses its launches until the server's advertised `X-RateLimit-Reset`. The
  pause spans the bucket because the server counts rejected requests against
  its window too; pacing into a blown window just buys more 429s. After the
  reset, clean responses creep the rate back up one rps per two-second stretch
  toward the ceiling. A 429's `X-RateLimit-Limit` value is debug-logged, so an
  endpoint metered outside the known buckets identifies itself in the trace.
  Workspace walks fan out on coordinator goroutines (capped at the gate's size)
  that hold no slot themselves, so parallelism follows the requests rather than
  a fixed per-workspace assignment. `go-tfe`'s retry and rate machinery stays
  dormant: its unbounded 5xx retry is replaced by a bounded doubling backoff at
  the transport, 429s are retried there too (with no local backoff, since
  re-entry waits out the cooldown in the governor) and convert to a transient
  error once their budget is spent, and the `X-RateLimit-Limit` header is
  stripped from every response so go-tfe's internal limiter never engages.
  Guard shared manifest/counter writes with a mutex. (Config-version ids are
  globally unique, so the shared `config-versions/` dir is race-free.)
- **Not point-in-time consistent**: a long archive of a live org sees new runs
  and state versions appear mid-run. Acceptable for a best-effort snapshot,
  stated so readers know the archive is not a single instant.
- **Multi-org**: if no org is specified, enumerate all organizations the token
  can see and archive each.
- **Serialization**: marshal the `go-tfe` structs through their vendored
  `hashicorp/jsonapi` tags (kebab-case), falling back to `encoding/json` only for
  the plain-`json:` audit-trail and pagination types. Most `go-tfe` types are
  jsonapi _response_ structs tagged `jsonapi` (not `json`); kebab output matches
  the public API docs and survives a `go-tfe` field rename better than
  `encoding/json`'s Go-field-name fallback. Caveats the code must handle:
  - **Ephemeral signed URLs are stored as returned.** The `*DownloadURL`,
    `*UploadURL`, and `LogReadURL` fields expire within minutes and merely
    duplicate blobs the archive captures directly, but they are part of the
    payload and full fidelity keeps them; they are dead links by the time
    anyone reads the archive, not live credentials.
  - **Hydrated relations are inconsistent**: an included relation pointer
    marshals as the full nested object, and as `null` when not included, so the
    same field varies shape and is hugely redundant. Serialize relations as ids
    where practical.
  - **Kebab, not Go field names**: a schema keyed on Go field names would drift
    silently if a `go-tfe` upgrade renamed a field, so the kebab tag names, which
    match the public API docs, are the stable choice for a reference archive.
    This is a marshaler choice, not an `encoding/json` tweak: `encoding/json`
    ignores the `jsonapi` tags entirely (hence the Go-field-name fallback), so
    kebab output means marshaling through go-tfe's vendored `hashicorp/jsonapi`
    (already a dependency), which also handles relations-as-ids and omitempty.
    It applies only to jsonapi-tagged objects: `jsonapi.MarshalPayload` requires a
    `jsonapi:"primary"` field, which the plain-`json:` audit-trail and pagination
    types lack, so those stay on the `encoding/json` (snake-case) path.

## Storage at scale

An org with thousands of workspaces, each with hundreds of runs and dozens of
state versions, produces millions of leaf objects. The tree in Output layout is
the logical namespace — every object's stable archive-relative path — and that
namespace is stored physically in two forms that keep it tractable: the ledger is
partitioned into **per-workspace shards** rather than one document, and frozen
history is **sealed into compressed, indexed bundles** rather than one file per
object. Both key on the unchanged archive-relative path, so resume, the
newest-first early-stop, and the per-collection watermarks (Behavior decisions)
behave exactly as they do for a single-file, one-file-per-object form.

Two independent pressures shape the two forms, and neither answers the other. A
single in-RAM `map[relpath]*Entry`, marshaled in full and atomic-rewritten on
every flush (a 10s cadence) and on shutdown, is a multi-GB document
re-serialized every ten seconds over a multi-GB resident map at millions of
entries — a per-tick, per-run cost independent of on-disk size, which sharding
removes and bundling does not (a bundled run still carries its ~12 ledger
entries). One file per object is millions of tiny leaves — inode pressure and
O(files) traversal locally, and on a remote object store a per-object overhead
(~8KB of name metadata, plus ~32KB of index on Glacier Deep Archive) that
dominates the bill: at ~2.7M objects the per-object tax is ~37x the ~15GB
compressed payload, so object count, not bytes, is the cost — which bundling
removes and sharding does not.

### Sharded ledger

The ledger lives as per-workspace and per-stack shards co-located with the
subtree they index (`.../workspaces/<ws>/.ledger/`, `.../stacks/<s>/.ledger/`),
a small org-root shard for org-scoped objects, and one shared
`config-versions/.ledger/` shard for the org-wide config-version entries. Each
entry belongs to the shard named by its relpath prefix, so its key is
byte-identical to the single-file form and every ledger operation —
`ShouldFetch`, `Entry`, the frozen early-stop, the watermarks and completion
flags — is unchanged; only the entry's physical location differs. Each shard
carries the marks and flags whose keys share its prefix. Every shard under the
org root loads when the run opens and stays resident for the run; what sharding
buys is not residency but flush cost — an append-only write path in place of
re-serializing one monolithic document on every flush tick.

A shard is a compacted `snapshot.json` plus an append-only `log.ndjson`.
Recording an entry appends one newline-terminated line, so no flush re-serializes
the whole ledger; the terminating newline is the commit marker and a torn
trailing line is dropped on read. Compaction folds a shard's log into its
snapshot once the log passes a size floor (64MiB) and outgrows the snapshot,
and unconditionally when the run finishes, writing the merged snapshot before
truncating the log; an unchanged record appends no line, so a re-run's
archive-then-stop boundary adds nothing. Each shard commits through the same
temp-write-and-atomic-rename as every other file, so a shard that exists is
whole. A shard with no file starts empty — the ledger, not file existence, is
the record, so deleting a `.ledger/` directory forgets that subtree's history
and the next run re-fetches it.

### Sealed cold storage

A **frozen** object is sealed into a local bundle. Frozen is the terminal
predicate the append-mostly early-stop already uses: a run whose status is
terminal with a `done` entry, or a state version, which is immutable once written.
Sealing is confined to frozen objects, so a mutable or in-flight object is never
bundled. The bundles are **write-once, generationally numbered**, and never
rewritten; new cold history is a new generation, so a re-run's sealing cost is
proportional to newly-frozen objects rather than to the archive. Write-once also means an operator
who later backs the archive up to an archival storage class never rewrites a bundle
and so never trips a minimum-storage-duration charge (Glacier Deep Archive's
180-day floor, say). Both cold transforms key on the archive-relative path and are
invisible to the collector.

- **Heavy audit-only artifacts pack into bundles.** The per-run logs (`plan.log`,
  `apply.log`, `cost-estimate.log`, `policy-check-*.log`), `plan.json`, and the
  raw and json state blobs are byte-heavy and read only during a narrow audit;
  they live in per-workspace-generation `zip` bundles.
- **Immutable small metadata coalesces into per-workspace NDJSON roll-ups.** Coalescing,
  not bundling the blobs, holds object count down: most _files_ per run
  are small JSON, so bundling the blobs alone leaves the ~1.6M small JSONs as the
  floor, while coalescing collapses object count far below it. The seven
  immutable run children and the state-version `meta.json` are
  `ShouldFetch`-gated and coalesce by filename alone. `run.json` is the one
  **mutable** small JSON, dedup-checked and refreshed at the walk boundary while
  a run is in-flight, so it coalesces through its own gate: at the seal
  boundary, a `run.json` that is settled **and** whose recorded status is
  terminal (read from the document itself; the ledger entry carries no run
  status) folds into `rollups/runs.ndjson`, its emptied run directory is
  pruned, and an in-flight run's summary stays loose and keeps refreshing.
  Coalescing stays confined to the seal boundary, where the object is frozen,
  single-threaded, and settled, so the append is pure and the relpath stays the
  ledger key; coalescing `run.json` at collect time (born as a line) would
  instead invert the relpath-is-the-key coupling, the newest-first early-stop,
  `WriteJSON`'s parse-free whole-file dedup, and per-object atomicity. What
  keeps the roll-up append-once is the collect side's sealed-elsewhere gate: a
  re-walk that re-reads a coalesced run and finds the payload byte-identical to
  the ledger's recorded done signature, with the loose file absent, skips the
  write instead of resurrecting the loose copy (which the next seal would fold
  again as a duplicate line). A run that legitimately changed after its seal (a
  canceled run force-canceled, say) writes loose again and the next seal
  appends a newer line; readers keep the newest line per path. Each coalesced
  line is a JSON object:
  `{"path": <relpath>, "sha256": <hex>, "content": <file bytes as an escaped JSON string>}`.
  The line is not byte-equal to the original file, but the `content` field carries the
  exact bytes verbatim (reproducing the file on extraction and staying greppable
  for field tokens) and the `sha256` is the content signature already in the
  ledger.

  The sealed-elsewhere gate is a deliberate, archive-wide shift in `Mutable`'s
  semantics: a hand-deleted mutable file whose fresh payload is unchanged is no
  longer re-materialized. That is consistent with `Object` — the ledger, not
  file existence, is the record — and with the ledger's own "deleting
  `.ledger/` forgets" stance (deleting the shard re-materializes the file once,
  and the next seal appends a duplicate-content line readers dedupe).

A sidecar index (`<bundle>.sidecar.ndjson`) sits beside each bundle, outside it:
one NDJSON line per member, with exactly the fields `name`, `bundle`, `method`,
`size`, `crc32`, `sha256`. `name` is the member's archive-relative path (also its
path inside the zip), `bundle` the bundle filename, and `method` `store` or
`deflate`. The index ties a metadata grep hit to a named member of a named bundle
it can extract whole, without opening or restoring the bundle. The generation lives only in the
bundle filename, not in any sidecar field; there is no offset, no generation
field, and no run or state-version id. Loose originals are unlinked only once
every member reads back out of the written bundle at its recorded SHA-256 (which
matches the ledger signature) and the sidecar is written, so the loose copy stays
canonical until the bundle is durable and verified; an interrupted seal leaves the
loose files canonical and simply re-runs.

### Remote offload and full-archive sync (optional)

With a `remote:` block configured, the object store holds a **complete copy**
of the archive, in two modes with different semantics:

- **Eviction** (upload → verify → delete local) moves the cold surfaces off
  disk: sealed bundles as each workspace seals, and org-wide
  `config-versions/<id>.tar.gz` tarballs at the close sweep, once the ledger
  proves them. Peak local disk is then bounded to the search layer plus
  in-flight unsealed work plus the run's still-unevicted tarballs (tarballs
  have no mid-run eviction driver, so they accumulate until the sweep), which
  is what lets a 5k+ workspace org's archive run on a machine that could
  never hold the whole thing at once.
- **Sync** (incremental upload, local kept) mirrors everything else — loose
  files, roll-ups, sidecar indexes, ledger snapshots. Local disk stays the
  canonical search layer, so browsing and grep remain offline operations;
  the bucket is the disaster-recovery copy (re-download the prefix to
  restore).

Sync converges during the run rather than in one closing sweep, in three
motions:

- **As written**: an org-scope file — anything outside a workspace subtree
  that is not an eviction surface: `org.json`, users, teams, the registry,
  project files, stacks — uploads the moment a write actually changes its
  on-disk content (the store's atomic commit reports whether the bytes
  changed, so an unchanged re-read costs no upload). The upload skips any
  probe: the bytes just changed, so the remote copy is stale by definition.
  Workspace-subtree files are deliberately not synced as written — sealing
  re-shapes them minutes later (children into roll-ups, blobs and logs into
  bundles), so eager copies would be pure churn the prune would delete. A
  semaphore at the shared concurrency ceiling bounds the burst, since the
  per-page archive fan-out is otherwise unbounded. The `.remote.json`
  marker also mirrors eagerly through the same motion (so its failure
  counts into `eager_failed` like any other): it is what an interrupted
  run's mirror needs to locate its evicted bundles. The marker is written
  before any collector runs but after the ledger's cross-process flock is
  held — it mutates the org root like any other write, and writing it
  outside the single-writer exclusion would let a losing concurrent process
  re-point a marker the surviving run then faithfully mirrors. A configured
  remote that differs from the marker's recorded URL/prefix refuses the run
  outright (see Migration below).
- **At each workspace's seal boundary**: right after a workspace seals
  (bundles evicted, roll-ups coalesced), its subtree holds only its final
  search layer, and that subtree syncs — one scoped inventory listing, the
  same incremental gate as the close sweep, files settled sequentially (the
  concurrently-sealing workspaces are the parallelism), and no prune. An
  interrupted run then loses at most the workspaces still mid-collection.
  Orphan zips are skipped, as in the close sweep's classification, and the
  workspace shard's `.ledger/` files are skipped whole: a mid-run snapshot
  is stale by construction.
- **The close sweep**, the backstop: a full-inventory incremental pass over
  the whole tree at each org run's close, after the final ledger flush and
  while the cross-process flock is still held. It retries eager failures,
  migrates pre-existing archives, evicts settled tarballs and any bundle
  the seal-time eviction missed, mirrors the post-compaction ledger
  snapshots (shards mirror only here), **verifies every already-evicted
  surface still answers at the store** (see below), and prunes stale remote
  keys — deletion reconciliation lives only here. A failed final flush with
  a remote configured also marks the run incomplete: the sweep deliberately
  mirrors only the snapshots (never the replay logs) on the guarantee the
  flush made them current, so a mirror of knowingly-stale snapshots must
  not hide behind a clean exit.

Eager failures (the as-written and seal-boundary motions) warn, count into
the close summary's `eager_failed`, and defer to the sweep; only the close
sweep's own failures mark the run incomplete.

A run with `remote:` configured proves the store manageable at startup: a
small probe object is written under the prefix through the eviction path
(an upload carrying recorded digests), headed, listed, read back over a
ranged request from a non-zero offset, and deleted before any collection
work begins — the same motions the mirror and a later `view` of an evicted
bundle perform — so a wrong bucket URL, credential set, or a store that
rejects or mangles range requests fails the run immediately instead of
surfacing as per-object failures hours into an archive (or at the first
audit years later). The probe's attributes read doubles as the digest
check, in two parts. The recorded metadata digests must read back exactly:
they are the currency of the eviction confirm and the sync gate, so a
store that drops or mangles object metadata — which would silently reduce
the custody transfer of the archive's only copies to a size comparison —
fails preflight outright. The backend's own digest attribute is merely
scored: one that mismatches the written bytes (an SSE-KMS-encrypted S3
bucket's ETag is hex but is not a content MD5) marks the attribute
untrusted for the rest of the run — the client then serves only the
metadata digests its own writes record, so digest comparisons stay
content-aware and size-matched files just pay one Head where a listing's
attribute would have served — while a store that records no attribute at
all passes unremarked, the metadata carrying the comparisons either way.

Sync is always-on when `remote:` is configured; there is no separate knob
or per-motion toggle: remote configured means the bucket converges on a
complete archive. Object keys mirror the local tree:
`<prefix>/<org>/<archive-relative path>`, so a bucket listing reads like
the archive and the sidecar's `bundle` field still names the object. The
backend is resolved from one gocloud.dev bucket URL (`s3://`, `azblob://`,
`file://`), and credentials come only from that provider's default chain;
the YAML carries the URL and transfer tuning, never a secret. Evicted
uploads stream through the backend's parted-upload path (concurrent parts
for large state bundles), and a write that dies midway aborts rather than
committing a truncated object. Every write — synced files and evictions
alike — records the body's full-object MD5 and SHA-256 as object metadata,
the MD5 also checked against the streamed bytes at commit, so an
egress-free content comparison exists even where the backend records no
digest of its own (a parted upload, or any body past S3's 16 MiB multipart
threshold, leaves no backend attribute). The digest ladder prefers that
recorded metadata over the backend's own attribute, which can be an ETag
that is no content MD5 at all (SSE-KMS); the attribute serves only as the
fallback for objects written without metadata (an older build's, a foreign
write), and not even that once preflight has scored it untrustworthy.
Every store operation retries a transient failure under a bounded doubling
backoff — the same in-run persistence the API transport gives fetches,
above whatever the provider SDK retries itself — while errors the store
pins on the request (absent key, permission denial, failed precondition)
surface immediately. Two guards keep that classification honest, mirroring
the API transport's own: a transport-level failure (a DNS blip, a dial
fault) is never trusted as a request fault whatever code the driver
stamped on it (azblob maps "no such host" to NotFound), so a resolver flap
can neither settle a prune's delete as already-removed nor answer an
eviction probe with a permanent absence; and every attempt runs under a
stall watchdog, so one wedged connection costs a bounded window instead of
hanging a worker, a seal, or the whole close sweep. Reads and listings,
whose progress is observable per delivered chunk or object, get a tight
idle window; writes get that window widened by the body's size at a
conservative floor rate (32 KiB/s), because their wire progress is
invisible from this side of the provider SDK — a sub-threshold body is
buffered at memory speed and transfers entirely inside the writer's
commit — and a tight window there would cut healthy slow-link uploads
that are moving bytes the whole time, a false failure that would repeat
every retry and every run and permanently block the mirror's convergence.
A cut attempt classifies transient and retries. Every write lands in the
store's default storage class.

The close sweep walks every regular file under the org root and classifies it
top-down (the eager motions honor the same classification — they change when
a file first settles, never what happens to it):

1. an atomicfile staging temp: skip (a crash's partial write);
2. `.ledger/lock`: skip (meaningful only as a kernel flock target);
3. `.ledger/log.ndjson`: skip — a stale remote log replayed onto a restored
   tree could resurrect old ledger state; the post-compaction `snapshot.json`
   (which the final flush guarantees is current) is the durable record;
4. `bundles/*.zip` with its sidecar beside it: evict, the sweep-side backstop
   for seal-time eviction (a workspace filtered out of later runs, a prior
   upload failure, a remote newly pointed at an old archive);
5. `bundles/*.zip` without a sidecar: skip (unverified orphan; never upload);
6. `config-versions/*.tar.gz` recorded done with the ledger signature's size
   matching the local bytes: evict; otherwise skip with a warning (an
   unproven tarball is never synced either, because a synced copy at the
   eviction key could later pass a proper eviction's size gate and let it
   delete the only proven local bytes);
7. everything else: sync incrementally.

The incremental gate is driven by one upfront listing inventory of the org
prefix (~1 request per 1000 keys, instead of a metadata probe per file) and
degrades in order: key absent → upload; size differs → upload; size equal →
compare the digest the inventory records against the local MD5; listing
carries no digest → one Head for the object's recorded metadata digest
(listings never carry metadata, and some backends' listings omit even the
attribute — fileblob — or the attribute is distrusted — SSE-KMS); no digest
recorded at all → trust the size match (a same-length content change to a
foreign or older-build object is skipped until its size changes; this
tool's own writes always record a metadata digest, so they never bottom
out here). Synced files fan out concurrently; evictions run sequentially.
A synced file at or past a streaming threshold (32MiB) streams from disk
instead of riding whole in memory — roll-ups grow with run history, and
the sweep's memory must not scale with the archive's largest file — with
its digests computed in a first pass and recorded as object metadata, the
same recording every smaller write gets. Per-file
failures warn and count, never abort: local stays canonical and the next run
re-sweeps. But because the mirror is the archive's long-term record, a close
sweep that failed files marks the whole run incomplete (non-zero exit, like
a dropped surface), so a scheduled run surfaces a mirror that is knowingly
behind instead of reporting success over it; no cross-run state is persisted
for this — the next run's sweep re-derives the gap from the store itself and
self-heals.

Before the prune, the sweep **verifies every already-evicted surface still
answers at the store** — the one class of gap a local walk cannot see and
no later run can repair. A generation sidecar whose zip is no longer
beside it, and a ledger entry recording a configuration-version tarball
done with no local file, are the local records that the store holds those
surfaces' only copies; the sweep checks each composed key against the
inventory already in hand (a map lookup, no extra requests), comparing
sizes where the ledger recorded one. A missing or diverged object — a
re-pointed bucket, a mis-scoped lifecycle rule, a manual delete — logs an
error naming the key and counts into the sweep's failures, so the run
exits incomplete instead of reporting a complete mirror over a hole in the
long-term record. Obligations are derived before the sweep's own evictions
run, since the pre-eviction inventory cannot yet hold what this sweep is
about to move.

After the uploads, a **prune** step makes the mirror true: every inventory key
the walk saw no local file for is deleted in a bounded fan-out, except the
evicted surfaces
(bundle zips, config-version tarballs) and the bundle sidecars beside them,
which are exempt **by key shape alone** — not by checking the local sidecar
or ledger entry that proved the eviction. After eviction the remote copy is
the only copy, so its survival must never hinge on local state: a wiped
`.ledger` or a subtree deletion that takes sidecars with it must not
cascade into deleting the archive's only bytes — nor the mirrored sidecar,
which after such a loss is the only index proving a remote-only zip's
members. The cost is that a deliberately deleted workspace leaves its
bundles and their sidecars in the bucket, to be cleaned up by hand
together. Without the prune the mirror would accumulate stale loose copies
of files later sealed into other forms: a restored stale
`runs/<id>/run.json` would shadow its newer roll-up line, since reads
prefer loose. For the search layer the consequence is deliberate and
mirrors the ledger's stance: deleting a subtree locally forgets it remotely
on the next run — where "deleting" means a deliberate act on a live
archive, which two guards distinguish from loss, and provenance
distinguishes both from the tool's own re-shapes. A stale key whose
relpath the ledger still holds an entry for is a re-shape artifact — the
archive owns the object; its loose remote copy went stale because a seal
coalesced or bundled it, and the self-heal after an interrupted run
legitimately produces thousands at once — so ledger-known keys prune
freely at any scale. The guards then cover what the ledger has never
heard of: a run that opened an empty ledger against a non-empty mirror is
a fresh or wrong `--output` pointed at an existing archive and refuses
the whole prune (restore the prefix — ledger included — before re-rooting
an archive); and a ledger-unknown delete set past a floor (100 keys) that
outnumbers the walk-matched keys means most of the mirror has no local
trace at all — loss, or a mass deletion that must be an explicit act —
and refuses just those keys, naming both ways out (restore the prefix, or
remove the keys from the bucket by hand). Either refusal counts a sweep
failure so the run exits incomplete rather than quietly diverging the
mirror. Every key a proceeding prune deletes is logged, so a deletion is
auditable rather than visible only as a count.
Bucket versioning (or Object Lock) is the recommended backstop either way:
it turns any surprising prune or overwrite into a recoverable event.

Bundle eviction extends verify-before-delete one hop without any cross-run
state: every step is derived from observable facts — is the local zip
present, is its sidecar present, do the local bytes still hash to their
recorded proof, does `HeadObject` answer and with what digests — so each
crash point self-heals on the next run:

- **zip, no sidecar** (died mid-`seal.Seal`): the zip is unverified and the
  loose files are still canonical. The sweep never uploads it; the next seal
  writes a fresh generation and the orphan leaks one number, as today.
- **zip + sidecar, no remote object** (sealed, upload never ran or died
  midway; an aborted parted upload is not an object): re-prove the local
  bytes, upload with the digests recorded as metadata, confirm with a Head
  (size plus every digest both sides carry), then delete the local zip. On
  S3, a bucket lifecycle rule aborting incomplete multipart uploads mops up
  parts a crash strands.
- **zip + sidecar + remote object** (died after upload, before the local
  delete): the Head finds the copy, size and recorded digests match the
  local bytes, and the zip is deleted without re-uploading.
- **sidecar only** (eviction finished): nothing local to sweep, but not
  nothing to prove — the sweep's evicted-surface verification confirms the
  store still answers for the zip at its key every run, so a bucket-side
  loss or a re-pointed remote surfaces at the next close instead of at the
  first audit years later.

The custody transfer is guarded at both ends, because after the local delete
the remote copy is the archive's only copy. Before any remote traffic the
local bytes are re-proven against the record that settled them — a bundle
zip member by member against its sidecar digests (the seal's own read-back
check, re-run at the moment it matters most), a configuration-version
tarball against its ledger signature's SHA-256 — so rot that crept in after
sealing is refused, kept local for inspection, and never becomes the
long-term record. On the remote side, an upload records the file's MD5 and
SHA-256 as object metadata, and the confirm compares size plus every digest
both sides carry; an object recorded by an older build carries no metadata
and still gates on size, the strongest egress-free comparison available for
it — but on the fresh-upload path, where the upload recorded both digests
one call earlier, the confirm demands at least one back, so a store that
silently drops object metadata (preflight's backstop) cannot reduce the
custody transfer to a size comparison. A mismatch on either side warns,
keeps the local file canonical, and
never overwrites remote history at the key. Failure to evict one bundle is
a warning, never an abort: the local zip simply stays canonical, exactly as
if no remote were configured. Because eviction removes zips but never sidecars, generation
numbering takes its maximum over both the `*.zip` and `*.zip.sidecar.ndjson`
names — a zip-only scan would restart at gen0001 once bundles left disk and
overwrite remote history at the same key. Tarball eviction recovers from the
same crash points the same way; its local proof is the ledger's done entry
and recorded signature (size and SHA-256) rather than a sidecar.

Migration needs no special mode in one direction only: pointing a `remote:`
block at an existing **all-local** archive makes the next run's sweep
upload the entire search layer (a one-time pass whose `sync_progress` log
lines track it) and evict every pre-existing bundle that has a sidecar and
every ledger-proven tarball. **Re-pointing** an already-mirrored archive at
a different bucket or prefix is the opposite of no-special-mode: once
surfaces have evicted, the recorded location holds their only copies, so a
config that differs from the marker's recorded URL/prefix refuses the run
outright, naming both locations and the way forward — copy the old prefix
to the new location, then update or delete `.remote.json` as consent. The
consent is verified, not trusted: the next close sweep's evicted-surface
check proves the new location actually answers for every only-copy before
the run can exit clean.

Each org's root gains a small `.remote.json` marker recording a schema
version and the read-relevant backend settings (the bucket URL and prefix);
the version lets a future build change the marker's shape while old markers
keep reading, and a reader refuses a marker newer than it understands, as
well as one that parses but records no bucket URL (naming the file, so a
damaged restore surfaces as "fix .remote.json" rather than a bare client
error at the first evicted read years later).
`view` reads it to serve a sealed member whose zip is no longer on disk: it
Heads the object, parses the zip central directory over a handful of ranged reads
(cached per session), then fetches the member's compressed span in **one**
ranged read and decompresses locally — never the whole bundle. There is no
restore workflow: an object an operator has tiered into a non-readable
archival class (S3 Glacier, Azure Archive) fails its reads with the
backend's error until restored by hand, which is why the mirror should not
be lifecycled into such tiers. A local-only archive has no marker and never
constructs a client.

### Container format and compression

Bundles are `zip` (Zip64), not `.tar.gz`. A zip's central directory is a member
index for random access after a restore, per-member framing isolates corruption
to a single member, and mixed per-member methods let one container **STORE** raw
state while **DEFLATE**-ing logs — with `grep -a` reading the stored members
directly and `unzip` readable decades out with no dependency on this tool. Gzip
is a single non-seekable, non-appendable stream — one member needs the whole
stream inflated from the front and one bad byte poisons the rest — so it is the
wrong container here. (Config-version tarballs stay `.tar.gz`; they arrive that
way from the API and are stored opaque.)

Logs compress (per-member DEFLATE); raw state is stored uncompressed. Per-member
framing, not compression, bounds bit-rot blast radius to a single member, so with
the durable copy on a remote object store (11-nines, cross-AZ erasure coding,
background scrub-and-heal, checksums on write) logs compress freely. Raw state
stays uncompressed for longevity rather than durability: an uncompressed
`tfstate.json` is grep-able and tool-independent decades out, and storing the
irreplaceable state raw costs pennies a month.

### Mapping onto object-store storage tiers

The tool writes every mirrored object in the store's default class and has
no restore workflow, so the mirror itself belongs in tiers that serve reads
directly. The layout is still deliberately split so that one tier covers
each kind of file — for an operator's own lifecycle rules on the mirror or a
separately managed backup — and so that an audit proceeds the same way on
disk: narrow first, then read one thing. The **search layer** (the loose
mutable workspace files, in-flight `run.json`, the NDJSON roll-ups, every
sidecar index, and the ledger shards) is small, listable, zero-latency to
grep, and churns as the archive evolves (re-compared, re-uploaded, pruned),
so it belongs in a hot, always-readable tier (S3 Standard, say). The **cold
bundles** are byte-heavy, write-once, and rarely read, so a directly-readable
infrequent-access tier (S3 Standard-IA, Azure Cool) is the safe cost lever;
tiering them into a non-readable archival class (Glacier Deep Archive, Azure
Archive) trades away `view`'s remote read path for the deepest discount and
means a by-hand restore before any read. No bundle is a sub-128KB object
(which an archival class bills as ~168KB, the reason the small metadata is
not itself bundled), and `logs.gen*.zip` and `state.gen*.zip` are separate
per generation, so one retrieval answers one audit question and a plan-log
fetch never drags the state along. An audit greps the search layer on disk
(free, and tighter than a flat tree, since the roll-ups collapse hundreds of
run directories), reads the bundle name and member from the sidecar, and
`unzip`s that one member. The greppable surface stays live and gets tighter.
A cold heavy artifact is no longer a directly-`cat`-able file, but that cost
falls on audit-only artifacts that are rarely read.

### What stays loose vs. what seals

The tool writes every row below as local files; the "Backup tier" column is
the object-store tier each kind maps onto, as set by an operator's own
lifecycle rules or backup.

| Tier                      | Objects                                                                                                                                                                     | Backup tier  | Form                                                              |
| ------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------ | ----------------------------------------------------------------- |
| Loose, mutable, greppable | the 9 per-ws settings files (`workspace.json`, `variables.json`, `tags.json`, team access, notification configs, ...), in-flight `run.json`, ledger shards, sidecar indexes | Standard     | one file per relpath                                              |
| Coalesced roll-ups        | the 7 immutable run children + state-version `meta.json` + the `run.json` of settled terminal runs (an in-flight run's stays loose until it freezes)                        | Standard     | per-ws `*.ndjson`, keyed by relpath                               |
| Cold bundles              | `plan.log` / `plan.json` / `apply.log` / `cost-estimate.log` / `policy-check-*.log`; raw + json state                                                                       | Deep Archive | write-once generational `logs.zip` + `state.zip`, sidecar-indexed |

At 1000 workspaces x 200 frozen runs x 30 state versions (config-version tarballs
excluded, equal either way), coalescing without the runs roll-up already holds
object count near ~200k where one file per object gives ~2.7M, about ~13x fewer;
folding terminal `run.json` — the archive's largest loose-file population, ~1
per run — collapses those ~200k loose summaries into one `runs.ndjson` line
each, leaving loose count proportional to _in-flight_ runs rather than run
history. The compressed backup sits near ~29GB where the raw tree is
~137GB, and the per-object tax falls from ~37x the payload to ~3x. An operator who
backs the result up to an object store sees a first-year bill near ~$12 against
~$142 for the naive layout. The dollars are small; the structural win (request
quotas, inventory and listing time, restore fees, and managing ~200k rather than
~2.7M objects) is the point, and it holds under wide log/state size variance,
which moves only payload GB, the part an archive tier already prices near zero.

### Physical layout (one workspace)

```
projects/<project>/workspaces/<ws>/
  workspace.json  variables.json  readme.md  tags.json ...   # loose, mutable
  runs/<run-id>/run.json                                     # loose while in flight; terminal runs coalesce
                                                             #   into rollups/runs.ndjson, emptied dirs pruned
  .ledger/
    snapshot.json                                            # ledger shard, keys = relpaths
    log.ndjson                                               #   append-only; compacts when log > snapshot
  rollups/
    runs.ndjson                                              # run.json of settled terminal runs
    config-versions.ndjson                                   # the immutable run children, one JSON line each
    plan-summaries.ndjson  apply-summaries.ndjson
    cost-estimates.ndjson  comments.ndjson  run-events.ndjson
    policy-checks.ndjson  task-stages.ndjson
    tf-policy-evaluations.ndjson  tf-policy-outcomes.ndjson
    state-versions.ndjson                                    #   state-version .meta.json sidecars
  bundles/
    logs.gen0001.zip.sidecar.ndjson                          # index, outside the bundle
    state.gen0001.zip.sidecar.ndjson
    logs.gen0001.zip                                         # cold, DEFLATE members
    state.gen0001.zip                                        #   STORED (uncompressed) members
```

## Config surface

Settings split by how much they vary. The token is a secret (environment only),
the output directory and per-run knobs are flags, and everything that describes
what and how to archive is a YAML file, validated against a JSON schema
generated from the Go type and embedded in the binary.

- Environment: `HCP_TOKEN`, then `TFC_TOKEN`, then `TFE_TOKEN` (required, first
  non-empty wins); `HCP_ARCHIVER_CONFIG` for the config file path.
- Flags: `--config` / `-c` (config path), `--output` / `-o` (archive root;
  resume/re-run is implied when it already holds an archive),
  `--progress=auto|human|json|quiet` (default `auto`: human on a TTY, quiet off
  one) with a progress-interval knob, `--retry-absent` to re-probe
  `absent` objects on a re-run, and the `--log-*` knobs.
- Config file (all keys optional, defaults applied per field): `address`
  (default `https://app.terraform.io`), `organizations` (all visible orgs if
  empty), `projects` and `workspaces` (filters within each org; everything if
  empty, and with both set a workspace must satisfy both), a `runHistory`
  block bounding each workspace's archived run history
  (`count` / `age`; whichever admits more history wins; unlimited by
  default), a `scope`
  block of toggles for the heavy or optional surfaces (`stacks`, `hyok`,
  `registryDetail`, `auditTrail`), each off by default, and a `remote`
  block enabling offload of sealed cold bundles to a remote object store
  (`url` required, its scheme selecting the backend — `s3://`, `azblob://`,
  `file://`; optional `prefix`, `partSize`, `concurrency`).
  Credentials are never in the file; each backend's provider default chain
  supplies them.

## Packaging

Self-contained Go module (own `go.mod`) so `go-tfe` deps stay isolated from any
host repo. Single `main.go` is fine to start.

## `go-tfe` API surface (verified against `main` @ `2b78c64`)

Client: `tfe.NewClient(&tfe.Config{Token, Address})`

Organizations:

- `Organizations.List(ctx, *OrganizationListOptions) (*OrganizationList, error)`
- `Organizations.Read(ctx, org string) (*Organization, error)`

Workspaces:

- `Workspaces.List(ctx, org string, *WorkspaceListOptions) (*WorkspaceList, error)`
  - `WorkspaceListOptions` embeds `ListOptions{PageNumber, PageSize}`
- `Workspaces.ReadByIDWithOptions(ctx, id, *WorkspaceReadOptions)`

Variables:

- `Variables.ListAll(ctx, workspaceID string, *VariableListOptions) (*VariableList, error)`
  (hits `.../all-vars`, so it includes variable-set-inherited variables)
- `Variable` fields: `Key, Value, Description, Category, HCL, Sensitive, VersionID`
  (a sensitive `Value` reads back empty from the API and is stored as returned)

State versions:

- `StateVersions.List(ctx, *StateVersionListOptions) (*StateVersionList, error)`
  - `StateVersionListOptions{ListOptions, Organization, Workspace}` (both are
    **name** filters: `filter[organization][name]`, `filter[workspace][name]`)
- `StateVersions.Download(ctx, url string) ([]byte, error)`: pass a version's
  `DownloadURL` (raw) or `JSONDownloadURL` (json-format)
- `StateVersion` fields: `ID, CreatedAt, DownloadURL (hosted-state-download-url),
JSONDownloadURL, Serial int64, Size int64, VCSCommitSHA, Status`

Runs:

- `Runs.List(ctx, workspaceID string, *RunListOptions) (*RunList, error)`
  - Set `RunListOptions.Include = []RunIncludeOpt{RunPlan, RunApply,
RunConfigVer, RunCreatedBy, RunCostEstimate}` to hydrate relations
- `Run` relations: `Plan *Plan`, `Apply *Apply`,
  `ConfigurationVersion *ConfigurationVersion`, `CreatedBy *User`,
  `Comments []*Comment`, `Workspace *Workspace`
- `Run` attrs incl.: `ID, CreatedAt, Message, Status, HasChanges, IsDestroy, ...`

Plans / Applies:

- `Plans.Logs(ctx, planID string) (io.Reader, error)`
- `Plans.ReadJSONOutput(ctx, planID string) ([]byte, error)`
- `Applies.Logs(ctx, applyID string) (io.Reader, error)`
- both `Plan`/`Apply` carry `ResourceAdditions/Changes/Destructions`, `Status`

Cost estimates:

- reachable via the `RunCostEstimate` include (already set) / `Run.CostEstimate`;
  persist the id-serialized attrs (`Delta/Prior/ProposedMonthlyCost`, matched /
  unmatched resource counts) plus `CostEstimates.Logs(id)` for the human-readable
  breakdown. Do not flatten it to a bare id like other relations.

Configuration versions:

- `ConfigurationVersions.List(ctx, workspaceID string, *ConfigurationVersionListOptions)`
- `ConfigurationVersions.Download(ctx, cvID string) ([]byte, error)`: tar.gz
- `Source`/`Speculative`/`Status` are top-level `ConfigurationVersion` attrs
  (always returned); add `include=ingress_attributes` only for the ingress
  relation (commit-sha/branch/PR/sender), which is independent of the tarball

Comments:

- `Comments.List(ctx, runID string) (*CommentList, error)`

Additional org surface (now in scope, verified against the same source; these
are shorthand; every `List` here takes an options arg and returns paginated
results to loop, per the pagination pattern below):

- **Projects**: `Projects.List/Read`; every workspace has a `Project` relation
  (`include=project`) carrying default execution mode, agent pool, auto-destroy,
  tag bindings.
- **Teams & access**: `Teams.List/Read` (OrganizationAccess matrix, SSO/SCIM),
  `OrganizationMemberships.List` (org roster; `include=user,teams`),
  `TeamMembers.ListOrganizationMemberships`, `TeamAccess.List` (workspace RBAC,
  filter by workspace id), `TeamProjectAccess.List` (project RBAC, filter by
  project id).
- **VCS**: `OAuthClients.List(org)` (`include=oauth_tokens,projects`),
  `OAuthTokens.List(org)`, and `GHAInstallations.List` for GitHub App
  integrations (metadata only; **user/token-scoped, not org-scoped**, so
  completeness depends on the archiving identity, and each consuming object still
  keeps its own `VCSRepo` wiring).
- **Governance**: `PolicySets.List(org)`, `PolicySetVersions`,
  `PolicySetParameters.List`, `Policies.List(org)` + `Policies.Download(id)`
  (the Sentinel/OPA source), `VariableSets.List(org)` +
  `VariableSetVariables.List(setID)`.
- **Per-run cost**: `CostEstimates.Read(id)` + `CostEstimates.Logs(id)`, reached
  via `Run.CostEstimate` (the `RunCostEstimate` include is already set).
- **Per-run governance/tasks**: `RunEvents.List(runID)` (`include=actor,comment`),
  `PolicyChecks.List(runID)` + `PolicyChecks.Logs(id)`,
  `TaskStages.List(runID)` (`include=task_results,policy_evaluations`) ->
  `TaskResults`, `PolicyEvaluations.List(taskStageID)` -> `PolicySetOutcomes.List`,
  and `TFPolicyEvaluationOutcomes` (set the `RunTFPolicyEvaluation` include).
- **Run wiring**: `RunTriggers.List(workspaceID)` (inbound),
  `RunTasks.List(org)` (defs) + `WorkspaceRunTasks.List(wsID)`,
  `NotificationConfigurations.List(id)` (the subscribable is
  polymorphic, so list it per workspace **and** per project **and** per team),
  `AgentPools.List(org)` (+ allowed & excluded workspaces, allowed projects).
- **Workspace extras**: `Workspaces.Readme(id)`, `Workspaces.ListTagBindings` /
  `ListEffectiveTagBindings`, `Workspaces.ListRemoteStateConsumers(id)` (only
  meaningful when `GlobalRemoteState` is false).
- **Registry**: `RegistryModules.List(org)` (+ `ReadVersion`, `ListCommits`; set
  `include=no-code-modules` to hydrate no-code config, then
  `RegistryNoCodeModules.ReadVariables` for per-version variable options, a BETA
  API), `RegistryProviders.List(org)` (+ `RegistryProviderVersions`,
  `RegistryProviderPlatforms`, shasum/sig download URLs), `GPGKeys.ListPrivate`
  (requires `Namespaces=[org name]`; keys are namespaced, not a flat org list).
- **Audit**: `AuditTrails.List` (with `Since`, paginate; elevated token) +
  `OrganizationAuditConfigurations.Read(org)` (whether/how auditing is on),
  `OrganizationTokenTTLPolicies.List(org)` (non-secret token-TTL governance), and
  `ReservedTagKeys.List(org)` (org tag governance; sources `reserved-tag-keys.json`).
- **HYOK** (optional): `HYOKConfigurations.List(org)`
  (`include=oidc_configuration,hyok_customer_key_versions`).
- **Stacks** (only if the org uses them): `Stacks.List(org)`,
  `StackConfigurations` (+ `JSONSchemas`), `StackDeploymentGroups.List(configID)`.
  Runs hang off **groups**, not named deployments:
  `StackDeploymentRuns.List(deploymentGroupID)` ->
  `StackDeploymentSteps.List(runID)` -> per-step artifacts + diagnostics.
  `StackDeployments.List(stackID)` gives named-deployment metadata (exposes only
  `LatestDeploymentRun`, no full run list). `StackStates` (`Description()` = full
  stack state).

Gotchas confirmed against source:

- `Run.Comments` is a struct relation but is NOT includable via `RunIncludeOpt`;
  comments come from the separate `Comments.List(runID)` call.
- `StateVersionOutputs` is intentionally skipped: the endpoint redacts sensitive
  outputs, so it is a lossy subset of the raw state already archived.

Pagination pattern: loop `PageNumber` from 1, advance while
`resp.Pagination.NextPage != 0` (or `CurrentPage < TotalPages`).

## Open items / TODO

- **Pin `go-tfe` at `v1.109.0`: do NOT float `@latest`, and do NOT move to v2.**
  The library now ships two coexisting modules from one repo: the classic
  `github.com/hashicorp/go-tfe` (root module, the hand-written jsonapi client
  this design targets) and `github.com/hashicorp/go-tfe/v2` (`/v2` submodule, a
  Microsoft-Kiota client regenerated nightly from an OpenAPI spec, tag `v2.1.0`, a
  completely different API). Import the classic path (no `/v2` suffix) and pin
  `v1.109.0`; `go get @latest` on the classic path floats to the newest v1 tag,
  which moves under you, and accidentally picking up `/v2` breaks every symbol
  here.
  - **v1 is frozen, not deprecated.** The whole client is consolidated into a
    single `v1.go` whose header calls it "the final version ... NO LONGER TESTED
    and SHOULD NOT BE EXTENDED"; new endpoints land only in v2. But frozen is not
    sunset (no removal date, still accepts critical/security fixes, ~100 releases
    of hardening) and `v1.109.0` is the last and most complete v1 tag. For a
    read-only archive this is the low-risk, highest-fidelity surface.
  - **v2 is disqualifying for _this_ tool: a breadth gap, not just
    `ChangeRequests`.** v2's spec omits ~20 operations the archive needs _today_
    (not "coming soon": absent from the spec, so nightly codegen will not produce
    them): the entire Stacks family, the entire private Registry (module
    `List`/`ReadVersion`/`ListCommits`, providers, provider versions/platforms,
    no-code modules), `GPGKeys.ListPrivate`, `ReservedTagKeys`, `RunEvents`,
    `OrganizationAuditConfigurations`, `CostEstimates.Logs`, and
    `TFPolicyEvaluationOutcomes`. It also regresses two things a bulk archiver
    leans on: `Runs` no longer accepts an `include` sideload (so hydrating
    plan/apply/config-version/created-by/cost-estimate collapses to N+1
    relationship walks) and `Plans.ReadJSONOutput`'s generated method is a no-op.
    v2 still self-labels beta (`version.go` = `2.0.0-beta1`, spec titled
    "v2-Beta") and regenerates nightly: churn and reproducibility hazards for a
    stable reference archive.
  - **Coexistence keeps v2 available later, additively.** The two modules have
    distinct import paths, so pinning v1 now forecloses nothing: if a genuinely
    v2-only resource is ever wanted (`ChangeRequests`, `RecoverableItems`
    soft-delete recovery, `AssessmentResults`, unified OIDC / `AuthenticationTokens`,
    `WorkspaceTransfers`), import v2 side-by-side purely for it and keep v1 for the
    rest. Full migration waits until the spec closes the Stacks / Registry gaps and
    v2 drops the beta labeling.
- The org-scoped and per-run objects above are folded into the layout. Scope
  knobs resolved: the heavy or org-specific surfaces (Stacks, HYOK, the registry
  version/platform/binary detail, and the audit trail) default off and are
  gathered only when a run opts in, since they matter only for some orgs, add many
  requests, and (for the audit trail) need an elevated token.
- Low-value / deferred (record as N/A in the manifest so they are not mistaken
  for gaps): token metadata (secrets are write-only), `SSHKeys` (names only),
  `WorkspaceResources` and `StateVersionOutputs` (derivable/redacted subsets of
  state), `OrganizationTags` (derivable: each workspace's legacy tags are
  already in `workspace.json` via `TagNames`), `Explorer` (derived CSV),
  ephemeral `Agents`/`QueryRuns`, `TestRuns`/`TestVariables`, and `Admin.*`
  (Terraform Enterprise only, 401/404 on HCP).
- The ledger (`.ledger/`) is a required per-object record (status, counts,
  timestamp, tool version) and the backbone of three required behaviors: resume,
  incremental re-run, and progress reporting, not just a summary. It also
  carries per-run records (`lastRunAt`, `runCount`, summary totals) and the
  per-collection high-water marks re-runs key on (see Behavior decisions), and is
  partitioned into per-workspace shards keyed under the same archive-relative
  paths (see Storage at scale).
