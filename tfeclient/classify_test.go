package tfeclient_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"

	"github.com/hashicorp/go-tfe"
	"github.com/stretchr/testify/assert"

	"go.jacobcolvin.com/hcp_archiver/tfeclient"
)

// timeoutErr is a net.Error whose Timeout reports true.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

var _ net.Error = timeoutError{}

func TestClassify(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		err  error
		want tfeclient.Kind
	}{
		"nil is unknown": {
			err:  nil,
			want: tfeclient.KindUnknown,
		},
		"wrapped resource not found is terminal": {
			err:  fmt.Errorf("read plan: %w", tfe.ErrResourceNotFound),
			want: tfeclient.KindTerminal,
		},
		"context canceled is transient": {
			err:  fmt.Errorf("do: %w", context.Canceled),
			want: tfeclient.KindTransient,
		},
		"deadline exceeded is transient": {
			err:  fmt.Errorf("do: %w", context.DeadlineExceeded),
			want: tfeclient.KindTransient,
		},
		"network timeout is transient": {
			err:  fmt.Errorf("dial: %w", timeoutError{}),
			want: tfeclient.KindTransient,
		},
		"rate limited is transient": {
			err:  fmt.Errorf("do: %w", tfeclient.ErrRateLimited),
			want: tfeclient.KindTransient,
		},
		"connection reset mid-stream is transient": {
			err: fmt.Errorf("read state: %w", &net.OpError{
				Op:  "read",
				Net: "tcp",
				Err: syscall.ECONNRESET,
			}),
			want: tfeclient.KindTransient,
		},
		"connection aborted is transient": {
			err:  fmt.Errorf("read log: %w", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNABORTED}),
			want: tfeclient.KindTransient,
		},
		"broken pipe is transient": {
			err:  fmt.Errorf("write: %w", &net.OpError{Op: "write", Net: "tcp", Err: syscall.EPIPE}),
			want: tfeclient.KindTransient,
		},
		"a body truncated short of its length is transient": {
			err:  fmt.Errorf("read log: %w", io.ErrUnexpectedEOF),
			want: tfeclient.KindTransient,
		},
		"a read on a closed connection is transient": {
			err:  fmt.Errorf("read log: %w", net.ErrClosed),
			want: tfeclient.KindTransient,
		},
		"a clean EOF is unknown, not an interrupted stream": {
			err:  fmt.Errorf("read log: %w", io.EOF),
			want: tfeclient.KindUnknown,
		},
		"generic error is unknown": {
			err:  errors.New("boom"),
			want: tfeclient.KindUnknown,
		},
		"unauthorized is unknown": {
			err:  fmt.Errorf("read: %w", tfe.ErrUnauthorized),
			want: tfeclient.KindUnknown,
		},
		"forbidden payload is forbidden": {
			err:  errors.New("forbidden\n\nTeam and Organization Tokens are not supported"),
			want: tfeclient.KindForbidden,
		},
		"wrapped forbidden is forbidden": {
			err:  fmt.Errorf("list github app installations: %w", errors.New("forbidden")),
			want: tfeclient.KindForbidden,
		},
		"forbidden status line is forbidden": {
			err:  errors.New("403 Forbidden"),
			want: tfeclient.KindForbidden,
		},
		"path containing forbidden with a non-403 cause is unknown": {
			err: fmt.Errorf(
				"fetch %q: %w",
				"workspaces/forbidden-experiments/runs",
				errors.New("500 Internal Server Error"),
			),
			want: tfeclient.KindUnknown,
		},
		"raw DoRaw 404 is terminal": {
			err:  fmt.Errorf("read artifact: %w", errors.New("error HTTP response: 404")),
			want: tfeclient.KindTerminal,
		},
		"raw DoRaw 403 is forbidden": {
			err:  fmt.Errorf("read artifact: %w", errors.New("error HTTP response: 403")),
			want: tfeclient.KindForbidden,
		},
		"raw DoRaw 500 is unknown": {
			err:  fmt.Errorf("read artifact: %w", errors.New("error HTTP response: 500")),
			want: tfeclient.KindUnknown,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, tfeclient.Classify(tc.err))
			assert.Equal(t, tc.want == tfeclient.KindTerminal, tfeclient.IsTerminal(tc.err))
			assert.Equal(t, tc.want == tfeclient.KindTransient, tfeclient.IsTransient(tc.err))
			assert.Equal(t, tc.want == tfeclient.KindForbidden, tfeclient.IsForbidden(tc.err))
		})
	}
}

func TestKindString(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		kind tfeclient.Kind
		want string
	}{
		"unknown":   {kind: tfeclient.KindUnknown, want: "unknown"},
		"transient": {kind: tfeclient.KindTransient, want: "transient"},
		"terminal":  {kind: tfeclient.KindTerminal, want: "terminal"},
		"forbidden": {kind: tfeclient.KindForbidden, want: "forbidden"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, tc.kind.String())
		})
	}
}
