package manifest

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReplayLogExcludesTornFragment(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "log.ndjson")

	// Two committed, newline-terminated records followed by a torn trailing
	// fragment with no commit marker, as a crash mid-append would leave.
	committed := []byte(`{"kind":"completed","key":"a"}` + "\n" +
		`{"kind":"completed","key":"b"}` + "\n")
	torn := []byte(`{"kind":"completed","key":"c`)

	require.NoError(t, os.WriteFile(path, append(append([]byte{}, committed...), torn...), 0o600))

	recs, size, err := replayLog(path, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	require.Len(t, recs, 2)

	// The reported size is the committed log, not the raw file: it excludes the
	// torn fragment the next append will truncate.
	require.Equal(t, int64(len(committed)), size)
}

func TestReplayLogTruncatesAtUnparsableLine(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "log.ndjson")

	// One intact record, then a zero-filled line where a power loss persisted the
	// batch's blocks out of order, then a later line the same loss left intact.
	// The newline makes the garbled line "complete", so only the parse failure
	// marks where the committed log ends.
	committed := []byte(`{"kind":"completed","key":"a"}` + "\n")

	garbled := append(make([]byte, 32), '\n')
	survivor := []byte(`{"kind":"completed","key":"b"}` + "\n")

	data := append(append(append([]byte{}, committed...), garbled...), survivor...)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	recs, size, err := replayLog(path, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	require.Len(t, recs, 1)
	require.Equal(t, "a", recs[0].Key)

	// The committed size stops at the garbled line's start, and the file is
	// physically truncated to match: the next append seeks to the file's last
	// newline, so a surviving line past the cut would otherwise entomb the
	// garbage and desync the shard's byte counter.
	require.Equal(t, int64(len(committed)), size)

	onDisk, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, committed, onDisk)
}
