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
	// directory's dentry is durable, and the deepest created directory is flushed
	// for its own inode too, so its mode is durable without a later write's sync.
	// The order matters: levels are processed shallowest-first, so each created
	// level is chmodded before the deeper level's parent-sync flushes its inode;
	// deepest-first would fsync the intermediate levels before their chmods,
	// leaving the just-set modes non-durable across a crash.
	want := []string{
		root,
		filepath.Join(root, "a"),
		filepath.Join(root, "a", "b"),
		filepath.Join(root, "a", "b", "c"),
		target,
	}
	assert.Equal(t, want, synced,
		"each created level's parent is flushed shallowest-first, plus the deepest directory itself")

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

func TestMkdirAllSync_chmodsCreatedLevelsPastUmask(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	target := filepath.Join(root, "a", "b")

	noop := func(string) error { return nil }

	// 0o770 carries a group-write bit the common 0o022 umask strips: os.MkdirAll
	// alone would land 0o750, so mkdirAllSync must chmod each created level to
	// honor the requested mode exactly, whatever the umask.
	require.NoError(t, atomicfile.MkdirAllSync(target, 0o770, noop))

	for _, dir := range []string{filepath.Join(root, "a"), target} {
		info, err := os.Stat(dir)
		require.NoError(t, err)
		assert.Equal(t, fs.FileMode(0o770), info.Mode().Perm(),
			"created directory %q lands the requested mode", dir)
	}
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

func TestAppend_appliesFileMode(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		existing []byte
		opts     []atomicfile.Option
		want     fs.FileMode
	}{
		"created file takes the default mode": {
			opts: nil,
			want: atomicfile.DefaultFileMode,
		},
		// 0o660 carries a group-write bit the common 0o022 umask strips from the
		// open, so landing it proves the explicit chmod ran.
		"created file takes the requested mode": {
			opts: []atomicfile.Option{atomicfile.WithFileMode(0o660)},
			want: 0o660,
		},
		"empty file is still uninitialized and takes the requested mode": {
			existing: []byte{},
			opts:     []atomicfile.Option{atomicfile.WithFileMode(0o660)},
			want:     0o660,
		},
		"file already holding records keeps the mode it was created with": {
			existing: []byte("committed\n"),
			opts:     []atomicfile.Option{atomicfile.WithFileMode(0o660)},
			want:     0o600,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			target := filepath.Join(t.TempDir(), "log.ndjson")

			if tc.existing != nil {
				require.NoError(t, os.WriteFile(target, tc.existing, 0o600))
			}

			_, err := atomicfile.Append(target, []byte("line\n"), tc.opts...)
			require.NoError(t, err)

			info, err := os.Stat(target)
			require.NoError(t, err)
			assert.Equal(t, tc.want, info.Mode().Perm())
		})
	}
}

func TestAppend_setupFailureLeavesTargetIntact(t *testing.T) {
	t.Parallel()

	committed := []byte("another writer's committed batch\n")

	tests := map[string]struct {
		// Runs inside the flush that guards a first append, standing in for
		// whatever lands in that window before the flush reports its failure.
		duringSync func(t *testing.T, target string)
		want       []byte
	}{
		"records a concurrent writer committed inside the window": {
			duringSync: func(t *testing.T, target string) {
				t.Helper()

				require.NoError(t, os.WriteFile(target, committed, 0o600))
			},
			want: committed,
		},
		"an empty target nothing else touched": {
			duringSync: func(*testing.T, string) {},
			want:       []byte{},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			target := filepath.Join(t.TempDir(), "log.ndjson")

			fail := func(string) error {
				tc.duringSync(t, target)

				return errBoom
			}

			_, err := atomicfile.AppendSync(target, []byte("mine\n"), fail)
			require.ErrorIs(t, err, errBoom)

			// The failure path never unlinks or truncates the target: a writer that
			// raced in and was told its batch was durable must still find it there,
			// and this call's own batch, never written, must not be there.
			got, readErr := os.ReadFile(target)
			require.NoError(t, readErr)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestAppend_retryAfterSetupFailureReinitializes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "log.ndjson")

	_, err := atomicfile.AppendSync(target, []byte("mine\n"), func(string) error { return errBoom })
	require.ErrorIs(t, err, errBoom)

	var synced []string

	rec := func(d string) error {
		synced = append(synced, d)

		return nil
	}

	start, err := atomicfile.AppendSync(target, []byte("mine\n"), rec, atomicfile.WithFileMode(0o660))
	require.NoError(t, err)
	assert.Equal(t, int64(0), start)

	// The abandoned attempt left the file present but empty, and emptiness is what
	// marks it uninitialized, so the retry re-runs the setup the failure skipped:
	// the directory entry is flushed and the mode enforced, neither of which a
	// file already holding records would get.
	assert.Equal(t, []string{dir}, synced, "the retry flushes the target's directory")

	info, err := os.Stat(target)
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o660), info.Mode().Perm())

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, []byte("mine\n"), got)
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
