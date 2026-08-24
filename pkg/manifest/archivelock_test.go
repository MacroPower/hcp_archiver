package manifest_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/manifest"
)

func TestLockArchiveExcludesLoad(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	lock, err := manifest.LockArchive(root)
	require.NoError(t, err)

	_, err = manifest.Load(root)
	require.ErrorIs(t, err, manifest.ErrLedgerLocked,
		"a held archive lock must keep the ledger from opening")

	require.NoError(t, lock.Close())

	ledger, err := manifest.Load(root)
	require.NoError(t, err, "a released archive lock must free the root")
	require.NoError(t, ledger.Close())
}

func TestLoadExcludesLockArchive(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	ledger, err := manifest.Load(root)
	require.NoError(t, err)

	_, err = manifest.LockArchive(root)
	require.ErrorIs(t, err, manifest.ErrLedgerLocked,
		"a loaded ledger must keep the archive lock from being taken")

	require.NoError(t, ledger.Close())

	lock, err := manifest.LockArchive(root)
	require.NoError(t, err, "a closed ledger must free the root")
	require.NoError(t, lock.Close())
}

func TestArchiveLockCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	lock, err := manifest.LockArchive(t.TempDir())
	require.NoError(t, err)

	require.NoError(t, lock.Close())
	require.NoError(t, lock.Close())
}

func TestIsSnapshotPath(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		relPath string
		want    bool
	}{
		"org-root snapshot":                  {relPath: ".ledger/snapshot.json", want: true},
		"workspace snapshot":                 {relPath: "projects/p/workspaces/w/.ledger/snapshot.json", want: true},
		"replay log":                         {relPath: ".ledger/log.ndjson"},
		"lock file":                          {relPath: ".ledger/lock"},
		"snapshot name outside a ledger dir": {relPath: "projects/p/snapshot.json"},
		"data file":                          {relPath: "org.json"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, manifest.IsSnapshotPath(tt.relPath))
		})
	}
}

func TestDecodeSnapshotMeta(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		snapshot string
		want     manifest.SnapshotMeta
		wantErr  bool
	}{
		"org-root snapshot carries run metadata": {
			snapshot: `{"version":2,"lastRunAt":"2026-08-24T10:00:00Z","runCount":7}`,
			want: manifest.SnapshotMeta{
				LastRunAt: time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC),
				RunCount:  7,
			},
		},
		"non-root snapshot decodes zero": {
			snapshot: `{"version":2,"entries":{}}`,
		},
		"corrupt snapshot errors": {
			snapshot: `{not json`,
			wantErr:  true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			meta, err := manifest.DecodeSnapshotMeta(strings.NewReader(tt.snapshot))
			if tt.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, meta)
		})
	}
}

func TestSnapshotMetaNewer(t *testing.T) {
	t.Parallel()

	earlier := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	later := earlier.Add(time.Hour)

	tests := map[string]struct {
		m     manifest.SnapshotMeta
		other manifest.SnapshotMeta
		want  bool
	}{
		"later run with equal count is newer": {
			m:     manifest.SnapshotMeta{LastRunAt: later, RunCount: 3},
			other: manifest.SnapshotMeta{LastRunAt: earlier, RunCount: 3},
			want:  true,
		},
		"later run with higher count is newer": {
			m:     manifest.SnapshotMeta{LastRunAt: later, RunCount: 4},
			other: manifest.SnapshotMeta{LastRunAt: earlier, RunCount: 3},
			want:  true,
		},
		"equal timestamps are unordered": {
			m:     manifest.SnapshotMeta{LastRunAt: later, RunCount: 4},
			other: manifest.SnapshotMeta{LastRunAt: later, RunCount: 3},
		},
		"a later run whose count went backward is unordered": {
			m:     manifest.SnapshotMeta{LastRunAt: later, RunCount: 2},
			other: manifest.SnapshotMeta{LastRunAt: earlier, RunCount: 3},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, tt.m.Newer(tt.other))
		})
	}
}
