package stacks_test

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tfe "github.com/hashicorp/go-tfe"

	"go.jacobcolvin.com/hcp_archiver/pkg/collect/stacks"
	"go.jacobcolvin.com/hcp_archiver/pkg/logtest"
	"go.jacobcolvin.com/hcp_archiver/pkg/manifest"
)

// unresolvedWarning is the log message the collector emits when it excludes a
// stack on a stand-in for its project name.
const unresolvedWarning = "stack project name unresolved; excluded by the project filter"

// skipWarning is the log message the collector emits when it swallows a failure
// and records the surface it could not archive.
const skipWarning = "skipping stacks object after failure"

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

func TestCollectStackWhoseProjectDoesNotRead(t *testing.T) {
	t.Parallel()

	// A stack's whole archive path hangs off its project's display name, so how
	// a failed project read is classified decides whether the stack has a
	// directory at all. A gone project is an answer every later run repeats, so
	// its id keys the path; anything a later run could answer differently must
	// not, or the id-keyed tree outlives the blip as a duplicate of the
	// name-keyed one the next run writes, with no rename breadcrumb between them
	// (the two hang off different projects, and a claim only compares siblings).
	for name, tc := range map[string]struct {
		status       int
		projects     []string
		wantArchived bool
		wantExcluded bool
	}{
		"a blip leaves the stack for a later run": {
			status: http.StatusInternalServerError,
		},
		"an access denial leaves the stack for a broader token": {
			status: http.StatusForbidden,
		},
		"a gone project keys the stack on its id": {
			status:       http.StatusNotFound,
			wantArchived: true,
		},
		"a blip under a filter leaves the stack for a later run": {
			status:   http.StatusInternalServerError,
			projects: []string{"keep"},
		},
		"a gone project no filter admits is excluded": {
			status:       http.StatusNotFound,
			projects:     []string{"keep"},
			wantExcluded: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			mux := http.NewServeMux()

			mux.HandleFunc("/api/v2/organizations/example-org/stacks", func(w http.ResponseWriter, _ *http.Request) {
				writeList(t, w, stackItem("st-blip", "alpha", "prj-blip"))
			})

			mux.HandleFunc("/api/v2/projects/prj-blip", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			})

			// A skipped stack must cost no API spend either: its collections
			// never open, so nothing under it is ever requested.
			mux.HandleFunc("/api/v2/stacks/st-blip/", func(w http.ResponseWriter, r *http.Request) {
				if !tc.wantArchived {
					t.Errorf("unexpected request for skipped stack: %s", r.URL.Path)

					return
				}

				writeEmptyList(t, w)
			})

			rec := logtest.NewRecorder()
			opts := []stacks.Option{stacks.WithLogger(rec.Logger())}

			if tc.projects != nil {
				opts = append(opts, stacks.WithProjects(tc.projects))
			}

			f := newStacksFixture(t, mux, opts...)
			st := f.store

			require.NoError(t, f.collector.Collect(t.Context()))

			want := manifest.Status("")
			if tc.wantArchived {
				want = manifest.StatusDone
			}

			assert.Equal(t, want, f.status(st.StackFile("prj-blip", "alpha", "stack.json")),
				"only a stable stand-in for the project name keys the stack's archive path")

			if !tc.wantArchived {
				exists, err := st.Exists("projects")
				require.NoError(t, err)
				assert.False(t, exists, "a skipped stack leaves no id-keyed tree behind")
			}

			// Either way the run must not close clean over the stack it could
			// not place: the drop is the operator's signal, and on the skip
			// path it is what brings a later run back to it.
			dropped := f.ledger.DroppedSurfaces()

			if tc.wantArchived {
				assert.Empty(t, dropped, "a stack that archived dropped nothing")
			} else {
				require.Len(t, dropped, 1)
				assert.Equal(t, st.StackDir("prj-blip", "alpha"), dropped[0].Surface)
				assert.Positive(t, f.ledger.Tally().SurfacesDropped)
			}

			// The two skips are different events: one is the filter judging a
			// stand-in it cannot match, the other is the collector refusing to
			// place a stack at all, so each announces itself in its own words.
			excluded, skipped := 0, 0

			switch {
			case tc.wantExcluded:
				excluded = 1
			case !tc.wantArchived:
				skipped = 1
			}

			assert.Len(t, rec.Events(unresolvedWarning), excluded)
			assert.Len(t, rec.Events(skipWarning), skipped)
		})
	}
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

	_, _, err := f.collector.ResolveProjectForTest(t.Context(), stack)
	require.ErrorIs(t, err, stacks.ErrProjectUnresolved,
		"a read that failed on a retryable answer resolves no name at all")

	// A cached failure would freeze the whole project's stacks under its id for
	// the rest of the run, so the blip must not be remembered.
	name, named, err := f.collector.ResolveProjectForTest(t.Context(), stack)
	require.NoError(t, err)
	assert.Equal(t, "keep", name, "the failure was not cached, so the retry resolves the name")
	assert.True(t, named)

	name, named, err = f.collector.ResolveProjectForTest(t.Context(), stack)
	require.NoError(t, err)
	assert.Equal(t, "keep", name)
	assert.True(t, named, "the cache hit carries the display-name flag, not just the name")
	assert.Equal(t, int32(2), reads.Load(), "the resolved name was served from the cache")
}

func TestResolveProjectCachesGoneProjects(t *testing.T) {
	t.Parallel()

	// A project that reads back gone is as stable an answer as one that reads
	// back nameless: no later run resolves a name for it, so the id caches and
	// keeps standing in for the segment. Without this the stack would be skipped
	// on every run from here on, and its archive would stop being updated over a
	// gap no operator can close.
	var reads atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/projects/prj-gone", func(w http.ResponseWriter, _ *http.Request) {
		reads.Add(1)
		w.WriteHeader(http.StatusNotFound)
	})

	f := newStacksFixture(t, mux)
	stack := &tfe.Stack{ID: "st-1", Name: "alpha", Project: &tfe.Project{ID: "prj-gone"}}

	name, named, err := f.collector.ResolveProjectForTest(t.Context(), stack)
	require.NoError(t, err, "a gone project is an answer, not a lookup to retry")
	assert.Equal(t, "prj-gone", name, "the id stands in for the name of a project that is gone")
	assert.False(t, named)

	name, _, err = f.collector.ResolveProjectForTest(t.Context(), stack)
	require.NoError(t, err)
	assert.Equal(t, "prj-gone", name)
	assert.Equal(t, int32(1), reads.Load(), "a gone project is cached, not re-read")
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

	name, named, err := f.collector.ResolveProjectForTest(t.Context(), stack)
	require.NoError(t, err)
	assert.Equal(t, "prj-empty", name, "the id stands in for the missing name")
	assert.False(t, named)

	name, named, err = f.collector.ResolveProjectForTest(t.Context(), stack)
	require.NoError(t, err)
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

	name, named, err := f.collector.ResolveProjectForTest(t.Context(), &tfe.Stack{ID: "st-1", Name: "alpha"})

	require.NoError(t, err, "a stack naming no project needs no lookup to fail")
	assert.Equal(t, "unknown-project", name)
	assert.False(t, named, "the sentinel is not a display name, so a filter rejects it")
}
