package tfeclient_test

import (
	"bytes"
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

	"go.jacobcolvin.com/hcp_archiver/tfeclient"
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
