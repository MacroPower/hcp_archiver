package collect_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/collect"
	"go.jacobcolvin.com/hcp_archiver/pkg/manifest"
	"go.jacobcolvin.com/hcp_archiver/pkg/store"
)

// newLedgerEnv builds an Env over a fresh store and ledger for tests exercising
// the ledger passthroughs directly, with no client behind them.
func newLedgerEnv(t *testing.T) (*collect.Env, *manifest.Ledger) {
	t.Helper()

	st := store.New(t.TempDir())

	ledger, err := manifest.Load(st.Root())
	require.NoError(t, err)

	t.Cleanup(func() { require.NoError(t, ledger.Close()) })

	ledger.StartRun()

	return collect.NewEnv(nil, st, ledger), ledger
}

func TestNotApplicableUnlessDone(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		record func(l *manifest.Ledger, relPath string)
		want   manifest.Status
	}{
		"no prior entry settles not-applicable": {
			record: func(*manifest.Ledger, string) {},
			want:   manifest.StatusNotApplicable,
		},
		"an errored entry settles not-applicable": {
			record: func(l *manifest.Ledger, relPath string) {
				l.RecordErrored(relPath, errors.New("boom"), false)
			},
			want: manifest.StatusNotApplicable,
		},
		"a done entry is not regressed": {
			record: func(l *manifest.Ledger, relPath string) {
				l.RecordDone(relPath, manifest.Signature{})
			},
			want: manifest.StatusDone,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			env, ledger := newLedgerEnv(t)
			relPath := "things/thing-1.json"

			tc.record(ledger, relPath)
			env.NotApplicableUnlessDone(relPath)

			entry, ok := ledger.Entry(relPath)
			require.True(t, ok)
			assert.Equal(t, tc.want, entry.Status)
		})
	}
}
