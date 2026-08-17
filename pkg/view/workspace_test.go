package view_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/remote/remotetest"
	"go.jacobcolvin.com/hcp_archiver/pkg/store"
	"go.jacobcolvin.com/hcp_archiver/pkg/view"
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

// mirrorTarball puts a configuration-version tarball in the fake's inventory
// with no local counterpart, the evicted surface a bare fixture has none of.
// It returns the tarball's archive-relative path.
func mirrorTarball(t *testing.T, fake *remotetest.Fake, cvID string) string {
	t.Helper()

	rel := store.ConfigVersionsDirName + "/" + cvID + ".tar.gz"
	sum := sha256.Sum256([]byte(tarballContent))

	fake.SetObject(viewPrefix+"/my-org/"+rel, remotetest.Object{
		Data:     []byte(tarballContent),
		Metadata: map[string]string{"sha256": hex.EncodeToString(sum[:])},
	})

	return rel
}

func TestWorkspaceExistsAgreesWithOpen(t *testing.T) {
	t.Parallel()

	// A probe on a bootstrap tree answers from the merged inventory, which
	// carries the evicted surfaces beside the warm layer. Crediting one showed
	// an object present that every read then reported absent.
	tests := map[string]struct {
		rel  string
		want bool
	}{
		"mirror-only loose json":     {rel: wsDir + "/workspace.json", want: true},
		"mirror-only rolled-up json": {rel: cvPath, want: true},
		"mirror-only bundle zip":     {rel: wsDir + "/" + store.BundlesDirName + "/logs.gen0001.zip"},
		"mirror-only config tarball": {rel: store.ConfigVersionsDirName + "/cv-9.tar.gz"},
		"nothing anywhere":           {rel: wsDir + "/runs/run-absent/run.json"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			fake := buildMirroredArchive(t)
			mirrorTarball(t, fake, "cv-9")

			orgs, _ := openBootstrap(t, fake)
			ws := orgs[0].Workspace("default", "app")

			// Probe before the read: an Open that lands persists its bytes, which
			// would make a later probe true for the wrong reason.
			ok, err := ws.Exists(tt.rel)
			require.NoError(t, err)
			assert.Equal(t, tt.want, ok)

			_, openErr := ws.Open(tt.rel)
			if tt.want {
				require.NoError(t, openErr, "a probe answering true names bytes Open serves")

				return
			}

			require.ErrorIs(t, openErr, view.ErrObjectNotFound,
				"a probe answering false names what Open refuses")
		})
	}
}

func TestWorkspaceIndexSkipsRollupLineNamingNoPath(t *testing.T) {
	t.Parallel()

	// A roll-up record that parses but carries no path used to index a sealed
	// object at the empty path, which sits under every prefix: the whole-org
	// listing grew an entry naming nothing and every extract reported it as a
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

			sum, err := view.NewArchive([]*view.Org{org}).Extract(t.Context(), target, "my-org", nil)
			require.NoError(t, err)
			assert.Zero(t, sum.Errored, "an extract has no phantom file to fail on")
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

			sum, err := view.NewArchive([]*view.Org{org}).Extract(t.Context(), target, "my-org", nil)
			require.NoError(t, err)
			assert.Zero(t, sum.Errored, "an extract has no phantom member to fail on")
		})
	}
}
