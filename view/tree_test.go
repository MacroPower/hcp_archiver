package view_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/remote"
	"go.jacobcolvin.com/hcp_archiver/view"
)

// buildMultiOrgArchive extends the standard fixture with two smaller
// organizations: one sorting before "my-org", and one whose name extends
// "my-org" so archive-level ordering must sort full archive paths, not
// concatenate name-sorted per-org listings ("my-org-extra/..." sorts before
// "my-org/...").
func buildMultiOrgArchive(t *testing.T) string {
	t.Helper()

	root := buildArchive(t)

	writeFile(t, filepath.Join(root, "acme"), "org.json",
		`{"data":{"id":"org-2","type":"organizations","attributes":{"name":"acme"}}}`)
	writeFile(t, filepath.Join(root, "acme"), "projects/p1/project.json",
		`{"data":{"id":"prj-2","type":"projects","attributes":{"name":"p1"}}}`)
	writeFile(t, filepath.Join(root, "my-org-extra"), "org.json",
		`{"data":{"id":"org-3","type":"organizations","attributes":{"name":"my-org-extra"}}}`)

	return root
}

// openArchiveTree opens the archive at dir and wraps it in a [*view.Archive].
func openArchiveTree(t *testing.T, dir string) *view.Archive {
	t.Helper()

	orgs, err := view.OpenArchive(dir)
	require.NoError(t, err)

	return view.NewArchive(orgs)
}

// archivePaths projects a listing onto its org-prefixed paths.
func archivePaths(entries []view.Entry) []string {
	return mapEntries(entries, view.Entry.ArchivePath)
}

func TestArchiveList(t *testing.T) {
	t.Parallel()

	root := buildMultiOrgArchive(t)
	arc := openArchiveTree(t, root)

	t.Run("whole archive is org-prefixed and sorted", func(t *testing.T) {
		t.Parallel()

		entries, err := arc.List("")
		require.NoError(t, err)

		paths := archivePaths(entries)
		assert.True(t, slices.IsSorted(paths), "entries sort by archive path: %v", paths)
		assert.Contains(t, paths, "acme/org.json")
		assert.Contains(t, paths, "my-org/org.json")
		assert.Contains(t, paths, "my-org-extra/org.json")
		assert.Contains(t, paths, "my-org/"+planPath)
	})

	t.Run("org prefix scopes to one organization", func(t *testing.T) {
		t.Parallel()

		entries, err := arc.List("acme")
		require.NoError(t, err)
		assert.Equal(t, []string{"acme/org.json", "acme/projects/p1/project.json"}, archivePaths(entries))
	})

	t.Run("org-dir open yields the same addresses", func(t *testing.T) {
		t.Parallel()

		// The addressing invariant: opening the archive on a single org
		// directory lists identical org-prefixed paths, so an address works
		// no matter which directory the archive was opened on.
		wholeRoot, err := arc.List("my-org")
		require.NoError(t, err)

		orgOnly := openArchiveTree(t, filepath.Join(root, "my-org"))

		wholeOrg, err := orgOnly.List("")
		require.NoError(t, err)

		assert.Equal(t, archivePaths(wholeRoot), archivePaths(wholeOrg))
	})

	t.Run("org names match whole segments only", func(t *testing.T) {
		t.Parallel()

		_, err := arc.List("my-o")
		require.ErrorIs(t, err, view.ErrNoOrg)
	})
}

func TestArchiveRead(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		archivePath string
		prepare     func(t *testing.T, root string)
		want        string
		err         error
	}{
		"org-prefixed loose file": {
			archivePath: "my-org/org.json",
			want:        `{"data":{"id":"org-1","type":"organizations","attributes":{"name":"my-org"}}}`,
		},
		"org-prefixed sealed member": {
			archivePath: "my-org/" + planPath,
			want:        planContent,
		},
		"organization root is not a file": {
			archivePath: "my-org",
			err:         view.ErrNotFile,
		},
		"directory is not a file": {
			archivePath: "my-org/" + wsDir,
			err:         view.ErrNotFile,
		},
		"fully sealed directory reports as a directory": {
			archivePath: "my-org/" + wsDir + "/runs/run-new",
			prepare: func(t *testing.T, root string) {
				t.Helper()

				// Remove the loose run directory so the run survives only as
				// sealed keys: no physical directory, but archived objects
				// beneath the path.
				require.NoError(t, os.RemoveAll(
					filepath.Join(root, "my-org", filepath.FromSlash(wsDir), "runs", "run-new")))
			},
			err: view.ErrNotFile,
		},
		"unknown organization": {
			archivePath: "nope/org.json",
			err:         view.ErrNoOrg,
		},
		"missing object": {
			archivePath: "my-org/" + wsDir + "/runs/run-new/apply.log",
			err:         view.ErrObjectNotFound,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			root := buildMultiOrgArchive(t)
			if tt.prepare != nil {
				tt.prepare(t, root)
			}

			arc := openArchiveTree(t, root)

			data, err := arc.Read(tt.archivePath)
			if tt.err != nil {
				require.ErrorIs(t, err, tt.err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, string(data))
		})
	}
}

func TestArchiveUnsealScopes(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		prefix string
	}{
		"whole archive":    {prefix: ""},
		"one organization": {prefix: "my-org"},
		"one project":      {prefix: "my-org/projects/default"},
		"one workspace":    {prefix: "my-org/" + wsDir},
		"one run":          {prefix: "my-org/" + wsDir + "/runs/run-new"},
		"one sealed file":  {prefix: "my-org/" + planPath},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			arc := openArchiveTree(t, buildMultiOrgArchive(t))
			target := t.TempDir()

			entries, err := arc.List(tt.prefix)
			require.NoError(t, err)
			require.NotEmpty(t, entries)

			sum, err := arc.Unseal(t.Context(), target, tt.prefix, nil)
			require.NoError(t, err)
			assert.Zero(t, sum.Errored)

			// The dry-run invariant: the listing IS the plan, so its count
			// and byte total match the run's summary exactly.
			assert.Equal(t, len(entries), sum.Files)

			var wantBytes int64

			for _, e := range entries {
				wantBytes += e.Size
			}

			assert.Equal(t, wantBytes, sum.Bytes)

			// The target holds exactly the listed paths, each byte-identical
			// to what the archive serves. Compared as sets: the walk yields
			// directory order, the listing archive-path order.
			assert.ElementsMatch(t, archivePaths(entries), targetPaths(t, target))

			for _, e := range entries {
				want, readErr := arc.Read(e.ArchivePath())
				require.NoError(t, readErr)

				got, fileErr := os.ReadFile(filepath.Join(target, filepath.FromSlash(e.ArchivePath())))
				require.NoError(t, fileErr)

				assert.Equal(t, want, got, "unsealed bytes for %s", e.ArchivePath())
			}
		})
	}
}

func TestArchiveUnseal_EmptyTarget(t *testing.T) {
	t.Parallel()

	arc := openArchiveTree(t, buildArchive(t))

	_, err := arc.Unseal(t.Context(), "", "", nil)
	require.ErrorIs(t, err, view.ErrNoTarget)
}

func TestArchiveUnseal_UnknownOrg(t *testing.T) {
	t.Parallel()

	arc := openArchiveTree(t, buildArchive(t))

	_, err := arc.Unseal(t.Context(), t.TempDir(), "nope", nil)
	require.ErrorIs(t, err, view.ErrNoOrg)
}

func TestArchiveUnseal_CanceledContext(t *testing.T) {
	t.Parallel()

	arc := openArchiveTree(t, buildArchive(t))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	sum, err := arc.Unseal(ctx, t.TempDir(), "", nil)
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, sum.Files, "a pre-canceled run writes nothing")
}

func TestArchiveUnseal_ProgressCallback(t *testing.T) {
	t.Parallel()

	arc := openArchiveTree(t, buildMultiOrgArchive(t))
	target := t.TempDir()

	var paths []string

	sum, err := arc.Unseal(t.Context(), target, "", func(relPath string, bytes int64, err error) {
		require.NoError(t, err)
		assert.Positive(t, bytes)

		paths = append(paths, relPath)
	})
	require.NoError(t, err)

	assert.Len(t, paths, sum.Files, "one callback per written file")

	for _, p := range paths {
		org, _, found := strings.Cut(p, "/")
		assert.True(t, found, "progress paths are org-prefixed: %s", p)
		assert.Contains(t, []string{"acme", "my-org", "my-org-extra"}, org)
	}
}

func TestArchiveUnseal_RemoteEviction(t *testing.T) {
	t.Parallel()

	root, fake := buildRemoteArchive(t)

	orgs, err := view.OpenArchive(root,
		view.WithContext(t.Context()),
		view.WithRemoteFactory(func(ctx context.Context, cfg remote.Config) (*remote.Client, error) {
			return remote.New(ctx, cfg, remote.WithBucket(fake.Bucket()), remote.WithRetry(0, 0))
		}),
	)
	require.NoError(t, err)

	arc := view.NewArchive(orgs)
	target := t.TempDir()

	sum, err := arc.Unseal(t.Context(), target, "my-org/"+wsDir, nil)
	require.NoError(t, err)
	assert.Zero(t, sum.Errored)

	got, err := os.ReadFile(filepath.Join(target, "my-org", filepath.FromSlash(planPath)))
	require.NoError(t, err)
	assert.Equal(t, planContent, string(got),
		"an evicted bundle member materializes from the remote store")
}
