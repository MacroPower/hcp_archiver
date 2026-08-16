package collect_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/collect"
	"go.jacobcolvin.com/hcp_archiver/manifest"
	"go.jacobcolvin.com/hcp_archiver/store"
)

func TestEnvMarkSurfaceDroppedKeysOnSegments(t *testing.T) {
	t.Parallel()

	// The dropped-surface record is last-writer-wins per key, so two sibling
	// surfaces dropping concurrently (two providers' version listings) must
	// land under distinct keys, and the distinguishing identity travels as
	// segments rather than being remembered at each call site.
	env, _, ledger := newEnv(t)

	env.MarkSurfaceDropped(errors.New("first cause"), "registry", "provider-versions", "hashicorp", "aws")
	env.MarkSurfaceDropped(errors.New("second cause"), "registry", "provider-versions", "hashicorp", "google")

	dropped := ledger.DroppedSurfaces()
	require.Len(t, dropped, 2, "each provider keeps its own record")

	surfaces := []string{dropped[0].Surface, dropped[1].Surface}
	assert.ElementsMatch(t, []string{
		"registry/provider-versions/hashicorp/aws",
		"registry/provider-versions/hashicorp/google",
	}, surfaces)
}

func TestEnvPageSlot(t *testing.T) {
	t.Parallel()

	const relPath = "audit-trails/page.json"

	tests := map[string]struct {
		seed func(l *manifest.Ledger)
		want collect.PageSlotState
	}{
		"no entry must write": {
			want: collect.PageSlotMustWrite,
		},
		"retryable entry must write": {
			seed: func(l *manifest.Ledger) {
				l.RecordErrored(relPath, errors.New("boom"), true)
			},
			want: collect.PageSlotMustWrite,
		},
		"settled done short-circuits": {
			seed: func(l *manifest.Ledger) {
				l.RecordDone(relPath, manifest.Signature{Hash: "h", Size: 1})
			},
			want: collect.PageSlotShortCircuited,
		},
		"settled absence has no file behind it": {
			seed: func(l *manifest.Ledger) {
				l.RecordAbsent(relPath, errors.New("resource not found"))
			},
			want: collect.PageSlotSettledAbsent,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			env, _, ledger := newEnv(t)

			if tc.seed != nil {
				tc.seed(ledger)
			}

			assert.Equal(t, tc.want, env.PageSlot(relPath))
		})
	}

	t.Run("retry-absent reopens a settled absence", func(t *testing.T) {
		t.Parallel()

		// Under retry-absent the slot is not settled, so the ordinary path
		// re-fetches it rather than treating it as a fileless gap.
		st := store.New(t.TempDir())

		ledger, err := manifest.Load(st.Root(), manifest.WithRetryAbsent(true))
		require.NoError(t, err)

		t.Cleanup(func() { require.NoError(t, ledger.Close()) })

		ledger.StartRun()
		ledger.RecordAbsent(relPath, errors.New("resource not found"))

		env := collect.NewEnv(nil, st, ledger)

		assert.Equal(t, collect.PageSlotMustWrite, env.PageSlot(relPath))
	})
}
