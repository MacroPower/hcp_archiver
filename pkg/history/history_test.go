package history_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/pkg/history"
	"go.jacobcolvin.com/hcp_archiver/pkg/logtest"
)

// sidecar returns a sidecar path in a fresh temp dir.
func sidecar(t *testing.T) string {
	t.Helper()

	return filepath.Join(t.TempDir(), "variables.history.ndjson")
}

// readRecords parses every committed line of a sidecar, oldest first.
func readRecords(t *testing.T, path string) []history.Record {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	var out []history.Record

	for line := range strings.SplitSeq(strings.TrimSuffix(string(data), "\n"), "\n") {
		var rec history.Record

		require.NoError(t, json.Unmarshal([]byte(line), &rec))

		out = append(out, rec)
	}

	return out
}

// damage appends a whole line that does not parse, the shape bit rot or a
// partial restore leaves behind. Unlike a torn tail it sits behind a newline,
// so the scan reaches it and has to decide what to do. It returns the exact
// bytes appended, without the terminating newline, so a test can locate them
// in the file.
func damage(t *testing.T, path string) string {
	t.Helper()

	const line = "{\"fetchedAt\":\"2026-08-13T00:00:00Z\",\"sha\xff"

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	require.NoError(t, err)

	_, err = f.WriteString(line + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	return line
}

// rotNewest rewrites the newest committed line in place, putting the damage
// on a record that already carries meaning rather than on an extra one
// arriving at the end.
func rotNewest(t *testing.T, path string) {
	t.Helper()

	const line = "{\"fetchedAt\":\"2026-08-13T00:00:00Z\",\"sha\xff"

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	require.NotEmpty(t, lines)

	lines[len(lines)-1] = line

	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
}

// quiet discards the skip warnings a scan over deliberately damaged bytes
// emits, keeping them off the suite's stderr.
func quiet() history.Option {
	return history.WithLogger(slog.New(slog.DiscardHandler))
}

// shaOf returns the hex-encoded SHA-256 of data, the digest a content record
// must carry.
func shaOf(data []byte) string {
	h := sha256.Sum256(data)

	return hex.EncodeToString(h[:])
}

func TestPath(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		objectPath string
		want       string
	}{
		"json file": {
			objectPath: "variables.json",
			want:       "variables.history.ndjson",
		},
		"non-json extension": {
			objectPath: "readme.md",
			want:       "readme.md.history.ndjson",
		},
		"nested path": {
			objectPath: "projects/prod/workspaces/api/workspace.json",
			want:       "projects/prod/workspaces/api/workspace.history.ndjson",
		},
		"no extension": {
			objectPath: "memberships",
			want:       "memberships.history.ndjson",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, history.Path(tc.objectPath))
		})
	}
}

func TestSupersede(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)
	v1 := []byte("{\n  \"v\": 1\n}")
	v2 := []byte("{\n  \"v\": 2\n}")

	t.Run("first append creates the sidecar", func(t *testing.T) {
		t.Parallel()

		path := sidecar(t)

		appended, err := history.Supersede(path, v1, at)
		require.NoError(t, err)
		assert.True(t, appended)

		recs := readRecords(t, path)
		require.Len(t, recs, 1)
		assert.Equal(t, string(v1), recs[0].Content)
		assert.Equal(t, shaOf(v1), recs[0].SHA256)
		assert.Equal(t, at, recs[0].FetchedAt)
		assert.False(t, recs[0].Deleted)
	})

	t.Run("identical content twice appends once", func(t *testing.T) {
		t.Parallel()

		// The crash-retry shape: a run appended the outgoing content but died
		// before the object file renamed, so the next run supersedes the same
		// bytes again and must not duplicate the record.
		path := sidecar(t)

		_, err := history.Supersede(path, v1, at)
		require.NoError(t, err)

		appended, err := history.Supersede(path, v1, at.Add(time.Hour))
		require.NoError(t, err)
		assert.False(t, appended, "the committed tail already carries this content")

		assert.Len(t, readRecords(t, path), 1)
	})

	t.Run("changed content appends", func(t *testing.T) {
		t.Parallel()

		path := sidecar(t)

		_, err := history.Supersede(path, v1, at)
		require.NoError(t, err)

		appended, err := history.Supersede(path, v2, at.Add(time.Hour))
		require.NoError(t, err)
		assert.True(t, appended)

		recs := readRecords(t, path)
		require.Len(t, recs, 2)
		assert.Equal(t, string(v1), recs[0].Content)
		assert.Equal(t, string(v2), recs[1].Content)
	})

	t.Run("dedupe skips a trailing tombstone", func(t *testing.T) {
		t.Parallel()

		// After [v1, tombstone], an object reappearing with changed bytes
		// supersedes its on-disk v1; Bury already flushed it, so the dedupe
		// must look through the tombstone and skip.
		path := sidecar(t)

		buried, err := history.Bury(path, v1, at, at.Add(time.Hour))
		require.NoError(t, err)
		require.True(t, buried)

		appended, err := history.Supersede(path, v1, at)
		require.NoError(t, err)
		assert.False(t, appended, "the newest content-bearing record already carries these bytes")

		assert.Len(t, readRecords(t, path), 2)
	})
}

func TestRestore(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)
	content := []byte("{\n  \"v\": 1\n}")

	t.Run("no sidecar is a no-op and creates none", func(t *testing.T) {
		t.Parallel()

		path := sidecar(t)

		appended, err := history.Restore(path, content, at)
		require.NoError(t, err)
		assert.False(t, appended)

		_, statErr := os.Stat(path)
		assert.True(t, os.IsNotExist(statErr), "Restore never creates a sidecar")
	})

	t.Run("newest content record is a no-op", func(t *testing.T) {
		t.Parallel()

		path := sidecar(t)

		_, err := history.Supersede(path, content, at)
		require.NoError(t, err)

		appended, err := history.Restore(path, content, at.Add(time.Hour))
		require.NoError(t, err)
		assert.False(t, appended)

		assert.Len(t, readRecords(t, path), 1)
	})

	t.Run("empty supersede appends nothing and creates no sidecar", func(t *testing.T) {
		t.Parallel()

		path := sidecar(t)

		appended, err := history.Supersede(path, []byte{}, at)
		require.NoError(t, err)
		assert.False(t, appended)

		_, statErr := os.Stat(path)
		assert.True(t, os.IsNotExist(statErr))
	})

	t.Run("empty content leaves the tombstone standing", func(t *testing.T) {
		t.Parallel()

		// An empty record would marshal with neither a content nor a deleted
		// field, a line readers can classify as neither a version nor a
		// tombstone; the deletion closes only once content worth keeping
		// returns.
		path := sidecar(t)

		_, err := history.Bury(path, content, at, at.Add(time.Hour))
		require.NoError(t, err)

		appended, err := history.Restore(path, []byte{}, at.Add(2*time.Hour))
		require.NoError(t, err)
		assert.False(t, appended)

		recs := readRecords(t, path)
		require.Len(t, recs, 2)
		assert.True(t, recs[1].Deleted, "the tombstone stays the newest record")
	})

	t.Run("newest tombstone appends the returning content", func(t *testing.T) {
		t.Parallel()

		path := sidecar(t)

		_, err := history.Bury(path, content, at, at.Add(time.Hour))
		require.NoError(t, err)

		appended, err := history.Restore(path, content, at.Add(2*time.Hour))
		require.NoError(t, err)
		assert.True(t, appended)

		recs := readRecords(t, path)
		require.Len(t, recs, 3)
		assert.True(t, recs[1].Deleted)
		assert.Equal(t, string(content), recs[2].Content,
			"the returning content closes the deletion")
		assert.Equal(t, at.Add(2*time.Hour), recs[2].FetchedAt)
	})
}

func TestBury(t *testing.T) {
	t.Parallel()

	fetched := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)
	deleted := fetched.Add(24 * time.Hour)
	content := []byte("{\n  \"v\": 1\n}")

	t.Run("flushes content then appends a tombstone", func(t *testing.T) {
		t.Parallel()

		path := sidecar(t)

		buried, err := history.Bury(path, content, fetched, deleted)
		require.NoError(t, err)
		assert.True(t, buried)

		recs := readRecords(t, path)
		require.Len(t, recs, 2)
		assert.Equal(t, string(content), recs[0].Content)
		assert.Equal(t, fetched, recs[0].FetchedAt)
		assert.True(t, recs[1].Deleted)
		assert.Empty(t, recs[1].Content, "a tombstone never carries content")
		assert.Equal(t, deleted, recs[1].FetchedAt,
			"a tombstone's fetchedAt is when the disappearance was observed")
	})

	t.Run("second bury is a no-op", func(t *testing.T) {
		t.Parallel()

		// Mutable re-404s every run; only the first observation records.
		path := sidecar(t)

		_, err := history.Bury(path, content, fetched, deleted)
		require.NoError(t, err)

		buried, err := history.Bury(path, content, fetched, deleted.Add(time.Hour))
		require.NoError(t, err)
		assert.False(t, buried)

		assert.Len(t, readRecords(t, path), 2)
	})

	t.Run("content the tombstone never covered earns its own", func(t *testing.T) {
		t.Parallel()

		// A commit whose tombstone-closing record did not land leaves the file
		// holding a version the sidecar never saw, under a trailing tombstone.
		// The next disappearance must record that version and a tombstone of
		// its own, not read the stale one as already covering it.
		path := sidecar(t)
		returned := []byte("{\n  \"v\": 2\n}")

		_, err := history.Bury(path, content, fetched, deleted)
		require.NoError(t, err)

		buried, err := history.Bury(path, returned, fetched.Add(48*time.Hour), deleted.Add(48*time.Hour))
		require.NoError(t, err)
		assert.True(t, buried)

		recs := readRecords(t, path)
		require.Len(t, recs, 4)
		assert.Equal(t, string(content), recs[0].Content)
		assert.True(t, recs[1].Deleted)
		assert.Equal(t, string(returned), recs[2].Content, "the unrecorded version is flushed")
		assert.True(t, recs[3].Deleted, "the second disappearance earns its own tombstone")
	})

	t.Run("nothing to bury creates no sidecar", func(t *testing.T) {
		t.Parallel()

		path := sidecar(t)

		buried, err := history.Bury(path, nil, time.Time{}, deleted)
		require.NoError(t, err)
		assert.False(t, buried)

		_, statErr := os.Stat(path)
		assert.True(t, os.IsNotExist(statErr))
	})

	t.Run("retry after a crash between flush and tombstone", func(t *testing.T) {
		t.Parallel()

		// The crash left the sidecar ending with the content flush but no
		// tombstone; the retried bury must dedupe the flush and append only
		// the tombstone.
		path := sidecar(t)

		_, err := history.Supersede(path, content, fetched)
		require.NoError(t, err)

		buried, err := history.Bury(path, content, fetched, deleted)
		require.NoError(t, err)
		assert.True(t, buried)

		recs := readRecords(t, path)
		require.Len(t, recs, 2, "the flush dedupes; only the tombstone is appended")
		assert.Equal(t, string(content), recs[0].Content)
		assert.True(t, recs[1].Deleted)
	})

	t.Run("sidecar without a file still gets its tombstone", func(t *testing.T) {
		t.Parallel()

		// Superseded history exists but the file is gone (hand-deleted): the
		// disappearance still closes the timeline.
		path := sidecar(t)

		_, err := history.Supersede(path, content, fetched)
		require.NoError(t, err)

		buried, err := history.Bury(path, nil, time.Time{}, deleted)
		require.NoError(t, err)
		assert.True(t, buried)

		recs := readRecords(t, path)
		require.Len(t, recs, 2)
		assert.True(t, recs[1].Deleted)
	})
}

func TestNewest(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)

	t.Run("missing sidecar reports none", func(t *testing.T) {
		t.Parallel()

		_, ok, err := history.Newest(sidecar(t))
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("empty sidecar reports none", func(t *testing.T) {
		t.Parallel()

		path := sidecar(t)
		require.NoError(t, os.WriteFile(path, nil, 0o600))

		_, ok, err := history.Newest(path)
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("returns the newest record", func(t *testing.T) {
		t.Parallel()

		path := sidecar(t)

		_, err := history.Supersede(path, []byte("old"), at)
		require.NoError(t, err)

		_, err = history.Supersede(path, []byte("new"), at.Add(time.Hour))
		require.NoError(t, err)

		rec, ok, err := history.Newest(path)
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, "new", rec.Content)
	})
}

func TestNewestIgnoresTornTail(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)
	path := sidecar(t)

	_, err := history.Supersede(path, []byte("committed"), at)
	require.NoError(t, err)

	// A crash mid-append leaves an unterminated fragment past the final
	// newline; reads must see only the committed record.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	require.NoError(t, err)

	_, err = f.WriteString(`{"fetchedAt":"2026-08-13T00:00:00Z","sha256":"torn`)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	// A torn tail sits past the final newline, so it is not a committed
	// record and a scan that walks over it has skipped nothing.
	rec, ok, err := history.Newest(path, history.WithLogger(
		logtest.FailOn(t, "history_records_skipped")))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "committed", rec.Content, "the torn fragment is ignored")
}

func TestScanReportsSkippedRecords(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)
	v1 := []byte("{\n  \"v\": 1\n}")

	t.Run("the reported offset is where the damaged bytes start", func(t *testing.T) {
		t.Parallel()

		// The offset is the whole operator-facing value of the event: it has
		// to be a place in the file they can seek to, so the test seeks to it.
		path := sidecar(t)

		_, err := history.Supersede(path, v1, at, quiet())
		require.NoError(t, err)

		damaged := damage(t, path)

		rec := logtest.NewRecorder()

		_, ok, err := history.Newest(path, history.WithLogger(rec.Logger()))
		require.NoError(t, err)
		require.True(t, ok)

		events := rec.Events("history_records_skipped")
		require.Len(t, events, 1)
		assert.Equal(t, path, events[0].Attrs["path"])
		assert.Equal(t, int64(1), events[0].Attrs["records"])

		data, err := os.ReadFile(path)
		require.NoError(t, err)

		want := int64(bytes.Index(data, []byte(damaged)))
		require.NotEqual(t, int64(-1), want, "the damaged line is in the file")

		off, isInt := events[0].Attrs["offset"].(int64)
		require.True(t, isInt)
		assert.Equal(t, want, off)

		f, err := os.Open(path)
		require.NoError(t, err)

		defer f.Close() //nolint:errcheck // Read-only descriptor.

		buf := make([]byte, len(damaged))
		_, err = f.ReadAt(buf, off)
		require.NoError(t, err)
		assert.Equal(t, damaged, string(buf), "the offset seeks to the damaged bytes")

		assert.Equal(t, byte('\n'), data[off-1],
			"the offset names the line's first byte, not the newline before it")
	})

	t.Run("the offset holds when the scan starts mid-file", func(t *testing.T) {
		t.Parallel()

		// A match landing in a window whose file offset is non-zero is what
		// separates an absolute offset from a window-relative one. Each record
		// carries distinct content, since Supersede dedupes identical bytes
		// against the tail and the sidecar would never outgrow one window.
		path := sidecar(t)

		for i := range 200 {
			_, err := history.Supersede(path, fmt.Appendf(nil, "{\"v\":%d}", i), at, quiet())
			require.NoError(t, err)
		}

		damaged := damage(t, path)

		data, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Greater(t, len(data), 3*4096, "the scan must start well inside the file")

		rec := logtest.NewRecorder()

		_, ok, err := history.Newest(path, history.WithLogger(rec.Logger()))
		require.NoError(t, err)
		require.True(t, ok)

		events := rec.Events("history_records_skipped")
		require.Len(t, events, 1)
		assert.Equal(t, int64(bytes.Index(data, []byte(damaged))), events[0].Attrs["offset"])
	})

	t.Run("one damaged record is one event however many windows the scan reads",
		func(t *testing.T) {
			t.Parallel()

			// A record far larger than the seed window makes the scan double
			// several times, re-reading the damaged line on every pass.
			path := sidecar(t)
			big := []byte(`{"data":"` + strings.Repeat("x", 64<<10) + `"}`)

			_, err := history.Supersede(path, []byte("small"), at, quiet())
			require.NoError(t, err)

			_, err = history.Supersede(path, big, at.Add(time.Hour), quiet())
			require.NoError(t, err)

			damage(t, path)

			rec := logtest.NewRecorder()

			got, ok, err := history.Newest(path, history.WithLogger(rec.Logger()))
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, string(big), got.Content)

			events := rec.Events("history_records_skipped")
			require.Len(t, events, 1, "the terminating window reports for the whole scan")
			assert.Equal(t, int64(1), events[0].Attrs["records"])
		})

	t.Run("a wholly rotted sidecar reports every line it walked", func(t *testing.T) {
		t.Parallel()

		path := sidecar(t)

		for i := range 3 {
			_, err := history.Supersede(path, fmt.Appendf(nil, "{\"v\":%d}", i), at, quiet())
			require.NoError(t, err)
		}

		lines := make([]string, 3)
		for i := range lines {
			lines[i] = "{\"fetchedAt\":\"2026-08-13T00:00:00Z\",\"sha\xff"
		}

		require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))

		rec := logtest.NewRecorder()

		_, ok, err := history.Newest(path, history.WithLogger(rec.Logger()))
		require.NoError(t, err)
		assert.False(t, ok, "nothing parses, so the scan answers with nothing")

		events := rec.Events("history_records_skipped")
		require.Len(t, events, 1)
		assert.Equal(t, int64(3), events[0].Attrs["records"])

		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, int64(bytes.LastIndexByte(data[:len(data)-1], '\n')+1),
			events[0].Attrs["offset"], "the offset names the newest line skipped")
	})

	t.Run("each scan an operation runs reports once", func(t *testing.T) {
		t.Parallel()

		// Bury scans twice, its own Newest and the Supersede it flushes
		// through, and both genuinely read past the damage.
		tests := map[string]struct {
			run  func(t *testing.T, path string, opt history.Option)
			want int
		}{
			"newest": {
				run: func(t *testing.T, path string, opt history.Option) {
					t.Helper()

					_, _, err := history.Newest(path, opt)
					require.NoError(t, err)
				},
				want: 1,
			},
			"restore": {
				run: func(t *testing.T, path string, opt history.Option) {
					t.Helper()

					_, err := history.Restore(path, v1, at.Add(time.Hour), opt)
					require.NoError(t, err)
				},
				want: 1,
			},
			"supersede": {
				run: func(t *testing.T, path string, opt history.Option) {
					t.Helper()

					_, err := history.Supersede(path, v1, at.Add(time.Hour), opt)
					require.NoError(t, err)
				},
				want: 1,
			},
			"bury": {
				run: func(t *testing.T, path string, opt history.Option) {
					t.Helper()

					_, err := history.Bury(path, v1, at, at.Add(time.Hour), opt)
					require.NoError(t, err)
				},
				want: 2,
			},
		}

		for name, tc := range tests {
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				path := sidecar(t)

				_, err := history.Supersede(path, []byte("seed"), at, quiet())
				require.NoError(t, err)

				damage(t, path)

				rec := logtest.NewRecorder()

				tc.run(t, path, history.WithLogger(rec.Logger()))

				assert.Len(t, rec.Events("history_records_skipped"), tc.want)
			})
		}
	})
}

func TestScanReportsNothingForIntactSidecar(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)
	v1 := []byte("{\n  \"v\": 1\n}")

	t.Run("a blank line among committed records", func(t *testing.T) {
		t.Parallel()

		path := sidecar(t)

		_, err := history.Supersede(path, v1, at, quiet())
		require.NoError(t, err)

		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
		require.NoError(t, err)

		_, err = f.WriteString("\n")
		require.NoError(t, err)
		require.NoError(t, f.Close())

		rec, ok, err := history.Newest(path, history.WithLogger(
			logtest.FailOn(t, "history_records_skipped")))
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, string(v1), rec.Content, "a blank line is not a record")
	})

	t.Run("an intact record the predicate declines", func(t *testing.T) {
		t.Parallel()

		// Supersede's dedupe looks past a trailing tombstone. Declining a
		// record is not damage, and reporting it as such would fire the
		// warning on every reappearance the archive handles correctly.
		path := sidecar(t)

		_, err := history.Bury(path, v1, at, at.Add(time.Hour), quiet())
		require.NoError(t, err)

		appended, err := history.Supersede(path, v1, at.Add(2*time.Hour), history.WithLogger(
			logtest.FailOn(t, "history_records_skipped")))
		require.NoError(t, err)
		assert.False(t, appended)
	})

	t.Run("damage older than the newest matching record", func(t *testing.T) {
		t.Parallel()

		// The backward scan answers before it ever reaches these bytes. The
		// event says "a read I just did walked past damage", not "this file
		// is damaged", so a read that walked past nothing stays silent.
		path := sidecar(t)

		_, err := history.Supersede(path, v1, at, quiet())
		require.NoError(t, err)

		damage(t, path)

		_, err = history.Supersede(path, []byte("newest"), at.Add(time.Hour), quiet())
		require.NoError(t, err)

		rec, ok, err := history.Newest(path, history.WithLogger(
			logtest.FailOn(t, "history_records_skipped")))
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, "newest", rec.Content)
	})

	t.Run("a missing sidecar", func(t *testing.T) {
		t.Parallel()

		_, ok, err := history.Newest(sidecar(t), history.WithLogger(
			logtest.FailOn(t, "history_records_skipped")))
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("an empty sidecar", func(t *testing.T) {
		t.Parallel()

		path := sidecar(t)
		require.NoError(t, os.WriteFile(path, nil, 0o600))

		_, ok, err := history.Newest(path, history.WithLogger(
			logtest.FailOn(t, "history_records_skipped")))
		require.NoError(t, err)
		assert.False(t, ok)
	})
}

func TestScanSkipsMalformedRecord(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)
	v1 := []byte("{\n  \"v\": 1\n}")
	v2 := []byte("{\n  \"v\": 2\n}")

	t.Run("a read walks past the damage to the newest intact record", func(t *testing.T) {
		t.Parallel()

		path := sidecar(t)

		_, err := history.Supersede(path, v1, at, quiet())
		require.NoError(t, err)

		damage(t, path)

		rec, ok, err := history.Newest(path, quiet())
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, string(v1), rec.Content)
	})

	t.Run("the write path keeps working", func(t *testing.T) {
		t.Parallel()

		// The scan runs on every history-retaining commit, so failing on the
		// damaged line would freeze the object it sits beside forever.
		path := sidecar(t)

		_, err := history.Supersede(path, v1, at, quiet())
		require.NoError(t, err)

		damage(t, path)

		appended, err := history.Supersede(path, v2, at.Add(time.Hour), quiet())
		require.NoError(t, err)
		assert.True(t, appended)

		rec, ok, err := history.Newest(path, quiet())
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, string(v2), rec.Content)
	})

	t.Run("dedupe still answers from behind the damage", func(t *testing.T) {
		t.Parallel()

		path := sidecar(t)

		_, err := history.Supersede(path, v1, at, quiet())
		require.NoError(t, err)

		damage(t, path)

		appended, err := history.Supersede(path, v1, at.Add(time.Hour), quiet())
		require.NoError(t, err)
		assert.False(t, appended, "the intact record behind the damage still dedupes")
	})

	t.Run("a damaged tombstone goes unclosed by restore", func(t *testing.T) {
		t.Parallel()

		// Restore reads past the damage to the content record beneath it, so
		// it cannot tell the object was ever deleted and declines to close
		// the deletion. The cost is a missing marker, not a missing version.
		path := sidecar(t)

		_, err := history.Bury(path, v1, at, at.Add(time.Hour), quiet())
		require.NoError(t, err)

		rotNewest(t, path)

		appended, err := history.Restore(path, v2, at.Add(2*time.Hour), quiet())
		require.NoError(t, err)
		assert.False(t, appended)
	})

	t.Run("a damaged tombstone earns a fresh one from bury", func(t *testing.T) {
		t.Parallel()

		// Bury reads past the damage the same way, so the disappearance it
		// can no longer see recorded is recorded again rather than dropped.
		path := sidecar(t)

		_, err := history.Bury(path, v1, at, at.Add(time.Hour), quiet())
		require.NoError(t, err)

		rotNewest(t, path)

		buried, err := history.Bury(path, v1, at, at.Add(2*time.Hour), quiet())
		require.NoError(t, err)
		assert.True(t, buried)

		rec, ok, err := history.Newest(path, quiet())
		require.NoError(t, err)
		require.True(t, ok)
		assert.True(t, rec.Deleted)
	})

	t.Run("a wholly unreadable sidecar still appends", func(t *testing.T) {
		t.Parallel()

		path := sidecar(t)

		damage(t, path)

		appended, err := history.Supersede(path, v1, at, quiet())
		require.NoError(t, err)
		assert.True(t, appended)

		rec, ok, err := history.Newest(path, quiet())
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, string(v1), rec.Content)
	})
}

func TestNewestLargeRecord(t *testing.T) {
	t.Parallel()

	// A record far larger than the seed window forces the backward scan to
	// double until the record's start comes into view.
	at := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)
	path := sidecar(t)

	big := []byte(`{"data":"` + strings.Repeat("x", 64<<10) + `"}`)

	_, err := history.Supersede(path, []byte("small"), at)
	require.NoError(t, err)

	_, err = history.Supersede(path, big, at.Add(time.Hour))
	require.NoError(t, err)

	rec, ok, err := history.Newest(path)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, string(big), rec.Content)
	assert.Equal(t, shaOf(big), rec.SHA256)
}

func TestAppendRejectsNonUTF8(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)
	invalid := []byte{0xff, 0xfe, 'x'}

	path := sidecar(t)

	_, err := history.Supersede(path, invalid, at)
	require.ErrorIs(t, err, history.ErrContentNotUTF8)

	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr), "a refused append creates no sidecar")

	// Restore checks the sidecar first, so seed a tombstone to reach its gate.
	_, err = history.Bury(path, []byte("ok"), at, at.Add(time.Hour))
	require.NoError(t, err)

	_, err = history.Restore(path, invalid, at.Add(2*time.Hour))
	require.ErrorIs(t, err, history.ErrContentNotUTF8)

	fresh := sidecar(t)

	_, err = history.Bury(fresh, invalid, at, at.Add(time.Hour))
	require.ErrorIs(t, err, history.ErrContentNotUTF8)
}

func TestSidecarModeOwnerOnly(t *testing.T) {
	t.Parallel()

	// The sidecar carries superseded variable values and other secret-bearing
	// payloads, so it takes the archive's owner-only mode.
	path := sidecar(t)

	_, err := history.Supersede(path, []byte("secret"), time.Time{})
	require.NoError(t, err)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestContentRoundTripsByteForByte(t *testing.T) {
	t.Parallel()

	// The 2-space indented serialize.Marshal shape with the characters JSON
	// encoders most like to rewrite: &, <, >, a newline, and a quote. The
	// sidecar must reproduce it exactly and digest the original bytes.
	original := []byte("{\n  \"url\": \"https://x?a=1&b=<2>\",\n  \"note\": \"line\\nquote\\\"\"\n}")

	path := sidecar(t)

	_, err := history.Supersede(path, original, time.Time{})
	require.NoError(t, err)

	recs := readRecords(t, path)
	require.Len(t, recs, 1)
	assert.Equal(t, string(original), recs[0].Content, "the content round-trips byte for byte")
	assert.Equal(t, shaOf(original), recs[0].SHA256, "the digest is over the exact original bytes")

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "a=1&b=<2>",
		"HTML escaping stays off so the sidecar stays greppable")

	assert.True(t, recs[0].FetchedAt.IsZero(), "an unknown fetch time round-trips as zero")
	assert.NotContains(t, string(raw), "fetchedAt", "a zero fetch time is omitted")
}
