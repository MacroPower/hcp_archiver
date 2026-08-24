package cli_test

import (
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/internal/cli"
	"go.jacobcolvin.com/hcp_archiver/pkg/config"
	"go.jacobcolvin.com/hcp_archiver/pkg/manifest"
	"go.jacobcolvin.com/hcp_archiver/pkg/remote"
)

const (
	pullOrgName     = "pull-org"
	pullOrgContent  = `{"data":{"id":"org-1","type":"organizations","attributes":{"name":"pull-org"}}}`
	pullRollup      = "projects/p1/workspaces/w1/runs.ndjson"
	pullTarball     = "config-versions/cv-9.tar.gz"
	pullTarballStub = pullTarball + ".remote.json"
)

// seedPullMirror builds a file:// mirror holding one organization's warm
// layer plus an evicted tarball, seeding the extra keys too (the junk seed
// adds a replay log the restore must leave behind and account as a
// leftover), returning the remote configuration section for it.
func seedPullMirror(t *testing.T, extra map[string]string) string {
	t.Helper()

	const prefix = "hcp"

	mirror := t.TempDir()
	bucket := (&url.URL{Scheme: "file", Path: mirror}).String()

	client, err := remote.New(t.Context(), remote.Config{URL: bucket, Prefix: prefix})
	require.NoError(t, err)

	defer func() {
		require.NoError(t, client.Close())
	}()

	seed := map[string]string{
		"org.json":              pullOrgContent,
		pullRollup:              "{\"id\":\"r1\"}\n",
		".ledger/snapshot.json": `{"version":2,"lastRunAt":"2026-08-24T10:00:00Z","runCount":1}`,
		pullTarball:             "tarball bytes",
	}
	maps.Copy(seed, extra)

	for rel, content := range seed {
		require.NoError(t, client.Put(t.Context(),
			prefix+"/"+pullOrgName+"/"+rel, []byte(content)))
	}

	return "remote:\n  url: '" + bucket + "'\n  prefix: '" + prefix + "'\n"
}

// junkSeed is the extra mirror key that keeps a restored tree's marker
// partial: a replay log a healthy sweep would never upload.
func junkSeed() map[string]string {
	return map[string]string{".ledger/log.ndjson": "{\"stale\":true}\n"}
}

func TestPullCmd_DryRunWritesNothing(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	remoteYAML := seedPullMirror(t, junkSeed())

	out, _, err := runCmdIn(t, root, remoteYAML, "pull", pullOrgName, "--dry-run")
	require.NoError(t, err)

	assert.Contains(t, out, "would restore")
	assert.Contains(t, out, "would settle the marker partial")

	dirents, err := os.ReadDir(root)
	require.NoError(t, err)
	assert.Empty(t, dirents, "a dry run against a pristine root writes nothing, not even .ledger")
}

func TestPullCmd_RestoresAndConverges(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	remoteYAML := seedPullMirror(t, junkSeed())
	orgRoot := filepath.Join(root, pullOrgName)

	// The organization comes from the mirror's own listing: no argument
	// names it.
	out, errOut, err := runCmdIn(t, root, remoteYAML, "pull")
	require.NoError(t, err)
	assert.Contains(t, out, "restored")
	assert.Contains(t, out, "marker partial")
	assert.Contains(t, errOut, ".ledger/log.ndjson",
		"the unaccounted replay log is named on stderr")

	data, err := os.ReadFile(filepath.Join(orgRoot, "org.json"))
	require.NoError(t, err)
	assert.JSONEq(t, pullOrgContent, string(data))

	assert.FileExists(t, filepath.Join(orgRoot, filepath.FromSlash(pullRollup)))
	assert.FileExists(t, filepath.Join(orgRoot, ".ledger", "snapshot.json"))
	assert.NoFileExists(t, filepath.Join(orgRoot, ".ledger", "log.ndjson"),
		"the mirrored replay log must never be restored")

	marker, ok, err := remote.ReadMarker(orgRoot)
	require.NoError(t, err)
	require.True(t, ok)
	assert.False(t, marker.Restoring)
	assert.True(t, marker.Partial,
		"the mirrored replay log is unaccounted for, so the marker stays partial")

	// A second run converges: nothing to restore, nothing rewritten.
	out, _, err = runCmdIn(t, root, remoteYAML, "pull", pullOrgName)
	require.NoError(t, err)
	assert.Contains(t, out, "nothing to restore")
}

func TestPullCmd_PromotesCleanMirror(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	remoteYAML := seedPullMirror(t, nil)
	orgRoot := filepath.Join(root, pullOrgName)

	out, _, err := runCmdIn(t, root, remoteYAML, "pull", pullOrgName)
	require.NoError(t, err)
	assert.Contains(t, out, "marker complete")

	assert.FileExists(t, filepath.Join(orgRoot, filepath.FromSlash(pullTarballStub)),
		"the evicted tarball's stub is backfilled from the mirror's metadata")

	marker, ok, err := remote.ReadMarker(orgRoot)
	require.NoError(t, err)
	require.True(t, ok)
	assert.False(t, marker.Partial)

	markerBytes, err := os.ReadFile(filepath.Join(orgRoot, remote.MarkerName))
	require.NoError(t, err)

	out, _, err = runCmdIn(t, root, remoteYAML, "pull", pullOrgName)
	require.NoError(t, err)
	assert.Contains(t, out, "nothing to restore")
	assert.Contains(t, out, "marker complete")

	after, err := os.ReadFile(filepath.Join(orgRoot, remote.MarkerName))
	require.NoError(t, err)
	assert.Equal(t, markerBytes, after, "a converged re-run leaves the marker bytes untouched")
}

func TestPullCmd_RejectsBadProgressMode(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	remoteYAML := seedPullMirror(t, nil)

	_, _, err := runCmdIn(t, root, remoteYAML, "pull", pullOrgName, "--progress", "bogus")
	require.ErrorIs(t, err, config.ErrInvalidProgressMode)

	dirents, err := os.ReadDir(root)
	require.NoError(t, err)
	assert.Empty(t, dirents, "a rejected flag fails before any I/O touches the root")
}

func TestPullCmd_RefusesHeldLock(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	remoteYAML := seedPullMirror(t, junkSeed())

	lock, err := manifest.LockArchive(filepath.Join(root, pullOrgName))
	require.NoError(t, err)

	defer func() {
		require.NoError(t, lock.Close())
	}()

	_, errOut, err := runCmdIn(t, root, remoteYAML, "pull", pullOrgName)
	require.ErrorIs(t, err, cli.ErrPullIncomplete)
	assert.Contains(t, errOut, "locked")
}

func TestPullCmd_RefusesMismatchedMarker(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	remoteYAML := seedPullMirror(t, junkSeed())
	orgRoot := filepath.Join(root, pullOrgName)

	require.NoError(t, os.MkdirAll(orgRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(orgRoot, remote.MarkerName),
		[]byte(`{"url":"s3://somewhere-else","version":1}`), 0o600))

	_, errOut, err := runCmdIn(t, root, remoteYAML, "pull", pullOrgName)
	require.ErrorIs(t, err, cli.ErrPullIncomplete)
	assert.Contains(t, errOut, "records its mirror")
}
