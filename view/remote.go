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

// maxMemberSize caps a single decompressed bundle member the viewer loads whole
// into memory. A member's compressed span is already bounded by the bundle
// object size (see [extractMember]), but DEFLATE expands up to roughly 1032:1,
// so a crafted or corrupt span within that guard can still inflate without
// bound and exhaust memory. The cap bounds that expansion while sitting far
// above any real archived state, log, or metadata object; it applies to both
// the remote ([decompressMember]) and local ([readLocalBundleMember]) inflate
// paths, which read the untrusted member with [io.ReadAll].
const maxMemberSize int64 = 1 << 30 // 1 GiB

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
// so the cache is mutex-guarded. A bundle build (a Head and a central-directory
// parse over ranged GETs) runs outside the mutex behind a per-key readiness
// signal, so a read of one cached bundle never waits on another's in-flight
// build and one key is built at most once.
type orgRemote struct {
	ctx        context.Context //nolint:containedctx // Reads run inside io.ReaderAt calls, which take none.
	newClient  remoteClientFactory
	client     *remote.Client
	clientErr  error
	bundles    map[string]*remoteBundle
	orgName    string
	cfg        remote.Config
	mu         sync.Mutex
	clientOnce sync.Once
}

// remoteBundle is one evicted bundle's cached read state: its parsed central
// directory and the ranged reader its member spans are fetched through.
//
// The ready channel is closed once the build settles; a caller finding an entry
// in the map waits on it, then reads err (a failed build) or zr and ra (a proven
// one). A failed build's entry is removed from the map after ready closes, so a
// waiter that captured it still sees err while the next caller rebuilds.
type remoteBundle struct {
	zr    *zip.Reader
	ra    io.ReaderAt
	ready chan struct{}
	err   error
	// Byte length of the bundle object, the bound a member's recorded compressed
	// span is checked against before it sizes an allocation.
	size int64
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
// first use. A cached or in-flight entry is served from the map; an absent one
// is claimed with a placeholder and built outside the mutex, so a read of a
// different cached bundle never waits on this build and every key is built at
// most once. On a build error the placeholder is dropped so the next caller
// retries: failures are not cached.
func (r *orgRemote) bundle(relBundle string) (*remoteBundle, error) {
	r.mu.Lock()

	if b, ok := r.bundles[relBundle]; ok {
		r.mu.Unlock()

		// An in-flight build's ready is still open; a completed one's is closed,
		// so this returns at once. Either way the fields are safe to read after.
		<-b.ready

		if b.err != nil {
			return nil, b.err
		}

		return b, nil
	}

	b := &remoteBundle{ready: make(chan struct{})}
	r.bundles[relBundle] = b
	r.mu.Unlock()

	zr, ra, size, err := r.buildBundle(relBundle)
	if err != nil {
		// Wake any same-key waiter that captured this entry, then drop it so the
		// next caller rebuilds. The order matters: closing ready before the delete
		// is what lets a waiter read err rather than block forever.
		b.err = err
		close(b.ready)

		r.mu.Lock()
		delete(r.bundles, relBundle)
		r.mu.Unlock()

		return nil, err
	}

	b.zr = zr
	b.ra = ra
	b.size = size
	close(b.ready)

	return b, nil
}

// buildBundle probes and parses one evicted bundle: one Head for its size,
// then a central-directory parse over a handful of ranged reads. It runs
// outside [orgRemote.mu] so distinct bundles build in parallel.
//
//nolint:contextcheck // Only the stored browse context exists behind ReaderAt.
func (r *orgRemote) buildBundle(relBundle string) (*zip.Reader, io.ReaderAt, int64, error) {
	client, err := r.clientBuild()
	if err != nil {
		return nil, nil, 0, err
	}

	key := r.cfg.Key(r.orgName, relBundle)

	headCtx, cancel := context.WithTimeout(r.ctx, remoteReadTimeout)
	defer cancel()

	info, err := client.Head(headCtx, key)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("probe remote bundle: %w", err)
	}

	// The reader lives as long as the cached central directory, so it cannot
	// be bound to one call's timeout; timedReaderAt gives each of its ranged
	// GETs a fresh deadline instead.
	ra := &timedReaderAt{ctx: r.ctx, client: client, key: key, size: info.Size}

	zr, err := zip.NewReader(ra, info.Size)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("parse remote bundle %q: %w", key, err)
	}

	return zr, ra, info.Size, nil
}

// clientBuild returns the lazily-built remote client. The build runs once per
// session behind a [sync.Once], so it needs no external lock and never
// re-probes the credential chain per read: a failure (a bad marker, no
// credentials) is remembered and returned on every subsequent read. The
// client deliberately lives for the browse session with no close path — the
// viewer exits with the process, which releases the backend connection.
//
// The stored browse context is the intended parent for the build: callers
// sit behind [io.ReaderAt]-shaped interfaces that carry no context.
//
//nolint:contextcheck // See above; there is no caller context to pass.
func (r *orgRemote) clientBuild() (*remote.Client, error) {
	r.clientOnce.Do(func() {
		ctx, cancel := context.WithTimeout(r.ctx, remoteReadTimeout)
		defer cancel()

		client, err := r.newClient(ctx, r.cfg)
		if err != nil {
			r.clientErr = fmt.Errorf("build remote client: %w", err)
		} else {
			r.client = client
		}
	})

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

	// The central directory is untrusted: bit rot in a member's CompressedSize64
	// can record a span larger than the bundle, and make([]byte, that) would OOM
	// or panic on a length past maxInt. Reject a span that overruns the object
	// before allocating, mirroring the CRC check that rejects corrupt content. The
	// oversized-size guard precedes the sum so the addition below cannot overflow.
	//nolint:gosec // b.size and offset are non-negative object lengths.
	if offset < 0 || f.CompressedSize64 > uint64(b.size) || uint64(offset)+f.CompressedSize64 > uint64(b.size) {
		return nil, fmt.Errorf("member %q has an out-of-range span in remote bundle", relPath)
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
// the archive seals members only as STORE or DEFLATE. The inflate is bounded by
// [maxMemberSize].
func decompressMember(method uint16, compressed []byte) ([]byte, error) {
	return decompressMemberBounded(method, compressed, maxMemberSize)
}

// decompressMemberBounded is [decompressMember] with the decompressed-size cap
// injected, so a test can drive the oversize rejection without a gibibyte-scale
// fixture. A STORE member is returned as-is: its length is the compressed span,
// already bounded against the bundle object (see [extractMember]).
func decompressMemberBounded(method uint16, compressed []byte, limit int64) ([]byte, error) {
	switch method {
	case zip.Store:
		return compressed, nil

	case zip.Deflate:
		fr := flate.NewReader(bytes.NewReader(compressed))

		// Inflate under an absolute cap rather than trusting the deflate stream or
		// the central directory's uncompressed size: the compressed span is bounded
		// but its expansion is not, so read one byte past the cap to tell an
		// oversized member from a merely large one instead of silently truncating.
		data, err := io.ReadAll(io.LimitReader(fr, limit+1))
		if err != nil {
			return nil, fmt.Errorf("inflate: %w", err)
		}

		closeErr := fr.Close()
		if closeErr != nil {
			return nil, fmt.Errorf("inflate: %w", closeErr)
		}

		if int64(len(data)) > limit {
			return nil, fmt.Errorf("inflated member exceeds the %d-byte cap", limit)
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
