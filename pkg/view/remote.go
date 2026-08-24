package view

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"go.jacobcolvin.com/hcp_archiver/pkg/atomicfile"
	"go.jacobcolvin.com/hcp_archiver/pkg/pathkit"
	"go.jacobcolvin.com/hcp_archiver/pkg/remote"
	"go.jacobcolvin.com/hcp_archiver/pkg/seal"
	"go.jacobcolvin.com/hcp_archiver/pkg/store"
)

// remoteReadTimeout bounds each individual ranged GET against the remote
// store, so a stalled request surfaces as a status-line error instead of a
// hung screen. Every read (a central-directory probe or a whole member's
// compressed span) is one request under its own timeout.
const remoteReadTimeout = 5 * time.Minute

// defaultListNoticeGrace is how long an organization's inventory listing may
// run before [WithListNotice]'s callback names it. A mirror of ordinary size
// answers well inside it, so a routine read never explains itself; one that
// outlives it is enumerating enough keys that the wait needs a reason.
const defaultListNoticeGrace = 2 * time.Second

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

// orgRemote serves one organization's mirrored objects from its remote store:
// ranged reads out of evicted bundles, whole-object fetches that persist at
// the local archive path ([orgRemote.ensureLocal]), and the session's cached
// mirror inventory ([orgRemote.objects]).
//
// It is built when the org root carries a [remote.MarkerName] marker or when
// [OpenArchive] was handed a remote through [WithRemote], so a local-only
// archive never touches a client or a credential chain. The client, the
// inventory, and each bundle's parsed central directory are built lazily on
// first use and cached for the session; reads run on Bubble Tea command
// goroutines, so the caches are mutex-guarded. A bundle build (a Head and a
// central-directory parse over ranged GETs) runs outside the mutex behind a
// per-key readiness signal, so a read of one cached bundle never waits on
// another's in-flight build and one key is built at most once.
type orgRemote struct {
	ctx       context.Context //nolint:containedctx // Reads run inside io.ReaderAt calls, which take none.
	newClient remoteClientFactory
	client    *remote.Client
	clientErr error
	bundles   map[string]*remoteBundle
	fetches   map[string]*inflightFetch

	// The org's mirror inventory keyed by archive-relative path, listed once
	// per session under listMu (its own lock, so a long listing never blocks a
	// bundle read's map access). It is built on first use rather than at the
	// open, and only by a merged remote, so an organization nothing reads
	// through never enumerates its prefix. Both the listing and a listing
	// failure are cached: the merged listing helpers consult the inventory per
	// keystroke, and re-listing an unreachable mirror on each would hammer the
	// bucket while the browser degrades to local content anyway. The byDir
	// field is the listing's per-directory child index (see [indexListing]),
	// built and replaced together with it.
	listing map[string]remote.ObjectInfo
	byDir   map[string]map[string]remoteChild
	listErr error

	// The in-flight listing's readiness signal, closed when the build settles
	// (see [orgRemote.objects]); nil when no listing is running. It keeps the
	// enumeration itself outside listMu, so the cheap readers never block for
	// the listing's duration.
	listBuild chan struct{}

	// Names this organization when its inventory listing outlives
	// noticeGrace, or nil to report nothing (see [WithListNotice]). It runs on
	// a timer goroutine, so an implementation sharing a writer with the caller
	// guards it.
	notify func(org string)

	orgName string
	cfg     remote.Config

	noticeGrace time.Duration

	listed bool

	// Whether the remote stands in for the whole tree rather than only the
	// evicted surfaces: listings union in the mirror's inventory and an
	// ordinary miss fetches through. The marker decides it, whether the remote
	// arrived through [WithRemote] or through the marker alone: a partial tree
	// (one a bootstrap materialized, see [writeMarker]) merges, and an archive
	// the archiver wrote is complete locally, so its marker keeps every local
	// read local.
	merged bool

	listMu sync.Mutex

	mu         sync.Mutex
	clientOnce sync.Once
}

// inflightFetch is one in-progress [orgRemote.ensureLocal] download. The ready
// channel closes once the fetch settles; a same-path caller waits on it and
// shares err instead of downloading the object a second time in parallel.
// Entries are removed once settled (a success is served from disk afterward,
// and a failure retries), so only concurrent callers ever share one.
type inflightFetch struct {
	ready chan struct{}
	err   error
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
	marker, ok, err := remote.ReadMarker(root)
	if err != nil {
		return nil, err //nolint:wrapcheck // The marker reader names the file and the fault.
	}

	if !ok {
		return nil, nil //nolint:nilnil // No marker simply means a local-only archive.
	}

	// A marker that parses but records no bucket ("{}" from a damaged
	// restore, a hand-cleared file) would otherwise surface years later as a
	// bare "bucket url is required" at the first evicted read, pointing at
	// nothing; name the file while the reader is looking at it.
	if marker.URL == "" {
		return nil, fmt.Errorf("remote marker %q records no bucket url; "+
			"restore or edit it to point at the archive's mirror", remote.MarkerName)
	}

	return &orgRemote{
		ctx:         opts.ctx,
		newClient:   opts.newClient,
		orgName:     orgName,
		cfg:         marker.Config(),
		bundles:     make(map[string]*remoteBundle),
		fetches:     make(map[string]*inflightFetch),
		notify:      opts.listNotice,
		noticeGrace: opts.noticeGrace,
		merged:      marker.Partial,
	}, nil
}

// newSuppliedOrgRemote builds an [*orgRemote] around the shared client for an
// archive opened against a remote supplied through [WithRemote]. The inventory
// is left to [orgRemote.objects] to list lazily from the organization's own
// prefix on first merged use, so an organization nothing reads through costs
// no listing at all, and one that does costs its own prefix rather than the
// whole mirror's.
//
// Merged-ness comes from the caller (see [mergedFromMarker]) rather than from
// the fact that a remote was supplied: configuring a remote says where the
// mirror is, not that the tree beside it is incomplete.
func newSuppliedOrgRemote(
	orgName string, cfg remote.Config, client *remote.Client,
	merged bool, opts archiveOptions,
) *orgRemote {
	return &orgRemote{
		ctx:         opts.ctx,
		newClient:   opts.newClient,
		client:      client,
		orgName:     orgName,
		cfg:         cfg,
		bundles:     make(map[string]*remoteBundle),
		fetches:     make(map[string]*inflightFetch),
		notify:      opts.listNotice,
		noticeGrace: opts.noticeGrace,
		merged:      merged,
	}
}

// mergedFromMarker reports whether a supplied remote must stand in for the
// whole local tree rather than for its evicted surfaces alone.
//
// The marker is the archive's own record of completeness
// ([remote.Marker.Partial]), written only over a close sweep that proved the
// mirror holds nothing the local tree does not account for, and it is the same
// field [loadOrgRemote] reads when no remote was supplied. An organization
// carrying no marker has never made that claim, so the mirror stands in; a
// marker with its url cleared is the operator's consent to re-record, and this
// open is about to stamp it partial.
//
// Only [remoteOrg] may call this. The same inputs mean the opposite in
// [loadOrgRemote], where an absent marker means no remote at all and a cleared
// url is a refusal, so the two must not be folded together.
func mergedFromMarker(marker remote.Marker, hasMarker bool) bool {
	return !hasMarker || marker.URL == "" || marker.Partial
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

// fetchObject streams one evicted object's bytes from the mirror into w and
// returns how many it wrote, verifying them against the proof the eviction
// recorded in the stub. The size must match, and so must the digest when the
// eviction proved one. It is the whole-object counterpart of
// [orgRemote.readMember], for the surface that leaves nothing local to read
// out of.
//
// Verification matters more here than anywhere else in the read path, because
// this is the archive's only copy. The roll-up and bundle readers check their
// members against digests recorded beside intact local machinery, while these
// bytes come back from the one place they exist. The caller stages the stream
// through an atomic write, so a mismatch leaves no file rather than a plausible
// one.
func (r *orgRemote) fetchObject(
	ctx context.Context, relPath string, stub store.RemoteStub, w io.Writer,
) (int64, error) {
	return r.downloadVerified(ctx, r.cfg.Key(r.orgName, relPath), stub.Size, stub.SHA256, "its eviction", w)
}

// downloadVerified streams one whole mirrored object into w and returns how
// many bytes it wrote, verifying them against the size and, when one exists,
// the digest its proof recorded; proof names that record ("its eviction", the
// stub; "its upload", the mirror's own metadata) so a mismatch says which
// promise was broken. A proof carrying no digest verifies on length alone:
// there is nothing else to compare, and refusing it would strand an object
// the archive holds. The hasher is built only when there is something to
// compare it against, so such a fetch does not spend a pass over gigabytes to
// throw the result away.
//
// The download deliberately runs under the caller's context alone, with none
// of [remoteReadTimeout] over it: that bound sizes one ranged GET, and a
// whole object would blow it as a pure function of its length, failing
// identically on every retry and every run. The client's own stall watchdog
// is the right bound for a transfer, since it cuts a wedged connection
// without capping a working one.
func (r *orgRemote) downloadVerified(
	ctx context.Context, key string, size int64, digest, proof string, w io.Writer,
) (int64, error) {
	client, err := r.clientBuild()
	if err != nil {
		return 0, err
	}

	dst := w

	var sum hash.Hash

	if digest != "" {
		sum = sha256.New()
		dst = io.MultiWriter(w, sum)
	}

	n, err := client.Download(ctx, key, size, dst)
	if err != nil {
		return n, fmt.Errorf("fetch remote object %q: %w", key, err)
	}

	if n != size {
		return n, fmt.Errorf("remote object %q served %d of the %d bytes %s recorded", key, n, size, proof)
	}

	if sum != nil && hex.EncodeToString(sum.Sum(nil)) != digest {
		return n, fmt.Errorf("remote object %q does not match the digest %s recorded", key, proof)
	}

	return n, nil
}

// objects returns the org's mirror inventory keyed by archive-relative path,
// listed once per session. Both the inventory and a listing failure are cached
// (see the field's doc); [*Org.RemoteWarning] surfaces the cached failure so
// callers can degrade to local content without going quiet about it.
//
// The listing runs under the stored browse context alone, with none of
// [remoteReadTimeout] over it: a large mirror's listing is many pages, and the
// client's own stall watchdog already cuts a wedged page without capping a
// working enumeration.
//
// The client build and the enumeration run outside listMu, behind an in-flight
// latch the same way a bundle build does: a second objects call waits for the
// first's result, but the cheap readers ([orgRemote.childrenAt],
// [orgRemote.listingErr], and with them the whole event loop) only ever wait on
// field access, never on the listing itself.
func (r *orgRemote) objects() (map[string]remote.ObjectInfo, error) {
	r.listMu.Lock()

	for !r.listed && r.listBuild != nil {
		ready := r.listBuild
		r.listMu.Unlock()

		<-ready

		r.listMu.Lock()
	}

	if r.listed {
		defer r.listMu.Unlock()

		return r.listing, r.listErr
	}

	ready := make(chan struct{})
	r.listBuild = ready
	r.listMu.Unlock()

	// A listing that settles inside the grace period says nothing, so ordinary
	// use stays quiet; one that outlives it is the wait a caller printing
	// nothing until it finishes needs explained.
	if r.notify != nil {
		notice := time.AfterFunc(r.noticeGrace, func() { r.notify(r.orgName) })
		defer notice.Stop()
	}

	listing, byDir, err := r.buildListing()

	r.listMu.Lock()

	r.listing = listing
	r.byDir = byDir
	r.listErr = err
	r.listed = true
	r.listBuild = nil
	r.listMu.Unlock()

	close(ready)

	return listing, err
}

// buildListing builds the org's mirror inventory and its per-directory child
// index, the body [orgRemote.objects] runs outside listMu.
func (r *orgRemote) buildListing() (map[string]remote.ObjectInfo, map[string]map[string]remoteChild, error) {
	client, err := r.clientBuild()
	if err != nil {
		return nil, nil, err
	}

	prefix := r.cfg.Key(r.orgName, "") + "/"

	inventory, err := client.List(r.ctx, prefix)
	if err != nil {
		return nil, nil, fmt.Errorf("list mirror inventory: %w", err)
	}

	listing := relativeListing(inventory, prefix)

	return listing, indexListing(listing), nil
}

// relativeListing strips a key prefix from every listed key, dropping keys
// outside the prefix and the prefix itself. An empty prefix keeps every key.
func relativeListing(inventory map[string]remote.ObjectInfo, prefix string) map[string]remote.ObjectInfo {
	out := make(map[string]remote.ObjectInfo, len(inventory))

	for key, info := range inventory {
		rel := strings.TrimPrefix(key, prefix)
		if (rel == key && prefix != "") || rel == "" {
			continue
		}

		// A bucket key is arbitrary bytes: one carrying an empty, ".", or ".."
		// segment (or a backslash) names no honest archive path, and admitting
		// it would surface listing entries every read and extract then refuses,
		// and per-directory children whose joined path escapes the directory.
		// Dropping it here keeps every downstream consumer of the inventory
		// clean by construction.
		if seal.ValidName(rel) != nil {
			continue
		}

		out[rel] = info
	}

	return out
}

// remoteChild is one indexed child of a mirrored directory: a file with its
// recorded size, or a subdirectory.
type remoteChild struct {
	size int64
	dir  bool
}

// indexListing derives the per-directory child index from a flat inventory,
// so the merged listing helpers answer one directory in one lookup instead of
// scanning every key the mirror holds on every screen build. It is built once
// beside the listing it derives from and read-only after.
func indexListing(listing map[string]remote.ObjectInfo) map[string]map[string]remoteChild {
	idx := make(map[string]map[string]remoteChild)

	set := func(dir, name string, c remoteChild) {
		m := idx[dir]
		if m == nil {
			m = make(map[string]remoteChild)
			idx[dir] = m
		}

		// A directory never downgrades to a file: a name can only appear as
		// both under a malformed listing, and the subtree is the one to keep.
		if old, ok := m[name]; ok && old.dir {
			return
		}

		m[name] = c
	}

	for rel, info := range listing {
		dir := path.Dir(rel)
		if dir == "." {
			dir = ""
		}

		set(dir, path.Base(rel), remoteChild{size: info.Size})

		for dir != "" {
			parent := path.Dir(dir)
			if parent == "." {
				parent = ""
			}

			set(parent, path.Base(dir), remoteChild{dir: true})

			dir = parent
		}
	}

	return idx
}

// childrenAt returns the indexed children of one mirrored directory, nil when
// the mirror holds nothing there (or the listing has not been built). The
// returned map is shared and read-only.
func (r *orgRemote) childrenAt(relDir string) map[string]remoteChild {
	r.listMu.Lock()
	defer r.listMu.Unlock()

	return r.byDir[relDir]
}

// listingErr returns the cached mirror-listing failure the session is
// degrading around, or nil when no listing has failed (or run yet).
func (r *orgRemote) listingErr() error {
	r.listMu.Lock()
	defer r.listMu.Unlock()

	return r.listErr
}

// head probes one mirrored object by its archive-relative path, resolving the
// size and digests the mirror records for it.
func (r *orgRemote) head(relPath string) (remote.ObjectInfo, error) {
	client, err := r.clientBuild()
	if err != nil {
		return remote.ObjectInfo{}, err
	}

	ctx, cancel := context.WithTimeout(r.ctx, remoteReadTimeout)
	defer cancel()

	info, err := client.Head(ctx, r.cfg.Key(r.orgName, relPath))
	if err != nil {
		//nolint:wrapcheck // The client names the key; callers classify with errors.Is.
		return remote.ObjectInfo{}, err
	}

	return info, nil
}

// ensureLocal fetches the whole object at an archive-relative path from the
// mirror and persists it atomically at its local archive path, verifying the
// bytes against the size and digest the mirror records. A path the mirror does
// not hold returns [ErrObjectNotFound]. Concurrent same-path calls share one
// download; distinct paths fetch in parallel.
//
// The write takes no cross-process lock against a concurrently running
// archiver: it is rename-atomic, and its content is byte-identical to what the
// archiver itself mirrored (the digest it verifies is the one the archiver's
// upload recorded), so whichever write lands last leaves the same bytes.
func (r *orgRemote) ensureLocal(root, relPath string) error {
	r.mu.Lock()

	if f, ok := r.fetches[relPath]; ok {
		r.mu.Unlock()
		<-f.ready

		return f.err
	}

	f := &inflightFetch{ready: make(chan struct{})}
	r.fetches[relPath] = f
	r.mu.Unlock()

	err := r.fetchLocal(root, relPath)

	// Settle before delete, so a waiter that captured this entry reads err;
	// the delete then lets the next caller retry a failure or hit the disk.
	f.err = err
	close(f.ready)

	r.mu.Lock()
	delete(r.fetches, relPath)
	r.mu.Unlock()

	return err
}

// fetchLocal is one [orgRemote.ensureLocal] download: a Head for the object's
// size and digest, then a stream through an atomic write that verifies before
// the rename, so a mismatch leaves no file rather than a plausible one.
//
// The Head runs under [remoteReadTimeout] (one small request); the download
// runs under the stored browse context alone, since a whole object's transfer
// time is a function of its length and the client's stall watchdog is the
// right bound for it (the same shape as [orgRemote.fetchObject]).
func (r *orgRemote) fetchLocal(root, relPath string) error {
	client, err := r.clientBuild()
	if err != nil {
		return err
	}

	key := r.cfg.Key(r.orgName, relPath)

	headCtx, cancel := context.WithTimeout(r.ctx, remoteReadTimeout)
	defer cancel()

	info, err := client.Head(headCtx, key)
	if err != nil {
		if errors.Is(err, remote.ErrNotFound) {
			return fmt.Errorf("%w: %s is not in the mirror", ErrObjectNotFound, relPath)
		}

		return fmt.Errorf("probe remote object %q: %w", key, err)
	}

	// Confine the landing path to the org root exactly as [*Org.AbsPath]
	// confines the read side: a relPath carrying ".." segments must not let
	// the write escape what the follow-up read would then miss.
	abs := pathkit.ConfineJoin(root, relPath)

	err = atomicfile.Write(abs, func(w io.Writer) error {
		_, dlErr := r.downloadVerified(r.ctx, key, info.Size, info.SHA256, "its upload", w)

		return dlErr
	})
	if err != nil {
		return fmt.Errorf("persist %q: %w", relPath, err)
	}

	return nil
}

// fetchEligible reports whether a locally absent archive-relative path may be
// materialized from the mirror. The evicted surfaces are excluded so
// read-through never undoes an eviction: a configuration-version tarball
// (served by [ErrRemoteOnly] and the extract's fetch instead) and a bundle zip
// (served by ranged member reads). Everything else in the warm layer,
// including roll-ups, sidecars, and identity files, is eligible; eviction
// stubs pass the guard harmlessly but are never mirrored, so a fetch of one
// simply misses.
func fetchEligible(rel string) bool {
	return !store.IsConfigTarball(rel) && !store.IsBundleZip(rel)
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
// client deliberately lives for the browse session with no close path: the
// viewer exits with the process, which releases the backend connection.
//
// The stored browse context is the intended parent for the build: callers
// sit behind [io.ReaderAt]-shaped interfaces that carry no context.
//
//nolint:contextcheck // See above; there is no caller context to pass.
func (r *orgRemote) clientBuild() (*remote.Client, error) {
	r.clientOnce.Do(func() {
		// A pre-built shared client (the bootstrap's, injected through
		// newSeededOrgRemote) is already in place; the Do still runs so every
		// later read of the field sits behind the Once's memory barrier.
		if r.client != nil {
			return
		}

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
// one small ranged GET per flate read (thousands for a large log), so the
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

// RemoteWarning returns the mirror-listing failure the organization's merged
// listings are currently degrading around, or nil for an organization with no
// remote or whose mirror has answered (or never been asked). Callers surface
// it once, on the status line or stderr, so a listing that silently shows
// local content only never claims to be the whole archive.
func (o *Org) RemoteWarning() error {
	if o.remote == nil {
		return nil
	}

	return o.remote.listingErr()
}

// remoteObjects returns the organization's mirror inventory, or nil when the
// mirror should not (or cannot) stand in for the local tree: an organization
// with no remote, one whose remote is not merged (a complete archive's
// marker, where local reads stay local), or one whose listing failed. Under a
// failed listing the merged helpers built on it degrade to local content,
// with the failure held for [*Org.RemoteWarning] rather than failing a browse
// that may need nothing remote at all.
func (o *Org) remoteObjects() map[string]remote.ObjectInfo {
	if o.remote == nil || !o.remote.merged {
		return nil
	}

	inventory, err := o.remote.objects()
	if err != nil {
		return nil
	}

	return inventory
}

// remoteHas returns the mirror's record of an archive-relative path from the
// cached inventory, without a network round trip of its own.
func (o *Org) remoteHas(rel string) (remote.ObjectInfo, bool) {
	info, ok := o.remoteObjects()[rel]

	return info, ok
}

// remoteDirExists reports whether the mirror holds objects strictly beneath an
// archive-relative path, making it a directory in the merged tree. The path
// being an object itself wins: a mirror never holds both.
func (o *Org) remoteDirExists(rel string) bool {
	inventory := o.remoteObjects()
	if len(inventory) == 0 {
		return false
	}

	if _, ok := inventory[rel]; ok {
		return false
	}

	return len(o.remote.childrenAt(rel)) > 0
}

// remoteChildren returns the immediate child names the mirror holds under an
// archive-relative directory: subdirectory names sorted, and file names with
// their recorded sizes. Both are empty for an organization with no remote and
// under a failed listing (see [*Org.remoteObjects]).
func (o *Org) remoteChildren(relDir string) ([]string, map[string]int64) {
	if len(o.remoteObjects()) == 0 {
		return nil, nil
	}

	children := o.remote.childrenAt(relDir)
	if len(children) == 0 {
		return nil, nil
	}

	var dirs []string

	files := make(map[string]int64)

	for name, c := range children {
		if c.dir {
			dirs = append(dirs, name)
		} else {
			files[name] = c.size
		}
	}

	slices.Sort(dirs)

	return dirs, files
}

// remoteKeysUnder returns the mirror's archive-relative paths at or beneath a
// prefix, empty for an organization with no remote and under a failed listing
// (see [*Org.remoteObjects]).
func (o *Org) remoteKeysUnder(prefix string) map[string]remote.ObjectInfo {
	inventory := o.remoteObjects()
	if len(inventory) == 0 {
		return nil
	}

	out := make(map[string]remote.ObjectInfo)

	for rel, info := range inventory {
		if pathkit.UnderPrefix(rel, prefix) {
			out[rel] = info
		}
	}

	return out
}

// fetchThrough materializes an absent object at its local archive path from
// the mirror, reporting whether it landed so the caller can re-read it from
// disk. A false with a nil error means the object cannot be fetched (no
// merged remote, an ineligible path, or a mirror that does not hold it), and
// the caller keeps its original miss; a non-nil error is a real fetch fault
// (a failed transfer, a digest mismatch) worth surfacing over the miss.
//
// Only a merged remote fetches through: a complete archive's miss means the
// object was never archived, and a probe would spend a network round trip
// (and possibly a credential error) to learn nothing.
func (o *Org) fetchThrough(rel string) (bool, error) {
	if o.remote == nil || !o.remote.merged || !fetchEligible(rel) {
		return false, nil
	}

	err := o.remote.ensureLocal(o.root, rel)

	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrObjectNotFound):
		return false, nil
	default:
		return false, err
	}
}

// readThrough answers a confirmed local miss: it materializes the object from
// the mirror when it can ([*Org.fetchThrough]) and reads the landed bytes
// back, and otherwise returns miss, the caller's original error, unchanged.
// It is the one place the fetch-then-reread contract lives, so every read
// path falls through identically.
func (o *Org) readThrough(rel string, miss error) ([]byte, error) {
	fetched, err := o.fetchThrough(rel)

	switch {
	case err != nil:
		return nil, err
	case fetched:
		data, readErr := os.ReadFile(o.AbsPath(rel))
		if readErr != nil {
			return nil, fmt.Errorf("read %q: %w", rel, readErr)
		}

		return data, nil

	default:
		return nil, miss
	}
}

// copyThrough is [*Org.readThrough]'s streaming shape for the extract path: on
// a fetch it streams the landed file into w and reports handled; a false
// leaves the caller to describe its own miss.
func (o *Org) copyThrough(rel string, w io.Writer) (int64, bool, error) {
	fetched, err := o.fetchThrough(rel)

	switch {
	case err != nil:
		return 0, true, err
	case fetched:
		n, copyErr := copyLoose(o.AbsPath(rel), rel, w)

		return n, true, copyErr

	default:
		return 0, false, nil
	}
}
