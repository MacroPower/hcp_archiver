package remote_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.jacobcolvin.com/hcp_archiver/remote"
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

			cfg := remote.Config{Bucket: "b", Prefix: tt.prefix}
			assert.Equal(t, tt.want, cfg.Key(tt.org, tt.relPath))
		})
	}
}

func TestMarkerRoundTrip(t *testing.T) {
	t.Parallel()

	cfg := remote.Config{
		Bucket:         "archive",
		Prefix:         "hcp",
		Endpoint:       "https://s3.example.com",
		Region:         "us-east-1",
		ForcePathStyle: true,

		// Write-side settings a marker intentionally drops.
		StorageClass: "DEEP_ARCHIVE",
		PartSize:     1 << 26,
		Concurrency:  4,
	}

	got := cfg.Marker().Config()

	assert.Equal(t, remote.Config{
		Bucket:         "archive",
		Prefix:         "hcp",
		Endpoint:       "https://s3.example.com",
		Region:         "us-east-1",
		ForcePathStyle: true,
	}, got, "marker should round-trip the read-relevant fields and drop the rest")
}
