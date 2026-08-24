package archiver

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/remote"
)

func writeTestMarker(t *testing.T, content string) string {
	t.Helper()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, remote.MarkerName), []byte(content), 0o600))

	return root
}

func TestCheckExistingMarkerRefusesRestoring(t *testing.T) {
	t.Parallel()

	root := writeTestMarker(t,
		`{"url":"s3://archive","version":2,"partial":true,"restoring":true}`)

	_, _, err := checkExistingMarker(root, remote.Config{URL: "s3://archive"}.Marker())
	require.ErrorIs(t, err, ErrRestoreInProgress,
		"an archive run against a mid-restore tree must refuse before it can rewrite the marker")
}

func TestCheckExistingMarkerCarriesPartialForward(t *testing.T) {
	t.Parallel()

	root := writeTestMarker(t, `{"url":"s3://archive","version":1,"partial":true}`)

	existing, ok, err := checkExistingMarker(root, remote.Config{URL: "s3://archive"}.Marker())
	require.NoError(t, err)
	require.True(t, ok)
	assert.True(t, existing.Partial)
}

func TestCheckExistingMarkerRefusesRelocation(t *testing.T) {
	t.Parallel()

	root := writeTestMarker(t, `{"url":"s3://old-archive","version":1}`)

	_, _, err := checkExistingMarker(root, remote.Config{URL: "s3://new-archive"}.Marker())
	require.ErrorIs(t, err, ErrRemoteRelocated)
}
