package tfeclient_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/tfeclient"
)

func TestIdleReadTimeoutBoundsMidBodyStall(t *testing.T) {
	t.Parallel()

	// The server sends headers and a first chunk, then goes silent mid-body. The
	// idle-read timeout must fail the stalled read promptly with an error that
	// classifies transient, rather than hanging the reader forever.
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}

		w.WriteHeader(http.StatusOK)

		_, werr := io.WriteString(w, "partial")
		if werr != nil {
			return
		}

		flusher.Flush()
		<-release
	}))
	// Cleanups run last-registered-first: unblock the handler before closing the
	// server, so the close does not deadlock on the still-blocked request.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	client := tfeclient.ResolveHTTPClient(tfeclient.WithIdleReadTimeout(50 * time.Millisecond))

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, http.NoBody)
	require.NoError(t, err)

	resp, err := client.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close() //nolint:errcheck // The idle timer may already have closed it.

	start := time.Now()
	_, err = io.ReadAll(resp.Body)
	elapsed := time.Since(start)

	require.Error(t, err, "a mid-body stall must fail rather than hang")
	require.ErrorIs(t, err, tfeclient.ErrIdleReadTimeout)

	var netErr net.Error

	require.ErrorAs(t, err, &netErr)
	assert.True(t, netErr.Timeout(), "the idle error reports itself a timeout")

	wrapped := fmt.Errorf("download state: %w", err)
	assert.Equal(t, tfeclient.KindTransient, tfeclient.Classify(wrapped),
		"a wrapped idle-timeout error classifies transient so a re-run retries the object")

	assert.Less(t, elapsed, 5*time.Second, "the failure is bounded by the idle timeout")
}

func TestIdleReadTimeoutAllowsSlowBodyAndCountsWireBytes(t *testing.T) {
	t.Parallel()

	// The body dribbles chunks at intervals well inside the idle timeout, adding
	// up to a transfer far longer than the timeout itself. The idle bound caps
	// only silence, so the slow-but-alive transfer must complete, and the wire
	// counter must see every byte as it arrives.
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

	// The timeout leaves the 20ms cadence a wide margin so a loaded CI machine
	// cannot stretch a gap past it and flake the test; it never fires on this
	// happy path, so the width costs no test time.
	counter := new(atomic.Int64)
	client := tfeclient.ResolveHTTPClient(
		tfeclient.WithIdleReadTimeout(250*time.Millisecond),
		tfeclient.WithWireBytes(counter),
	)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, http.NoBody)
	require.NoError(t, err)

	resp, err := client.Do(req)
	require.NoError(t, err)

	defer func() { assert.NoError(t, resp.Body.Close()) }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "a slow but live transfer is never capped")

	want := strings.Repeat("chunk", chunks)
	assert.Equal(t, want, string(body))
	assert.Equal(t, int64(len(want)), counter.Load(), "the wire counter sees every byte served")
}

func TestLoggerDebugLogsEveryRequest(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	var buf bytes.Buffer

	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	client := tfeclient.ResolveHTTPClient(tfeclient.WithLogger(logger))

	const requests = 3

	for range requests {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/api/v2/ping", http.NoBody)
		require.NoError(t, err)

		resp, err := client.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
	}

	logged := buf.String()

	assert.Equal(t, requests, strings.Count(logged, "http_request"),
		"every request on the wire logs exactly one debug line")
	assert.Contains(t, logged, "method=GET")
	assert.Contains(t, logged, "url="+srv.URL+"/api/v2/ping")
	assert.Contains(t, logged, "status=404")
	assert.Contains(t, logged, "duration=")
}

func TestLoggerDebugLogsTransportError(t *testing.T) {
	t.Parallel()

	// A server that closes immediately leaves nothing listening, so the round
	// trip fails before any response; the debug line carries the error instead
	// of a status.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()

	var buf bytes.Buffer

	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	client := tfeclient.ResolveHTTPClient(tfeclient.WithLogger(logger))

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, http.NoBody)
	require.NoError(t, err)

	resp, err := client.Do(req) //nolint:bodyclose // The request errors, so there is no body.
	require.Error(t, err)
	require.Nil(t, resp)

	logged := buf.String()

	assert.Contains(t, logged, "http_request")
	assert.Contains(t, logged, "error=")
	assert.NotContains(t, logged, "status=", "a failed attempt logs its error, not a status")
}

func TestIdleReadNilWireCounterIsSafe(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, werr := io.WriteString(w, "payload")
		if werr != nil {
			return
		}
	}))
	t.Cleanup(srv.Close)

	client := tfeclient.ResolveHTTPClient()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, http.NoBody)
	require.NoError(t, err)

	resp, err := client.Do(req)
	require.NoError(t, err)

	defer func() { assert.NoError(t, resp.Body.Close()) }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "payload", string(body))
}

func TestRunsListPathClassification(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		path string
		want bool
	}{
		"workspace runs list": {
			path: "/api/v2/workspaces/ws-abc123/runs",
			want: true,
		},
		"organization runs list": {
			path: "/api/v2/organizations/acme/runs",
			want: true,
		},
		"enterprise base path prefix": {
			path: "/tfe/api/v2/workspaces/ws-1/runs",
			want: true,
		},
		"run creation": {
			path: "/api/v2/runs",
			want: false,
		},
		"run read": {
			path: "/api/v2/runs/run-abc123",
			want: false,
		},
		"deeper child of the listing": {
			path: "/api/v2/workspaces/ws-1/runs/queue",
			want: false,
		},
		"stack deployment runs": {
			path: "/api/v2/stack-deployment-groups/sdg-1/stack-deployment-runs",
			want: false,
		},
		"empty owner id": {
			path: "/api/v2/workspaces//runs",
			want: false,
		},
		"other owner type": {
			path: "/api/v2/projects/prj-1/runs",
			want: false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, tfeclient.RunsListPath(tc.path))
		})
	}
}

// rateLimit429 answers every request with a 429 whose reset advertises a long
// cooldown, so the bucket that absorbed it stays visibly paused for the
// asserting test.
func rateLimit429(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("X-Ratelimit-Limit", "30")
	w.Header().Set("X-Ratelimit-Reset", "30")
	w.WriteHeader(http.StatusTooManyRequests)
}

func TestRunsList429LeavesGeneralBucketUntouched(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(rateLimit429))
	t.Cleanup(srv.Close)

	client := tfeclient.ResolveHTTPClient()
	general, runs := tfeclient.ThrottleGovernors(client)
	require.NotNil(t, general)
	require.NotNil(t, runs)

	// The first attempt draws a runs-bucket token and sees the 429; the retry
	// then parks in the paused runs governor until the deadline ends the
	// request, so the test stays bounded.
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v2/workspaces/ws-1/runs", http.NoBody)
	require.NoError(t, err)

	resp, err := client.Do(req) //nolint:bodyclose // The request errors, so there is no body.
	require.Error(t, err)
	require.Nil(t, resp)

	rate, paused := runs.Snapshot()
	assert.Positive(t, paused, "the runs-list bucket absorbed the 429's cooldown")
	assert.InDelta(t, tfeclient.DefaultRunsListRateLimit, rate, 1e-9,
		"the runs-list rate is already at its floor, so the halving cannot move it")

	rate, paused = general.Snapshot()
	assert.Zero(t, paused, "a runs-list 429 pauses no general traffic")
	assert.InDelta(t, tfeclient.DefaultRateLimit, rate, 1e-9,
		"a runs-list 429 does not halve the general rate")
}

func TestGeneral429LeavesRunsListBucketUntouched(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(rateLimit429))
	t.Cleanup(srv.Close)

	client := tfeclient.ResolveHTTPClient()
	general, runs := tfeclient.ThrottleGovernors(client)
	require.NotNil(t, general)
	require.NotNil(t, runs)

	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet, srv.URL+"/api/v2/organizations/acme/workspaces", http.NoBody)
	require.NoError(t, err)

	resp, err := client.Do(req) //nolint:bodyclose // The request errors, so there is no body.
	require.Error(t, err)
	require.Nil(t, resp)

	rate, paused := general.Snapshot()
	assert.Positive(t, paused, "the general bucket absorbed the 429's cooldown")
	assert.InDelta(t, tfeclient.DefaultRateLimit/2, rate, 1e-9, "the general rate halves on its own 429")

	rate, paused = runs.Snapshot()
	assert.Zero(t, paused, "a general 429 pauses no runs-list traffic")
	assert.InDelta(t, tfeclient.DefaultRunsListRateLimit, rate, 1e-9)
}

func TestLoggerDebugLogsRateLimitHeaders(t *testing.T) {
	t.Parallel()

	// The first response is a 429 carrying the bucket-identifying headers; the
	// retry then succeeds, so the test observes the logged headers without
	// waiting out a long cooldown (the tiny reset keeps the retry prompt).
	var hits atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("X-Ratelimit-Limit", "42")
			w.Header().Set("X-Ratelimit-Reset", "0.01")
			w.WriteHeader(http.StatusTooManyRequests)

			return
		}

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	var buf bytes.Buffer

	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	client := tfeclient.ResolveHTTPClient(tfeclient.WithLogger(logger))

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/api/v2/ping", http.NoBody)
	require.NoError(t, err)

	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	logged := buf.String()

	assert.Contains(t, logged, "status=429")
	assert.Contains(t, logged, "ratelimit_limit=42",
		"a 429 logs the limit header so an undocumented bucket identifies itself")
	assert.Contains(t, logged, "ratelimit_reset=0.01")
}
