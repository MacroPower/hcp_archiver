package view

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"go.jacobcolvin.com/hcp_archiver/store"
)

// writeObject streams the bytes at an archive-relative path into w and returns
// how many it wrote, resolving the path exactly as [*Org.Read] does. Both run
// the shared [*Org.resolveRel] prologue, then take a workspace-scoped path
// through the workspace's forms and anything else as a loose file, and both
// give the same [ErrNotFile] and [ErrObjectNotFound] answers.
//
// It differs from Read in what it will do about an evicted object and in what
// it holds in memory. A configuration-version tarball whose bytes were evicted
// is fetched from the mirror rather than refused, so [ErrRemoteOnly] survives
// only where no fetch is possible (see [*Org.fetchOffloaded]). Neither that
// tarball nor a loose state blob is ever buffered whole, since the unbounded
// forms stream, while the bounded ones (a roll-up line, a bundle member) still
// resolve to a slice behind this signature.
//
//nolint:contextcheck // Read-through fetches run under the stored browse context (see orgRemote).
func (o *Org) writeObject(ctx context.Context, relPath string, w io.Writer) (int64, error) {
	rel, err := o.resolveRel(relPath)
	if err != nil {
		return 0, err
	}

	if ws := o.workspaceFor(rel); ws != nil {
		return ws.writeObject(ctx, rel, w)
	}

	n, err := copyLoose(o.AbsPath(rel), rel, w)
	if errors.Is(err, fs.ErrNotExist) {
		// A warm object the mirror holds is pulled down to its archive path
		// first, so the extract both recovers it and leaves it referenceable
		// locally; the evicted surfaces stream to the target alone.
		if n, handled, copyErr := o.copyThrough(rel, w); handled {
			return n, copyErr
		}

		return o.fetchOffloaded(ctx, rel, w)
	}

	return n, err
}

// fetchOffloaded streams an object whose local bytes are gone from the
// organization's mirror, verifying what arrives against the proof the eviction
// left behind.
//
// Everything it cannot fetch is described by [*Org.describeMiss], so a path
// that names no evicted object, an organization that records no mirror, and a
// stub too damaged to trust all answer here exactly what a read of the same
// path answers. The last of those is deliberate: the stub's mere existence
// proves the eviction happened, which is the fact an operator can act on, even
// when nothing else about it can be believed.
//
//nolint:contextcheck // The stub-synthesis Head runs under the stored browse context (see orgRemote).
func (o *Org) fetchOffloaded(ctx context.Context, rel string, w io.Writer) (int64, error) {
	stub, ok := o.offloadedStub(rel)
	if !ok {
		return 0, o.describeMiss(rel)
	}

	return o.remote.fetchObject(ctx, rel, stub, w)
}

// offloadedStub returns the eviction stub standing in for an absent object when
// its bytes can actually be fetched: the path names the one surface eviction
// leaves a stub for and the organization records where its mirror is. Under a
// merged remote, a local stub that is absent or refuses to read back
// truthfully falls through to the mirror's own record of the tarball, so a
// bootstrap archive (which holds no stubs at all; the sweep never mirrors
// them) fetches with the same verification an eviction recorded: the size and
// digest come from the upload the archiver itself verified. A complete
// archive answers from its stubs alone, spending no probe on a path whose
// absence its own tree already explains. Anything else leaves the refusal to
// [*Org.describeMiss].
func (o *Org) offloadedStub(rel string) (store.RemoteStub, bool) {
	if o.remote == nil || !store.IsConfigTarball(rel) {
		return store.RemoteStub{}, false
	}

	stub, ok := o.readRemoteStub(store.RemoteStubPath(rel))
	if ok {
		return stub, true
	}

	if !o.remote.merged {
		return store.RemoteStub{}, false
	}

	return o.headStub(rel)
}

// headStub synthesizes an eviction stub from the mirror's record of the
// tarball itself: one Head resolves the size and, when the upload recorded
// one, the digest the fetch verifies against. A tarball the mirror does not
// hold reports no stub, leaving the miss to [*Org.describeMiss], and so does
// a probe the mirror could not answer at all (bad credentials, a timeout).
// The two are deliberately indistinguishable here: the stub either proves a
// fetch can be verified or it does not exist, and the probe's own fault
// surfaces when something actually fetches.
func (o *Org) headStub(rel string) (store.RemoteStub, bool) {
	info, err := o.remote.head(rel)
	if err != nil {
		return store.RemoteStub{}, false
	}

	return store.RemoteStub{
		Version: store.RemoteStubVersion,
		Size:    info.Size,
		SHA256:  info.SHA256,
	}, true
}

// writeObject streams one workspace-scoped object into dst, mirroring
// [Workspace.Open]'s precedence: a loose file wins, then the workspace's
// roll-ups and bundles. A path holding no object but carrying sealed objects
// beneath it is a logical directory, reported as [ErrNotFile] the way a read of
// the same path reports it (see [Workspace.missOrDir]).
//
// Only the loose branch streams. The sealed forms are bounded by construction
// (a roll-up line is one JSON record, a bundle member by [maxMemberSize]), so
// they resolve whole and are handed to dst in one write.
//
// The context is accepted for the signature's sake and is deliberately unused,
// because a member of an evicted bundle is fetched through the organization's
// cached [io.ReaderAt], which carries the browse context rather than this
// call's. That is what makes the extract progress screen's cancel stop the loop
// between files without aborting an in-flight ranged GET, the behavior
// [extractProgressScreen.update] documents.
//
//nolint:contextcheck // See above; every remote read derives from the stored browse context.
func (w *Workspace) writeObject(_ context.Context, rel string, dst io.Writer) (int64, error) {
	n, err := copyLoose(w.org.AbsPath(rel), rel, dst)
	if !errors.Is(err, fs.ErrNotExist) {
		return n, err
	}

	data, err := w.openSealed(rel)
	if err != nil {
		if errors.Is(err, ErrObjectNotFound) {
			// The mirror may hold the loose object nothing local carries;
			// fetching it to its archive path both recovers it and leaves it
			// referenceable locally, matching [Workspace.Open]'s precedence.
			if n, handled, copyErr := w.org.copyThrough(rel, dst); handled {
				return n, copyErr
			}

			return 0, w.missOrDir(rel, err)
		}

		return 0, err
	}

	written, err := dst.Write(data)
	if err != nil {
		return int64(written), fmt.Errorf("write %q: %w", rel, err)
	}

	return int64(written), nil
}

// copyLoose streams the loose file at an absolute path into w, naming it by its
// archive-relative path in any error, the way the rest of the read layer does.
// An absent file reports [fs.ErrNotExist] through the wrap, which is what
// routes a caller to the object's evicted or sealed copy.
func copyLoose(abs, rel string, w io.Writer) (int64, error) {
	//nolint:gosec // The path is composed from the archive root being browsed.
	f, err := os.Open(abs)
	if err != nil {
		return 0, fmt.Errorf("read %q: %w", rel, err)
	}

	defer func() {
		//nolint:errcheck // Read-only handle.
		_ = f.Close()
	}()

	n, err := io.Copy(w, f)
	if err != nil {
		return n, fmt.Errorf("read %q: %w", rel, err)
	}

	return n, nil
}
