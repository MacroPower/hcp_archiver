package cli_test

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/internal/cli"
	"go.jacobcolvin.com/hcp_archiver/pkg/manifest"
	"go.jacobcolvin.com/hcp_archiver/pkg/remote"
)

const (
	pullOrgName    = "pull-org"
	pullOrgContent = `{"data":{"id":"org-1","type":"organizations","attributes":{"name":"pull-org"}}}`
	pullRollup     = "projects/p1/workspaces/w1/runs.ndjson"
)

// seedPullMirror builds a file:// mirror holding one organization's warm
// layer plus a replay log the restore must leave behind, returning the
// remote configuration section for it.
func seedPullMirror(t *testing.T) string {
	t.Helper()

	const prefix = "hcp"

	mirror := t.TempDir()
	bucket := (&url.URL{Scheme: "file", Path: mirror}).String()

	client, err := remote.New(t.Context(), remote.Config{URL: bucket, Prefix: prefix})
	require.NoError(t, err)

	defer func() {
		require.NoError(t, client.Close())
	}()

	for rel, content := range map[string]string{
		"org.json":              pullOrgContent,
		pullRollup:              "{\"id\":\"r1\"}\n",
		".ledger/snapshot.json": `{"version":2,"lastRunAt":"2026-08-24T10:00:00Z","runCount":1}`,
		".ledger/log.ndjson":    "{\"stale\":true}\n",
	} {
		require.NoError(t, client.Put(t.Context(),
			prefix+"/"+pullOrgName+"/"+rel, []byte(content)))
	}

	return "remote:\n  url: '" + bucket + "'\n  prefix: '" + prefix + "'\n"
}

func TestPullCmd_DryRunWritesNothing(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	remoteYAML := seedPullMirror(t)

	out, _, err := runCmdIn(t, root, remoteYAML, "pull", pullOrgName, "--dry-run")
	require.NoError(t, err)

	assert.Contains(t, out, "would restore")

	dirents, err := os.ReadDir(root)
	require.NoError(t, err)
	assert.Empty(t, dirents, "a dry run against a pristine root writes nothing, not even .ledger")
}

func TestPullCmd_RestoresAndConverges(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	remoteYAML := seedPullMirror(t)
	orgRoot := filepath.Join(root, pullOrgName)

	// The organization comes from the mirror's own listing: no argument
	// names it.
	out, _, err := runCmdIn(t, root, remoteYAML, "pull")
	require.NoError(t, err)
	assert.Contains(t, out, "restored")

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
	assert.True(t, marker.Partial)

	// A second run converges: nothing to restore, nothing rewritten.
	out, _, err = runCmdIn(t, root, remoteYAML, "pull", pullOrgName)
	require.NoError(t, err)
	assert.Contains(t, out, "nothing to restore")
}

func TestPullCmd_RefusesHeldLock(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	remoteYAML := seedPullMirror(t)

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
	remoteYAML := seedPullMirror(t)
	orgRoot := filepath.Join(root, pullOrgName)

	require.NoError(t, os.MkdirAll(orgRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(orgRoot, remote.MarkerName),
		[]byte(`{"url":"s3://somewhere-else","version":1}`), 0o600))

	_, errOut, err := runCmdIn(t, root, remoteYAML, "pull", pullOrgName)
	require.ErrorIs(t, err, cli.ErrPullIncomplete)
	assert.Contains(t, errOut, "records its mirror")
}
