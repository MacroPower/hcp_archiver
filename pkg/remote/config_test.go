package remote_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/remote"
)

func TestConfigKey(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		prefix  string
		org     string
		relPath string
		want    string
	}{
		"no prefix": {
			org:     "acme",
			relPath: "projects/p/workspaces/w/bundles/logs.gen0001.zip",
			want:    "acme/projects/p/workspaces/w/bundles/logs.gen0001.zip",
		},
		"prefix": {
			prefix:  "hcp-archive",
			org:     "acme",
			relPath: "org.json",
			want:    "hcp-archive/acme/org.json",
		},
		"prefix with stray slashes": {
			prefix:  "/hcp-archive/",
			org:     "acme",
			relPath: "org.json",
			want:    "hcp-archive/acme/org.json",
		},
		"nested prefix": {
			prefix:  "team/infra",
			org:     "acme",
			relPath: "a/b",
			want:    "team/infra/acme/a/b",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg := remote.Config{URL: "mem://", Prefix: tt.prefix}
			assert.Equal(t, tt.want, cfg.Key(tt.org, tt.relPath))
		})
	}
}

func TestMarkerRoundTrip(t *testing.T) {
	t.Parallel()

	cfg := remote.Config{
		URL:    "s3://archive?region=us-east-1",
		Prefix: "hcp",

		// Write-side tuning a marker intentionally drops.
		PartSize:    1 << 26,
		Concurrency: 4,
	}

	marker := cfg.Marker()
	assert.Equal(t, remote.MarkerVersion, marker.Version,
		"a written marker should record the current schema version")

	assert.Equal(t, remote.Config{
		URL:    "s3://archive?region=us-east-1",
		Prefix: "hcp",
	}, marker.Config(), "marker should round-trip the read-relevant fields and drop the rest")
}

func TestRestoringMarker(t *testing.T) {
	t.Parallel()

	cfg := remote.Config{URL: "s3://archive", Prefix: "hcp"}

	marker := cfg.RestoringMarker()

	assert.Equal(t, remote.MarkerVersionRestoring, marker.Version,
		"a restoring marker must stamp the version older builds refuse")
	assert.True(t, marker.Restoring)
	assert.True(t, marker.Partial,
		"a mid-restore tree holds a subset of the mirror, so a reader must merge listings")
	assert.Equal(t, cfg.URL, marker.URL)
	assert.Equal(t, cfg.Prefix, marker.Prefix)
}

// writeMarkerFile writes raw marker JSON at a fresh org root and returns the
// root.
func writeMarkerFile(t *testing.T, content string) string {
	t.Helper()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, remote.MarkerName), []byte(content), 0o600))

	return root
}

func TestReadMarkerVersionCeiling(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		content       string
		wantRestoring bool
		wantErr       bool
	}{
		"steady-state version reads": {
			content: `{"url":"s3://archive","version":1}`,
		},
		"restoring version reads with its flag": {
			content:       `{"url":"s3://archive","version":2,"partial":true,"restoring":true}`,
			wantRestoring: true,
		},
		"a newer version refuses": {
			content: `{"url":"s3://archive","version":3}`,
			wantErr: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			root := writeMarkerFile(t, tt.content)

			marker, ok, err := remote.ReadMarker(root)
			if tt.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, tt.wantRestoring, marker.Restoring)
		})
	}
}
