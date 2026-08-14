package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/store"
)

func TestRemoteStubTarget(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		relPath string
		want    string
		isStub  bool
	}{
		"a tarball stub resolves to its tarball": {
			relPath: "config-versions/cv-1.tar.gz.remote.json",
			want:    "config-versions/cv-1.tar.gz",
			isStub:  true,
		},
		"the tarball itself is not a stub": {
			relPath: "config-versions/cv-1.tar.gz",
		},
		"the organization's remote marker is not a stub": {
			relPath: ".remote.json",
		},
		"the suffix over a non-tarball is an ordinary object": {
			relPath: "config-versions/notes.remote.json",
		},
		"the suffix outside config-versions is an ordinary object": {
			relPath: "projects/p1/workspaces/w1/state.tar.gz.remote.json",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			target, ok := store.RemoteStubTarget(tt.relPath)
			assert.Equal(t, tt.isStub, ok)
			assert.Equal(t, tt.want, target)
		})
	}
}

func TestRemoteStubPathRoundTrips(t *testing.T) {
	t.Parallel()

	s := store.New(t.TempDir())
	tarball := s.ConfigVersionTarball("cv-abc")

	stub := store.RemoteStubPath(tarball)
	assert.Equal(t, "config-versions/cv-abc.tar.gz.remote.json", stub)

	target, ok := store.RemoteStubTarget(stub)
	require.True(t, ok)
	assert.Equal(t, tarball, target, "the stub names the object it stands in for")
}

func TestIsConfigTarball(t *testing.T) {
	t.Parallel()

	assert.True(t, store.IsConfigTarball("config-versions/cv-1.tar.gz"))
	assert.False(t, store.IsConfigTarball("config-versions/cv-1.tar"))
	assert.False(t, store.IsConfigTarball("config-versions"))
	assert.False(t, store.IsConfigTarball("projects/p1/cv-1.tar.gz"),
		"only the org-wide directory holds configuration-version tarballs")
}

func TestIsBundleZip(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		relPath string
		want    bool
	}{
		"a zip directly under a workspace's bundles directory": {
			relPath: "projects/p1/workspaces/w1/bundles/runs.gen0001.zip",
			want:    true,
		},
		"a non-zip in the bundles directory": {
			relPath: "projects/p1/workspaces/w1/bundles/runs.gen0001.zip.ndjson",
		},
		"a zip in a workspace named bundles": {
			relPath: "projects/p1/workspaces/bundles/runs/run-1/artifact.zip",
		},
		"a zip at a workspace root named bundles": {
			relPath: "projects/p1/workspaces/bundles/artifact.zip",
		},
		"a zip nested below the bundles directory": {
			relPath: "projects/p1/workspaces/w1/bundles/deep/runs.gen0001.zip",
		},
		"a zip outside any workspace": {
			relPath: "bundles/runs.gen0001.zip",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, store.IsBundleZip(tt.relPath))
		})
	}
}

func TestInSealedForm(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		relPath string
		want    bool
	}{
		"a file in a workspace's bundles directory": {
			relPath: "projects/p1/workspaces/w1/bundles/runs.gen0001.zip",
			want:    true,
		},
		"a file in a workspace's rollups directory": {
			relPath: "projects/p1/workspaces/w1/rollups/runs.ndjson",
			want:    true,
		},
		"the bundles directory itself": {
			relPath: "projects/p1/workspaces/w1/bundles",
			want:    true,
		},
		"an object in a workspace named bundles": {
			relPath: "projects/p1/workspaces/bundles/variables.json",
		},
		"an object in a workspace named rollups": {
			relPath: "projects/p1/workspaces/rollups/workspace.json",
		},
		"an object in a project named rollups": {
			relPath: "projects/rollups/workspaces/w1/workspace.json",
		},
		"an org-level path naming bundles": {
			relPath: "bundles/notes.json",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, store.InSealedForm(tt.relPath))
		})
	}
}
