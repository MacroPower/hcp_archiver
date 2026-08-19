# hcp_archiver

Archive an HCP Terraform (formerly Terraform Cloud) organization to plain
files on disk for **long-term reference**. The archive is a predictable tree
of JSON documents and raw blobs: grep-able, diff-able, and readable years
from now with no dependency on HCP or on this tool still existing.

It captures everything the API will still return: state history, run history,
plan and apply logs, configuration versions, and the surrounding org-level
metadata. It is not a restore tool; nothing goes back into HCP Terraform.

## Install

<details>
<summary><strong>Homebrew</strong></summary>

`hcp_archiver` is published as a cask in my
[tap](https://tap.jacobcolvin.com), for macOS and Linux.

With `brew`:

```bash
brew install macropower/tap/hcp_archiver --cask
```

With your `Brewfile`:

```ruby
tap "macropower/tap"
cask "hcp_archiver"
```

</details>

<details>
<summary><strong>Nix</strong></summary>

`hcp_archiver` is published as a package in my
[NUR](https://nur.jacobcolvin.com), for macOS and Linux.

With `nix-env`:

```bash
nix-env -iA hcp_archiver -f https://nur.jacobcolvin.com/archive/main.tar.gz
```

With `nix-shell`:

```bash
nix-shell -A hcp_archiver https://nur.jacobcolvin.com/archive/main.tar.gz
```

With your `flake.nix`:

```nix
{
  inputs = {
    macropower.url = "git+https://nur.jacobcolvin.com";
  };
  # Reference the package as `inputs.macropower.packages.<system>.hcp_archiver`
}
```

With [`devbox`](https://www.jetify.com/docs/devbox/):

```bash
devbox add git+https://nur.jacobcolvin.com#hcp_archiver
```

</details>

<details>
<summary><strong>Go</strong></summary>

```bash
go install go.jacobcolvin.com/hcp_archiver/cmd/hcp_archiver@latest
```

</details>

<details>
<summary><strong>Docker</strong></summary>

Images are published to
[ghcr.io/macropower](https://git.jacobcolvin.com/hcp_archiver/pkgs/container/hcp_archiver)
and mirrored at `oci.jacobcolvin.com/hcp_archiver`, tagged `latest`, `vX`,
`vX.Y`, and `vX.Y.Z`.

The image is `scratch` plus the static binary and a root certificate bundle:
no shell, and every path it touches is one you mount. Mount the archive root
and a configuration file whose `archive.path` names it (`/archive` here). The
image has no working directory, so it runs from `/` and a file mounted at
`/.hcp_archiver.yaml` is found as the default; a different in-container path
needs `-c` or `$HCP_ARCHIVER_CONFIG`:

```bash
docker run --rm -e HCP_TOKEN \
  -v "$PWD/archive:/archive" \
  -v "$PWD/.hcp_archiver.yaml:/.hcp_archiver.yaml:ro" \
  oci.jacobcolvin.com/hcp_archiver:latest
```

</details>

<details>
<summary><strong>GitHub CLI</strong></summary>

```bash
gh release download -R MacroPower/hcp_archiver \
  -p "hcp_archiver_$(uname -s)_$(uname -m | sed s/aarch64/arm64/).tar.gz" -O - | tar -xz
```

And then move `hcp_archiver` to a directory in your `PATH`.

</details>

<details>
<summary><strong>Curl</strong></summary>

```bash
curl -s https://api.github.com/repos/MacroPower/hcp_archiver/releases/latest | \
  jq -r ".assets[] |
    select(.name | test(\"hcp_archiver_$(uname -s)_$(uname -m | sed s/aarch64/arm64/).tar.gz\")) |
    .browser_download_url" | \
  xargs curl -L | tar -xz
```

And then move `hcp_archiver` to a directory in your `PATH`.

</details>

Or, download a binary from
[releases](https://git.jacobcolvin.com/hcp_archiver/releases). Builds cover
Linux and macOS on `x86_64` and `arm64`.

<details>
<summary><strong>Verifying a download</strong></summary>

Each release's `checksums.txt` is signed with [cosign][cosign] keylessly, so
verifying the file's signature and then the checksums covers every artifact:

```bash
HCP_ARCHIVER_TAG=v0.10.1 # the tag you downloaded
gh release download -R MacroPower/hcp_archiver "$HCP_ARCHIVER_TAG" \
  -p 'checksums.txt' -p 'checksums.txt.sigstore.json'
cosign verify-blob \
  --certificate-identity "https://github.com/MacroPower/hcp_archiver/.github/workflows/release.yaml@refs/tags/$HCP_ARCHIVER_TAG" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --bundle ./checksums.txt.sigstore.json \
  ./checksums.txt
sha256sum --ignore-missing -c checksums.txt
```

The release also attests build provenance for everything `checksums.txt`
covers, which the GitHub CLI checks directly against the downloaded archive:

```bash
gh attestation verify --owner MacroPower hcp_archiver_*.tar.gz
```

Published images are signed against the same identity:

```bash
cosign verify -o text \
  --certificate-identity "https://github.com/MacroPower/hcp_archiver/.github/workflows/release.yaml@refs/tags/$HCP_ARCHIVER_TAG" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  "oci.jacobcolvin.com/hcp_archiver:$HCP_ARCHIVER_TAG"
```

[cosign]: https://github.com/sigstore/cosign

</details>

## Quick start

The API token comes from the environment (`HCP_TOKEN`, falling back to
`TFC_TOKEN` and `TFE_TOKEN`). Every command reads a YAML configuration file,
and its one required key is the archive root.

```bash
export HCP_TOKEN=...              # a user, team, or organization token

printf 'archive:\n  path: ./archive\n' > .hcp_archiver.yaml

hcp_archiver        # archive every organization the token sees
hcp_archiver view   # browse the result in the terminal
```

Run the same command again to update the archive: re-runs are incremental,
an interrupted run (Ctrl-C) continues on the next invocation, and runs under
differently scoped tokens against the same directory accumulate the union of
what each can read. `hcp_archiver --help` explains the resume model and the
statuses in the end-of-run summary; the short version is that a non-zero
`errored` count is the one to investigate.

## Configuration

What and where to archive lives in a required YAML file:
`./.hcp_archiver.yaml` in the working directory, or the path `--config`
(`-c`) or `$HCP_ARCHIVER_CONFIG` names. `archive.path` is its one required
key; with no organization filters, every visible organization is archived
with the default surfaces.

```yaml
# yaml-language-server: $schema=https://jacobcolvin.com/hcp_archiver/config.schema.json

archive:
  path: ./archive # required: the directory every command reads or writes
organizations: # omit to archive every visible organization
  - my-org
runHistory: # bound the run history a run fetches; unlimited by default
  fetch:
    count: 500
include: # heavy or org-specific surfaces, each off by default
  stacks: true
  auditTrail: true
remote: # mirror the archive to object storage; omit to stay local
  url: s3://my-archive-bucket?region=us-east-1
```

Every other key is optional, and the `yaml-language-server` line gives editors
completion and validation from the schema embedded in the binary, so each
key's full contract is a hover away. The same documentation lives in the
commented [`hcp_archiver.example.yaml`](hcp_archiver.example.yaml) at the
repository root, and the schema is published at
<https://jacobcolvin.com/hcp_archiver/config.schema.json> and
attached to each GitHub release.

## Reading the archive

Five subcommands read an archive; none needs an HCP token, each reads the
same configuration file for the archive directory, and each documents its
full contract in `--help`:

```bash
hcp_archiver view                    # interactive terminal UI
hcp_archiver list my-org/projects    # one line per object
hcp_archiver show my-org/org.json    # exact bytes to stdout
hcp_archiver extract my-org          # back to plain files, into extract.path
hcp_archiver export                  # markdown tree for mkdocs, into export.path
```

`view` mirrors the HCP interface: organizations open into projects,
workspaces, runs, and state versions, with any archived document readable in
a scrolling viewer (`enter` descends, `esc` returns, `/` filters, `q` quits).

`list`, `show`, and `extract` are the scriptable equivalents: objects are
addressed by org-prefixed archive paths (`<org>/<path>`), `--json` switches
to machine-readable output, and `extract --dry-run` predicts a run without
writing.

`export` renders the archive's metadata as markdown that a static site
generator with directory-based navigation (mkdocs and its kin) builds
without configuration. Pages are curated for sharing: sensitive values are
withheld, and content that can embed secrets (state, logs) is represented by
name, size, and timestamp only. Page templates can be overridden per file
through the configuration's `export.templates.path` key.

All of these read the archive's physical forms transparently: a freshly
collected tree and one whose cold artifacts have been sealed into bundles or
evicted to a mirror answer identically.

## Mirroring to object storage

With a `remote:` block configured, the bucket converges on a complete copy
of the archive. The heavy, write-once artifacts (sealed per-workspace
bundles, settled configuration tarballs) are **evicted**: uploaded,
verified, then removed locally, so local disk stays bounded to the grep-able
search layer. Everything else **syncs** incrementally at each run's close,
with the local copy kept as canonical and stale remote copies pruned.

Credentials never appear in the file: the URL's scheme selects the backend
(`s3://` including MinIO/R2/Ceph, `azblob://`, `file://`), and each
authenticates through its provider's default chain. Enable bucket versioning
as a backstop, and do not lifecycle mirrored objects into a non-readable
archival tier (S3 Glacier, Azure Archive); there is no restore workflow.

The read commands work against the mirror too: pointed at an empty directory
by a configuration whose `remote` section names the mirror, they bootstrap a
local tree from the bucket and fetch objects on demand, so restoring is
either one bulk download of the org prefix or just browsing in place. The
full semantics, verification layers, and failure modes are in
[DESIGN.md](DESIGN.md).

## Output layout

Each organization archives into its own subtree of plain files: org-level
metadata at the root, and everything a project owns nested beneath it.

```
archive/<org>/
├── org.json                        # organization metadata
├── teams/, users/, memberships.json
├── variable-sets/, policy-sets/, policies/
├── oauth-clients/, agent-pools/, registry/, ...
├── config-versions/<cv-id>.tar.gz  # deduped org-wide; runs reference by id
└── projects/<project-name>/
    ├── project.json, team-access.json, ...
    ├── workspaces/<ws-name>/
    │   ├── workspace.json, variables.json, tags.json, ...
    │   ├── *.history.ndjson        # every superseded version of mutable files
    │   ├── state-versions/         # raw + JSON state, per-version metadata
    │   └── runs/<run-id>/          # summary, plan/apply logs, plan JSON, ...
    └── stacks/<name>/
```

The tree above is the logical namespace, and every object keeps its stable
path whatever physical form holds it: as a workspace's history freezes, its
heavy artifacts seal into per-workspace zip bundles and its small immutable
metadata coalesces into NDJSON roll-ups, which is what keeps a large archive
listable and grep-able. When a re-run finds a settings file changed
upstream, the outgoing content is appended to a `*.history.ndjson` sidecar
beside it, so every version is retained. The fully annotated layout and the
reasoning behind it are in [DESIGN.md](DESIGN.md).

## Security and limitations

- **Treat the archive as secret at rest.** Raw state embeds sensitive
  values in cleartext, and anything the API does return (a notification
  token, say) is stored verbatim; nothing is redacted locally. Every file is
  written owner-only (0600). The `export` output is the one curated,
  shareable surface.
- **Write-only secrets read back blank.** Sensitive variable values and
  token secrets are write-only upstream; the archive stores the blank the
  API returned.
- **Best-effort where the API is.** Plan/apply logs and configuration
  tarballs expire on the platform's retention schedule, plan JSON exists
  only for recent Terraform versions, and the audit trail needs an elevated
  token; the archive grabs whatever is still downloadable and records the
  rest as absent. A long run against a live organization is a rolling
  snapshot, not a point-in-time one.

The full inventory of what can and cannot be captured is in
[DESIGN.md](DESIGN.md).

## Development

```bash
devbox install      # provision the toolchain (or run `direnv allow`)
task check          # local gate: lint + test
task check:all      # everything CI runs (adds the Dagger-backed gates)
```

This is a self-contained Go module so the `go-tfe` dependency tree stays
isolated. CI runs through a local Dagger toolchain (`dagger call ci <task>`),
composing shared toolchains from [go.jacobcolvin.com/x][x].

[x]: https://git.jacobcolvin.com/x
