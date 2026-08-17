package orgscope_test

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/collect/orgscope"
	"go.jacobcolvin.com/hcp_archiver/pkg/manifest"
	"go.jacobcolvin.com/hcp_archiver/pkg/store"
)

// plantPolicyMetadata writes the archived policy.json a previous run's
// metadata refresh would have left, carrying the updated-at the backfill
// reads.
func plantPolicyMetadata(t *testing.T, st *store.Store, id string, updatedAt time.Time) {
	t.Helper()

	doc := `{"data":{"id":"` + id + `","type":"policies","attributes":{"updated-at":"` +
		updatedAt.UTC().Format(time.RFC3339) + `"}}}`

	relPath := st.Policy(id, "json")
	require.NoError(t, os.MkdirAll(st.AbsPath("policies"), 0o755))
	require.NoError(t, os.WriteFile(st.AbsPath(relPath), []byte(doc), 0o600))
}

func TestLedgerMigration(t *testing.T) {
	t.Parallel()

	updated := time.Date(2026, time.June, 1, 9, 30, 0, 0, time.UTC)

	st := store.New(t.TempDir())
	plantPolicyMetadata(t, st, "pol-1", updated)

	mig := orgscope.LedgerMigration(st)

	done := manifest.Entry{Status: manifest.StatusDone}

	tests := map[string]struct {
		relPath     string
		entry       manifest.Entry
		wantChanged bool
	}{
		"plain source entry backfills from the archived metadata": {
			relPath:     st.Policy("pol-1", "sentinel"),
			entry:       done,
			wantChanged: true,
		},
		"stamped revision name is untouched": {
			relPath: st.Policy("pol-1", "20260601T093000Z.sentinel"),
			entry:   done,
		},
		"policy metadata itself is untouched": {
			relPath: st.Policy("pol-1", "json"),
			entry:   done,
		},
		"entry already carrying a stamp is untouched": {
			relPath: st.Policy("pol-1", "sentinel"),
			entry:   manifest.Entry{Status: manifest.StatusDone, UpdatedAt: updated},
		},
		"unsettled source entry is untouched": {
			relPath: st.Policy("pol-1", "sentinel"),
			entry:   manifest.Entry{Status: manifest.StatusErrored},
		},
		"source without archived metadata keeps the fallback": {
			relPath: st.Policy("pol-2", "rego"),
			entry:   done,
		},
		"entry outside the policies directory is untouched": {
			relPath: "org.json",
			entry:   done,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			out, changed := mig(tc.relPath, 1, tc.entry)
			assert.Equal(t, tc.wantChanged, changed)

			if tc.wantChanged {
				assert.Equal(t, updated, out.UpdatedAt,
					"the archived metadata's updated-at lands on the entry")
			} else {
				assert.Equal(t, tc.entry.UpdatedAt, out.UpdatedAt)
			}
		})
	}
}

func TestLedgerMigrationBackfillsThroughLoad(t *testing.T) {
	t.Parallel()

	// A v1 archive's plain source entry carries no server stamp; opened with
	// the migration registered, the entry backfills from the archived
	// metadata, so revision naming keeps the server-clock baseline pre-v2
	// releases derived by re-reading the file on every run.
	updated := time.Date(2026, time.June, 1, 9, 30, 0, 0, time.UTC)

	st := store.New(t.TempDir())
	plantPolicyMetadata(t, st, "pol-1", updated)

	relPath := st.Policy("pol-1", "sentinel")

	ledger, err := manifest.Load(st.Root())
	require.NoError(t, err)

	ledger.StartRun()
	ledger.RecordDone(relPath, manifest.Signature{Hash: "h", Size: 1})
	require.NoError(t, ledger.Flush())
	require.NoError(t, ledger.Close())

	reloaded, err := manifest.Load(st.Root(),
		manifest.WithMigrations(orgscope.LedgerMigration(st)))
	require.NoError(t, err)

	t.Cleanup(func() { require.NoError(t, reloaded.Close()) })

	entry, ok := reloaded.Entry(relPath)
	require.True(t, ok)
	assert.Equal(t, updated, entry.UpdatedAt)
}
