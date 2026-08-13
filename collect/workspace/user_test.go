package workspace_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/hashicorp/go-tfe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"go.jacobcolvin.com/hcp_archiver/manifest"
)

func TestArchiveRunEventsArchivesActors(t *testing.T) {
	t.Parallel()

	events := []*tfe.RunEvent{
		{ID: "re-1", Action: "created", Actor: &tfe.User{ID: "user-1", Username: "alice"}},
		{ID: "re-2", Action: "applied", Actor: &tfe.User{ID: "user-2", Username: "bob"}},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/runs/run-1/run-events", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONAPI(t, w, marshalJSONAPI(t, events))
	})

	f := newWSFixture(t, mux)
	st := f.store

	require.NoError(t, f.collector.ArchiveRunEvents(t.Context(), "proj", "ws", &tfe.Run{ID: "run-1"}))

	// The event timeline is archived, and each hydrated actor becomes its own
	// users/<id>.json rather than a permanently-opaque id ref.
	assert.Equal(t, []string{"re-1", "re-2"},
		f.dataIDs(t, st.RunFile("proj", "ws", "run-1", "run-events.json")))

	alice := f.attrs(t, st.User("user-1"), "users", "user-1")
	assert.Equal(t, "alice", alice["username"])

	bob := f.attrs(t, st.User("user-2"), "users", "user-2")
	assert.Equal(t, "bob", bob["username"])
}

func TestArchiveUserConcurrentReferencesClaimOneWrite(t *testing.T) {
	t.Parallel()

	// One creator or actor recurs across the runs of a page and across the
	// workspaces the shared collector fans out over, so the same user arrives
	// from many goroutines at once. The claim must hand the write to one of
	// them and leave the object settled by the time every caller returns: the
	// reference gate each caller mirrors its write into reads exactly that.
	f := newWSFixture(t, http.NewServeMux())
	st := f.store

	user := &tfe.User{ID: "user-1", Username: "alice", Email: "alice@example.com"}

	var g errgroup.Group

	settled := make([]bool, 16)

	for i := range settled {
		g.Go(func() error {
			err := f.collector.ArchiveUser(t.Context(), user)
			if err != nil {
				return fmt.Errorf("archive user: %w", err)
			}

			settled[i] = f.status(st.User("user-1")) == manifest.StatusDone

			return nil
		})
	}

	require.NoError(t, g.Wait())

	for i, ok := range settled {
		assert.Truef(t, ok, "caller %d returned before the user settled", i)
	}

	alice := f.attrs(t, st.User("user-1"), "users", "user-1")
	assert.Equal(t, "alice", alice["username"])

	exists, err := st.Exists(st.HistoryPath(st.User("user-1")))
	require.NoError(t, err)
	assert.False(t, exists, "one write per run leaves nothing to supersede")
}

func TestArchiveUserNilIsNoOp(t *testing.T) {
	t.Parallel()

	f := newWSFixture(t, http.NewServeMux())

	require.NoError(t, f.collector.ArchiveUser(t.Context(), nil))
}
