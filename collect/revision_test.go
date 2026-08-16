package collect_test

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/collect"
	"go.jacobcolvin.com/hcp_archiver/manifest"
	"go.jacobcolvin.com/hcp_archiver/store"
)

func TestEnvRevisionPath(t *testing.T) {
	t.Parallel()

	const (
		plain   = "policies/pol-1.sentinel"
		stamped = "policies/pol-1.20260601T093000Z.sentinel"
	)

	var (
		fetched  = time.Date(2026, time.July, 8, 12, 0, 0, 0, time.UTC)
		captured = fetched.Add(-time.Hour)
		replaced = fetched.Add(-30 * time.Minute)
		sig      = manifest.Signature{Hash: "h", Size: 1}
	)

	tests := map[string]struct {
		seed      func(l *manifest.Ledger)
		updatedAt time.Time
		want      string
	}{
		"nothing archived keeps the plain name": {
			updatedAt: replaced,
			want:      plain,
		},
		"unsettled plain capture keeps the plain name": {
			seed: func(l *manifest.Ledger) {
				l.RecordErrored(plain, errors.New("boom"), true)
			},
			updatedAt: replaced,
			want:      plain,
		},
		"zero updated-at keeps the plain name": {
			seed: func(l *manifest.Ledger) {
				l.RecordDone(plain, sig, manifest.WithUpdatedAt(captured))
			},
			want: plain,
		},
		"unchanged revision resolves to the plain name": {
			seed: func(l *manifest.Ledger) {
				l.RecordDone(plain, sig, manifest.WithUpdatedAt(captured))
			},
			updatedAt: captured,
			want:      plain,
		},
		"newer revision earns the stamped name": {
			seed: func(l *manifest.Ledger) {
				l.RecordDone(plain, sig, manifest.WithUpdatedAt(captured))
			},
			updatedAt: fetched.Add(time.Hour),
			want:      stamped,
		},
		"revision updated before the local fetch stamp still earns its name": {
			// The upload landed between the run's download and its ledger
			// stamp, so the new revision's updated-at sorts before the
			// recorded fetch time. Against the server-clock baseline it is
			// newer than the captured revision and must not look archived.
			seed: func(l *manifest.Ledger) {
				l.RecordDone(plain, sig, manifest.WithUpdatedAt(captured))
			},
			updatedAt: replaced,
			want:      stamped,
		},
		"pre-v2 entry falls back to the local fetch time": {
			seed: func(l *manifest.Ledger) {
				l.RecordDone(plain, sig)
			},
			updatedAt: replaced,
			want:      plain,
		},
		"pre-v2 entry with an updated-at past the fetch time stamps": {
			seed: func(l *manifest.Ledger) {
				l.RecordDone(plain, sig)
			},
			updatedAt: fetched.Add(time.Minute),
			want:      stamped,
		},
		"unsettled stamped name lands the retry": {
			seed: func(l *manifest.Ledger) {
				l.RecordDone(plain, sig, manifest.WithUpdatedAt(replaced))
				l.RecordErrored(stamped, errors.New("boom"), true)
			},
			// The listed revision is not newer than the plain capture's, so
			// the baseline alone would fold back to the settled plain name and
			// strand the unsettled stamped entry.
			updatedAt: replaced,
			want:      stamped,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			st := store.New(t.TempDir())

			ledger, err := manifest.Load(st.Root(),
				manifest.WithClock(func() time.Time { return fetched }))
			require.NoError(t, err)

			t.Cleanup(func() { require.NoError(t, ledger.Close()) })

			ledger.StartRun()

			if tc.seed != nil {
				tc.seed(ledger)
			}

			env := collect.NewEnv(nil, st, ledger)

			assert.Equal(t, tc.want, env.RevisionPath(plain, stamped, tc.updatedAt))
		})
	}
}
