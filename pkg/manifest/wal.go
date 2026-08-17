package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"time"

	"go.jacobcolvin.com/hcp_archiver/pkg/atomicfile"
)

// walKind tags a log record with the ledger field it mutates, so a replay
// applies each record to the same in-memory structure [Ledger.Flush] appended
// it from.
type walKind string

const (
	// WalEntry records one object's [Entry] under its archive-relative path.
	walEntry walKind = "entry"
	// WalWatermark records an append-mostly collection's high-water mark.
	walWatermark walKind = "watermark"
	// WalCompleted records that a collection has been walked to its end.
	walCompleted walKind = "completed"
	// WalSettled records whether a collection's newest full walk settled it. It
	// carries its boolean value because, unlike completion, settledness is
	// non-monotonic: a walk that reaches a still-running element flips it back to
	// false.
	walSettled walKind = "settled"
	// WalRun records the run-level metadata (last run time, count, and summary).
	walRun walKind = "run"
)

// walRecord is one line of the append-only log: a single mutation the ledger
// accumulated since the last compaction. Only the fields the [walKind] uses are
// set, so a record stays compact and a replay reconstructs the same state a full
// snapshot would.
type walRecord struct {
	At        time.Time  `json:"at,omitzero"`
	LastRunAt time.Time  `json:"lastRunAt,omitzero"`
	Entry     *Entry     `json:"entry,omitempty"`
	LastRun   *RunRecord `json:"lastRun,omitempty"`
	Kind      walKind    `json:"kind"`
	Path      string     `json:"path,omitempty"`
	Key       string     `json:"key,omitempty"`
	// Shard names the shard the record belongs to, so a replay of the single
	// org-level log routes it to the shard whose snapshot it folds into. The
	// org root is the empty key and is omitted.
	Shard    string `json:"shard,omitempty"`
	RunCount int    `json:"runCount,omitempty"`
	// Settled carries the value of a walSettled record. It is omitempty, so a
	// false value replays as the zero value, which is the intended false; the
	// walSettled kind still identifies the record.
	Settled bool `json:"settled,omitempty"`
}

// recordShardKey derives the shard key a record's own payload routes to: the
// fallback identity a replay consults when the record's shard tag names a
// path that no longer exists (a rename alias whose link the operator has
// since removed) while the payload's physical subtree survives. A run record
// carries no path and belongs to the org root.
func recordShardKey(rec *walRecord) string {
	switch rec.Kind {
	case walEntry:
		return shardKey(rec.Path)
	case walWatermark, walCompleted, walSettled:
		return shardKey(rec.Key)
	case walRun:
		return ""
	default:
		return ""
	}
}

// walRecordClass ranks a record for the durability order one appended batch
// must satisfy (see [shard.drainDirty]): collection unsettlements lead, then
// unsettled-status entries, then settled-status entries, then the skip
// signals (settled gate entries alongside the collection-level watermark,
// completion, and settlement records), then the run metadata. Merging several
// shards' drains into one batch stable-sorts by this class, so the guard rule
// holds across the whole batch, not merely within one shard's slice of it.
//
// A settled gate entry (a cleared reference or obligation) ranks behind the
// ordinary settled entries even though its status is settled too, because the
// clear is itself a skip signal for the entry it mirrors: a healing run
// records the healed foreign entry and the gate clear in one batch, and a
// tear that persisted the clear without the healed entry would disable the
// retry the gate exists to force. Trailing, a tear loses at most the clear,
// which costs one redundant re-list and converges.
func walRecordClass(rec *walRecord) int {
	switch rec.Kind {
	case walSettled:
		if !rec.Settled {
			return 0
		}

		return 3

	case walEntry:
		if rec.Entry == nil {
			return 2
		}

		switch {
		case !rec.Entry.Status.Settled():
			return 1
		case rec.Entry.Status.IsGate():
			return 3
		default:
			return 2
		}

	case walWatermark, walCompleted:
		return 3
	case walRun:
		return 4
	default:
		return 4
	}
}

// appendLog appends recs to the log at path as newline-terminated JSON lines,
// creating the file and any missing parent as it goes, and returns the number of
// bytes written.
//
// Each record is one line and the terminating newline is the commit marker: a
// crash mid-append leaves an uncommitted fragment past the final newline that
// the next append trims (see [atomicfile.Append]) and a replay drops. The batch
// is written in one call and flushed to stable storage before returning, but a
// power loss during that flush may persist its blocks out of order, so a replay
// trusts complete lines only up to the first that fails to parse (see
// [replayLog]): a torn write loses at most the interrupted batch's suffix, and
// never the records committed before it.
func appendLog(path string, recs []walRecord) (int64, error) {
	var buf bytes.Buffer

	for i := range recs {
		line, err := json.Marshal(&recs[i])
		if err != nil {
			return 0, fmt.Errorf("marshal log record: %w", err)
		}

		buf.Write(line)
		buf.WriteByte('\n')
	}

	_, err := atomicfile.Append(path, buf.Bytes())
	if err != nil {
		return 0, fmt.Errorf("append log: %w", err)
	}

	return int64(buf.Len()), nil
}

// replayLog reads the append-only log at path and returns its committed records
// in order along with the committed log's size in bytes, or nil and zero when no
// log exists. Recovery events (a truncated torn suffix) are reported to logger.
//
// The newline is each record's commit marker, but [appendLog] writes a whole
// batch in one multi-block call, and a power loss mid-fsync may persist a later
// block without an earlier one, so a complete line is trusted only up to the
// first line that fails to parse. Bytes after the final newline are an
// incomplete trailing write with no commit marker and are dropped; a complete
// line that does not parse ends the committed log at its own start: the records
// before it replay, and the file is truncated to that offset and flushed so the
// surviving suffix cannot land ahead of the next append (see
// [atomicfile.Append], which seeks to the file's last newline). Every log
// record is re-derivable by re-walking, so the truncation costs a re-fetch of
// the discarded delta rather than the shard's history.
//
// The reported size always matches the committed log left on disk, so the
// shard's byte counter never sits ahead of the file and trips compaction early
// before the next fold resets it.
func replayLog(path string, logger *slog.Logger) ([]walRecord, int64, error) {
	//nolint:gosec // The log path is derived from the operator-chosen manifest path.
	data, err := os.ReadFile(path)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, 0, nil
	case err != nil:
		return nil, 0, fmt.Errorf("read log %q: %w", path, err)
	}

	recs := make([]walRecord, 0, bytes.Count(data, []byte("\n")))

	// The committed offset is the start of the next unread line. On a clean scan
	// it lands just past the final newline, excluding any torn trailing fragment,
	// which the next append truncates before writing.
	var committed int64

	for committed < int64(len(data)) {
		nl := bytes.IndexByte(data[committed:], '\n')
		if nl < 0 {
			break
		}

		line := data[committed : committed+int64(nl)]

		if len(bytes.TrimSpace(line)) > 0 {
			var rec walRecord

			err = json.Unmarshal(line, &rec)
			if err != nil {
				truncErr := truncateLog(path, committed)
				if truncErr != nil {
					return nil, 0, truncErr
				}

				logger.Warn("ledger_log_truncated",
					slog.String("path", path),
					slog.Int64("committed_bytes", committed),
					slog.Int64("discarded_bytes", int64(len(data))-committed),
					slog.String("error", err.Error()),
				)

				return recs, committed, nil
			}

			recs = append(recs, rec)
		}

		committed += int64(nl) + 1
	}

	return recs, committed, nil
}

// truncateLog cuts the log at path back to size bytes and flushes the cut, so a
// discarded torn suffix cannot resurface after a crash or shift where the next
// append lands.
func truncateLog(path string, size int64) error {
	//nolint:gosec // The log path is derived from the operator-chosen manifest path.
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open log %q: %w", path, err)
	}

	opErr := f.Truncate(size)
	if opErr == nil {
		opErr = f.Sync()
	}

	closeErr := f.Close()

	switch {
	case opErr != nil:
		return fmt.Errorf("truncate log %q: %w", path, opErr)
	case closeErr != nil:
		return fmt.Errorf("close log %q: %w", path, closeErr)
	}

	return nil
}
