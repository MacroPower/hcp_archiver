package tfeclient_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/hashicorp/go-tfe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/tfeclient"
)

// roundTripFunc adapts a function to an [http.RoundTripper].
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// newOfflineClient builds a [tfeclient.Client] whose HTTP transport answers
// go-tfe's constructor ping locally, so no test touches the network.
func newOfflineClient(t *testing.T, opts ...tfeclient.Option) *tfeclient.Client {
	t.Helper()

	hc := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    r,
			}, nil
		}),
	}

	base := []tfeclient.Option{
		tfeclient.WithToken("test-token"),
		tfeclient.WithHTTPClient(hc),
	}

	c, err := tfeclient.New(append(base, opts...)...)
	require.NoError(t, err)

	return c
}

func TestNewMissingToken(t *testing.T) {
	t.Parallel()

	c, err := tfeclient.New(tfeclient.WithAddress("https://example.com"))

	require.ErrorIs(t, err, tfeclient.ErrMissingToken)
	assert.Nil(t, c)
}

func TestNewWiresUnderlyingClient(t *testing.T) {
	t.Parallel()

	c := newOfflineClient(t)

	assert.NotNil(t, c.TFE())
}

func TestDoInvokesFnAndReturnsError(t *testing.T) {
	t.Parallel()

	c := newOfflineClient(t)

	t.Run("returns fn error and passes the client", func(t *testing.T) {
		t.Parallel()

		sentinel := errors.New("from fn")

		var got *tfe.Client

		err := c.Do(t.Context(), func(_ context.Context, tc *tfe.Client) error {
			got = tc

			return sentinel
		})

		require.ErrorIs(t, err, sentinel)
		assert.Same(t, c.TFE(), got)
	})

	t.Run("returns nil when fn succeeds", func(t *testing.T) {
		t.Parallel()

		called := false

		err := c.Do(t.Context(), func(_ context.Context, _ *tfe.Client) error {
			called = true

			return nil
		})

		require.NoError(t, err)
		assert.True(t, called)
	})

	t.Run("canceled context is not passed to fn", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		called := false

		err := c.Do(ctx, func(_ context.Context, _ *tfe.Client) error {
			called = true

			return nil
		})

		require.Error(t, err)
		assert.False(t, called)
	})
}

func TestResponseHeaderTimeoutBoundsStalledConnection(t *testing.T) {
	t.Parallel()

	// The server blocks before writing any response header, so with a tiny header
	// timeout the request must fail promptly rather than hang the worker.
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	// Cleanups run last-registered-first: unblock the handler before closing the
	// server, so the close does not deadlock on the still-blocked request.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	// Server-error retry is disabled so the elapsed bound measures the header
	// timeout alone rather than the retry backoff on top of it.
	client := tfeclient.ResolveHTTPClient(
		tfeclient.WithResponseHeaderTimeout(50*time.Millisecond),
		tfeclient.WithServerErrorRetry(0, 0),
	)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, http.NoBody)
	require.NoError(t, err)

	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)

	if resp != nil {
		require.NoError(t, resp.Body.Close())
	}

	require.Error(t, err, "a stalled connection must fail rather than hang")
	assert.Less(t, elapsed, 5*time.Second, "the failure is bounded by the header timeout")
}

func TestResponseHeaderTimeoutAllowsSlowBody(t *testing.T) {
	t.Parallel()

	// Headers arrive at once, then the body dribbles out over writes that in total
	// far exceed the header timeout. The transfer must still succeed: the timeout
	// bounds only the time to first byte, so large streaming downloads are safe.
	const chunks = 5

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}

		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		for range chunks {
			time.Sleep(20 * time.Millisecond)

			_, werr := io.WriteString(w, "chunk")
			if werr != nil {
				return
			}

			flusher.Flush()
		}
	}))
	t.Cleanup(srv.Close)

	client := tfeclient.ResolveHTTPClient(tfeclient.WithResponseHeaderTimeout(10 * time.Millisecond))

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, http.NoBody)
	require.NoError(t, err)

	resp, err := client.Do(req)
	require.NoError(t, err)

	defer func() { assert.NoError(t, resp.Body.Close()) }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("chunk", chunks), string(body))
}

func TestResolveHTTPClientTransportTuning(t *testing.T) {
	t.Parallel()

	// The built transport must keep the connection-churn tuning: a handshake
	// bound generous enough to survive a saturated link, and a per-host idle pool
	// wide enough that HTTP/1 chunked log reads reuse connections instead of
	// redialing (and re-handshaking) per chunk.
	tr := tfeclient.UnderlyingTransport(tfeclient.ResolveHTTPClient())
	require.NotNil(t, tr)

	assert.Equal(t, tfeclient.DefaultTLSHandshakeTimeout, tr.TLSHandshakeTimeout)
	assert.Equal(t, tr.MaxIdleConns, tr.MaxIdleConnsPerHost,
		"every pooled connection may idle per host")
	assert.False(t, tr.ForceAttemptHTTP2,
		"HTTP/2 stays disabled so the header and idle-read bounds keep working")
	assert.Equal(t, tfeclient.DefaultResponseHeaderTimeout, tr.ResponseHeaderTimeout)
}

func TestPaginate(t *testing.T) {
	t.Parallel()

	c := newOfflineClient(t)

	t.Run("accumulates all pages and halts at next page zero", func(t *testing.T) {
		t.Parallel()

		pages := [][]int{{1, 2}, {3, 4}, {5, 6}}
		nextPage := []int{2, 3, 0}

		var (
			calls    int
			seenPage []int
		)

		fetch := func(
			_ context.Context,
			_ *tfe.Client,
			opts tfe.ListOptions,
		) ([]int, *tfe.Pagination, error) {
			seenPage = append(seenPage, opts.PageNumber)
			idx := calls
			calls++

			return pages[idx], &tfe.Pagination{
				CurrentPage: opts.PageNumber,
				NextPage:    nextPage[idx],
			}, nil
		}

		got, err := tfeclient.Paginate(t.Context(), c, fetch)

		require.NoError(t, err)
		assert.Equal(t, []int{1, 2, 3, 4, 5, 6}, got)
		assert.Equal(t, 3, calls)
		assert.Equal(t, []int{1, 2, 3}, seenPage)
	})

	t.Run("propagates fetch error", func(t *testing.T) {
		t.Parallel()

		sentinel := errors.New("fetch broke")

		fetch := func(
			_ context.Context,
			_ *tfe.Client,
			_ tfe.ListOptions,
		) ([]int, *tfe.Pagination, error) {
			return nil, nil, sentinel
		}

		got, err := tfeclient.Paginate(t.Context(), c, fetch)

		require.ErrorIs(t, err, sentinel)
		assert.Nil(t, got)
	})

	t.Run("nil pagination halts after one page", func(t *testing.T) {
		t.Parallel()

		fetch := func(
			_ context.Context,
			_ *tfe.Client,
			_ tfe.ListOptions,
		) ([]int, *tfe.Pagination, error) {
			return []int{7}, nil, nil
		}

		got, err := tfeclient.Paginate(t.Context(), c, fetch)

		require.NoError(t, err)
		assert.Equal(t, []int{7}, got)
	})

	t.Run("non-advancing next page is an error, not a truncated listing", func(t *testing.T) {
		t.Parallel()

		var calls int

		fetch := func(
			_ context.Context,
			_ *tfe.Client,
			opts tfe.ListOptions,
		) ([]int, *tfe.Pagination, error) {
			calls++

			return []int{calls}, &tfe.Pagination{
				CurrentPage: opts.PageNumber,
				NextPage:    opts.PageNumber, // claims more pages but never advances
			}, nil
		}

		got, err := tfeclient.Paginate(t.Context(), c, fetch)

		require.ErrorIs(t, err, tfeclient.ErrPaginationStalled)
		assert.Nil(t, got, "a stalled pagination must not surface a partial listing as complete")
		assert.Equal(t, 1, calls, "the stall is detected on the first non-advancing page")
	})
}

// recordingGate records the order of gate and fn events so tests can assert
// that Do brackets fn with an acquire and a release.
type recordingGate struct {
	mu         sync.Mutex
	events     []string
	acquireErr error
}

func (g *recordingGate) Acquire(_ context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.acquireErr != nil {
		return g.acquireErr
	}

	g.events = append(g.events, "acquire")

	return nil
}

func (g *recordingGate) Release() {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.events = append(g.events, "release")
}

func (g *recordingGate) record(event string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.events = append(g.events, event)
}

func TestDoGate(t *testing.T) {
	t.Parallel()

	t.Run("brackets fn with acquire and release", func(t *testing.T) {
		t.Parallel()

		gate := &recordingGate{}
		c := newOfflineClient(t, tfeclient.WithGate(gate))

		err := c.Do(t.Context(), func(_ context.Context, _ *tfe.Client) error {
			gate.record("fn")

			return nil
		})
		require.NoError(t, err)

		assert.Equal(t, []string{"acquire", "fn", "release"}, gate.events)
	})

	t.Run("releases the slot when fn errors", func(t *testing.T) {
		t.Parallel()

		gate := &recordingGate{}
		c := newOfflineClient(t, tfeclient.WithGate(gate))

		sentinel := errors.New("from fn")

		err := c.Do(t.Context(), func(_ context.Context, _ *tfe.Client) error {
			return sentinel
		})
		require.ErrorIs(t, err, sentinel)

		assert.Equal(t, []string{"acquire", "release"}, gate.events)
	})

	t.Run("acquire error propagates and skips fn", func(t *testing.T) {
		t.Parallel()

		sentinel := errors.New("gate closed")
		gate := &recordingGate{acquireErr: sentinel}
		c := newOfflineClient(t, tfeclient.WithGate(gate))

		called := false

		err := c.Do(t.Context(), func(_ context.Context, _ *tfe.Client) error {
			called = true

			return nil
		})

		require.ErrorIs(t, err, sentinel)
		assert.False(t, called, "fn must not run without a slot")
		assert.Empty(t, gate.events, "a denied acquire leaves nothing to release")
	})
}

// newServerClient builds a [tfeclient.Client] against a live test server
// serving mux, answering the constructor ping itself, so download methods
// exercise the real request path end to end.
func newServerClient(t *testing.T, mux *http.ServeMux) *tfeclient.Client {
	t.Helper()

	c, _ := newServerClientURL(t, mux)

	return c
}

// newServerClientURL is [newServerClient] that also returns the server's base
// URL, for exercising the Open methods that fetch an absolute signed URL.
func newServerClientURL(t *testing.T, mux *http.ServeMux) (*tfeclient.Client, string) {
	t.Helper()

	mux.HandleFunc("/api/v2/ping", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := tfeclient.New(tfeclient.WithToken("test-token"), tfeclient.WithAddress(srv.URL))
	require.NoError(t, err)

	return c, srv.URL
}

func TestOpenPlanLog(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		served string
		hasURL bool
		want   string
		err    error
	}{
		"trims stx etx framing": {
			served: "\x02plan output\x03",
			hasURL: true,
			want:   "plan output",
		},
		"unframed log passes through": {
			served: "plain output",
			hasURL: true,
			want:   "plain output",
		},
		"etx kept without a leading stx": {
			served: "tail\x03",
			hasURL: true,
			want:   "tail\x03",
		},
		"missing log url": {
			err: tfeclient.ErrMissingLogURL,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var artifactHits atomic.Int64

			mux := http.NewServeMux()
			mux.HandleFunc("/api/v2/plans/plan-1", func(w http.ResponseWriter, r *http.Request) {
				logURL := ""
				if tc.hasURL {
					logURL = "http://" + r.Host + "/artifact/plan-1"
				}

				w.Header().Set("Content-Type", "application/vnd.api+json")

				_, werr := fmt.Fprintf(
					w,
					`{"data":{"id":"plan-1","type":"plans","attributes":{"log-read-url":%q}}}`,
					logURL,
				)
				if werr != nil {
					return
				}
			})
			mux.HandleFunc("/artifact/plan-1", func(w http.ResponseWriter, r *http.Request) {
				artifactHits.Add(1)

				// Archivist rejects the SDK's JSON:API accept header with a
				// 400, so the fake does too; the download must send an accept
				// that admits text/plain.
				if !strings.Contains(r.Header.Get("Accept"), "*/*") {
					w.WriteHeader(http.StatusBadRequest)

					return
				}

				_, werr := io.WriteString(w, tc.served)
				if werr != nil {
					return
				}
			})

			c := newServerClient(t, mux)

			r, err := c.OpenPlanLog(t.Context(), "plan-1")

			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)

				return
			}

			require.NoError(t, err)

			got, err := io.ReadAll(r)
			require.NoError(t, err)
			require.NoError(t, r.Close())

			assert.Equal(t, tc.want, string(got))
			assert.Equal(t, int64(1), artifactHits.Load(), "the whole log downloads in one request")
		})
	}
}

func TestOpenApplyLog(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/applies/apply-1", func(w http.ResponseWriter, r *http.Request) {
		logURL := "http://" + r.Host + "/artifact/apply-1"

		w.Header().Set("Content-Type", "application/vnd.api+json")

		_, werr := fmt.Fprintf(w, `{"data":{"id":"apply-1","type":"applies","attributes":{"log-read-url":%q}}}`, logURL)
		if werr != nil {
			return
		}
	})
	mux.HandleFunc("/artifact/apply-1", func(w http.ResponseWriter, r *http.Request) {
		// Mirrors archivist's rejection of the JSON:API accept header; see
		// TestOpenPlanLog.
		if !strings.Contains(r.Header.Get("Accept"), "*/*") {
			w.WriteHeader(http.StatusBadRequest)

			return
		}

		_, werr := io.WriteString(w, "\x02apply output\x03")
		if werr != nil {
			return
		}
	})

	c := newServerClient(t, mux)

	r, err := c.OpenApplyLog(t.Context(), "apply-1")
	require.NoError(t, err)

	got, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())

	assert.Equal(t, "apply output", string(got))
}

func TestLogReader(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		in   string
		want string
	}{
		"framed trim":              {in: "\x02plan output\x03", want: "plan output"},
		"unframed passthrough":     {in: "plain output", want: "plain output"},
		"etx without stx is kept":  {in: "tail\x03", want: "tail\x03"},
		"interior etx unframed":    {in: "a\x03b", want: "a\x03b"},
		"empty":                    {in: "", want: ""},
		"framed empty":             {in: "\x02\x03", want: ""},
		"stx only":                 {in: "\x02", want: ""},
		"interior etx framed":      {in: "\x02a\x03b\x03", want: "a\x03b"},
		"double trailing etx kept": {in: "\x02abc\x03\x03", want: "abc\x03"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// Whole read.
			r := tfeclient.NewLogReader(io.NopCloser(strings.NewReader(tc.in)))

			got, err := io.ReadAll(r)
			require.NoError(t, err)
			require.NoError(t, r.Close())
			assert.Equal(t, tc.want, string(got))

			// One byte at a time over a one-byte-at-a-time underlying reader, to
			// exercise the hold-back across chunk boundaries in both directions.
			chunked := tfeclient.NewLogReader(io.NopCloser(iotest.OneByteReader(strings.NewReader(tc.in))))

			require.NoError(t, err)
			assert.Equal(t, tc.want, readByteAtATime(t, chunked))
			require.NoError(t, chunked.Close())
		})
	}
}

// bytesThenErr yields data once, then returns err on every subsequent read, so a
// reader can be driven to a non-EOF read fault at a chosen point in the stream.
type bytesThenErr struct {
	err  error
	data []byte
	off  int
}

func (r *bytesThenErr) Read(p []byte) (int, error) {
	if r.off < len(r.data) {
		n := copy(p, r.data[r.off:])
		r.off += n

		return n, nil
	}

	return 0, r.err
}

func TestLogReaderSurfacesPeekError(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")

	// A framed stream delivers its trailing ETX, then the underlying read faults
	// with a non-EOF error before EOF is reached. The ETX must not be emitted as
	// content; the fault surfaces instead.
	src := &bytesThenErr{data: []byte("\x02abc\x03"), err: errBoom}
	r := tfeclient.NewLogReader(io.NopCloser(src))

	got, err := io.ReadAll(r)
	require.ErrorIs(t, err, errBoom)
	assert.Equal(t, "abc", string(got))
	require.NoError(t, r.Close())
}

// readByteAtATime drains r one byte per Read call, so a reader is exercised at
// the smallest possible buffer size.
func readByteAtATime(t *testing.T, r io.Reader) string {
	t.Helper()

	var out []byte

	buf := make([]byte, 1)

	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)

		if errors.Is(err, io.EOF) {
			return string(out)
		}

		require.NoError(t, err)
	}
}

func TestOpenState(t *testing.T) {
	t.Parallel()

	// A raw payload with an embedded NUL proves the blob streams byte-for-byte,
	// unparsed and untransformed, rather than round-tripping through a decoder.
	const body = "raw state blob\x00\x01\x02 bytes"

	var hits atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("/dl/state", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)

		assert.Equal(t, "application/json", r.Header.Get("Accept"))

		_, werr := io.WriteString(w, body)
		if werr != nil {
			return
		}
	})

	c, base := newServerClientURL(t, mux)

	r, err := c.OpenState(t.Context(), base+"/dl/state")
	require.NoError(t, err)

	got, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())

	assert.Equal(t, body, string(got))
	assert.Equal(t, int64(1), hits.Load(), "the blob streams in one request")
}

func TestOpenConfigurationVersion(t *testing.T) {
	t.Parallel()

	const tarball = "\x1f\x8b\x08 fake tar.gz \x00\x01\x02"

	var hits atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/configuration-versions/cv-1/download", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)

		_, werr := io.WriteString(w, tarball)
		if werr != nil {
			return
		}
	})

	c := newServerClient(t, mux)

	r, err := c.OpenConfigurationVersion(t.Context(), "cv-1")
	require.NoError(t, err)

	got, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())

	assert.Equal(t, tarball, string(got))
	assert.Equal(t, int64(1), hits.Load(), "the tarball streams in one request")
}

func TestOpenPlanJSON(t *testing.T) {
	t.Parallel()

	// A raw payload with an embedded NUL proves the artifact streams byte-for-byte
	// rather than round-tripping through a JSON decoder.
	const body = "structured plan\x00 output"

	var hits atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/plans/plan-1/json-output", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)

		_, werr := io.WriteString(w, body)
		if werr != nil {
			return
		}
	})

	c := newServerClient(t, mux)

	r, err := c.OpenPlanJSON(t.Context(), "plan-1")
	require.NoError(t, err)

	got, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())

	assert.Equal(t, body, string(got))
	assert.Equal(t, int64(1), hits.Load(), "the structured plan streams in one request")
}

func TestOpenClassifiesStatus(t *testing.T) {
	t.Parallel()

	// DoRaw discards the SDK's typed error, so each Open method translates the
	// captured status back to the Kind the buffered path produced. 410 is left
	// unmapped on purpose, since the buffered checkResponseCode did not map it.
	tests := map[string]struct {
		status int
		want   tfeclient.Kind
	}{
		"404 is terminal":  {status: http.StatusNotFound, want: tfeclient.KindTerminal},
		"403 is forbidden": {status: http.StatusForbidden, want: tfeclient.KindForbidden},
		"401 is unknown":   {status: http.StatusUnauthorized, want: tfeclient.KindUnknown},
		"410 is unknown":   {status: http.StatusGone, want: tfeclient.KindUnknown},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			mux := http.NewServeMux()
			mux.HandleFunc("/api/v2/configuration-versions/cv-1/download",
				func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(tc.status)
				})

			c := newServerClient(t, mux)

			r, err := c.OpenConfigurationVersion(t.Context(), "cv-1")
			if r != nil {
				require.NoError(t, r.Close())
			}

			require.Error(t, err)
			assert.Equal(t, tc.want, tfeclient.Classify(err))
		})
	}
}

func TestRateLimitCounterCounts429s(t *testing.T) {
	t.Parallel()

	// The counter observes raw wire responses below any retry loop: two 429
	// answers followed by a 200 must count exactly two, and the eventual
	// success must count nothing.
	status := []int{
		http.StatusTooManyRequests,
		http.StatusTooManyRequests,
		http.StatusOK,
	}

	var mu sync.Mutex

	call := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()

		code := status[min(call, len(status)-1)]
		call++
		mu.Unlock()

		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)

	counter := new(atomic.Int64)
	client := tfeclient.ResolveHTTPClient(tfeclient.WithRateLimitCounter(counter))

	for range status {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, http.NoBody)
		require.NoError(t, err)

		resp, err := client.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
	}

	assert.Equal(t, int64(2), counter.Load())
}

// doGet issues a context-bound GET through hc, closes the body, and returns
// the status code, which is all the retry tests assert on.
func doGet(t *testing.T, hc *http.Client, url string) int {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	require.NoError(t, err)

	resp, err := hc.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	return resp.StatusCode
}

func TestServerErrorRetry(t *testing.T) {
	t.Parallel()

	t.Run("a transient 5xx is retried and succeeds", func(t *testing.T) {
		t.Parallel()

		var hits atomic.Int64

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if hits.Add(1) <= 2 {
				w.WriteHeader(http.StatusInternalServerError)

				return
			}

			_, werr := io.WriteString(w, "ok")
			if werr != nil {
				return
			}
		}))
		t.Cleanup(srv.Close)

		hc := tfeclient.ResolveHTTPClient(tfeclient.WithServerErrorRetry(3, 0))

		status := doGet(t, hc, srv.URL)

		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, int64(3), hits.Load(), "two failed attempts, then the success")
	})

	t.Run("a persistent 5xx returns after the bounded attempts", func(t *testing.T) {
		t.Parallel()

		var hits atomic.Int64

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.WriteHeader(http.StatusBadGateway)
		}))
		t.Cleanup(srv.Close)

		hc := tfeclient.ResolveHTTPClient(tfeclient.WithServerErrorRetry(2, 0))

		start := time.Now()
		status := doGet(t, hc, srv.URL)

		assert.Equal(t, http.StatusBadGateway, status, "the final attempt's response is returned as-is")
		assert.Equal(t, int64(3), hits.Load(), "the first attempt plus the configured retries")
		assert.Less(t, time.Since(start), 5*time.Second, "the stall is bounded")
	})

	t.Run("a request with a body is not retried", func(t *testing.T) {
		t.Parallel()

		var hits atomic.Int64

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(srv.Close)

		hc := tfeclient.ResolveHTTPClient(tfeclient.WithServerErrorRetry(3, 0))

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader("{}"))
		require.NoError(t, err)

		resp, err := hc.Do(req)
		require.NoError(t, err)

		t.Cleanup(func() { assert.NoError(t, resp.Body.Close()) })

		assert.Equal(t, int64(1), hits.Load(), "a consumed body cannot be re-sent")
	})

	t.Run("a 429 passes through to the rate-limit handling", func(t *testing.T) {
		t.Parallel()

		var hits atomic.Int64

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		t.Cleanup(srv.Close)

		hc := tfeclient.ResolveHTTPClient(tfeclient.WithServerErrorRetry(3, 0))

		status := doGet(t, hc, srv.URL)

		assert.Equal(t, http.StatusTooManyRequests, status)
		assert.Equal(t, int64(1), hits.Load(),
			"rate limiting is the go-tfe client's to handle, honoring the server's reset time")
	})
}

func TestServerErrorStallIsBoundedEndToEnd(t *testing.T) {
	t.Parallel()

	// The regression this pins: with go-tfe's own RetryServerErrors enabled, a
	// persistently failing endpoint was retried 30 times under a linearly
	// growing backoff -- over six minutes per request, none of it configurable.
	// The bounded transport retry replaces it, so the whole go-tfe call must
	// come back in seconds after exactly the configured attempts.
	var hits atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/ping", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/v2/organization/audit-trail", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := tfeclient.New(
		tfeclient.WithToken("test-token"),
		tfeclient.WithAddress(srv.URL),
		tfeclient.WithServerErrorRetry(2, 0),
	)
	require.NoError(t, err)

	start := time.Now()

	err = c.Do(t.Context(), func(ctx context.Context, tc *tfe.Client) error {
		_, e := tc.AuditTrails.List(ctx, &tfe.AuditTrailListOptions{})
		if e != nil {
			return fmt.Errorf("list audit trails: %w", e)
		}

		return nil
	})

	require.Error(t, err, "the failure still surfaces to the caller for recording")
	assert.Equal(t, int64(3), hits.Load(), "the first attempt plus the configured retries, not go-tfe's 30")
	assert.Less(t, time.Since(start), 10*time.Second)
}
