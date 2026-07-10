package atomicfile_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/atomicfile"
)

// errBoom is the sentinel a failing callback returns midway through a write.
var errBoom = errors.New("boom")

// countStaging returns the number of leftover staging files in dir.
func countStaging(t *testing.T, dir string) int {
	t.Helper()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	n := 0

	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".atomicfile-") {
			n++
		}
	}

	return n
}

func TestWrite_roundTrip(t *testing.T) {
	t.Parallel()

	payload := []byte("the quick brown fox")

	tests := map[string]struct {
		write func(name string) error
		want  []byte
	}{
		"WriteFile": {
			write: func(name string) error {
				return atomicfile.WriteFile(name, payload)
			},
			want: payload,
		},
		"WriteReader": {
			write: func(name string) error {
				return atomicfile.WriteReader(name, bytes.NewReader(payload))
			},
			want: payload,
		},
		"Write callback": {
			write: func(name string) error {
				return atomicfile.Write(name, func(w io.Writer) error {
					_, err := w.Write(payload)
					if err != nil {
						return fmt.Errorf("write payload: %w", err)
					}

					return nil
				})
			},
			want: payload,
		},
		"WriteFile empty": {
			write: func(name string) error {
				return atomicfile.WriteFile(name, nil)
			},
			want: []byte{},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			target := filepath.Join(dir, "object")

			require.NoError(t, tc.write(target))

			got, err := os.ReadFile(target)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Zero(t, countStaging(t, dir))
		})
	}
}

func TestWrite_overwriteReplacesAtomically(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "object")

	require.NoError(t, atomicfile.WriteFile(target, []byte("first")))
	require.NoError(t, atomicfile.WriteFile(target, []byte("second")))

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, []byte("second"), got)
	assert.Zero(t, countStaging(t, dir))
}

func TestWrite_createsParentDirs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "deeply", "nested", "path", "object")

	require.NoError(t, atomicfile.WriteReader(target, strings.NewReader("body")))

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, []byte("body"), got)
}

func TestWrite_appliesFileMode(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		opts []atomicfile.Option
		want fs.FileMode
	}{
		"default": {
			opts: nil,
			want: atomicfile.DefaultFileMode,
		},
		"custom": {
			opts: []atomicfile.Option{atomicfile.WithFileMode(0o600)},
			want: 0o600,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			target := filepath.Join(t.TempDir(), "object")

			require.NoError(t, atomicfile.WriteFile(target, []byte("x"), tc.opts...))

			info, err := os.Stat(target)
			require.NoError(t, err)
			assert.Equal(t, tc.want, info.Mode().Perm())
		})
	}
}

func TestWrite_callbackErrorLeavesTargetUntouched(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "object")

	require.NoError(t, atomicfile.WriteFile(target, []byte("keep")))

	err := atomicfile.Write(target, func(w io.Writer) error {
		// Emit partial output before failing to prove it never reaches the
		// target.
		_, werr := io.WriteString(w, "garbage")
		require.NoError(t, werr)

		return errBoom
	})
	require.ErrorIs(t, err, errBoom)

	got, readErr := os.ReadFile(target)
	require.NoError(t, readErr)
	assert.Equal(t, []byte("keep"), got)
	assert.Zero(t, countStaging(t, dir))
}

func TestWrite_callbackErrorLeavesTargetAbsent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "object")

	err := atomicfile.Write(target, func(w io.Writer) error {
		_, werr := io.WriteString(w, "garbage")
		require.NoError(t, werr)

		return errBoom
	})
	require.ErrorIs(t, err, errBoom)

	_, statErr := os.Stat(target)
	require.ErrorIs(t, statErr, fs.ErrNotExist)
	assert.Zero(t, countStaging(t, dir))
}

func TestWriteReader_readerErrorLeavesTargetUntouched(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "object")

	require.NoError(t, atomicfile.WriteFile(target, []byte("keep")))

	err := atomicfile.WriteReader(target, failingReader{})
	require.ErrorIs(t, err, errBoom)

	got, readErr := os.ReadFile(target)
	require.NoError(t, readErr)
	assert.Equal(t, []byte("keep"), got)
	assert.Zero(t, countStaging(t, dir))
}

// failingReader yields some bytes and then fails, exercising the streaming
// path's mid-copy error handling.
type failingReader struct{}

func (failingReader) Read(p []byte) (int, error) {
	return copy(p, "partial"), errBoom
}

func TestMkdirAllSync_flushesEveryCreatedAncestor(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	// The root already exists; a, b, c, and d are the four levels created below it.
	target := filepath.Join(root, "a", "b", "c", "d")

	var synced []string

	rec := func(dir string) error {
		synced = append(synced, dir)

		return nil
	}

	require.NoError(t, atomicfile.MkdirAllSync(target, 0o700, rec))

	// The parent of every created level is flushed exactly once, so each new
	// directory's dentry is durable, not just the leaf's.
	want := []string{
		root,
		filepath.Join(root, "a"),
		filepath.Join(root, "a", "b"),
		filepath.Join(root, "a", "b", "c"),
	}
	assert.ElementsMatch(t, want, synced, "each created level's parent is flushed once")

	info, err := os.Stat(target)
	require.NoError(t, err)
	assert.True(t, info.IsDir(), "the full subtree exists")
}

func TestMkdirAllSync_existingDirTakesHotPath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	target := filepath.Join(root, "a", "b")

	count := 0
	rec := func(string) error {
		count++

		return nil
	}

	require.NoError(t, atomicfile.MkdirAllSync(target, 0o700, rec))
	assert.Positive(t, count, "the first creation flushes the new levels")

	// A second write into the now-existing directory must flush nothing, holding
	// the hot-path perf contract: an ordinary write costs no ancestor fsync.
	count = 0

	require.NoError(t, atomicfile.MkdirAllSync(target, 0o700, rec))
	assert.Zero(t, count, "a second call into the existing directory flushes nothing")
}

func TestWrite_concurrentSiblingSubtrees(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	const workers = 8

	var wg sync.WaitGroup

	wg.Add(workers)

	errs := make([]error, workers)
	leaf := func(i int) string {
		return filepath.Join(root, "shared", "nested", fmt.Sprintf("w%d", i), "object")
	}

	for i := range workers {
		go func() {
			defer wg.Done()

			// Every worker shares the "shared/nested" ancestors, so they race to
			// create them; the unconditional per-level fsync must tolerate that.
			errs[i] = atomicfile.WriteFile(leaf(i), []byte("body"))
		}()
	}

	wg.Wait()

	for i := range workers {
		require.NoError(t, errs[i], "worker %d wrote without error", i)

		got, err := os.ReadFile(leaf(i))
		require.NoError(t, err)
		assert.Equal(t, []byte("body"), got, "worker %d's file landed", i)
	}
}

func TestAppend_roundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "log.ndjson")

	first := []byte("one\n")
	second := []byte("two\nthree\n")

	start, err := atomicfile.Append(target, first)
	require.NoError(t, err)
	assert.Equal(t, int64(0), start)

	start, err = atomicfile.Append(target, second)
	require.NoError(t, err)
	assert.Equal(t, int64(len(first)), start)

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, append(append([]byte{}, first...), second...), got)
}

func TestAppend_createsParentDir(t *testing.T) {
	t.Parallel()

	target := filepath.Join(t.TempDir(), "nested", "deep", "log.ndjson")

	start, err := atomicfile.Append(target, []byte("line\n"))
	require.NoError(t, err)
	assert.Equal(t, int64(0), start)

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, []byte("line\n"), got)
}

func TestAppend_trimsTornTailBeforeAppending(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		existing  []byte
		wantStart int64
		wantHead  []byte
	}{
		"torn fragment after a committed line": {
			existing:  []byte("committed\n{\"partial\":"),
			wantStart: int64(len("committed\n")),
			wantHead:  []byte("committed\n"),
		},
		"wholly unterminated file": {
			existing:  []byte("{\"partial\":"),
			wantStart: 0,
			wantHead:  nil,
		},
		"clean trailing newline is preserved": {
			existing:  []byte("committed\n"),
			wantStart: int64(len("committed\n")),
			wantHead:  []byte("committed\n"),
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			target := filepath.Join(dir, "log.ndjson")

			// Simulate a crash-torn prior append written directly, bypassing Append.
			require.NoError(t, os.WriteFile(target, tc.existing, 0o600))

			next := []byte("recovered\n")

			start, err := atomicfile.Append(target, next)
			require.NoError(t, err)
			assert.Equal(t, tc.wantStart, start)

			got, err := os.ReadFile(target)
			require.NoError(t, err)

			// The torn fragment is gone and the new record lands on a clean
			// boundary, so every line is a whole record.
			assert.Equal(t, append(append([]byte{}, tc.wantHead...), next...), got)
			assert.Equal(t, next, got[start:])
		})
	}
}
