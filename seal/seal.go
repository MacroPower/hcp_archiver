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

	"go.jacobcolvin.com/hcp_archiver/atomicfile"
)

// sidecarSuffix is appended to a bundle path to name its sidecar index.
const sidecarSuffix = ".sidecar.ndjson"

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
	Name   string `json:"name"`
	Bundle string `json:"bundle"`
	Method string `json:"method"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	CRC32  uint32 `json:"crc32"`
}

// Seal packs members into a zip bundle at bundlePath, writes a sidecar index
// beside it, verifies every member reads back intact, and only then removes the
// loose sources. It returns the sidecar entries.
//
// The bundle and sidecar are each committed atomically, and the sources are
// removed last, so a failure at any step leaves the loose files in place as the
// canonical copy and the next run re-seals. An empty member set writes nothing
// and returns no entries.
func Seal(bundlePath string, members []Member) ([]Entry, error) {
	if len(members) == 0 {
		return nil, nil
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

	err = writeSidecar(bundlePath+sidecarSuffix, entries)
	if err != nil {
		return nil, err
	}

	err = verifyBundle(bundlePath, entries)
	if err != nil {
		return nil, err
	}

	for i := range members {
		err = os.Remove(members[i].Source)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("remove sealed source %q: %w", members[i].Source, err)
		}
	}

	return entries, nil
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
		return fmt.Errorf("sealed member %q failed verification: got %s, want %s",
			entry.Name, got, entry.SHA256)
	}

	return nil
}
