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
    <page>.json                     # who-did-what-when (elevated token; windowed)
  reserved-tag-keys.json            # org tag governance (optional)
  hyok-configurations/<id>/         # HYOK encryption config (optional)
    hyok-configuration.json         # KEK id, KMS options, status
    oidc-configuration.json         # concrete AWS/GCP/Azure/Vault OIDC config
    key-versions/<kv-id>.json       # per customer-key-version status
  registry/
    modules/<ns>-<name>-<provider>/module.json    # + versions, last commits
    no-code-modules/<id>.json                     # version pin + variable options
                                                   #   (BETA API; via module include)
    providers/<ns>-<name>/provider.json           # + versions, platforms, shasums
    gpg-keys/<namespace>-<key-id>.json            # ascii-armor public signing keys
                                                   #   (namespaced; ListPrivate)
  config-versions/<cv-id>.tar.gz    # deduped org-wide; runs reference by id

  # --- project-scoped objects: everything a project owns nests beneath it ---
  projects/<project-name>/
    project.json                    # id, defaults (exec mode, agent pool,
                                     #   auto-destroy), tag bindings, team access
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
        configuration.json          # + JSON schema, diagnostics
        deployment-groups/<gid>/
          group.json
          runs/<run-id>/
            run.json
            steps/<step-id>/        # per-step artifacts (plan/apply desc) + diag
      deployments/<name>/
        deployment.json             # named-deployment metadata + latest-run ref
      states/<generation>.json      # StackStates Description() = full stack state
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
  per-object status (`done` / `absent-permanently` / `skipped` / `errored` /
  `not-applicable`, with counts, timestamps, and the failing error), and resume
  consults it. `skipped` and `not-applicable` are settled like `done` (never
  re-requested), so a deferred or intentionally-skipped object is not mistaken
  for a gap.
  Downloads write to a temp path and atomic-rename on success, so an
  interrupted run never leaves a truncated `.tfstate.json` / `.tar.gz` that
  looks complete. The manifest is flushed durably (write-temp + atomic-rename)
  at a bounded cadence and on shutdown, so a kill -9 loses at most the last
  in-flight batch, never the ledger itself.
  - **Restart semantics**: a re-invocation against a non-empty output dir loads
    the existing manifest and, per object, _skips_ `done` and
    `absent-permanently`, _retries_ `errored` and anything absent from the
    ledger. `absent-permanently` is sticky (a 404/410 is not re-requested every
    run); a `--recheck-absent` toggle forces re-probing when the operator
    suspects a since-restored object. Resume and a clean first run are the same
    code path: a first run just starts from an empty ledger.
  - **Interrupted vs. failed** are the same to resume: both leave objects in
    `errored`/absent, and both are picked up on the next invocation. Transient
    errors (429, 5xx, timeouts, context cancellation) are recorded distinctly
    from terminal ones (404/410) so resume never mistakes a rate-limit blip for
    a permanent absence.
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
  - off a terminal (a pipe, a redirect, CI), the same signal as a periodic
    logfmt line (phase, current target, counts, `completed=x/y` while
    determinate, bytes, elapsed, a rough rate);
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

- **Concurrency**: worker pool over workspaces (default ~4); sequential within a
  workspace. `go-tfe` retries per request on rate limits, but N workers each
  paginating and downloading multiply the request rate; use a shared rate
  limiter, not just per-request retry. Guard shared manifest/counter writes with
  a mutex. (Config-version ids are globally unique, so the shared
  `config-versions/` dir is race-free.)
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

The ledger lives as per-workspace shards co-located with the subtree they index
(`.../workspaces/<ws>/.ledger/`), a small org-root shard for org-scoped objects,
and per-`cvID`-prefix sub-shards for the config-version entries (numerous enough
to reconstitute the monolith on their own). Each entry belongs to the shard named
by its relpath prefix, so its key is byte-identical to the single-file form and
every ledger operation — `ShouldFetch`, `Entry`, the frozen early-stop, the
watermarks and completion flags — is unchanged; only the entry's physical
location differs. Each shard carries the marks and flags whose keys share its
prefix, and resident ledger memory is bounded to `concurrency x one shard`: a
shard loads when its workspace's walk begins and is released when it ends.

A shard is a compacted `snapshot.json` plus an append-only `log.ndjson`.
Recording an entry appends one newline-terminated line, so no flush re-serializes
the whole ledger; the terminating newline is the commit marker and a torn
trailing line is dropped on read. Compaction folds a shard's log into its
snapshot once the log has outgrown it, writing the merged snapshot before
truncating the log, and an unchanged record appends no line, so a re-run's
archive-then-stop boundary adds nothing. Each shard commits through the same
temp-write-and-atomic-rename as every other file, so a shard that exists is
whole. A shard with no file is re-derived from the on-disk tree rather than read
as empty.

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
  floor, while coalescing collapses object count ~13x. `run.json` is the one
  **mutable** small JSON, dedup-checked and refreshed at the walk boundary while a
  run is in-flight, so it is never coalesced; it stays a loose file, the one
  remaining loose per-run object. Its seven sibling children and the state-version
  `meta.json` are immutable and `ShouldFetch`-gated, and those are what coalesce.
  Coalescing is confined to the seal boundary, where the object is already frozen,
  single-threaded, and settled, so the append is pure and the relpath stays the
  ledger key: the immutable children and `meta.json` coalesce directly, while
  `run.json` stays loose. Coalescing `run.json` at collect time (born as a line)
  would instead invert the relpath-is-the-key coupling, the newest-first
  early-stop, `WriteJSON`'s parse-free whole-file dedup, and per-object atomicity,
  and folding a still-mutable file would only churn it, so it stays loose (there is
  no `runs.ndjson`). Each coalesced line is a JSON object:
  `{"path": <relpath>, "sha256": <hex>, "content": <file bytes as an escaped JSON string>}`.
  The line is not byte-equal to the original file, but the `content` field carries the
  exact bytes verbatim (reproducing the file on extraction and staying greppable
  for field tokens) and the `sha256` is the content signature already in the
  ledger.

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

### Mapping onto object-store storage classes

The tool writes only local files; it performs no object-store uploads and sets no
storage class. But the layout is deliberately split so an operator who backs the
archive up to an object store can point one lifecycle rule at each kind of file,
and so that an audit proceeds the same way on disk: narrow first, then read one
thing. The **search layer** (the loose mutable workspace files, `run.json`, the
NDJSON roll-ups, every sidecar index, and the ledger shards) is small, listable,
and zero-latency to grep, so it belongs in a hot, always-listable class (S3
Standard, say). The **cold bundles** are byte-heavy and rarely read, so they map
onto an archival class (Glacier Deep Archive, say). Two properties make that
mapping clean: the bundles are write-once, so an operator can set the archival
class on first upload rather than paying to transition from Standard, and no
bundle is a sub-128KB object (which an archival class bills as ~168KB, the reason
the small metadata is not itself bundled). `logs.gen*.zip` and `state.gen*.zip`
are separate per generation, so one restore answers one audit question and a
plan-log thaw never drags the state along. An audit greps the search layer on
disk (free, and tighter than a flat tree, since the roll-ups collapse hundreds of run
directories), reads the bundle name and member from the sidecar, and `unzip`s
that one member (thawing the one bundle first if it has been tiered off to an
archival class). The greppable surface stays live and gets tighter. A cold heavy artifact is no
longer a directly-`cat`-able file, but that cost falls on audit-only artifacts
that are rarely read.

### What stays loose vs. what seals

The tool writes every row below as local files; the "Backup tier" column is the
object-store storage class an operator's own backup lifecycle would map each kind
onto, not something the tool sets.

| Tier                      | Objects                                                                                                                                                           | Backup tier  | Form                                                              |
| ------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------ | ----------------------------------------------------------------- |
| Loose, mutable, greppable | the 9 per-ws settings files (`workspace.json`, `variables.json`, `tags.json`, team access, notification configs, ...), `run.json`, ledger shards, sidecar indexes | Standard     | one file per relpath                                              |
| Coalesced roll-ups        | the 7 immutable run children + state-version `meta.json` (`run.json` is mutable and stays loose)                                                                  | Standard     | per-ws `*.ndjson`, keyed by relpath                               |
| Cold bundles              | `plan.log` / `plan.json` / `apply.log` / `cost-estimate.log` / `policy-check-*.log`; raw + json state                                                             | Deep Archive | write-once generational `logs.zip` + `state.zip`, sidecar-indexed |

At 1000 workspaces x 200 frozen runs x 30 state versions (config-version tarballs
excluded, equal either way), object count holds near ~200k where one file per
object gives ~2.7M, about ~13x fewer, since `run.json` stays a loose file per run
rather than folding. The compressed backup sits near ~29GB where the raw tree is
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
  runs/<run-id>/run.json                                     # loose, mutable; one per run, never coalesced
  .ledger/
    snapshot.json                                            # ledger shard, keys = relpaths
    log.ndjson                                               #   append-only; compacts when log > snapshot
  rollups/
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
  one) with a progress-interval knob, `--recheck-absent` to re-probe
  `absent-permanently` objects on a re-run, and the `--log-*` knobs.
- Config file (all keys optional, defaults applied per field): `address`
  (default `https://app.terraform.io`), `organizations` (all visible orgs if
  empty), `concurrency`, and a `scope` block of toggles for the heavy or
  optional surfaces (Stacks, HYOK, registry version/platform/binary detail,
  audit trails), each off by default.

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
