package store_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/history"
	"go.jacobcolvin.com/hcp_archiver/logtest"
	"go.jacobcolvin.com/hcp_archiver/store"
)

// historyRecords parses the object's sidecar, oldest record first.
func historyRecords(t *testing.T, s *store.Store, relPath string) []history.Record {
	t.Helper()

	data, err := os.ReadFile(s.AbsPath(s.HistoryPath(relPath)))
	require.NoError(t, err)

	var out []history.Record

	for line := range strings.SplitSeq(strings.TrimSuffix(string(data), "\n"), "\n") {
		var rec history.Record

		require.NoError(t, json.Unmarshal([]byte(line), &rec))

		out = append(out, rec)
	}

	return out
}

// damageSidecar appends a committed line that does not parse to the object's
// history sidecar, creating the sidecar when the object has none yet.
func damageSidecar(t *testing.T, s *store.Store, relPath string) {
	t.Helper()

	abs := s.AbsPath(s.HistoryPath(relPath))

	f, err := os.OpenFile(abs, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	require.NoError(t, err)

	_, err = f.WriteString("{\"fetchedAt\":\"2026-08-13T00:00:00Z\",\"sha\xff\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

// rotNewestLine rewrites a sidecar's newest committed line so it no longer
// parses, putting the damage on a record that already carries meaning.
func rotNewestLine(t *testing.T, abs string) {
	t.Helper()

	data, err := os.ReadFile(abs)
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	require.NotEmpty(t, lines)

	lines[len(lines)-1] = "{\"fetchedAt\":\"2026-08-13T00:00:00Z\",\"sha\xff"

	require.NoError(t, os.WriteFile(abs, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
}

// sidecarExists reports whether the object at relPath has a history sidecar.
func sidecarExists(t *testing.T, s *store.Store, relPath string) bool {
	t.Helper()

	present, err := s.Exists(s.HistoryPath(relPath))
	require.NoError(t, err)

	return present
}

func TestStore_HistoryPath(t *testing.T) {
	t.Parallel()

	s := store.New(t.TempDir())

	assert.Equal(t, "projects/p/workspaces/ws/variables.history.ndjson",
		s.HistoryPath("projects/p/workspaces/ws/variables.json"))
}

func TestStore_WriteJSONBytes_RottenExistingContentStillCommits(t *testing.T) {
	t.Parallel()

	// A history-kept object whose on-disk bytes rotted past valid UTF-8 must
	// not wedge: the version is unrecordable either way, so it is skipped with
	// a warning and the fresh bytes land, rather than every future commit
	// repeating the refusal until an operator hand-deletes the file.
	const relPath = "projects/p/workspaces/ws/variables.json"

	fetchedAt := time.Date(2026, time.August, 11, 9, 0, 0, 0, time.UTC)
	s := store.New(t.TempDir())

	require.NoError(t, os.MkdirAll(filepath.Dir(s.AbsPath(relPath)), 0o750))
	require.NoError(t, os.WriteFile(s.AbsPath(relPath), []byte("{\"v\":\xff}"), 0o600))

	v2 := []byte("{\n  \"v\": 2\n}")

	res, err := s.WriteJSONBytes(relPath, v2, store.WithHistory(fetchedAt, fetchedAt))
	require.NoError(t, err)
	assert.True(t, res.Changed)

	got, err := os.ReadFile(s.AbsPath(relPath))
	require.NoError(t, err)
	assert.Equal(t, v2, got, "the fresh bytes land past the unrecordable version")

	assert.False(t, sidecarExists(t, s, relPath), "the rotted version is not recorded")
}

func TestStore_BuryHistory_RottenContentStillLandsTombstone(t *testing.T) {
	t.Parallel()

	const relPath = "projects/p/workspaces/ws/variables.json"

	fetchedAt := time.Date(2026, time.August, 11, 9, 0, 0, 0, time.UTC)
	deletedAt := fetchedAt.Add(48 * time.Hour)
	v1 := []byte("{\n  \"v\": 1\n}")
	v2 := []byte("{\n  \"v\": 2\n}")

	s := store.New(t.TempDir())

	_, err := s.WriteJSONBytes(relPath, v1, store.WithHistory(time.Time{}, fetchedAt))
	require.NoError(t, err)

	_, err = s.WriteJSONBytes(relPath, v2, store.WithHistory(fetchedAt, fetchedAt))
	require.NoError(t, err)

	// The file rots after the commit; the disappearance must still record.
	require.NoError(t, os.WriteFile(s.AbsPath(relPath), []byte("{\"v\":\xff}"), 0o600))

	buried, err := s.BuryHistory(relPath, fetchedAt, deletedAt)
	require.NoError(t, err)
	assert.True(t, buried)

	recs := historyRecords(t, s, relPath)
	require.Len(t, recs, 2, "the superseded version, then the tombstone; the rot flushes nothing")
	assert.Equal(t, string(v1), recs[0].Content)
	assert.True(t, recs[1].Deleted, "the tombstone lands past the unrecordable content")
}

func TestStore_WriteJSONBytes_WithHistory(t *testing.T) {
	t.Parallel()

	const relPath = "projects/p/workspaces/ws/variables.json"

	fetchedAt := time.Date(2026, time.August, 11, 9, 0, 0, 0, time.UTC)
	now := fetchedAt.Add(24 * time.Hour)
	v1 := []byte("{\n  \"v\": 1\n}")
	v2 := []byte("{\n  \"v\": 2\n}")

	t.Run("a changed commit supersedes the outgoing content", func(t *testing.T) {
		t.Parallel()

		s := store.New(t.TempDir())

		_, err := s.WriteJSONBytes(relPath, v1, store.WithHistory(time.Time{}, fetchedAt))
		require.NoError(t, err)

		res, err := s.WriteJSONBytes(relPath, v2, store.WithHistory(fetchedAt, now))
		require.NoError(t, err)
		assert.True(t, res.Changed)

		got, err := os.ReadFile(s.AbsPath(relPath))
		require.NoError(t, err)
		assert.Equal(t, v2, got, "the file holds the new content")

		recs := historyRecords(t, s, relPath)
		require.Len(t, recs, 1)
		assert.Equal(t, string(v1), recs[0].Content, "the sidecar holds the superseded content")
		assert.Equal(t, fetchedAt, recs[0].FetchedAt, "stamped with the prior run's fetch time")
	})

	t.Run("an unchanged commit appends nothing", func(t *testing.T) {
		t.Parallel()

		s := store.New(t.TempDir())

		_, err := s.WriteJSONBytes(relPath, v1, store.WithHistory(time.Time{}, fetchedAt))
		require.NoError(t, err)

		res, err := s.WriteJSONBytes(relPath, v1, store.WithHistory(fetchedAt, now))
		require.NoError(t, err)
		assert.False(t, res.Changed)

		assert.False(t, sidecarExists(t, s, relPath),
			"an object that never changes never grows a sidecar")
	})

	t.Run("a first write creates no sidecar", func(t *testing.T) {
		t.Parallel()

		s := store.New(t.TempDir())

		_, err := s.WriteJSONBytes(relPath, v1, store.WithHistory(time.Time{}, now))
		require.NoError(t, err)

		assert.False(t, sidecarExists(t, s, relPath))
	})

	t.Run("a zero fetchedAt falls back to the file's modification time", func(t *testing.T) {
		t.Parallel()

		// A rebuilt ledger carries no prior FetchedAt; the file itself still
		// knows roughly when its content landed.
		s := store.New(t.TempDir())

		_, err := s.WriteJSONBytes(relPath, v1)
		require.NoError(t, err)

		mtime := time.Date(2026, time.June, 1, 8, 30, 0, 0, time.UTC)
		require.NoError(t, os.Chtimes(s.AbsPath(relPath), mtime, mtime))

		_, err = s.WriteJSONBytes(relPath, v2, store.WithHistory(time.Time{}, now))
		require.NoError(t, err)

		recs := historyRecords(t, s, relPath)
		require.Len(t, recs, 1)
		assert.True(t, mtime.Equal(recs[0].FetchedAt),
			"the superseded record is stamped with the file's mtime")
	})

	t.Run("a zero-length existing file supersedes nothing", func(t *testing.T) {
		t.Parallel()

		// An empty file holds no version, and a record carrying it would lose
		// its content field to omitempty: neither a version nor a tombstone.
		s := store.New(t.TempDir())

		require.NoError(t, os.MkdirAll(filepath.Dir(s.AbsPath(relPath)), 0o700))
		require.NoError(t, os.WriteFile(s.AbsPath(relPath), nil, 0o600))

		_, err := s.WriteJSONBytes(relPath, v1, store.WithHistory(fetchedAt, now))
		require.NoError(t, err)

		assert.False(t, sidecarExists(t, s, relPath))
	})

	t.Run("a tombstone stays open until the returning bytes land", func(t *testing.T) {
		t.Parallel()

		if os.Geteuid() == 0 {
			t.Skip("root bypasses directory permissions, so the rename cannot be made to fail")
		}

		// The returning content may only close a tombstone once the file holds
		// it. Recorded ahead of a rename that then failed, it would be a
		// version the file never held, and the retry would supersede the
		// file's older content on top of it, ordering the timeline backward.
		s := store.New(t.TempDir())

		_, err := s.WriteJSONBytes(relPath, v1, store.WithHistory(time.Time{}, fetchedAt))
		require.NoError(t, err)

		_, err = s.BuryHistory(relPath, fetchedAt, now)
		require.NoError(t, err)

		// A directory the store may read but not stage a temp file in fails
		// the rename while leaving every history read intact.
		dir := filepath.Dir(s.AbsPath(relPath))
		require.NoError(t, os.Chmod(dir, 0o500))

		t.Cleanup(func() {
			//nolint:errcheck // Best-effort restore so TempDir cleanup can unlink.
			_ = os.Chmod(dir, 0o700)
		})

		_, err = s.WriteJSONBytes(relPath, v2, store.WithHistory(fetchedAt, now))
		require.Error(t, err)

		recs := historyRecords(t, s, relPath)
		require.Len(t, recs, 2)
		assert.True(t, recs[1].Deleted, "the tombstone is still the newest record")

		// The retry lands the bytes and only then closes the tombstone.
		require.NoError(t, os.Chmod(dir, 0o700))

		_, err = s.WriteJSONBytes(relPath, v2, store.WithHistory(fetchedAt, now))
		require.NoError(t, err)

		got, err := os.ReadFile(s.AbsPath(relPath))
		require.NoError(t, err)
		assert.Equal(t, v2, got)

		recs = historyRecords(t, s, relPath)
		require.Len(t, recs, 3)
		assert.Equal(t, string(v1), recs[0].Content, "the buried version stays oldest")
		assert.True(t, recs[1].Deleted)
		assert.Equal(t, string(v2), recs[2].Content, "the returning version closes the tombstone")
	})

	t.Run("a tombstone that cannot be closed leaves the commit standing", func(t *testing.T) {
		t.Parallel()

		if os.Geteuid() == 0 {
			t.Skip("root bypasses file permissions, so the sidecar append cannot be made to fail")
		}

		// The closing record is a consistency marker, not the only copy of
		// anything. Failing the commit would leave the ledger describing
		// content the archive no longer holds, so the bytes stand and the
		// cause is reported for the caller to classify.
		s := store.New(t.TempDir())

		_, err := s.WriteJSONBytes(relPath, v1, store.WithHistory(time.Time{}, fetchedAt))
		require.NoError(t, err)

		_, err = s.BuryHistory(relPath, fetchedAt, now)
		require.NoError(t, err)

		// A read-only sidecar fails the append, which opens read-write, while
		// the backward scan and the object's own rename both still succeed.
		sc := s.AbsPath(s.HistoryPath(relPath))
		require.NoError(t, os.Chmod(sc, 0o400))

		res, err := s.WriteJSONBytes(relPath, v2, store.WithHistory(fetchedAt, now))
		require.ErrorIs(t, err, store.ErrHistoryNotClosed)
		assert.True(t, res.Changed, "the result describes the bytes that landed")

		got, err := os.ReadFile(s.AbsPath(relPath))
		require.NoError(t, err)
		assert.Equal(t, v2, got, "the object file holds the committed bytes")
		assert.Len(t, historyRecords(t, s, relPath), 2, "the sidecar did not move")

		// The next commit re-attempts the close, in order.
		require.NoError(t, os.Chmod(sc, 0o600))

		_, err = s.WriteJSONBytes(relPath, v2, store.WithHistory(fetchedAt, now))
		require.NoError(t, err)

		recs := historyRecords(t, s, relPath)
		require.Len(t, recs, 3)
		assert.True(t, recs[1].Deleted)
		assert.Equal(t, string(v2), recs[2].Content, "the returning version closes the tombstone")
	})

	t.Run("a history append that cannot land fails the write intact", func(t *testing.T) {
		t.Parallel()

		// A directory squatting on the sidecar path makes the append fail;
		// the object file must keep its old content so no version is lost.
		s := store.New(t.TempDir())

		_, err := s.WriteJSONBytes(relPath, v1, store.WithHistory(time.Time{}, fetchedAt))
		require.NoError(t, err)

		require.NoError(t, os.MkdirAll(s.AbsPath(s.HistoryPath(relPath)), 0o700))

		_, err = s.WriteJSONBytes(relPath, v2, store.WithHistory(fetchedAt, now))
		require.Error(t, err)

		got, readErr := os.ReadFile(s.AbsPath(relPath))
		require.NoError(t, readErr)
		assert.Equal(t, v1, got, "the object file keeps the old content")
	})
}

func TestStore_WriteJSONBytes_DefaultKeepsNoHistory(t *testing.T) {
	t.Parallel()

	// Without the option a commit only overwrites: changed content replaces
	// the file and nothing else is written.
	const relPath = "org.json"

	s := store.New(t.TempDir())

	_, err := s.WriteJSONBytes(relPath, []byte(`{"v":1}`))
	require.NoError(t, err)

	res, err := s.WriteJSONBytes(relPath, []byte(`{"v":2}`))
	require.NoError(t, err)
	assert.True(t, res.Changed)

	assert.False(t, sidecarExists(t, s, relPath))
}

func TestStore_HistoryLoggerReachesHistory(t *testing.T) {
	t.Parallel()

	// The sidecar's absolute path is the operator-facing value of the event
	// and the one thing the store composes, so every call site is exercised
	// through the logger it was handed.
	const relPath = "projects/p/workspaces/ws/variables.json"

	fetchedAt := time.Date(2026, time.August, 11, 9, 0, 0, 0, time.UTC)
	now := fetchedAt.Add(24 * time.Hour)
	v1 := []byte("{\n  \"v\": 1\n}")
	v2 := []byte("{\n  \"v\": 2\n}")

	tests := map[string]struct {
		run  func(t *testing.T, s *store.Store)
		want int
	}{
		"supersede on a changed commit": {
			run: func(t *testing.T, s *store.Store) {
				t.Helper()

				_, err := s.WriteJSONBytes(relPath, v1, store.WithHistory(time.Time{}, fetchedAt))
				require.NoError(t, err)

				damageSidecar(t, s, relPath)

				// The superseded record lands over the damage, so the close
				// that follows matches it without reading any further.
				_, err = s.WriteJSONBytes(relPath, v2, store.WithHistory(fetchedAt, now))
				require.NoError(t, err)
			},
			want: 1,
		},
		"close on an unchanged commit": {
			run: func(t *testing.T, s *store.Store) {
				t.Helper()

				_, err := s.WriteJSONBytes(relPath, v1, store.WithHistory(time.Time{}, fetchedAt))
				require.NoError(t, err)

				_, err = s.BuryHistory(relPath, fetchedAt, now)
				require.NoError(t, err)

				rotNewestLine(t, s.AbsPath(s.HistoryPath(relPath)))

				// Re-committing the same bytes leaves only the tombstone
				// close to run, so the event can come from nowhere else.
				_, err = s.WriteJSONBytes(relPath, v1, store.WithHistory(fetchedAt, now))
				require.NoError(t, err)
			},
			want: 1,
		},
		"bury scans twice": {
			run: func(t *testing.T, s *store.Store) {
				t.Helper()

				_, err := s.WriteJSONBytes(relPath, v1, store.WithHistory(time.Time{}, fetchedAt))
				require.NoError(t, err)

				damageSidecar(t, s, relPath)

				_, err = s.BuryHistory(relPath, fetchedAt, now)
				require.NoError(t, err)
			},
			want: 2,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			rec := logtest.NewRecorder()
			s := store.New(t.TempDir(), store.WithLogger(rec.Logger()))

			tc.run(t, s)

			events := rec.Events("history_records_skipped")
			require.Len(t, events, tc.want)

			for _, e := range events {
				assert.Equal(t, s.AbsPath(s.HistoryPath(relPath)), e.Attrs["path"],
					"the event names the sidecar's absolute path")
			}
		})
	}
}

func TestStore_BuryHistory(t *testing.T) {
	t.Parallel()

	const relPath = "projects/p/workspaces/ws/tags.json"

	fetchedAt := time.Date(2026, time.August, 11, 9, 0, 0, 0, time.UTC)
	deletedAt := fetchedAt.Add(24 * time.Hour)
	content := []byte("{\n  \"v\": 1\n}")

	t.Run("buries once per disappearance", func(t *testing.T) {
		t.Parallel()

		s := store.New(t.TempDir())

		_, err := s.WriteJSONBytes(relPath, content)
		require.NoError(t, err)

		buried, err := s.BuryHistory(relPath, fetchedAt, deletedAt)
		require.NoError(t, err)
		assert.True(t, buried)

		// Mutable re-404s every run; the repeated bury must not append.
		buried, err = s.BuryHistory(relPath, fetchedAt, deletedAt.Add(time.Hour))
		require.NoError(t, err)
		assert.False(t, buried)

		recs := historyRecords(t, s, relPath)
		require.Len(t, recs, 2)
		assert.Equal(t, string(content), recs[0].Content)
		assert.Equal(t, fetchedAt, recs[0].FetchedAt)
		assert.True(t, recs[1].Deleted)
		assert.Equal(t, deletedAt, recs[1].FetchedAt)

		got, err := os.ReadFile(s.AbsPath(relPath))
		require.NoError(t, err)
		assert.Equal(t, content, got, "the last-known file is left in place")
	})

	t.Run("nothing to bury creates no sidecar", func(t *testing.T) {
		t.Parallel()

		s := store.New(t.TempDir())

		buried, err := s.BuryHistory(relPath, fetchedAt, deletedAt)
		require.NoError(t, err)
		assert.False(t, buried)

		assert.False(t, sidecarExists(t, s, relPath))
	})

	t.Run("a zero fetchedAt falls back to the file's modification time", func(t *testing.T) {
		t.Parallel()

		s := store.New(t.TempDir())

		_, err := s.WriteJSONBytes(relPath, content)
		require.NoError(t, err)

		mtime := time.Date(2026, time.June, 1, 8, 30, 0, 0, time.UTC)
		require.NoError(t, os.Chtimes(s.AbsPath(relPath), mtime, mtime))

		buried, err := s.BuryHistory(relPath, time.Time{}, deletedAt)
		require.NoError(t, err)
		assert.True(t, buried)

		recs := historyRecords(t, s, relPath)
		require.Len(t, recs, 2)
		assert.True(t, mtime.Equal(recs[0].FetchedAt),
			"the flushed content is stamped with the file's mtime")
	})
}

func TestStore_WriteJSONBytes_ConcurrentHistory(t *testing.T) {
	t.Parallel()

	// An object several collections address at once (a user, hydrated from
	// teams, run creators, and event actors alike) is committed from many
	// goroutines. Appending to a sidecar reads it first, so without the store's
	// per-object serialization two commits scan the same tail and each records
	// the version the other already did, or overlap at one offset and tear the
	// line at the seam.
	const (
		relPath = "users/user-1.json"
		writers = 16
	)

	fetchedAt := time.Date(2026, time.August, 11, 9, 0, 0, 0, time.UTC)
	now := fetchedAt.Add(24 * time.Hour)
	seed := []byte("{\n  \"v\": 0\n}")

	s := store.New(t.TempDir())

	_, err := s.WriteJSONBytes(relPath, seed, store.WithHistory(time.Time{}, fetchedAt))
	require.NoError(t, err)

	var wg sync.WaitGroup

	for i := range writers {
		wg.Go(func() {
			payload := fmt.Appendf(nil, "{\n  \"v\": %d\n}", i+1)

			_, wErr := s.WriteJSONBytes(relPath, payload, store.WithHistory(fetchedAt, now))
			assert.NoError(t, wErr)
		})
	}

	wg.Wait()

	// Serialized, the writers displace each other in some order: the seed plus
	// every payload but the last one to land is recorded, each exactly once, and
	// the content the file ends up holding is not in the sidecar at all.
	recs := historyRecords(t, s, relPath)
	require.Len(t, recs, writers, "one record per displaced version, no duplicates")

	final, err := os.ReadFile(s.AbsPath(relPath))
	require.NoError(t, err)

	seen := make(map[string]struct{}, len(recs))

	for i, rec := range recs {
		assert.NotEqualf(t, string(final), rec.Content, "record %d records the content the file still holds", i)

		_, dup := seen[rec.Content]
		assert.Falsef(t, dup, "record %d duplicates a version already recorded", i)

		seen[rec.Content] = struct{}{}
	}
}
