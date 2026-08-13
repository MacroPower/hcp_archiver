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
func (o *Org) fetchOffloaded(ctx context.Context, rel string, w io.Writer) (int64, error) {
	stub, ok := o.offloadedStub(rel)
	if !ok {
		return 0, o.describeMiss(rel)
	}

	return o.remote.fetchObject(ctx, rel, stub, w)
}

// offloadedStub returns the eviction stub standing in for an absent object when
// its bytes can actually be fetched: the path names the one surface eviction
// leaves a stub for, the stub reads back truthfully, and the organization
// records where its mirror is. Anything else leaves the refusal to
// [*Org.describeMiss].
func (o *Org) offloadedStub(rel string) (store.RemoteStub, bool) {
	if o.remote == nil || !store.IsConfigTarball(rel) {
		return store.RemoteStub{}, false
	}

	return o.readRemoteStub(store.RemoteStubPath(rel))
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
// call's. That is what makes the unseal progress screen's cancel stop the loop
// between files without aborting an in-flight ranged GET, the behavior
// [unsealProgressScreen.update] documents.
func (w *Workspace) writeObject(_ context.Context, rel string, dst io.Writer) (int64, error) {
	n, err := copyLoose(w.org.AbsPath(rel), rel, dst)
	if !errors.Is(err, fs.ErrNotExist) {
		return n, err
	}

	data, err := w.openSealed(rel)
	if err != nil {
		if errors.Is(err, ErrObjectNotFound) {
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
