package collect_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/remote"
	"go.jacobcolvin.com/hcp_archiver/pkg/remote/remotetest"
)

func TestSyncArchivePruneRefusesDuringRestore(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)
	f.resume()

	// A stale key a steady-state sweep would prune freely.
	f.fake.SetObject(f.key("projects/old/workspaces/gone/workspace.json"),
		remotetest.Object{Data: []byte("history")})
	f.write(t, "org.json", []byte(`{"org":"acme"}`))

	// The restoring marker: an interrupted restore's durable record that the
	// local tree is a subset of the mirror, so nothing missing locally may be
	// read as deleted, whatever the ledger's resume state says.
	f.write(t, remote.MarkerName,
		[]byte(`{"url":"s3://archive","version":2,"partial":true,"restoring":true}`))

	stats := f.env.SyncArchive(t.Context())

	assert.Zero(t, stats.Pruned)
	assert.Empty(t, f.fake.Deleted())
	assert.Positive(t, stats.Failed, "a refused prune must mark the run incomplete")
}

func TestSyncArchivePruneRefusesOverUnreadableMarker(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)
	f.resume()

	f.fake.SetObject(f.key("projects/old/workspaces/gone/workspace.json"),
		remotetest.Object{Data: []byte("history")})
	f.write(t, "org.json", []byte(`{"org":"acme"}`))

	// A marker that cannot be read might record a restore in progress; doubt
	// must not delete.
	f.write(t, remote.MarkerName, []byte(`{not json`))

	stats := f.env.SyncArchive(t.Context())

	assert.Zero(t, stats.Pruned)
	assert.Empty(t, f.fake.Deleted())
	assert.Positive(t, stats.Failed, "a refused prune must mark the run incomplete")
}

func TestSyncArchivePruneProceedsOverSettledMarker(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)
	f.resume()

	f.fake.SetObject(f.key("projects/old/workspaces/gone/workspace.json"),
		remotetest.Object{Data: []byte("history")})
	f.write(t, "org.json", []byte(`{"org":"acme"}`))

	// A settled marker (a finished restore's final form included) carries no
	// restore-in-progress signal, so the prune runs normally.
	f.write(t, remote.MarkerName, []byte(`{"url":"s3://archive","version":1,"partial":true}`))

	stats := f.env.SyncArchive(t.Context())

	require.Equal(t, 1, stats.Pruned, "a settled marker must not block the prune")
	assert.Len(t, f.fake.Deleted(), 1)
}
