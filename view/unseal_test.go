package view_test

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/remote"
	"go.jacobcolvin.com/hcp_archiver/remote/remotetest"
	"go.jacobcolvin.com/hcp_archiver/seal"
	"go.jacobcolvin.com/hcp_archiver/store"
	"go.jacobcolvin.com/hcp_archiver/view"
)

// buildUnsealArchive extends the standard fixture with the pieces an unseal
// must skip (a ledger shard, an atomic writer's staging leftover, and the
// identity sidecar the collector stamps into every name-keyed directory) plus
// a stack subtree, which only the project scope covers.
func buildUnsealArchive(t *testing.T) string {
	t.Helper()

	root := buildArchive(t)
	org := filepath.Join(root, "my-org")

	writeFile(t, org, wsDir+"/runs/.ledger/frozen.json", `{"shard":true}`)
	writeFile(t, org, wsDir+"/runs/run-old/.atomicfile-123.tmp", "torn write")
	writeFile(t, org, "projects/default/stacks/net/stack.json",
		`{"data":{"id":"st-1","type":"stacks","attributes":{"name":"net"}}}`)

	for _, dir := range []string{"projects/default", wsDir, "projects/default/stacks/net"} {
		writeFile(t, org, dir+"/"+store.IdentityFileName,
			`{"firstSeen":"2024-01-01T00:00:00Z","id":"obj-1"}`)
	}

	return root
}

// openOrg opens the archive at root and returns its single organization.
func openOrg(t *testing.T, root string, opts ...view.ArchiveOption) *view.Org {
	t.Helper()

	orgs, err := view.OpenArchive(root, opts...)
	require.NoError(t, err)
	require.Len(t, orgs, 1)

	return orgs[0]
}

// unsealWorkspace plans and runs the fixture workspace's unseal into target,
// returning the per-file events and summary.
func unsealWorkspace(t *testing.T, org *view.Org, target string) ([]view.UnsealEvent, view.UnsealSummary) {
	t.Helper()

	jobs, err := view.PlanWorkspaceUnsealForTest(org, org.Workspace("default", "app"))
	require.NoError(t, err)

	return view.RunUnsealForTest(t.Context(), org, target, jobs)
}

// readTarget reads the file at an archive-relative path under the unsealed
// target tree.
func readTarget(t *testing.T, target, rel string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(target, "my-org", filepath.FromSlash(rel)))
	require.NoError(t, err)

	return string(data)
}

// targetPaths walks the target tree and returns every file's slash-separated
// path relative to target.
func targetPaths(t *testing.T, target string) []string {
	t.Helper()

	var paths []string

	err := filepath.WalkDir(target, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		rel, relErr := filepath.Rel(target, p)
		if relErr != nil {
			return fmt.Errorf("relativize %q: %w", p, relErr)
		}

		paths = append(paths, filepath.ToSlash(rel))

		return nil
	})
	require.NoError(t, err)

	return paths
}

// appendRollupLine appends one raw NDJSON line to a fixture roll-up file.
func appendRollupLine(t *testing.T, root, rollup, line string) {
	t.Helper()

	abs := filepath.Join(root, "my-org", filepath.FromSlash(wsDir), "rollups", rollup)

	f, err := os.OpenFile(abs, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)

	_, err = f.WriteString(line + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

func TestUnseal_WorkspaceLocalForms(t *testing.T) {
	t.Parallel()

	root := buildUnsealArchive(t)
	org := openOrg(t, root)
	target := t.TempDir()

	events, sum := unsealWorkspace(t, org, target)

	for _, ev := range events {
		require.NoError(t, ev.Err, "no per-file failure expected for %s", ev.Path)
	}

	assert.Zero(t, sum.Errored)
	assert.Positive(t, sum.Bytes)

	// Every physical form expands back to a plain file with exact bytes; the
	// comparisons are deliberately byte-for-byte, not JSON-semantic.
	assert.Equal(t, "plan output line\n", readTarget(t, target, wsDir+"/runs/run-new/plan.log"),
		"a bundled member is extracted")
	//nolint:testifylint // Byte-exact reproduction is the point of an unseal.
	assert.Equal(t,
		`{"data":{"id":"cv-1","type":"configuration-versions","attributes":{"source":"tfe-api"}}}`,
		readTarget(t, target, wsDir+"/runs/run-new/config-version.json"),
		"a rolled-up member is expanded")
	//nolint:testifylint // Byte-exact reproduction is the point of an unseal.
	assert.Equal(t, `{"data":[]}`, readTarget(t, target, wsDir+"/runs/run-old/comments.json"),
		"a loose file is copied")
	//nolint:testifylint // Byte-exact reproduction is the point of an unseal.
	assert.Equal(t, `{"serial":2}`, readTarget(t, target, wsDir+"/state-versions/20240101T000000Z-sv-1.tfstate.json"),
		"a bundled state blob is extracted")

	// The sealed forms and the archive's bookkeeping never reach the target.
	for _, p := range targetPaths(t, target) {
		assert.NotContains(t, p, "/rollups/", "roll-up files are omitted: %s", p)
		assert.NotContains(t, p, "/bundles/", "bundle zips and sidecars are omitted: %s", p)
		assert.NotContains(t, p, ".sidecar", "sidecar indexes are omitted: %s", p)
		assert.NotContains(t, p, ".ledger", "ledger shards are omitted: %s", p)
		assert.NotContains(t, p, ".remote.json", "the remote marker is omitted: %s", p)
		assert.NotContains(t, p, ".identity.json", "identity sidecars are omitted: %s", p)
		assert.NotContains(t, p, ".atomicfile-", "staging leftovers are omitted: %s", p)
		assert.True(t, strings.HasPrefix(p, "my-org/"+wsDir+"/"),
			"a workspace unseal writes only under the workspace's mirrored path: %s", p)
	}
}

func TestUnseal_EvictedTarballWithoutMirrorIsCountedNotSkipped(t *testing.T) {
	t.Parallel()

	// An organization that records no mirror has nowhere to fetch an evicted
	// tarball from, so an unseal cannot recover it. It must say so: an object
	// missing from the plan would let the run report a clean recovery of an
	// archive it had not fully recovered.
	root := buildArchive(t)
	evictTarball(t, root, "cv-9")

	org := openOrg(t, root)
	target := t.TempDir()

	sum, err := view.NewArchive([]*view.Org{org}).Unseal(t.Context(), target, "my-org", nil)
	require.NoError(t, err)

	assert.Equal(t, 1, sum.Errored, "the object the run could not recover is counted")
	assert.Positive(t, sum.Files, "the rest of the archive still recovered")

	for _, p := range targetPaths(t, target) {
		assert.NotContains(t, p, store.ConfigVersionsDirName,
			"nothing is written for an object whose bytes are elsewhere: %s", p)
	}
}

// unsealOrg runs a whole-organization unseal into target, returning the summary
// and the per-file byte counts by archive-relative path.
func unsealOrg(t *testing.T, ctx context.Context, org *view.Org, target string,
) (view.UnsealSummary, map[string]int64, error) {
	t.Helper()

	bytesByPath := make(map[string]int64)

	sum, err := view.NewArchive([]*view.Org{org}).Unseal(ctx, target, "my-org",
		func(archivePath string, n int64, _ error) {
			bytesByPath[strings.TrimPrefix(archivePath, "my-org/")] = n
		})

	return sum, bytesByPath, err //nolint:wrapcheck // A test shim; the caller asserts on the error.
}

func TestUnseal_EvictedTarballFetchedFromMirror(t *testing.T) {
	t.Parallel()

	// The mirror is where the bytes went, and the organization records where
	// the mirror is, so the unseal fetches them back rather than declaring the
	// object lost.
	root := buildArchive(t)
	fake := remotetest.New()

	writeFile(t, filepath.Join(root, "my-org"), ".remote.json", viewMarker)

	rel := evictTarballRemote(t, root, "cv-9", fake, tarballContent)

	org := openRemoteOrg(t, root, fake)
	target := t.TempDir()

	sum, bytesByPath, err := unsealOrg(t, t.Context(), org, target)
	require.NoError(t, err)

	assert.Zero(t, sum.Errored)
	assert.Equal(t, tarballContent, readTarget(t, target, rel), "the fetched object is byte-exact")
	assert.EqualValues(t, len(tarballContent), bytesByPath[rel],
		"the run counts what it wrote, which is the size the stub recorded")
}

func TestUnseal_EvictedTarballDigestMismatchWritesNothing(t *testing.T) {
	t.Parallel()

	// Same length, different bytes: only the digest the eviction recorded
	// catches it. Staging the fetch is what keeps the mismatch from landing as
	// a plausible-looking file.
	root := buildArchive(t)
	fake := remotetest.New()

	writeFile(t, filepath.Join(root, "my-org"), ".remote.json", viewMarker)

	corrupt := strings.Repeat("x", len(tarballContent))
	rel := evictTarballRemote(t, root, "cv-9", fake, corrupt)

	org := openRemoteOrg(t, root, fake)
	target := t.TempDir()

	var failure error

	sum, err := view.NewArchive([]*view.Org{org}).Unseal(t.Context(), target, "my-org",
		func(archivePath string, _ int64, progErr error) {
			if archivePath == "my-org/"+rel {
				failure = progErr
			}
		})
	require.NoError(t, err)

	assert.Equal(t, 1, sum.Errored)
	require.ErrorContains(t, failure, "does not match the digest its eviction recorded")
	assert.NoFileExists(t, filepath.Join(target, "my-org", filepath.FromSlash(rel)),
		"a fetch that fails its digest leaves no file")
	assert.Positive(t, sum.Files, "the rest of the archive still recovered")
}

func TestUnseal_EvictedTarballAbsentFromMirror(t *testing.T) {
	t.Parallel()

	// The stub says the object went to the mirror and the mirror does not have
	// it. That is one file's failure, named, with the run carrying on.
	root := buildArchive(t)
	fake := remotetest.New()

	writeFile(t, filepath.Join(root, "my-org"), ".remote.json", viewMarker)

	rel := evictTarball(t, root, "cv-9")

	org := openRemoteOrg(t, root, fake)
	target := t.TempDir()

	var failure error

	sum, err := view.NewArchive([]*view.Org{org}).Unseal(t.Context(), target, "my-org",
		func(archivePath string, _ int64, progErr error) {
			if archivePath == "my-org/"+rel {
				failure = progErr
			}
		})
	require.NoError(t, err)

	assert.Equal(t, 1, sum.Errored)
	require.ErrorIs(t, failure, remote.ErrNotFound)
	assert.Contains(t, failure.Error(), viewPrefix+"/my-org/"+rel, "the message names the key it looked for")
	assert.NoFileExists(t, filepath.Join(target, "my-org", filepath.FromSlash(rel)))

	// The staging write creates the parents before it asks for the first byte,
	// so a file that never arrives leaves its directory behind. That is the
	// price of discarding a bad fetch instead of writing it, and it is recorded
	// here rather than left to be discovered.
	assert.DirExists(t, filepath.Join(target, "my-org", store.ConfigVersionsDirName))
}

func TestUnseal_EvictedTarballCancellationWritesNothing(t *testing.T) {
	t.Parallel()

	root := buildArchive(t)
	fake := remotetest.New()

	writeFile(t, filepath.Join(root, "my-org"), ".remote.json", viewMarker)

	rel := evictTarballRemote(t, root, "cv-9", fake, tarballContent)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// A SIGINT landing inside the fetch. It ends the run rather than counting
	// the file it interrupted as one that failed.
	fake.RangeHook = func(context.Context) { cancel() }

	org := openRemoteOrg(t, root, fake)
	target := t.TempDir()

	sum, _, err := unsealOrg(t, ctx, org, target)
	require.ErrorIs(t, err, context.Canceled)

	assert.Zero(t, sum.Errored, "an interrupted file is not a failed one")
	assert.NoFileExists(t, filepath.Join(target, "my-org", filepath.FromSlash(rel)))
}

func TestUnseal_WorkspaceRemote(t *testing.T) {
	t.Parallel()

	root, fake := buildRemoteArchive(t)

	org := openRemoteOrg(t, root, fake)

	target := t.TempDir()
	_, sum := unsealWorkspace(t, org, target)

	assert.Zero(t, sum.Errored)
	assert.Equal(t, "plan output line\n", readTarget(t, target, wsDir+"/runs/run-new/plan.log"),
		"an evicted bundle member materializes from the remote store")
	//nolint:testifylint // Byte-exact reproduction is the point of an unseal.
	assert.Equal(t, `{"serial":2}`, readTarget(t, target, wsDir+"/state-versions/20240101T000000Z-sv-1.tfstate.json"))

	ranges := fake.Ranges()
	require.NotEmpty(t, ranges)

	for _, r := range ranges {
		assert.GreaterOrEqual(t, r.Length, int64(0),
			"every remote read must be a bounded ranged request, never a full download")
	}
}

func TestUnseal_Project(t *testing.T) {
	t.Parallel()

	root := buildUnsealArchive(t)
	org := openOrg(t, root)

	jobs, err := view.PlanProjectUnsealForTest(org, "default")
	require.NoError(t, err)

	target := t.TempDir()
	_, sum := view.RunUnsealForTest(t.Context(), org, target, jobs)

	assert.Zero(t, sum.Errored)

	//nolint:testifylint // Byte-exact reproduction is the point of an unseal.
	assert.Equal(t, `{"data":{"id":"prj-1","type":"projects","attributes":{"name":"default"}}}`,
		readTarget(t, target, "projects/default/project.json"), "the project document is included")
	//nolint:testifylint // Byte-exact reproduction is the point of an unseal.
	assert.Equal(t, `{"data":{"id":"st-1","type":"stacks","attributes":{"name":"net"}}}`,
		readTarget(t, target, "projects/default/stacks/net/stack.json"), "the stacks subtree is included")
	assert.Equal(t, "plan output line\n", readTarget(t, target, wsDir+"/runs/run-new/plan.log"),
		"every workspace's sealed members are included")
	//nolint:testifylint // Byte-exact reproduction is the point of an unseal.
	assert.Equal(t, `{"data":[]}`, readTarget(t, target, wsDir+"/runs/run-old/comments.json"))
}

func TestUnseal_OverwriteIsIdempotent(t *testing.T) {
	t.Parallel()

	root := buildUnsealArchive(t)
	org := openOrg(t, root)
	target := t.TempDir()

	_, first := unsealWorkspace(t, org, target)
	require.Zero(t, first.Errored)

	_, second := unsealWorkspace(t, org, target)
	require.Zero(t, second.Errored, "re-running over an existing target overwrites cleanly")

	assert.Equal(t, first.Files, second.Files)
	assert.Equal(t, first.Bytes, second.Bytes)
	assert.Equal(t, "plan output line\n", readTarget(t, target, wsDir+"/runs/run-new/plan.log"))
}

func TestUnseal_PathTraversalRejected(t *testing.T) {
	t.Parallel()

	root := buildUnsealArchive(t)

	// A crafted roll-up name that would climb out of the target if joined
	// naively. It carries the workspace-dir prefix, so the sealed-index prefix
	// filter cannot drop it: only writeUnsealed's validation stands between it
	// and the join.
	escape := wsDir + "/../../../../../../escape"
	appendRollupLine(t, root, "config-versions.ndjson", `{"path":"`+escape+`","content":"boom"}`)

	org := openOrg(t, root)

	// Give the target a parent all its own, so an escape would land somewhere
	// this test can see.
	parent := t.TempDir()
	target := filepath.Join(parent, "out")

	events, sum := unsealWorkspace(t, org, target)

	assert.Equal(t, 1, sum.Errored)

	var found bool

	for _, ev := range events {
		if ev.Path == escape {
			found = true

			require.ErrorIs(t, ev.Err, seal.ErrMemberName)
		}
	}

	require.True(t, found, "the crafted name surfaces as a per-file event")

	assert.NoFileExists(t, filepath.Join(parent, "escape"))
	assert.NoFileExists(t, filepath.Join(target, "escape"))
	assert.NoFileExists(t, filepath.Join(target, "my-org", "escape"))
}

func TestUnseal_NewestRollupLineWins(t *testing.T) {
	t.Parallel()

	root := buildUnsealArchive(t)
	rel := wsDir + "/runs/run-new/config-version.json"

	// A member re-frozen after its content changed appends a newer line under
	// the same path; the unseal must reproduce the newest. The line carries no
	// digest, matching a hand-built fixture, so it is served as-is.
	appendRollupLine(t, root, "config-versions.ndjson", `{"path":"`+rel+`","content":"updated content"}`)

	org := openOrg(t, root)
	target := t.TempDir()

	_, sum := unsealWorkspace(t, org, target)

	assert.Zero(t, sum.Errored)
	assert.Equal(t, "updated content", readTarget(t, target, rel))
}

func TestUnseal_LooseFileWinsOverSealed(t *testing.T) {
	t.Parallel()

	root := buildUnsealArchive(t)
	rel := wsDir + "/runs/run-new/config-version.json"

	// A loose survivor of an interrupted seal is the canonical copy, matching
	// Workspace.Open's precedence.
	writeFile(t, filepath.Join(root, "my-org"), rel, "loose survivor")

	org := openOrg(t, root)
	target := t.TempDir()

	_, sum := unsealWorkspace(t, org, target)

	assert.Zero(t, sum.Errored)
	assert.Equal(t, "loose survivor", readTarget(t, target, rel))
}

func TestUnseal_MissingLocalBundleWithoutRemote(t *testing.T) {
	t.Parallel()

	root := buildUnsealArchive(t)
	require.NoError(t, os.Remove(
		filepath.Join(root, "my-org", filepath.FromSlash(wsDir), "bundles", "logs.gen0001.zip")))

	org := openOrg(t, root)
	target := t.TempDir()

	events, sum := unsealWorkspace(t, org, target)

	assert.Equal(t, 1, sum.Errored, "only the evicted-looking member errors")

	var found bool

	for _, ev := range events {
		if ev.Path == wsDir+"/runs/run-new/plan.log" {
			found = true

			require.ErrorIs(t, ev.Err, view.ErrObjectNotFound)
		}
	}

	require.True(t, found)

	// The run continued past the failure: the rest of the workspace is intact.
	//nolint:testifylint // Byte-exact reproduction is the point of an unseal.
	assert.Equal(t, `{"data":[]}`, readTarget(t, target, wsDir+"/runs/run-old/comments.json"))
	//nolint:testifylint // Byte-exact reproduction is the point of an unseal.
	assert.Equal(t, `{"serial":2}`, readTarget(t, target, wsDir+"/state-versions/20240101T000000Z-sv-1.tfstate.json"))
}

func TestUnseal_RollupChecksumMismatch(t *testing.T) {
	t.Parallel()

	root := buildUnsealArchive(t)
	rel := wsDir + "/runs/run-new/config-version.json"

	// A newest line whose content no longer hashes to its recorded digest
	// models rot inside the roll-up; serving it silently would hand back
	// corrupt bytes as if archived.
	appendRollupLine(t, root, "config-versions.ndjson",
		`{"path":"`+rel+`","sha256":"`+strings.Repeat("0", 64)+`","content":"rotten"}`)

	org := openOrg(t, root)
	target := t.TempDir()

	events, sum := unsealWorkspace(t, org, target)

	assert.Equal(t, 1, sum.Errored)

	var found bool

	for _, ev := range events {
		if ev.Path == rel {
			found = true

			require.ErrorIs(t, ev.Err, view.ErrRollupChecksum)
		}
	}

	require.True(t, found)
}

func TestWriteUnsealed_RejectsUnsafeNames(t *testing.T) {
	t.Parallel()

	target := t.TempDir()

	err := view.WriteUnsealedForTest(t.Context(), target, "my-org", "../escape", []byte("x"))
	require.ErrorIs(t, err, seal.ErrMemberName)

	err = view.WriteUnsealedForTest(t.Context(), target, "my-org", "ok/file.json", []byte("x"))
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(target, "my-org", "ok", "file.json"))
}
