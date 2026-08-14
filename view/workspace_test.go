package view_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/view"
)

// appendSidecarLine appends one raw NDJSON line to a fixture bundle's sidecar
// index.
func appendSidecarLine(t *testing.T, root, bundle, line string) {
	t.Helper()

	abs := filepath.Join(root, "my-org", filepath.FromSlash(wsDir), "bundles", bundle+".sidecar.ndjson")

	f, err := os.OpenFile(abs, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)

	_, err = f.WriteString(line + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

func TestWorkspaceIndexSkipsRollupLineNamingNoPath(t *testing.T) {
	t.Parallel()

	// A roll-up record that parses but carries no path used to index a sealed
	// object at the empty path, which sits under every prefix: the whole-org
	// listing grew an entry naming nothing and every unseal reported it as a
	// file it could not write.
	tests := map[string]struct {
		line string
	}{
		"null record":        {line: `null`},
		"empty object":       {line: `{}`},
		"empty path":         {line: `{"path":"","sha256":"","content":"orphan"}`},
		"path-less content":  {line: `{"content":"orphan"}`},
		"unparseable record": {line: `{"path":`},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			root := buildArchive(t)
			appendRollupLine(t, root, tt.line)

			org := openOrg(t, root)

			entries, err := org.List("")
			require.NoError(t, err)

			for _, e := range entries {
				assert.NotEmpty(t, e.Path, "a record naming no path is not indexed")
			}

			// The intact record beside it still resolves: the skip is per line.
			assert.Contains(t, entryPaths(entries), cvPath)

			data, err := org.Workspace("default", "app").Open(cvPath)
			require.NoError(t, err)
			assert.Equal(t, cvContent, string(data)) //nolint:testifylint // Byte-exact content, not JSON-semantic.

			target := t.TempDir()

			sum, err := view.NewArchive([]*view.Org{org}).Unseal(t.Context(), target, "my-org", nil)
			require.NoError(t, err)
			assert.Zero(t, sum.Errored, "an unseal has no phantom file to fail on")
		})
	}
}

func TestWorkspaceIndexSkipsSidecarLineNamingNoMember(t *testing.T) {
	t.Parallel()

	// The sidecar's counterpart: an empty name indexes a member at the empty
	// path, and an empty bundle points a member at the bundles directory rather
	// than at a zip, so a read of it opens a directory as an archive.
	tests := map[string]struct {
		line    string
		phantom string
	}{
		"null record":  {line: `null`},
		"empty object": {line: `{}`},
		"empty name":   {line: `{"name":"","bundle":"logs.gen0001.zip","size":0}`},
		"empty bundle": {
			line:    `{"name":"` + wsDir + `/runs/run-new/orphan.log","bundle":"","size":3}`,
			phantom: wsDir + "/runs/run-new/orphan.log",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			root := buildArchive(t)
			appendSidecarLine(t, root, "logs.gen0001.zip", tt.line)

			org := openOrg(t, root)

			entries, err := org.List("")
			require.NoError(t, err)

			for _, e := range entries {
				assert.NotEqual(t, tt.phantom, e.Path, "a record naming no member or no bundle is not indexed")
			}

			// The bundle's intact member still resolves.
			assert.Contains(t, entryPaths(entries), planPath)

			data, err := org.Workspace("default", "app").Open(planPath)
			require.NoError(t, err)
			assert.Equal(t, planContent, string(data))

			target := t.TempDir()

			sum, err := view.NewArchive([]*view.Org{org}).Unseal(t.Context(), target, "my-org", nil)
			require.NoError(t, err)
			assert.Zero(t, sum.Errored, "an unseal has no phantom member to fail on")
		})
	}
}
