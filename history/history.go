package history

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.jacobcolvin.com/hcp_archiver/atomicfile"
)

// Suffix is the filename suffix of every history sidecar; [Path] derives a
// sidecar name with it.
const Suffix = ".history.ndjson"

// seedWindow is the initial backward-read window of a sidecar scan, doubling
// until a committed record is found; it mirrors the chunk size of
// atomicfile's own record-boundary scan.
const seedWindow = 4096

// ErrContentNotUTF8 indicates content that cannot be carried losslessly as a
// JSON string: the encoder would replace invalid bytes with U+FFFD, desyncing
// the recorded content from the digest taken over the raw bytes.
var ErrContentNotUTF8 = errors.New("history content must be valid UTF-8")

// Record is one committed line of a history sidecar: a superseded version of
// the object carried verbatim, or a tombstone marking an observed
// disappearance.
type Record struct {
	// FetchedAt is when the recorded content was fetched, or, on a
	// tombstone, when the disappearance was observed. It is omitted when
	// unknown (a superseded file whose fetch time no ledger entry or
	// modification time could supply).
	FetchedAt time.Time `json:"fetchedAt,omitzero"`
	// SHA256 is the hex-encoded SHA-256 of Content's exact bytes, matching
	// the ledger signature the content was recorded done under.
	SHA256 string `json:"sha256,omitempty"`
	// Content is the superseded content byte for byte, carried as an escaped
	// JSON string so the original file round-trips exactly.
	Content string `json:"content,omitempty"`
	// Deleted marks a tombstone: the object answered a confirmed 404. A
	// tombstone never carries Content.
	Deleted bool `json:"deleted,omitempty"`
}

// Path returns the sidecar path for the object at objectPath, trimming a
// trailing ".json" before appending [Suffix], so "variables.json" maps to
// "variables.history.ndjson" and a non-JSON name simply gains the suffix.
func Path(objectPath string) string {
	return strings.TrimSuffix(objectPath, ".json") + Suffix
}

// config holds the resolved settings for a single sidecar call.
type config struct {
	logger *slog.Logger
}

// Option configures a sidecar read or append.
//
// The available options are:
//   - [WithLogger]
type Option func(*config)

// resolveConfig seeds the default logger and applies opts, the shared option
// handling of every function that scans a sidecar.
func resolveConfig(opts ...Option) config {
	cfg := config{logger: slog.Default()}

	for _, opt := range opts {
		opt(&cfg)
	}

	return cfg
}

// WithLogger sets the structured logger a scan reports skipped records to (a
// committed line that does not parse), overriding [slog.Default]. A nil
// logger keeps the default. It returns an [Option].
func WithLogger(logger *slog.Logger) Option {
	return func(c *config) {
		if logger != nil {
			c.logger = logger
		}
	}
}

// Supersede appends the outgoing content of a changed object, creating the
// sidecar on its first append. It dedupes against the sidecar's own committed
// tail: when the newest content-bearing record (skipping trailing tombstones)
// already carries this content's sha256, nothing is appended, so a write
// retried after a crash between the append and the object's rename does not
// duplicate the record, and a reappearance whose old bytes [Bury] already
// flushed is not flushed again. It reports whether a record was appended.
//
// Empty content appends nothing: it carries no version worth keeping, and its
// record would marshal with neither a content nor a deleted field, a line
// readers could classify as neither a version nor a tombstone.
func Supersede(path string, content []byte, fetchedAt time.Time, opts ...Option) (bool, error) {
	if !utf8.Valid(content) {
		return false, ErrContentNotUTF8
	}

	if len(content) == 0 {
		return false, nil
	}

	cfg := resolveConfig(opts...)
	sha := sum(content)

	newest, ok, err := newestMatching(path, func(r *Record) bool { return !r.Deleted }, cfg.logger)
	if err != nil {
		return false, err
	}

	if ok && newest.SHA256 == sha {
		return false, nil
	}

	err = appendRecord(path, &Record{
		FetchedAt: fetchedAt,
		SHA256:    sha,
		Content:   string(content),
	})
	if err != nil {
		return false, err
	}

	return true, nil
}

// Restore appends content only when the sidecar's newest record is a
// tombstone: the object came back, and the returning content closes the
// deletion so the timeline stays ordered and a later disappearance records a
// fresh tombstone. It never creates a sidecar and is a no-op on any other
// state, reporting whether a record was appended. Empty content appends
// nothing and leaves the tombstone standing, for the reason [Supersede]
// documents; the deletion closes once content worth keeping returns.
func Restore(path string, content []byte, fetchedAt time.Time, opts ...Option) (bool, error) {
	newest, ok, err := Newest(path, opts...)
	if err != nil || !ok {
		return false, err
	}

	if !newest.Deleted {
		return false, nil
	}

	if !utf8.Valid(content) {
		return false, ErrContentNotUTF8
	}

	if len(content) == 0 {
		return false, nil
	}

	err = appendRecord(path, &Record{
		FetchedAt: fetchedAt,
		SHA256:    sum(content),
		Content:   string(content),
	})
	if err != nil {
		return false, err
	}

	return true, nil
}

// Bury records an observed disappearance: it flushes the object's last-known
// content (unless the sidecar's newest record already carries it), then
// appends a tombstone stamped deletedAt, so the newest version is preserved
// before the file stops refreshing. A nil content means no file remains to
// flush; the tombstone still lands when a sidecar exists.
//
// It is a no-op when a trailing tombstone already covers the content on disk
// (the disappearance was recorded on an earlier run and nothing has been
// archived since) and when there is nothing to bury at all (no file and no
// sidecar), so a never-archived object grows no sidecar however often it
// re-probes 404. A flush that does land under a trailing tombstone earns a
// second one: the object came back and went away again between the two
// observations, and the returning version must not be left looking like the
// deletion's last word. A crash between the flush and the tombstone is healed
// on retry: the flush dedupes against the committed tail and only the
// tombstone is appended. It reports whether anything was appended; the report
// is meaningful only when err is nil, since a tombstone append can fail after
// the flush landed.
func Bury(path string, content []byte, fetchedAt, deletedAt time.Time, opts ...Option) (bool, error) {
	newest, ok, err := Newest(path, opts...)
	if err != nil {
		return false, err
	}

	if content == nil && !ok {
		return false, nil
	}

	flushed := false

	if content != nil {
		flushed, err = Supersede(path, content, fetchedAt, opts...)
		if err != nil {
			return false, err
		}
	}

	if ok && newest.Deleted && !flushed {
		return false, nil
	}

	err = appendRecord(path, &Record{
		FetchedAt: deletedAt,
		Deleted:   true,
	})
	if err != nil {
		return false, err
	}

	return true, nil
}

// Newest returns the sidecar's newest committed record and whether one
// exists. A missing or empty sidecar reports false; a torn fragment past the
// final newline (a crash mid-append) is ignored, matching what the next
// append repairs, and a committed line that does not parse is skipped rather
// than failing the read, the scan warning through its logger (see
// [WithLogger]) as it walks past.
func Newest(path string, opts ...Option) (Record, bool, error) {
	cfg := resolveConfig(opts...)

	return newestMatching(path, func(*Record) bool { return true }, cfg.logger)
}

// newestMatching scans the sidecar backward from EOF in a doubling window and
// returns the newest committed record match admits. Only whole lines behind
// the final newline are parsed, so a torn tail never corrupts a read.
//
// Only the window the scan stops on reports its skips, and that one report
// covers the whole scan. Every window is an EOF-anchored suffix, so among the
// windows holding a newline the absolute end of the scanned region is fixed
// and its absolute start only moves earlier: each larger window is a strict
// superset of the one before it. A window that returned no match walked every
// line it held, so the terminating window's tally already counts every line
// any earlier window skipped, and one damaged record is announced once per
// scan rather than once per doubling.
func newestMatching(path string, match func(*Record) bool, logger *slog.Logger) (Record, bool, error) {
	//nolint:gosec // The sidecar path is composed by the caller from its archive root.
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Record{}, false, nil
	}

	if err != nil {
		return Record{}, false, fmt.Errorf("open history %q: %w", path, err)
	}

	defer f.Close() //nolint:errcheck // Read-only descriptor.

	info, err := f.Stat()
	if err != nil {
		return Record{}, false, fmt.Errorf("stat history %q: %w", path, err)
	}

	size := info.Size()
	if size == 0 {
		return Record{}, false, nil
	}

	for window := int64(seedWindow); ; window *= 2 {
		window = min(window, size)
		off := size - window
		buf := make([]byte, window)

		_, err = f.ReadAt(buf, off)
		if err != nil && !errors.Is(err, io.EOF) {
			return Record{}, false, fmt.Errorf("read history %q: %w", path, err)
		}

		rec, ok, walked := scanWindow(buf, off, match)

		if ok || off == 0 {
			walked.report(logger, path)

			return rec, ok, nil
		}
	}
}

// skips tallies the committed lines one backward-read window walked past
// because they did not parse: how many there were, and the absolute file
// offset of the newest along with why that one failed. Offset and error name
// the same line, the newest lost, since that is the one whose absence moved
// the tail the scan reports.
type skips struct {
	err    error
	offset int64
	n      int
}

// report warns about the skipped lines as one aggregated record, so a wholly
// rotted sidecar costs a line of log rather than one per damaged record. It
// is a no-op when nothing was skipped.
func (s skips) report(logger *slog.Logger, path string) {
	if s.n == 0 {
		return
	}

	logger.Warn("history_records_skipped",
		slog.String("path", path),
		slog.Int("records", s.n),
		slog.Int64("offset", s.offset),
		slog.String("error", s.err.Error()),
	)
}

// scanWindow parses the committed whole lines inside one backward-read window
// newest-first and returns the first record match admits. The window covers
// buf starting at file offset off; a window at offset zero begins on a line
// whose start needs no preceding newline, while in any other the bytes before
// the window's first newline are the tail of a line the next, larger window
// will see complete.
//
// A line that does not parse is skipped and the scan keeps walking backward,
// so damage to one record costs at most the dedupe it would have answered
// (see the package doc). The skips are returned rather than reported here,
// because only the caller knows whether this window is the one the scan
// stopped on. Their offsets are absolute rather than window-relative, so a
// report names a place in the file an operator can seek to, not a place in a
// window that no longer exists.
func scanWindow(buf []byte, off int64, match func(*Record) bool) (Record, bool, skips) {
	end := bytes.LastIndexByte(buf, '\n')
	if end < 0 {
		return Record{}, false, skips{}
	}

	start := 0
	if off > 0 {
		start = bytes.IndexByte(buf, '\n') + 1
	}

	if start >= end {
		return Record{}, false, skips{}
	}

	lines := bytes.Split(buf[start:end], []byte("\n"))

	// The region's last line ends where its terminating newline sits, and
	// each line before it ends one byte behind the newline that terminates
	// it, so walking the ends backward yields every line's absolute start.
	lineEnd := off + int64(end)

	var walked skips

	for _, line := range slices.Backward(lines) {
		lineStart := lineEnd - int64(len(line))
		lineEnd = lineStart - 1

		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		var rec Record

		err := json.Unmarshal(line, &rec)
		if err != nil {
			if walked.n == 0 {
				walked.err = err
				walked.offset = lineStart
			}

			walked.n++

			continue
		}

		if match(&rec) {
			return rec, true, walked
		}
	}

	return Record{}, false, walked
}

// appendRecord encodes rec as one newline-terminated JSON line and appends it
// durably. The append lands on a record boundary even after a torn prior
// append (see [atomicfile.Append]), and the sidecar is created owner-only,
// matching the archive's at-rest posture.
func appendRecord(path string, rec *Record) error {
	var buf bytes.Buffer

	// Encode with HTML escaping off so &, <, and > in the carried content
	// survive byte for byte and a raw search over the sidecar still finds
	// them, matching the seal roll-ups' convention. Encode appends the
	// newline that frames the record.
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	err := enc.Encode(rec)
	if err != nil {
		return fmt.Errorf("encode history record for %q: %w", path, err)
	}

	_, err = atomicfile.Append(path, buf.Bytes())
	if err != nil {
		return fmt.Errorf("append history %q: %w", path, err)
	}

	return nil
}

// sum returns the hex-encoded SHA-256 of data.
func sum(data []byte) string {
	h := sha256.Sum256(data)

	return hex.EncodeToString(h[:])
}
