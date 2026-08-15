package orgscope_test

import (
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-tfe/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/manifest"
)

// policySource serves one policy's source download, counting the requests it
// answers and letting a test replace the content between archive passes the way
// an upload replaces it upstream. It is safe for concurrent use: the archive
// reads it from the client's goroutines.
type policySource struct {
	content  string
	status   int
	requests int
	mu       sync.Mutex
}

// serve answers a download with the current content, or with the pending status
// when one is set, consuming it so only the first request fails.
func (p *policySource) serve(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.requests++

	if p.status != 0 {
		status := p.status
		p.status = 0

		w.WriteHeader(status)

		return
	}

	//nolint:errcheck // A write to the test client's connection is not what these tests assert.
	w.Write([]byte(p.content))
}

// replace stands in for an upload rewriting the policy's source in place.
func (p *policySource) replace(content string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.content = content
}

// downloads returns how many download requests the archive has made.
func (p *policySource) downloads() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.requests
}

// assertSources asserts the archive's policies directory holds exactly the
// files want names, each with its content and each recorded done.
func (f orgFixture) assertSources(t *testing.T, want map[string]string) {
	t.Helper()

	names, err := os.ReadDir(f.store.AbsPath("policies"))
	require.NoError(t, err)

	got := make(map[string]string, len(names))

	for _, name := range names {
		relPath := "policies/" + name.Name()

		content, err := os.ReadFile(f.store.AbsPath(relPath))
		require.NoError(t, err)

		got[relPath] = string(content)
	}

	assert.Equal(t, want, got)

	for relPath := range want {
		entry, ok := f.ledger.Entry(relPath)
		require.True(t, ok, "%s should have a ledger entry", relPath)
		assert.Equal(t, manifest.StatusDone, entry.Status, "%s should be recorded done", relPath)
	}
}

// fetchedAt is the fixed clock reading every outcome the fixture ledger records
// carries, so a policy updated after it names a new source revision and one
// updated before it does not.
var fetchedAt = time.Date(2026, 8, 14, 10, 15, 0, 0, time.UTC)

// The source before and after the upload each case models.
const (
	beforeUpload = "main = rule { true }"
	afterUpload  = "main = rule { false }"
)

func TestCollectPolicySource(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		kind      tfe.PolicyKind
		before    time.Time // the policy's updated-at on the first pass
		after     time.Time // the policy's updated-at on the passes that follow
		want      map[string]string
		downloads int
	}{
		"unchanged policy keeps its one source file": {
			kind:   tfe.Sentinel,
			before: fetchedAt.Add(-time.Hour),
			after:  fetchedAt.Add(-time.Hour),
			want: map[string]string{
				"policies/pol-1.sentinel": beforeUpload,
			},
			downloads: 1,
		},
		"replaced source archives the new revision beside the old": {
			kind:   tfe.Sentinel,
			before: fetchedAt.Add(-time.Hour),
			after:  fetchedAt.Add(time.Hour),
			want: map[string]string{
				"policies/pol-1.sentinel":                  beforeUpload,
				"policies/pol-1.20260814T111500Z.sentinel": afterUpload,
			},
			downloads: 2,
		},
		"opa revision keeps the rego extension": {
			kind:   tfe.OPA,
			before: fetchedAt.Add(-time.Hour),
			after:  fetchedAt.Add(time.Hour),
			want: map[string]string{
				"policies/pol-1.rego":                  beforeUpload,
				"policies/pol-1.20260814T111500Z.rego": afterUpload,
			},
			downloads: 2,
		},
		"unset updated-at keeps the plain name": {
			kind:   tfe.Sentinel,
			before: time.Time{},
			after:  time.Time{},
			want: map[string]string{
				"policies/pol-1.sentinel": beforeUpload,
			},
			downloads: 1,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			src := &policySource{content: beforeUpload}
			f := newPolicyFixture(t, src)

			require.NoError(t, f.collector.CollectPolicySource(t.Context(),
				&tfe.Policy{ID: "pol-1", Kind: tc.kind, UpdatedAt: tc.before}))

			// The source is replaced upstream, and the two passes that follow stand
			// for the runs after it: the first archives the replacement when the
			// policy reports it changed, and the second must settle for what is
			// already archived rather than downloading a revision twice.
			src.replace(afterUpload)

			replaced := &tfe.Policy{ID: "pol-1", Kind: tc.kind, UpdatedAt: tc.after}

			require.NoError(t, f.collector.CollectPolicySource(t.Context(), replaced))
			require.NoError(t, f.collector.CollectPolicySource(t.Context(), replaced))

			f.assertSources(t, tc.want)
			assert.Equal(t, tc.downloads, src.downloads(), "policy source downloads")
		})
	}
}

func TestCollectPolicySourceRetriesFailedCapture(t *testing.T) {
	t.Parallel()

	// A capture that failed leaves the plain name unsettled, so the next run
	// retries it there rather than archiving the same revision under a new name
	// and stranding the failure forever.
	src := &policySource{content: beforeUpload, status: http.StatusInternalServerError}
	f := newPolicyFixture(t, src)

	policy := &tfe.Policy{ID: "pol-1", Kind: tfe.Sentinel, UpdatedAt: fetchedAt.Add(-time.Hour)}

	require.NoError(t, f.collector.CollectPolicySource(t.Context(), policy))

	entry, ok := f.ledger.Entry("policies/pol-1.sentinel")
	require.True(t, ok)
	require.Equal(t, manifest.StatusErrored, entry.Status)

	// The retry sees a policy the API now reports as updated, which is what a
	// failure followed by an upload looks like from here.
	policy.UpdatedAt = fetchedAt.Add(time.Hour)

	require.NoError(t, f.collector.CollectPolicySource(t.Context(), policy))

	f.assertSources(t, map[string]string{"policies/pol-1.sentinel": beforeUpload})
}

// newPolicyFixture builds a fixture whose fake server answers pol-1's source
// download from src.
func newPolicyFixture(t *testing.T, src *policySource) orgFixture {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/policies/pol-1/download", src.serve)

	return newOrgFixtureServer(t, mux, fetchedAt)
}
