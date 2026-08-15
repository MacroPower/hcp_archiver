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
go install go.jacobcolvin.com/hcp_archiver/cmd/hcp_archiver@latest
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
(every visible organization, default surfaces only).

```yaml
# yaml-language-server: $schema=./config/config.schema.json

# HCP Terraform API address (the default).
address: https://app.terraform.io

# Ceiling on how fast requests launch, in requests per second. The default of
# 30 is HCP Terraform's documented limit. Lower it for an organization whose
# granted limit sits well below that; the client still adapts downward from
# the server's rate-limit feedback on its own.
rateLimit: 30

# Organizations to archive; omit or leave empty for every visible org.
organizations:
  - my-org

# Projects and workspaces to archive within each organization; omit or leave
# either empty for everything. With both set, a workspace must satisfy both.
projects:
  - my-project
workspaces:
  - my-workspace

# Bound on each workspace's archived run history, unlimited by default: count
# keeps the newest N runs, age keeps runs created within the window (a Go
# duration). With both set, whichever admits more history wins.
runHistory:
  count: 500
  age: 2160h

# Heavy or org-specific surfaces, each off by default.
scope:
  stacks: true
  hyok: true
  registryDetail: true
  auditTrail: true

# Mirror the archive to a remote object store: sealed cold bundles and
# settled configuration tarballs are evicted there (uploaded, verified,
# then removed locally), and every other file syncs there at each org run's
# close, so the bucket holds a complete copy while local disk stays the
# grep-able search layer plus in-flight work. Omit the block to keep the
# whole archive on local disk. The URL's scheme selects the backend;
# credentials come from that provider's default chain (AWS SDK chain,
# Azure environment/DefaultAzureCredential), never from here.
remote:
  url: s3://my-archive-bucket?region=us-east-1 # required to enable the mirror
  # url: s3://my-bucket?endpoint=s3.example.com&use_path_style=true # MinIO, R2, Ceph
  # url: azblob://my-container # Azure Blob Storage
  # url: file:///mnt/archive-mirror # local directory tree
  prefix: hcp-archive # optional key prefix
  # partSize: 67108864 # parted-upload tuning; defaults are fine
  # concurrency: 4
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
file tree) readable in a scrolling viewer. It needs no HCP token and, for a
fully local archive, no network.

```bash
hcp_archiver view ./archive          # the archive root, or one org's directory
```

Navigation descends with `enter`, returns with `esc`, filters any list with
`/`, and quits with `q`. The browser reads the archive's physical forms
transparently, so an object displays the same whether it is still a loose file
or has been sealed into an NDJSON roll-up or a zip bundle.

For an archive the archiver wrote and mirrored to object storage (see
[Mirroring the archive](#mirroring-the-archive-to-object-storage)),
browsing and grep stay fully offline — the search layer, including every
sidecar index, is local — and only opening a sealed member whose bundle was
evicted reaches the remote store, fetching just that member with ranged reads
rather than the whole bundle. (A tree bootstrapped _from_ a mirror is the one
exception: it fetches what it lacks on demand, per
[Reading an archive back from its mirror](#reading-an-archive-back-from-its-mirror).)
That read path needs object-store credentials from the backend provider's
default chain; a read-only identity scoped to the archive prefix is the right
shape.

Evicted configuration tarballs stay visible too: eviction leaves a small stub
in each tarball's place, so `list` still reports the object at its true size,
shown as `remote`. `unseal` fetches one back from the mirror like any other
evicted object, verifying the bytes against the size and digest the eviction
proved. Only `show` and the browser's viewer stop at such an object, since both
hold what they read whole in memory and a tarball has no bound; they name its
mirrored key (`<prefix>/<org>/config-versions/<id>.tar.gz`) to fetch with any
client for the backing store.

### Listing and unsealing files

Three plain, scriptable commands cover the same read surface without an
interactive session: `list` enumerates archived objects, `show` prints one
object's exact bytes to stdout, and `unseal` extracts any scope back into a
plain directory tree. All three read the physical forms transparently, so a
freshly collected tree and a fully sealed one answer identically.

Objects are addressed by org-prefixed archive paths (`<org>/<path>`), the same
layout an unseal reproduces beneath its target, whether the command points at
the archive root or a single organization's directory. Positional arguments
bind left to right: with two arguments the first is the archive directory and
the second the archive path; a single argument to `list` and `unseal` names
the archive directory (pass `.` explicitly to address a path in the current
directory), while a single argument to `show` is the archive path, read from
the current directory.

```bash
hcp_archiver list ./archive                       # every object, every org
hcp_archiver list ./archive my-org/projects       # any path prefix
hcp_archiver show ./archive my-org/org.json       # exact bytes to stdout
hcp_archiver unseal ./archive my-org --target ./restore
```

`list` prints one line per object (size, physical form, path), with sealed
members labeled by the form that carries them and anything evicted to the
mirror shown as `remote` (reading one needs object-store credentials, like the
browser's evicted-read path):

```
4.1 KiB  remote  my-org/config-versions/cv-abc.tar.gz
  142 B  loose   my-org/org.json
   88 B  rollup  my-org/projects/default/workspaces/app/runs/run-new/config-version.json
   17 B  bundle  my-org/projects/default/workspaces/app/runs/run-new/plan.log
   12 B  remote  my-org/projects/default/workspaces/app/state-versions/20240101T000000Z-sv-1.tfstate.json
```

`--json` switches `list` to NDJSON (one object per line, with `path`, `org`,
`form`, and `size` on every line, plus `container`, `modified`, and
`offloaded` where they apply) and `unseal` to a JSON summary. `unseal
--dry-run` reports what a run would write without writing, planning from the
same listing over the same prefix, so it predicts the run it describes: every
listed object is accounted for, either as one that would be written or as one
already known to be unrecoverable, each of the latter named on stderr. Only an
evicted object in an organization recording no mirror falls in the second
group; everything else evicted is reported separately, as the volume the run
would fetch (`remoteFiles` and `remoteBytes` under `--json`), since a run
fetches it without asking and this is where that cost is visible first. That
byte figure is an upper bound: it counts each object's archived length, which
is what a tarball costs exactly, while a member of an evicted bundle travels as
its compressed span. The prediction is a lower bound on the failures, since a
corrupt bundle, an unreachable mirror, or an object missing from the bucket all
error only when something reads them, which sizing the plan does not (the one
egress a dry run may spend is materializing a bootstrapped workspace's absent
sealed indexes, so its plan can cover sealed objects at all). Both the text
summary and the `errored` field of `--json` carry the count.

`unseal -v` streams one line per recovered file to stderr, and per-file
failures always land there. A run in which every object recovers exits 0; if
any object fails, the failures are counted and reported and the command exits
with status 1, and a dry run whose plan already holds one exits the same way.
An interrupted run reports its partial totals to stderr and exits 0; a fetch
cut off mid-object leaves no partial file behind.

### Exporting a browsable site

`export` renders an archive's metadata as a tree of markdown files that a
static site generator with directory-based navigation (mkdocs and its kin)
builds without configuration: every directory carries an `index.md`, so the
generated tree navigates as-is.

```bash
hcp_archiver export ./archive --target ./site
```

Pages carry metadata only: organization, team, variable-set, policy-set,
project, workspace, and stack settings, variable tables (a sensitive
variable shows its key with a `(sensitive)` marker, its stored value never
read), and run and state-version history. Content that can embed secret
values -- state blobs, plan and apply logs, cost estimates -- is represented
by presence alone: names, sizes, and timestamps, never bytes. A workspace's
archived readme is copied beside its page verbatim. The tree is written
world-readable, curated for sharing, unlike the owner-only archive.

The target is created when absent and refused when non-empty; `--force`
replaces its contents (refusing when the archive itself sits beneath the
target). Like the other read commands, the export reads the physical forms
transparently; exporting a sealed archive whose roll-ups were offloaded
fetches that metadata back from the mirror.

#### Customizing export pages

Every page renders through a Go `text/template`, and the configuration
file's `export.templates` key names a directory of overrides:

```yaml
export:
  templates: export-templates # relative paths resolve against this file
```

The directory's `*.md.tmpl` files replace the same-named built-in templates
-- `archive.md.tmpl`, `org.md.tmpl`, `teams.md.tmpl`, `variable-sets.md.tmpl`,
`policy-sets.md.tmpl`, `projects.md.tmpl`, `project.md.tmpl`,
`workspace.md.tmpl`, and `stack.md.tmpl` -- while pages without an override
keep their default. A file whose name matches no page template is refused
rather than silently ignored, and a broken override is refused before the
target directory is touched. The defaults ship embedded in the binary (the
`export` package's `DefaultTemplates` serves them, or copy them from
`export/templates/` in this repository) and are the reference for each page's
data; the data is built with every sensitive value already redacted, so a
custom template can restyle the pages but cannot reach withheld content.

### Mirroring the archive to object storage

With a `remote:` block configured, the bucket converges on a **complete copy**
of the archive, in two motions. The cold surfaces are **evicted** (uploaded,
verified, then removed locally): each workspace's sealed `logs.gen*.zip` and
`state.gen*.zip` at the moment it seals, and each org-wide
`config-versions/<id>.tar.gz` once the ledger has proven its bytes. Peak
local disk then stays bounded to the grep-able search layer plus in-flight
work. Everything else is **synced** at each org run's close — loose JSON,
roll-ups, sidecar indexes, ledger snapshots — with the local copy kept: local
disk stays the canonical, searchable archive, and the bucket is the long-term
and disaster-recovery copy. Restoring is one download of the org prefix.

The sync is incremental: one bucket listing per run inventories the mirror
(~1 request per 1000 keys) and only absent or changed files upload, compared
by size and the full-object MD5 digest the tool records with each synced
write (fetched per file when a backend's listings omit it; on a store that
recorded no digest at all the comparison degrades to size alone). The mirror
also **prunes**: a remote copy of a file that no longer exists locally — a
loose `run.json` later coalesced into a roll-up, or a subtree you deleted —
is removed on the next run, so the bucket tracks the archive rather than
accumulating stale copies. Evicted bundles and tarballs are exempt; they are
remote-only by design. Eviction verification is layered: a bundle is
uploaded only after it has sealed and read back intact locally, a write that
dies mid-stream is aborted rather than committed as a truncated object, and
the local file is removed only once a follow-up probe confirms the object at
the expected size. Pointing a `remote:` block at an existing all-local
archive migrates it: the next run's sweep uploads everything (a one-time pass
that can take a while on a large archive; `sync_progress` log lines track it)
and evicts every previously sealed bundle.

Any failure leaves the local copy in place as canonical and warns; no single
file's failure aborts the sweep or the organization. What differs is the exit
status. An upload before the close is an optimization: a bundle that fails to
evict at its seal warns, defers to the close sweep, and never reaches the
exit status on its own. The close sweep is what makes the mirror a guarantee,
so a sweep that ends with anything unsettled (a file, the bucket listing, a
prune) marks the run **incomplete** and exits non-zero. Unlike a single
object's error, which is recorded and retried without failing the run, the
mirror is the archive's long-term record: a scheduled run must not report
success over one it knows is behind. Nothing is persisted for this, so the
next run's sweep re-derives the gap from the store itself and a transient
outage clears on the following run; an interrupted run keeps the clean exit
described under [Resuming and re-running](#resuming-and-re-running) even when
its sweep had already counted failures.

One remote failure never clears itself. If the store already holds an object
at an eviction key whose size or digest differs from the proven local bytes,
the sweep refuses to overwrite remote history at that key: it keeps the local
file canonical and warns (`sync_evict_error`, or `bundle_evict_error` at a
seal boundary) that the `remote copy differs from the proven local file ...
(needs manual inspection)`. It reports the mismatch again on every run, so
the run keeps exiting non-zero until an operator inspects the diverged object
and clears it by hand.

Credentials are never configured in YAML. The `url` scheme selects the
backend, and each backend authenticates through its provider's default chain:
`s3://` uses the AWS SDK chain (`AWS_ACCESS_KEY_ID` /
`AWS_SECRET_ACCESS_KEY`, `~/.aws/config` profiles, SSO, an instance/task
role); `azblob://` uses Azure's environment variables
(`AZURE_STORAGE_ACCOUNT` with a key, SAS token, or connection string) or
`DefaultAzureCredential`. The archiving identity needs write, read, list, and
delete on the archive prefix; `view` needs only read, and a separate
read-only identity for browsing is the recommended split.

Every object is written in the store's default storage class. Three bucket
choices are worth making deliberately:

- **Enable bucket versioning** (or Object Lock / blob soft delete). The
  mirror prunes remote copies of files that no longer exist locally, and the
  evicted bundles exist nowhere else; versioning turns any surprising delete
  or overwrite into a recoverable event instead of a permanent one.
- **Abort incomplete parted uploads** after a few days (an S3 lifecycle
  rule): an upload killed mid-flight is re-run safely by the next sweep, but
  its already-uploaded parts otherwise linger as billable storage.
- **Do not lifecycle mirrored objects into an archival tier** (S3 Glacier,
  Azure Archive). The tool has no restore workflow: a `view` read of an
  archived bundle fails with the backend's error until the object is
  restored by hand, and synced search-layer files are re-compared and
  re-uploaded as the archive changes, which archival tiers' minimum-storage
  charges punish. Infrequent-access tiers that serve reads directly
  (S3 Standard-IA, Azure Cool) are the safe cost lever.

### Reading an archive back from its mirror

Every read command works even when the local files are partly or completely
absent, as long as the mirror holds them. `view`, `list`, `show`, and `unseal`
all accept the mirror's location three ways: `--remote <bucket-url>` (with
`--remote-prefix` for the key prefix), `-c/--config` naming a configuration
file whose `remote:` block records it, or `$HCP_ARCHIVER_CONFIG` pointing at
that file.

```bash
mkdir restored && hcp_archiver view restored \
  --remote 's3://the-bucket?region=us-east-1' --remote-prefix archives
```

Pointed at an empty directory, the command bootstraps it: the mirror's
organizations are discovered from one listing, each gets its `org.json` and a
`.remote.json` marker materialized locally, and the flags are never needed
again, since later invocations find the mirror through the marker alone.
Local files always win when present; anything absent is fetched from the
mirror, verified against the digest its upload recorded, and persisted at its
archive path, so everything read once is local from then on. Listings union
the mirror's inventory into the local tree (`list` shows not-yet-fetched
objects as `remote`), and the two evicted surfaces keep their usual behavior:
bundle members are read with ranged requests without ever pulling the zip
down, and configuration tarballs stay remote-only for reads while `unseal`
streams them to its target.

The bootstrapped tree is a browse cache, not a canonical archive: its marker
records `"partial": true`, which is what tells later reads to keep falling
through to the mirror. Running the archiver against the same directory
collects and mirrors it properly, rewriting the marker and returning the tree
to the fully-local read behavior of any archive the archiver wrote. A
supplied `--remote` that disagrees with the mirror an existing marker records
is refused, exactly as the archiver refuses a re-pointed `remote:` block; and
if the mirror cannot be listed (offline, missing credentials), the commands
degrade to local content and say so on stderr (or the browser's status line)
rather than failing. Reads need the same object-store credentials as any
evicted-bundle access, from the backend provider's default chain.

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

### Reading the run summary

A run ends with a per-status summary line; the counts are the resume model made
visible:

- `done`: fetched and written.
- `absent`: gone (a 404, confirmed by an in-run re-probe), re-probed only
  with `--retry-absent`.
- `forbidden`: the token may not read it (a 403); retried on the next run, so
  a broader token can still capture it (see above). Counted apart from an error.
- `errored`: a transient or unclassified failure, retried next run; a healthy
  archive ends with `errored=0`.
- `skipped` / `n/a`: intentionally deferred or not applicable to this
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
  quiet off one) with a `--progress-interval` knob; and `--retry-absent`
  re-probes objects previously recorded as absent.
- **Configuration file** (every key optional, defaulted per field): `address`
  (the API endpoint, default `https://app.terraform.io`), `rateLimit` (the
  request-launch ceiling in requests per second, default 30, HCP Terraform's
  documented limit; lower it for an organization granted less), `organizations` (a
  list; empty or omitted archives every organization the token can see in turn),
  `projects` and `workspaces` (lists filtering what is archived within each
  organization; empty or omitted archives everything, and with both set a
  workspace must satisfy both),
  a `runHistory` block bounding each workspace's archived run history
  (`count` keeps the newest N runs, `age` keeps runs created within a
  Go-duration window; with both set,
  whichever admits more history wins; unlimited by default), a `scope`
  block of toggles for the heavy or optional surfaces (`stacks`, `hyok`,
  `registryDetail`, `auditTrail`), each off by default, and a `remote` block
  ([Mirroring the archive](#mirroring-the-archive-to-object-storage))
  naming the object store the archive is mirrored to (`url` required to
  enable; optional `prefix`, `partSize`, `concurrency`).

## Output layout

```
📁 archive/<org>/
│
│   # org root
├── 📄 org.json                          # organization metadata
├── 📁 .ledger/                          # sharded per-object ledger + run records & watermarks
├── 📄 .remote.json                      # only with a remote: where the mirror lives
│
│   # org-level objects (not scoped to a single project)
├── 📁 teams/<id>/
│   ├── 📄 team.json                     # definition + access matrix, members, SSO/SCIM
│   └── 📄 notification-configs.json     # team-scoped alerting
├── 📄 memberships.json                  # org roster: email, status, user + team refs
├── 📁 users/
│   └── 📄 <id>.json                     # users referenced by runs, events, teams
├── 📁 oauth-clients/<id>/               # VCS connection + per-token metadata
├── 📄 github-app-installations.json     # GitHub App installs (metadata only)
├── 📁 variable-sets/<id>/               # set metadata + variables
├── 📁 policy-sets/<id>/                 # set metadata, versions, parameters
├── 📁 policies/
│   ├── 📄 <id>.json                     # policy metadata
│   └── 📄 <id>.<ext>                    # Sentinel/OPA source
├── 📄 run-tasks.json                    # org run-task definitions
├── 📁 agent-pools/
│   └── 📄 <id>.json                     # pool config + allowed/excluded scopes
├── 📄 token-ttl-policies.json           # org token max-TTL governance
├── 📁 audit-trails/                     # audit config + windowed who-did-what pages
├── 📄 reserved-tag-keys.json            # org tag governance
├── 📁 hyok-configurations/<id>/         # HYOK + OIDC config, key versions (optional)
├── 📁 registry/                         # modules, no-code modules, providers, GPG keys
├── 📁 config-versions/
│   ├── 📄 <cv-id>.tar.gz                # deduped org-wide; runs reference by id
│   └── 📄 <cv-id>.tar.gz.remote.json    # only once evicted: the stub standing in for it
│
│   # project-scoped objects nest beneath the owning project
└── 📁 projects/<project-name>/
    ├── 📄 .identity.json                # binds this name-keyed directory to the id it archives
    ├── 📄 project.json                  # defaults + tag bindings
    ├── 📄 team-access.json              # project RBAC
    ├── 📄 effective-tag-bindings.json   # resolved bindings, including inherited
    ├── 📄 notification-configs.json     # project-scoped alerting
    ├── 📁 workspaces/<ws-name>/
    │   ├── 📄 workspace.json            # full settings + project ref
    │   ├── 📄 variables.json            # sensitive values read back blank upstream
    │   ├── 📄 *.history.ndjson          # superseded versions + tombstones, per changed object
    │   ├── 📄 readme.md                 # workspace README
    │   ├── 📄 tags.json                 # workspace tags
    │   ├── 📄 team-access.json          # workspace RBAC
    │   ├── 📄 notification-configs.json # workspace-scoped alerting
    │   ├── 📄 run-triggers.json         # inbound run triggers
    │   ├── 📄 run-tasks.json            # workspace run-task attachments
    │   ├── 📄 remote-state-consumers.json # who may read this state
    │   ├── 📁 state-versions/           # raw + JSON state blobs, per-version metadata
    │   └── 📁 runs/<run-id>/            # run summary, config version, plan/apply logs,
    │                                    # plan json, cost estimate, comments, events,
    │                                    # policy checks, task stages, TF policy outcomes
    └── 📁 stacks/<name>/                # stack config, deployment groups, runs, states
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
  index), and the immutable run children, state-version metadata, and the
  `run.json` of finished runs coalesce into NDJSON roll-ups under `rollups/`
  (`runs.ndjson` among them); an in-flight run's `run.json` stays loose until
  the run reaches a terminal state. Every object keeps its stable path as the
  key, so resume and incremental re-run are unaffected.
- **Re-runs keep every version of mutable metadata.** When a re-run finds a
  settings file changed upstream (an edited variable, a renamed workspace),
  the outgoing content is appended to a history sidecar beside the file
  before the new content replaces it: `variables.json` gains
  `variables.history.ndjson`, holding every superseded version in order plus
  a tombstone line (`"deleted": true`) if the object disappears upstream; the
  last-known file itself is never removed. Objects that never change grow no
  sidecar. Each line carries the original bytes verbatim, so
  `jq -r 'select(.content) | .content' variables.history.ndjson` reproduces
  any retained version exactly, byte for byte.

## Limitations

Some data cannot be archived at full fidelity, or at all. The archive records
what it can and marks the rest so gaps are never mistaken for missing objects.

- **Write-only secrets read back blank.** Sensitive variable, variable-set,
  and policy-set-parameter values and token secrets are write-only upstream:
  the API returns them blank, and the archive stores exactly what was
  returned. Nothing is redacted locally, so any secret the API does return (a
  notification token, say) is stored verbatim. SSH private keys never come
  back at all.
- **Raw state is sensitive and stored in cleartext.** State blobs embed
  sensitive variable, output, and resource values; the API only redacts them
  through an endpoint that returns a lossy subset. **Treat the archive as
  secret at rest.** Every file is written owner-only (0600).
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
  errored, appends what is new, and refreshes mutable metadata (retaining
  every superseded version in a history sidecar), without re-downloading
  immutable blobs.
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
  jsonapi tags (kebab-case, matching the public API docs) and flattens hydrated
  relations to ids. It is otherwise byte-faithful: nothing is redacted or
  stripped, and ephemeral signed URLs ride along as dead links.

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
