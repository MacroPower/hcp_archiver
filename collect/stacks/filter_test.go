package stacks_test

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tfe "github.com/hashicorp/go-tfe"

	"go.jacobcolvin.com/hcp_archiver/collect/stacks"
	"go.jacobcolvin.com/hcp_archiver/logtest"
	"go.jacobcolvin.com/hcp_archiver/manifest"
)

// unresolvedWarning is the log message the collector emits when it excludes a
// stack on a stand-in for its project name.
const unresolvedWarning = "stack project name unresolved; excluded by the project filter"

// stackItem renders a stack list entry carrying only the name and the project
// relation the collector reads. The relation needs no sideloaded project: the
// collector resolves the display name by reading the project itself.
func stackItem(id, name, projectID string) string {
	return `{"type":"stacks","id":"` + id + `","attributes":{"name":"` + name + `"},` +
		`"relationships":{"project":{"data":{"type":"projects","id":"` + projectID + `"}}}}`
}

// writeProject answers a project read with a JSON:API single-resource document.
func writeProject(t *testing.T, w http.ResponseWriter, id, name string) {
	t.Helper()

	w.Header().Set("Content-Type", "application/vnd.api+json")

	_, err := w.Write([]byte(`{"data":{"type":"projects","id":"` + id + `","attributes":{"name":"` + name + `"}}}`))
	require.NoError(t, err)
}

// filterMux serves an organization holding two stacks in one project and a
// third in another, with both projects readable. Two stacks sharing a project
// keeps the shared lookup on the path under test; which one takes the cache
// miss is a race under the fan-out, so nothing here asserts a read count. The
// drop handler receives every request aimed at the lone stack of the second
// project, so each caller decides whether reaching it is an error.
func filterMux(t *testing.T, drop http.HandlerFunc) *http.ServeMux {
	t.Helper()

	mux := http.NewServeMux()

	mux.HandleFunc("/api/v2/organizations/example-org/stacks", func(w http.ResponseWriter, _ *http.Request) {
		writeList(t, w,
			stackItem("st-keep", "alpha", "prj-keep"),
			stackItem("st-alt", "gamma", "prj-keep"),
			stackItem("st-drop", "beta", "prj-drop"),
		)
	})

	mux.HandleFunc("/api/v2/projects/prj-keep", func(w http.ResponseWriter, _ *http.Request) {
		writeProject(t, w, "prj-keep", "keep")
	})

	mux.HandleFunc("/api/v2/projects/prj-drop", func(w http.ResponseWriter, _ *http.Request) {
		writeProject(t, w, "prj-drop", "drop")
	})

	// A stack's configurations, deployments, and states all hang off its own
	// subtree, so one empty-listing handler covers each admitted stack's walk.
	for _, id := range []string{"st-keep", "st-alt"} {
		mux.HandleFunc("/api/v2/stacks/"+id+"/", func(w http.ResponseWriter, _ *http.Request) {
			writeEmptyList(t, w)
		})
	}

	mux.HandleFunc("/api/v2/stacks/st-drop/", drop)

	return mux
}

func TestCollectSkipsStacksOutsideProjectFilter(t *testing.T) {
	t.Parallel()

	// Every request aimed at the excluded stack fails the test: the filter must
	// spare the API spend, not just the disk. A bare unregistered path would not
	// do, since the 404s it answers with are swallowed as tolerated absences.
	mux := filterMux(t, func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request for excluded stack: %s", r.URL.Path)
	})

	rec := logtest.NewRecorder()
	f := newStacksFixture(t, mux,
		stacks.WithProjects([]string{"keep"}), stacks.WithLogger(rec.Logger()))
	st := f.store

	require.NoError(t, f.collector.Collect(t.Context()))

	assert.Equal(t, manifest.StatusDone, f.status(st.StackFile("keep", "alpha", "stack.json")),
		"the admitted project's stack still archives")

	assert.Equal(t, manifest.StatusDone, f.status(st.StackFile("keep", "gamma", "stack.json")),
		"a second stack in the admitted project archives too")

	assert.Equal(t, manifest.Status(""), f.status(st.StackFile("drop", "beta", "stack.json")),
		"the excluded stack archives nothing")

	// The skip lands ahead of the directory claim, which would otherwise leave a
	// project directory behind, holding nothing but the excluded stack's claimed
	// directory and its identity sidecar.
	exists, err := st.Exists(st.ProjectDir("drop"))
	require.NoError(t, err)
	assert.False(t, exists, "the excluded project leaves no directory behind")

	assert.Empty(t, rec.Events(unresolvedWarning),
		"an ordinary name mismatch is the filter working, not something to warn about")
}

func TestCollectSkipsStackWhoseProjectNameIsUnresolved(t *testing.T) {
	t.Parallel()

	// The project read fails, so the collector falls back to the raw project id,
	// which no name filter can admit. Under a filter the stack is excluded
	// rather than archived outside the boundary the operator configured.
	mux := http.NewServeMux()

	mux.HandleFunc("/api/v2/organizations/example-org/stacks", func(w http.ResponseWriter, _ *http.Request) {
		writeList(t, w, stackItem("st-blip", "alpha", "prj-blip"))
	})

	mux.HandleFunc("/api/v2/projects/prj-blip", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	mux.HandleFunc("/api/v2/stacks/st-blip/", func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request for excluded stack: %s", r.URL.Path)
	})

	rec := logtest.NewRecorder()
	f := newStacksFixture(t, mux,
		stacks.WithProjects([]string{"keep"}), stacks.WithLogger(rec.Logger()))
	st := f.store

	require.NoError(t, f.collector.Collect(t.Context()))

	assert.Equal(t, manifest.Status(""), f.status(st.StackFile("prj-blip", "alpha", "stack.json")),
		"an unresolvable project name is excluded, not archived under its id")

	exists, err := st.Exists("projects")
	require.NoError(t, err)
	assert.False(t, exists, "nothing is archived under any project")

	// Nothing else records this exclusion, so the warning is the operator's only
	// evidence that a stack was dropped on an answer the filter never got.
	assert.Len(t, rec.Events(unresolvedWarning), 1,
		"an exclusion the operator did not ask for is announced")
}

func TestCollectArchivesUnresolvedProjectWithoutFilter(t *testing.T) {
	t.Parallel()

	// Same failing project read as above, but no filter configured. The id
	// standing in for the name is only a problem for a filter matching on names,
	// so with none set the stack still archives under the id. This guards the
	// unresolved flag from growing into a gate on the unfiltered path.
	mux := http.NewServeMux()

	mux.HandleFunc("/api/v2/organizations/example-org/stacks", func(w http.ResponseWriter, _ *http.Request) {
		writeList(t, w, stackItem("st-blip", "alpha", "prj-blip"))
	})

	mux.HandleFunc("/api/v2/projects/prj-blip", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	mux.HandleFunc("/api/v2/stacks/st-blip/", func(w http.ResponseWriter, _ *http.Request) {
		writeEmptyList(t, w)
	})

	rec := logtest.NewRecorder()
	f := newStacksFixture(t, mux, stacks.WithLogger(rec.Logger()))
	st := f.store

	require.NoError(t, f.collector.Collect(t.Context()))

	assert.Equal(t, manifest.StatusDone, f.status(st.StackFile("prj-blip", "alpha", "stack.json")),
		"with no filter the stack archives under the id standing in for its project name")

	assert.Empty(t, rec.Events(unresolvedWarning),
		"an unresolved name costs nothing when no filter can reject it")
}

func TestCollectArchivesEveryStackWithoutProjectFilter(t *testing.T) {
	t.Parallel()

	// No filter configured, so every stack archives: an empty list admits every
	// project, which is what keeps the zero configuration whole.
	mux := filterMux(t, func(w http.ResponseWriter, _ *http.Request) {
		writeEmptyList(t, w)
	})

	f := newStacksFixture(t, mux)
	st := f.store

	require.NoError(t, f.collector.Collect(t.Context()))

	assert.Equal(t, manifest.StatusDone, f.status(st.StackFile("keep", "alpha", "stack.json")))
	assert.Equal(t, manifest.StatusDone, f.status(st.StackFile("keep", "gamma", "stack.json")))
	assert.Equal(t, manifest.StatusDone, f.status(st.StackFile("drop", "beta", "stack.json")))
}

func TestResolveProjectCachesNamesButNotReadFailures(t *testing.T) {
	t.Parallel()

	// Resolution drives both the archive path and the filter decision, so both
	// halves of its answer, the path segment and the display-name flag, have to
	// survive the cache intact. Driving it directly rather than through Collect
	// keeps the sequence deterministic: under the concurrent fan-out, which stack
	// takes the cache miss is a race.
	var reads atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/projects/prj-keep", func(w http.ResponseWriter, _ *http.Request) {
		if reads.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)

			return
		}

		writeProject(t, w, "prj-keep", "keep")
	})

	f := newStacksFixture(t, mux)
	stack := &tfe.Stack{ID: "st-1", Name: "alpha", Project: &tfe.Project{ID: "prj-keep"}}

	name, named := f.collector.ResolveProjectForTest(t.Context(), stack)
	assert.Equal(t, "prj-keep", name, "a failed read falls back to the id")
	assert.False(t, named, "an id is not a display name")

	// A cached failure would freeze the whole project's stacks under its id for
	// the rest of the run, so the blip must not be remembered.
	name, named = f.collector.ResolveProjectForTest(t.Context(), stack)
	assert.Equal(t, "keep", name, "the failure was not cached, so the retry resolves the name")
	assert.True(t, named)

	name, named = f.collector.ResolveProjectForTest(t.Context(), stack)
	assert.Equal(t, "keep", name)
	assert.True(t, named, "the cache hit carries the display-name flag, not just the name")
	assert.Equal(t, int32(2), reads.Load(), "the resolved name was served from the cache")
}

func TestResolveProjectCachesProjectsWithNoName(t *testing.T) {
	t.Parallel()

	// A project that reads back without a name is a stable answer, not a blip,
	// so the id standing in for it caches and no later stack re-reads it. The
	// segment stays the id, which no allow-list of names admits, so this
	// project's stacks drop out of a filtered run deliberately rather than on a
	// failed read.
	var reads atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/projects/prj-empty", func(w http.ResponseWriter, _ *http.Request) {
		reads.Add(1)
		writeProject(t, w, "prj-empty", "")
	})

	f := newStacksFixture(t, mux)
	stack := &tfe.Stack{ID: "st-1", Name: "alpha", Project: &tfe.Project{ID: "prj-empty"}}

	name, named := f.collector.ResolveProjectForTest(t.Context(), stack)
	assert.Equal(t, "prj-empty", name, "the id stands in for the missing name")
	assert.False(t, named)

	name, named = f.collector.ResolveProjectForTest(t.Context(), stack)
	assert.Equal(t, "prj-empty", name)
	assert.False(t, named, "the cached stand-in is still not a display name")
	assert.Equal(t, int32(1), reads.Load(), "a stable empty name is cached, not re-read")
}

func TestResolveProjectWithoutProjectRelation(t *testing.T) {
	t.Parallel()

	// A stack listing that carries no project relation has no id to read or to
	// key a cache on, so resolution answers with the sentinel the archive path
	// falls back to. No endpoint is registered: reaching one would be a bug.
	f := newStacksFixture(t, http.NewServeMux())

	name, named := f.collector.ResolveProjectForTest(t.Context(), &tfe.Stack{ID: "st-1", Name: "alpha"})

	assert.Equal(t, "unknown-project", name)
	assert.False(t, named, "the sentinel is not a display name, so a filter rejects it")
}
