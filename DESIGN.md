# TFC Archiver: Design Notes

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

- **Metadata-only for every secret** (not just sensitive vars). Write-only
  fields come back blank on read, so the archive records existence/metadata
  only, with the value as `[REDACTED]`. This covers sensitive variable,
  variable-set, and policy-set-parameter values, every `*Token` secret (team /
  org / agent / user tokens), `OAuthClient.Secret`, `RunTask.HMACKey`, and
  `NotificationConfiguration.Token`. `SSHKey` is a related but distinct case:
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
  manifest.json                     # per-object ledger + run records &
                                     #   watermarks driving resume / re-run
                                     #   (see Behavior decisions)

  # --- org-level objects (not scoped to a single project) ---
  teams/<id>/
    team.json                       # def + OrganizationAccess matrix, members,
                                     #   SSO/SCIM linkage
    notification-configs.json       # team-scoped alerting (redact Token)
  memberships.json                  # org roster: email, status, user + team refs
  oauth-clients/<id>.json           # VCS connection def + tokens (redact Secret)
  github-app-installations.json     # GitHub App VCS installs (metadata only;
                                     #   user/token-scoped, not org-scoped)
  variable-sets/<id>/
    variable-set.json               # name, global/priority, applied scopes
    variables.json                  # sensitive values -> "[REDACTED]"
  policy-sets/<id>/
    policy-set.json                 # kind, scope, current/newest version meta
    parameters.json                 # sensitive values -> "[REDACTED]"
  policies/<id>.json                # metadata
  policies/<id>.<ext>               # Sentinel/OPA source (Policies.Download)
  run-tasks.json                    # org run-task definitions (redact HMACKey)
  agent-pools/<id>.json             # pool config + allowed & excluded ws, allowed projects
  token-ttl-policies.json           # org max-TTL-per-token-type governance
  audit-trails/
    config.json                     # whether/how auditing is on (elevated token)
    <page>.json                     # who-did-what-when (elevated token; windowed)
  reserved-tag-keys.json            # org tag governance (optional)
  hyok-configurations/<id>.json     # HYOK encryption config (optional)
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
    notification-configs.json       # project-scoped alerting (redact Token)

    workspaces/<ws-name>/
      workspace.json                # full settings + Project ref, GlobalRemoteState
      variables.json                # sensitive values -> "[REDACTED]"
      readme.md                     # workspace README (Workspaces.Readme)
      tags.json                     # tag + effective-tag bindings
      team-access.json              # workspace RBAC (which team, what access)
      notification-configs.json     # alerting wiring (redact Token)
      run-triggers.json             # inbound cross-workspace trigger sources
      run-tasks.json                # per-workspace run-task bindings
      remote-state-consumers.json   # only when GlobalRemoteState=false
      state-versions/
        <created-at>-<id>.tfstate.json  # raw state (hosted-state-download-url)
        <created-at>-<id>.json          # JSON-format state, if available
        <created-at>-<id>.meta.json     # serial, created-at, run ref, size, vcs sha
      runs/<run-id>/
        run.json                    # summary, status, timestamps, trigger, message
        config-version.json         # cv id + ingress attrs (commit sha/branch/PR)
        plan.log                    # plan log output
        plan.json                   # structured plan (ReadJSONOutput), if available
        apply.log                   # apply log output
        cost-estimate.json          # monthly cost deltas + resource counts, if any
        cost-estimate.log           # human-readable cost breakdown (Logs)
        comments.json               # run comments
        run-events.json             # actor-attributed timeline (confirm/discard)
        policy-checks.json          # Sentinel results (+ policy-check-<id>.log)
        task-stages.json            # task stages -> task results + policy evals
        tf-policy-outcomes.json     # native TF policy results, if any

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
  (store once). A run references its cv by **id** plus a relative path;
  `config-version.json` keeps the ingress attributes even when the tarball
  itself is no longer downloadable.
- Runs record only the cv **id** as the join key, never the expiring signed
  `upload-url` that a `RunConfigVer` include would otherwise nest in run.json (a
  `ConfigurationVersion` carries only `upload-url`; the `hosted-*-url` fields are
  a `StateVersion` concern).
- Any org-level object with a write-only secret stores metadata only
  (see Hard limits).
- **Raw state blobs are sensitive; treat the archive as secret at rest.** The
  `.tfstate.json` pulled from `hosted-state-download-url` embeds sensitive
  variable / output / resource values in cleartext; the API redacts them only
  through `StateVersionOutputs` (which we skip). The "metadata-only for every
  secret" rule is about write-only _config_ fields, not state contents.
- **Notification configs are polymorphic** across workspace / project / team
  scope, so they are archived at all three levels: in each
  `projects/<name>/workspaces/<ws>/`, in `projects/<name>/`, and in `teams/<id>/`,
  not just workspaces.

## Behavior decisions

- **Best-effort, not fail-fast**: a 404/410 on one object (e.g. an expired
  config version or missing log) records the outcome in `manifest.json` and
  continues. One bad object must not abort the whole archive.
- **Resumable via a real ledger, not file-existence** (required). A
  permanently-gone object (404/410) and a not-yet-fetched one both leave no
  file, so existence alone cannot drive resume. `manifest.json` records
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
  - a periodic human-readable line (current org/project/workspace, objects
    done / errored / remaining, bytes downloaded, elapsed and a rough rate), so
    an operator watching a terminal sees forward motion;
  - the same signal in a machine-readable form (`--progress=json`, one JSON
    object per line: phase, counts, current target) for wrapping in CI or a
    watcher, defaulting to the human format on a TTY and quiet/log-only when not
    on one.

  Progress is derived from the same in-memory counters that back the manifest
  (a single mutex-guarded tally), so the reported numbers and the ledger's own
  tally never disagree (the on-disk copy trails only by the last unflushed
  batch). A final summary line (totals per status class, wall
  time, any orgs/workspaces that errored) prints on completion and is also
  written to the manifest as the run record.

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
  - **Redact by mutation**: there is no tag-driven omission, so sensitive
    `Value` fields and every write-only secret (each `*Token`, `OAuthClient.Secret`,
    `RunTask.HMACKey`, `NotificationConfiguration.Token`) are overwritten with the
    `[REDACTED]` sentinel on the struct before marshaling, set to the marker, not
    zeroed, so the archive records the secret's existence rather than an empty
    value indistinguishable from a genuinely unset field.
  - **Drop every ephemeral signed URL, by pattern not enumeration.** Match any
    signed out-of-band URL field: both `*DownloadURL` _and_ `*UploadURL`
    (`DownloadURL`, `UploadURL`, `JSONUploadURL`, `SanitizedStateUploadURL`) plus
    the `LogReadURL` on hydrated `Plan`/`Apply` relations (tag `log-read-url`):
    they expire within minutes, can embed tokens, and duplicate blobs already
    archived. (The provider `Shasums*URL` values need no handling; they are
    methods on `RegistryProviderVersion`, not struct fields, so neither marshaler
    serializes them.)
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

## Config surface (planned)

- `TFC_TOKEN` / `TFE_TOKEN` (required)
- address, default `https://app.terraform.io`
- org (optional; all visible orgs if omitted)
- output dir (resume/re-run is implied when it already holds an archive)
- workspace concurrency
- scope toggles for heavy/optional surfaces (Stacks, HYOK, registry
  version/platform/binary detail, audit trails)
- `--progress=auto|human|json|quiet` (default `auto`: human on a TTY, quiet
  off one) and a progress-interval knob
- `--recheck-absent` to re-probe `absent-permanently` objects on a re-run

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
  (redact `Value` when `Sensitive`)

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
- **VCS**: `OAuthClients.List(org)` (`include=oauth_tokens,projects`; redact
  `Secret`), `OAuthTokens.List(org)`, and `GHAInstallations.List` for GitHub App
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
  `RunTasks.List(org)` (defs; redact `HMACKey`) + `WorkspaceRunTasks.List(wsID)`,
  `NotificationConfigurations.List(id)` (redact `Token`; the subscribable is
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
- `manifest.json` is a required per-object ledger (status, counts, timestamp,
  tool version) and the backbone of three required behaviors: resume,
  incremental re-run, and progress reporting, not just a summary. It also
  carries per-run records (`lastRunAt`, `runCount`, summary totals) and the
  per-collection high-water marks re-runs key on (see Behavior decisions).
