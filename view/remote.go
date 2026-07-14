package view

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.jacobcolvin.com/hcp_archiver/remote"
)

// remoteReadTimeout bounds each individual ranged GET against the remote
// store, so a stalled request surfaces as a status-line error instead of a
// hung screen. Every read — a central-directory probe or a whole member's
// compressed span — is one request under its own timeout.
const remoteReadTimeout = 5 * time.Minute

// remoteClientFactory builds the client an organization's remote bundle
// reads go through. [OpenArchive] defaults it to [remote.New]; tests inject
// a fake-backed builder through [WithRemoteFactory].
type remoteClientFactory func(ctx context.Context, cfg remote.Config) (*remote.Client, error)

// orgRemote serves one organization's evicted bundles from its remote store.
//
// It is built only when the org root carries a [remote.MarkerName] marker, so
// a local-only archive never touches a client or a credential chain. The
// client and each bundle's parsed central directory are built lazily on first
// use and cached for the session; reads run on Bubble Tea command goroutines,
// so the cache is mutex-guarded.
type orgRemote struct {
	ctx        context.Context //nolint:containedctx // Reads run inside io.ReaderAt calls, which take none.
	newClient  remoteClientFactory
	client     *remote.Client
	clientErr  error
	bundles    map[string]*remoteBundle
	orgName    string
	cfg        remote.Config
	mu         sync.Mutex
	clientOnce bool
}

// remoteBundle is one evicted bundle's cached read state: its parsed central
// directory and the ranged reader its member spans are fetched through.
type remoteBundle struct {
	zr *zip.Reader
	ra io.ReaderAt
}

// loadOrgRemote reads the organization root's remote marker into an
// [*orgRemote], or nil when no marker exists (a local-only archive).
func loadOrgRemote(root, orgName string, opts archiveOptions) (*orgRemote, error) {
	//nolint:gosec // The path is composed from the archive root being browsed.
	data, err := os.ReadFile(filepath.Join(root, remote.MarkerName))

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil //nolint:nilnil // No marker simply means a local-only archive.
	case err != nil:
		return nil, fmt.Errorf("read remote marker: %w", err)
	}

	var marker remote.Marker

	err = json.Unmarshal(data, &marker)
	if err != nil {
		return nil, fmt.Errorf("parse remote marker %q: %w", remote.MarkerName, err)
	}

	if marker.Version > remote.MarkerVersion {
		return nil, fmt.Errorf("remote marker %q is version %d, newer than this build reads (%d)",
			remote.MarkerName, marker.Version, remote.MarkerVersion)
	}

	return &orgRemote{
		ctx:       opts.ctx,
		newClient: opts.newClient,
		orgName:   orgName,
		cfg:       marker.Config(),
		bundles:   make(map[string]*remoteBundle),
	}, nil
}

// readMember extracts one member from an evicted bundle at its
// archive-relative path.
func (r *orgRemote) readMember(relBundle, relPath string) ([]byte, error) {
	b, err := r.bundle(relBundle)
	if err != nil {
		return nil, err
	}

	for _, f := range b.zr.File {
		if f.Name == relPath {
			return extractMember(b, f, relPath)
		}
	}

	return nil, fmt.Errorf("%w: %s in remote bundle %q", ErrObjectNotFound, relPath, relBundle)
}

// bundle returns the cached read state for an evicted bundle, building it on
// first use: one Head for size and storage class, then a central-directory
// parse over a handful of ranged GETs. An object parked unrestored in an
// archival storage class surfaces [remote.ErrRestoreRequired] rather than a
// read that can never succeed.
//
//nolint:contextcheck // Only the stored browse context exists behind ReaderAt.
func (r *orgRemote) bundle(relBundle string) (*remoteBundle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if b, ok := r.bundles[relBundle]; ok {
		return b, nil
	}

	client, err := r.clientLocked()
	if err != nil {
		return nil, err
	}

	key := r.cfg.Key(r.orgName, relBundle)

	headCtx, cancel := context.WithTimeout(r.ctx, remoteReadTimeout)
	defer cancel()

	info, err := client.Head(headCtx, key)
	if err != nil {
		return nil, fmt.Errorf("probe remote bundle: %w", err)
	}

	if info.Archived && !info.Restored {
		return nil, fmt.Errorf("%w: %s (storage class %s); request a restore, wait for it to complete, and retry",
			remote.ErrRestoreRequired, key, info.StorageClass)
	}

	// The reader lives as long as the cached central directory, so it cannot
	// be bound to one call's timeout; timedReaderAt gives each of its ranged
	// GETs a fresh deadline instead.
	ra := &timedReaderAt{ctx: r.ctx, client: client, key: key, size: info.Size}

	zr, err := zip.NewReader(ra, info.Size)
	if err != nil {
		return nil, fmt.Errorf("parse remote bundle %q: %w", key, err)
	}

	b := &remoteBundle{zr: zr, ra: ra}
	r.bundles[relBundle] = b

	return b, nil
}

// clientLocked returns the lazily-built remote client. The build is
// attempted once per session: a failure (a bad marker, no credentials) is
// remembered and returned on every subsequent read rather than re-probing
// the credential chain per keypress.
//
// The stored browse context is the intended parent for the build: callers
// sit behind [io.ReaderAt]-shaped interfaces that carry no context.
//
//nolint:contextcheck // See above; there is no caller context to pass.
func (r *orgRemote) clientLocked() (*remote.Client, error) {
	if !r.clientOnce {
		r.clientOnce = true

		ctx, cancel := context.WithTimeout(r.ctx, remoteReadTimeout)
		defer cancel()

		client, err := r.newClient(ctx, r.cfg)
		if err != nil {
			r.clientErr = fmt.Errorf("build remote client: %w", err)
		} else {
			r.client = client
		}
	}

	if r.clientErr != nil {
		return nil, r.clientErr
	}

	return r.client, nil
}

// extractMember reads one member's compressed span in a single ranged GET
// and decompresses it locally.
//
// Streaming through the bundle's [io.ReaderAt] instead (f.Open) would issue
// one small ranged GET per flate read — thousands for a large log — so the
// span is fetched whole. The member's CRC-32 is verified after
// decompression, matching the integrity check zip's own reader performs.
func extractMember(b *remoteBundle, f *zip.File, relPath string) ([]byte, error) {
	offset, err := f.DataOffset()
	if err != nil {
		return nil, fmt.Errorf("locate member %q: %w", relPath, err)
	}

	compressed := make([]byte, f.CompressedSize64)

	if len(compressed) > 0 {
		_, err = b.ra.ReadAt(compressed, offset)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("read member %q: %w", relPath, err)
		}
	}

	data, err := decompressMember(f.Method, compressed)
	if err != nil {
		return nil, fmt.Errorf("decompress member %q: %w", relPath, err)
	}

	if crc32.ChecksumIEEE(data) != f.CRC32 {
		return nil, fmt.Errorf("member %q does not match its recorded checksum", relPath)
	}

	return data, nil
}

// decompressMember expands one member's compressed span by its zip method:
// the archive seals members only as STORE or DEFLATE.
func decompressMember(method uint16, compressed []byte) ([]byte, error) {
	switch method {
	case zip.Store:
		return compressed, nil

	case zip.Deflate:
		fr := flate.NewReader(bytes.NewReader(compressed))

		data, err := io.ReadAll(fr)
		if err != nil {
			return nil, fmt.Errorf("inflate: %w", err)
		}

		closeErr := fr.Close()
		if closeErr != nil {
			return nil, fmt.Errorf("inflate: %w", closeErr)
		}

		return data, nil

	default:
		return nil, fmt.Errorf("unsupported compression method %d", method)
	}
}

// timedReaderAt adapts [remote.Client.ReadAt] to [io.ReaderAt], giving each
// read its own timeout, so a reader cached for the whole session (inside a
// parsed [*zip.Reader]) still bounds every individual request.
type timedReaderAt struct {
	ctx    context.Context //nolint:containedctx // ReadAt has no context parameter.
	client *remote.Client
	key    string
	size   int64
}

// ReadAt performs one ranged GET under its own deadline.
func (t *timedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	ctx, cancel := context.WithTimeout(t.ctx, remoteReadTimeout)
	defer cancel()

	//nolint:wrapcheck // A transparent pass-through; the client already wraps.
	return t.client.ReadAt(ctx, t.key, t.size, p, off)
}
