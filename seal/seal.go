package seal

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"go.jacobcolvin.com/hcp_archiver/atomicfile"
)

// SidecarSuffix is appended to a bundle path to name its sidecar index; it is
// exported so the sweeps that classify bundle files compose the sidecar path
// from one owner instead of hardcoding the convention.
const SidecarSuffix = ".sidecar.ndjson"

// Method names recorded in a sidecar entry, matching the zip compression method
// a member was packed with.
const (
	methodStore   = "store"
	methodDeflate = "deflate"
)

// Member is one loose file to pack into a bundle.
//
// Create a slice of these and hand them to [Seal].
type Member struct {
	// Name is the archive-relative path recorded for the member and reproduced as
	// its path inside the zip, so an extract rebuilds the loose tree byte for
	// byte.
	Name string
	// Source is the absolute path of the loose file to pack.
	Source string
	// Compress selects DEFLATE over STORE. Set it for compressible, low-stakes
	// artifacts (logs); leave it false to store irreplaceable state uncompressed
	// and greppable.
	Compress bool
}

// Entry is one member's record in a bundle's sidecar index.
//
// Instances are produced by [Seal] and written, one JSON object per line, to the
// bundle's sidecar file.
type Entry struct {
	// Name is the member's archive-relative path, its identity within the bundle.
	Name string `json:"name"`
	// Bundle is the base filename of the zip the member was packed into.
	Bundle string `json:"bundle"`
	// Method is the zip compression method the member was packed with, either
	// [methodStore] or [methodDeflate].
	Method string `json:"method"`
	// SHA256 is the lowercase hex SHA-256 of the member's content, verified
	// against a read-back before its loose source is removed.
	SHA256 string `json:"sha256"`
	// Size is the member's content length in bytes.
	Size int64 `json:"size"`
	// CRC32 is the IEEE CRC-32 of the member's content, the zip's own integrity
	// check.
	CRC32 uint32 `json:"crc32"`
}

var (
	// ErrMemberName rejects a [Member] whose name is not a clean archive-relative
	// path. Its name is reproduced verbatim as the entry path inside the bundle,
	// so a name with a ".." segment or a leading slash would let an extractor
	// write outside the destination directory (a Zip-Slip); [Seal] and [Rollup]
	// refuse it.
	ErrMemberName = errors.New("member name must be a clean archive-relative path")

	// ErrContentNotUTF8 marks a roll-up source whose bytes are not valid UTF-8. A
	// roll-up carries content as a JSON string, which cannot represent arbitrary
	// bytes: encoding coerces an invalid byte to U+FFFD, so the stored content
	// would no longer reproduce the source or match its recorded digest. Such a
	// source is refused rather than silently mangled.
	ErrContentNotUTF8 = errors.New("roll-up content must be valid UTF-8")

	// ErrDuplicateMember rejects two [Member]s sharing a name within one bundle. A
	// duplicate would write two zip entries under one path, and read-back
	// verification keys on the name, so it would compare one member's digest
	// against the other's bytes; [Seal] and [Rollup] refuse it before writing.
	ErrDuplicateMember = errors.New("member names must be unique within a bundle")
)

// checkMemberNames verifies every member names a clean path that stays within
// the archive tree and is unique within the bundle, so an extract cannot escape
// its destination directory and read-back verification maps each name to one
// member. Names are slash-separated bundle paths: an empty name, a leading slash
// (absolute), a backslash anywhere (a Windows separator a '/'-only split would
// step over), or any empty, ".", or ".." segment is rejected, which covers "//",
// a trailing slash, and every traversal form without depending on the host
// separator; a name repeated across members is rejected too.
func checkMemberNames(members []Member) error {
	seen := make(map[string]struct{}, len(members))

	for i := range members {
		name := members[i].Name
		if name == "" || strings.HasPrefix(name, "/") || strings.ContainsRune(name, '\\') {
			return fmt.Errorf("%w: %q", ErrMemberName, name)
		}

		for seg := range strings.SplitSeq(name, "/") {
			if seg == "" || seg == "." || seg == ".." {
				return fmt.Errorf("%w: %q", ErrMemberName, name)
			}
		}

		if _, dup := seen[name]; dup {
			return fmt.Errorf("%w: %q", ErrDuplicateMember, name)
		}

		seen[name] = struct{}{}
	}

	return nil
}

// Seal packs members into a zip bundle at bundlePath, verifies every member
// reads back intact, writes a sidecar index beside it, and only then removes the
// loose sources. It returns the sidecar entries.
//
// The bundle and sidecar are each committed atomically, and the sources are
// removed last, so a failure at any step leaves the loose files in place as the
// canonical copy and the next run re-seals. An empty member set writes nothing
// and returns no entries. A member whose name is not a clean archive-relative
// path is rejected with [ErrMemberName].
//
// Removing the loose sources is best-effort: the bundle and its sidecar are
// already durable and verified before any removal, so a source that cannot be
// removed (a read-only parent, a transient error) is a recognizable survivor,
// not a loss. The caller reconciles it on the next run -- a survivor whose bytes
// still hash to the sidecar entry is already sealed and dropped, one whose bytes
// diverge is sealed afresh -- so a removal failure is skipped rather than
// returned, which would strand the seal with the bundle already committed.
func Seal(bundlePath string, members []Member) ([]Entry, error) {
	if len(members) == 0 {
		return nil, nil
	}

	nameErr := checkMemberNames(members)
	if nameErr != nil {
		return nil, nameErr
	}

	entries := make([]Entry, 0, len(members))
	bundleName := filepath.Base(bundlePath)

	err := atomicfile.Write(bundlePath, func(w io.Writer) error {
		zw := zip.NewWriter(w)

		for i := range members {
			entry, memberErr := writeMember(zw, bundleName, &members[i])
			if memberErr != nil {
				return memberErr
			}

			entries = append(entries, entry)
		}

		closeErr := zw.Close()
		if closeErr != nil {
			return fmt.Errorf("finalize bundle: %w", closeErr)
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("write bundle %q: %w", bundlePath, err)
	}

	err = verifyBundle(bundlePath, entries)
	if err != nil {
		return nil, err
	}

	err = writeSidecar(bundlePath+SidecarSuffix, entries)
	if err != nil {
		return nil, err
	}

	for i := range members {
		//nolint:errcheck // Best-effort; a survivor is reconciled on the next run.
		_ = os.Remove(members[i].Source)
	}

	return entries, nil
}

// ReadSidecar reads a bundle's sidecar index, decoding the newline-delimited
// JSON entries [Seal] wrote. A sidecar that does not exist yields no entries and
// no error, so a caller can probe for one without a separate stat.
func ReadSidecar(path string) ([]Entry, error) {
	//nolint:gosec // The sidecar path is composed by the caller from its archive root.
	f, err := os.Open(path)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("open sidecar %q: %w", path, err)
	}

	defer func() {
		//nolint:errcheck // Read-only handle; a close failure cannot lose data.
		_ = f.Close()
	}()

	var entries []Entry

	dec := json.NewDecoder(f)

	for {
		var entry Entry

		decErr := dec.Decode(&entry)
		if errors.Is(decErr, io.EOF) {
			break
		}

		if decErr != nil {
			return nil, fmt.Errorf("decode sidecar %q: %w", path, decErr)
		}

		entries = append(entries, entry)
	}

	return entries, nil
}

// SourceDigest returns the lowercase hex SHA-256 of the file at path, streaming
// it through the hash the same way [writeMember] does. It lets a caller prove a
// loose source still matches a sidecar entry's recorded content before treating
// it as already sealed.
func SourceDigest(path string) (string, error) {
	//nolint:gosec // The source path is composed by the caller from its archive root.
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open source %q: %w", path, err)
	}

	sha := sha256.New()

	_, copyErr := io.Copy(sha, f)
	closeErr := f.Close()

	switch {
	case copyErr != nil:
		return "", fmt.Errorf("hash source %q: %w", path, copyErr)
	case closeErr != nil:
		return "", fmt.Errorf("close source %q: %w", path, closeErr)
	}

	return hex.EncodeToString(sha.Sum(nil)), nil
}

// writeMember packs one source file into the zip, hashing it as it copies, and
// returns its sidecar entry.
func writeMember(zw *zip.Writer, bundleName string, m *Member) (Entry, error) {
	method := zip.Store
	methodName := methodStore

	if m.Compress {
		method = zip.Deflate
		methodName = methodDeflate
	}

	dst, err := zw.CreateHeader(&zip.FileHeader{Name: m.Name, Method: method})
	if err != nil {
		return Entry{}, fmt.Errorf("create member %q: %w", m.Name, err)
	}

	//nolint:gosec // The source path is composed by the caller from its archive root.
	src, err := os.Open(m.Source)
	if err != nil {
		return Entry{}, fmt.Errorf("open source %q: %w", m.Source, err)
	}

	sha := sha256.New()
	crc := crc32.NewIEEE()

	n, copyErr := io.Copy(io.MultiWriter(dst, sha, crc), src)
	closeErr := src.Close()

	switch {
	case copyErr != nil:
		return Entry{}, fmt.Errorf("pack member %q: %w", m.Name, copyErr)
	case closeErr != nil:
		return Entry{}, fmt.Errorf("close source %q: %w", m.Source, closeErr)
	}

	return Entry{
		Name:   m.Name,
		Bundle: bundleName,
		Method: methodName,
		Size:   n,
		CRC32:  crc.Sum32(),
		SHA256: hex.EncodeToString(sha.Sum(nil)),
	}, nil
}

// writeSidecar commits the sidecar index as newline-delimited JSON beside the
// bundle.
func writeSidecar(path string, entries []Entry) error {
	var buf bytes.Buffer

	enc := json.NewEncoder(&buf)

	for i := range entries {
		err := enc.Encode(&entries[i])
		if err != nil {
			return fmt.Errorf("encode sidecar entry: %w", err)
		}
	}

	err := atomicfile.WriteFile(path, buf.Bytes())
	if err != nil {
		return fmt.Errorf("write sidecar %q: %w", path, err)
	}

	return nil
}

// verifyBundle re-opens the written bundle and confirms every member reads back
// to its recorded SHA-256, so the loose sources are only removed once the bundle
// is proven intact.
func verifyBundle(bundlePath string, entries []Entry) error {
	zr, err := zip.OpenReader(bundlePath)
	if err != nil {
		return fmt.Errorf("open bundle %q to verify: %w", bundlePath, err)
	}

	defer func() {
		//nolint:errcheck // Read-only handle; a close failure cannot lose data.
		_ = zr.Close()
	}()

	members := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		members[f.Name] = f
	}

	for i := range entries {
		err = verifyMember(members[entries[i].Name], &entries[i])
		if err != nil {
			return err
		}
	}

	return nil
}

// verifyMember confirms one packed member hashes to its recorded digest.
func verifyMember(f *zip.File, entry *Entry) error {
	if f == nil {
		return fmt.Errorf("sealed member %q is missing from the bundle", entry.Name)
	}

	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("open member %q to verify: %w", entry.Name, err)
	}

	sha := sha256.New()

	//nolint:gosec // The bundle was just written by this process; its members are trusted.
	_, copyErr := io.Copy(sha, rc)
	closeErr := rc.Close()

	switch {
	case copyErr != nil:
		return fmt.Errorf("read member %q to verify: %w", entry.Name, copyErr)
	case closeErr != nil:
		return fmt.Errorf("close member %q after verify: %w", entry.Name, closeErr)
	}

	got := hex.EncodeToString(sha.Sum(nil))
	if got != entry.SHA256 {
		return fmt.Errorf("sealed member %q does not match its recorded digest: got %s, want %s",
			entry.Name, got, entry.SHA256)
	}

	return nil
}

// rollupLine is one member's record in a roll-up: its archive-relative path, the
// SHA-256 of its content, and the content itself carried byte for byte as a JSON
// string.
type rollupLine struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Content string `json:"content"`
}

// Rollup appends each member's content, keyed by its archive-relative name, as
// one JSON line to the roll-up at rollupPath, then removes the loose sources.
//
// Unlike a bundle, a roll-up stays plain and greppable: the content is carried
// byte for byte as an escaped JSON string, so an extract reproduces the original
// file and a field token is still found by a raw search. The batch is appended
// and flushed, the appended tail is read back and byte-compared, and only then
// are the sources removed, so an interrupted roll-up loses nothing and re-runs.
//
// A roll-up only grows: no line is ever rewritten. A crash between the flushed
// append and a source's removal re-folds that member next run, appending an
// identical line, and a member whose content legitimately changed between
// seals (a terminal run.json re-frozen after an update) appends a newer,
// different line under the same path; readers keep the newest line per path
// either way. An empty member set appends nothing. A source whose bytes are
// not valid UTF-8 cannot be carried losslessly as a JSON string and is refused
// with [ErrContentNotUTF8] rather than mangled.
func Rollup(rollupPath string, members []Member) error {
	if len(members) == 0 {
		return nil
	}

	nameErr := checkMemberNames(members)
	if nameErr != nil {
		return nameErr
	}

	payload, err := encodeRollup(members)
	if err != nil {
		return err
	}

	start, err := appendPayload(rollupPath, payload)
	if err != nil {
		return err
	}

	err = verifyAppended(rollupPath, start, payload)
	if err != nil {
		return err
	}

	for i := range members {
		err = os.Remove(members[i].Source)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove rolled-up source %q: %w", members[i].Source, err)
		}
	}

	return nil
}

// encodeRollup reads each member's content and renders the batch as
// newline-terminated JSON lines.
func encodeRollup(members []Member) ([]byte, error) {
	var buf bytes.Buffer

	// A roll-up must stay greppable, so encode with HTML escaping off: the default
	// encoder rewrites &, <, and > into \u escapes a raw search over the roll-up
	// would then miss for any content carrying them (a URL query string, an
	// angle-bracketed tag). U+2028 and U+2029 stay \u-escaped even with HTML
	// escaping off, since encoding/json always escapes those two, so a raw search
	// for a token carrying them still misses it; they decode losslessly, so an
	// extract and the recorded digest are unaffected. Encode also appends the
	// trailing newline that frames each record.
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	for i := range members {
		//nolint:gosec // The source path is composed by the caller from its archive root.
		data, err := os.ReadFile(members[i].Source)
		if err != nil {
			return nil, fmt.Errorf("read source %q: %w", members[i].Source, err)
		}

		// A JSON string cannot carry invalid UTF-8: the encoder replaces it with
		// U+FFFD, desyncing the stored content from the digest taken over the raw
		// bytes. Refuse it so the recorded content stays byte-for-byte recoverable.
		if !utf8.Valid(data) {
			return nil, fmt.Errorf("%w: %q", ErrContentNotUTF8, members[i].Name)
		}

		sum := sha256.Sum256(data)

		err = enc.Encode(&rollupLine{
			Path:    members[i].Name,
			SHA256:  hex.EncodeToString(sum[:]),
			Content: string(data),
		})
		if err != nil {
			return nil, fmt.Errorf("encode roll-up line for %q: %w", members[i].Name, err)
		}
	}

	return buf.Bytes(), nil
}

// appendPayload appends payload to the roll-up, flushes it to stable storage, and
// returns the offset the append began at so the write can be verified.
//
// It relies on [atomicfile.Append] to land the batch on a record boundary even
// when a prior append was torn by a crash, and to sync the parent directory so a
// roll-up created by a first append survives one.
func appendPayload(path string, payload []byte) (int64, error) {
	start, err := atomicfile.Append(path, payload)
	if err != nil {
		return 0, fmt.Errorf("append roll-up %q: %w", path, err)
	}

	return start, nil
}

// verifyAppended confirms the bytes written at start read back exactly, so the
// sources are only removed once the appended lines are durable and intact.
func verifyAppended(path string, start int64, payload []byte) error {
	//nolint:gosec // The roll-up path is composed by the caller from its archive root.
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open roll-up %q to verify: %w", path, err)
	}

	got := make([]byte, len(payload))

	_, readErr := f.ReadAt(got, start)
	closeErr := f.Close()

	switch {
	case readErr != nil && !errors.Is(readErr, io.EOF):
		return fmt.Errorf("read roll-up %q tail to verify: %w", path, readErr)
	case closeErr != nil:
		return fmt.Errorf("close roll-up %q after verify: %w", path, closeErr)
	}

	if !bytes.Equal(got, payload) {
		return fmt.Errorf("roll-up %q does not read back the appended bytes", path)
	}

	return nil
}
