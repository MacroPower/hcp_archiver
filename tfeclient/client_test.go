package tfeclient_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

	client := tfeclient.ResolveHTTPClient(tfeclient.WithResponseHeaderTimeout(50 * time.Millisecond))

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
}
