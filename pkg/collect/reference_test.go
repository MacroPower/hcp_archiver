package collect_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/collect"
	"go.jacobcolvin.com/hcp_archiver/pkg/manifest"
	"go.jacobcolvin.com/hcp_archiver/pkg/store"
)

func TestReferenceCarriesAForeignAbsenceToARetryAbsentRun(t *testing.T) {
	t.Parallel()

	// A run's config-version tarball expired and settled StatusAbsent in the
	// config-versions shard, a shard the runs walk never scans. The reference
	// gate must leave an absence mark in the run's own shard, or a retry-absent
	// run early-stops over the settled runs collection and the flag can never
	// reach the one entry it exists to re-probe.
	root := t.TempDir()

	const (
		prefix  = "projects/p/workspaces/w/runs"
		gate    = prefix + "/r1/config-version-tarball.ref"
		tarball = "config-versions/cv-9.tar.gz"
	)

	// The shard subtrees exist as production leaves them, so records survive
	// the reloads below.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "projects", "p", "workspaces", "w"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "config-versions"), 0o755))

	st := store.New(root)

	ledger, err := manifest.Load(root)
	require.NoError(t, err)

	env := collect.NewEnv(nil, st, ledger)

	ledger.RecordAbsent(tarball, errors.New("resource not found"))
	env.Reference(gate, tarball)

	entry, ok := ledger.Entry(gate)
	require.True(t, ok, "the mirrored absence leaves a trace in the run's shard")
	assert.Equal(t, manifest.StatusReferenceAbsent, entry.Status)

	assert.False(t, ledger.Collection(prefix).HasUnsettled(),
		"a normal run settles over the mirrored absence")

	require.NoError(t, ledger.Flush())
	require.NoError(t, ledger.Close())

	// A retry-absent run reads the persisted trace: the runs collection
	// re-opens, so the walk revisits the run whose tarball it must re-probe,
	// and the gated-listing predicate sees the absence too.
	retry, err := manifest.Load(root, manifest.WithRetryAbsent(true))
	require.NoError(t, err)

	t.Cleanup(func() { require.NoError(t, retry.Close()) })

	assert.True(t, retry.Collection(prefix).HasUnsettled(),
		"the gate re-opens the runs walk under retry-absent")
	assert.True(t, retry.HasRetryableAbsentUnder(prefix+"/r1"))

	// The re-probe succeeds: the restored tarball settles done, and the
	// recomputed gate retires the absence mark to an ordinary clear.
	retryEnv := collect.NewEnv(nil, st, retry)

	retry.RecordDone(tarball, manifest.Signature{Size: 1})
	retryEnv.Reference(gate, tarball)

	entry, ok = retry.Entry(gate)
	require.True(t, ok)
	assert.Equal(t, manifest.StatusReferenceCleared, entry.Status,
		"the trace retires with the absence it stood for")
	assert.False(t, retry.Collection(prefix).HasUnsettled())
}
