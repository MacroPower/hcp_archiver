package collect

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"time"
)

// identityFileName is the sidecar, kept beside a name-keyed directory's
// archived objects, that binds the directory to the id of the object it
// archives. The dot prefix keeps it out of the way of the browsable tree,
// matching the co-located .ledger directory.
const identityFileName = ".identity.json"

// ErrIdentityMismatch reports that a name-keyed directory is owned by a
// different object than the one being archived into it: the object that
// originally claimed the name was deleted and a new one now carries it.
// Archiving the new object in place would overwrite the deleted object's
// final metadata, the very history the archive exists to keep, so the
// caller must skip the object and leave the operator to move the old
// directory aside.
var ErrIdentityMismatch = errors.New("directory identity mismatch")

// Identity is the sidecar record binding a name-keyed directory (a project,
// workspace, or stack, all addressed by their display names) to the id of the
// object archived in it.
//
// Names are mutable and reusable upstream while ids are neither, so the tree's
// readable name-keyed layout needs this record to notice when a name stops
// meaning the same object: a reused name fails its claim
// ([ErrIdentityMismatch]) instead of silently merging two objects' histories,
// and a renamed object leaves a RenamedTo breadcrumb in its old directory
// instead of silently forking into a duplicate.
type Identity struct {
	// FirstSeen is when the directory was first claimed.
	FirstSeen time.Time `json:"firstSeen"`
	// RenamedAt is when the rename was noticed.
	RenamedAt time.Time `json:"renamedAt,omitzero"`
	// ID is the id of the object that owns the directory.
	ID string `json:"id"`
	// RenamedTo names the sibling directory the owning object moved to when it
	// was renamed upstream. The directory it is recorded in is a kept-for-history
	// orphan: no run archives into it again unless the object is renamed back.
	RenamedTo string `json:"renamedTo,omitempty"`
}

// ClaimDir binds the name-keyed directory at dir (archive-relative) to the
// object id about to be archived into it, and reports whether the object was
// renamed: renamedFrom names the sibling directory that previously archived
// the same id, or is empty.
//
// An unclaimed directory is claimed; a directory already owned by id passes.
// When the directory carries a stale rename breadcrumb, the object was renamed
// back: the breadcrumb is cleared, the sibling directory the object just
// abandoned is stamped as the orphan it now is, and the abandoned sibling is
// reported through renamedFrom like any other rename. A directory owned by a
// different id fails with [ErrIdentityMismatch], and any
// failure (a mismatch, or an identity file that cannot be read or written)
// records dir as a dropped surface, so the caller must skip archiving the
// object and the run reports incomplete until the operator intervenes.
//
// When a fresh directory is claimed, the parent's other children are scanned
// once (cached per run) for a directory that already archives the same id;
// finding one means the object was renamed upstream, so the old directory is
// stamped with a RenamedTo breadcrumb and reported through renamedFrom for the
// caller to log. The old directory is left in place: the archive keeps
// history, and only the breadcrumb marks that its object lives on elsewhere.
//
// Every collector that archives into a name-keyed directory must claim it
// before its first write; today those are the project, workspace, and stack
// collectors.
func (e *Env) ClaimDir(dir, id string) (string, error) {
	e.idMu.Lock()
	defer e.idMu.Unlock()

	renamedFrom, err := e.claimDirLocked(dir, id)
	if err != nil {
		e.ledger.MarkSurfaceDropped(dir, err)
	}

	return renamedFrom, err
}

// claimDirLocked is the body of [Env.ClaimDir] with the identity mutex held.
func (e *Env) claimDirLocked(dir, id string) (string, error) {
	relPath := e.store.Join(dir, identityFileName)

	current, err := e.readIdentity(relPath)

	switch {
	case err != nil:
		// Unreadable or corrupt: ownership cannot be verified, so fail closed
		// rather than guess.
		return "", fmt.Errorf("read identity %q: %w", relPath, err)

	case current != nil && current.ID == id:
		// The object was renamed away and back again; the breadcrumb is stale.
		if current.RenamedTo != "" {
			return e.reclaimRenamedBackLocked(dir, id, current)
		}

		return "", nil

	case current != nil:
		return "", fmt.Errorf(
			"%w: %q archives %s, not %s; the name was reused upstream, so move the directory aside to archive the new object",
			ErrIdentityMismatch,
			dir,
			current.ID,
			id,
		)
	}

	// A fresh directory: the object is new under this parent, or was renamed
	// from a sibling. One scan of the siblings (cached per parent per run)
	// tells which. A scan fault fails the claim closed rather than proceeding as
	// if the parent had no siblings, which would miss the rename and commit the
	// directory so no later run ever re-scans it.
	owners, scanErr := e.ownersUnderLocked(path.Dir(dir))
	if scanErr != nil {
		return "", scanErr
	}

	prior := owners[id]
	if prior != "" && prior != path.Base(dir) {
		renameErr := e.markRenamed(path.Dir(dir), prior, path.Base(dir))
		if renameErr != nil {
			return "", renameErr
		}
	} else {
		prior = ""
	}

	err = e.writeIdentity(relPath, &Identity{ID: id, FirstSeen: time.Now().UTC()})
	if err != nil {
		return "", err
	}

	if owners := e.idOwners[path.Dir(dir)]; owners != nil {
		owners[id] = path.Base(dir)
	}

	return prior, nil
}

// reclaimRenamedBackLocked handles a re-claim of dir by id when dir still
// carries a rename breadcrumb: the object was renamed away and back again. The
// sibling directory the object just abandoned still records an unmarked claim
// of id, so it is stamped as an orphan before dir's stale breadcrumb is
// cleared; that order leaves any partial failure re-runnable, where the
// reverse would strand the sibling as a live-looking duplicate that no later
// claim ever revisits. The stamped sibling is returned as renamedFrom.
//
// The sibling is found through the owners scan rather than by following dir's
// own breadcrumb, which names only the first hop of a longer rename cycle. A
// sibling may also be legitimately absent (the operator moved it aside), in
// which case only the stale breadcrumb is cleared.
func (e *Env) reclaimRenamedBackLocked(dir, id string, current *Identity) (string, error) {
	owners, err := e.ownersUnderLocked(path.Dir(dir))
	if err != nil {
		return "", err
	}

	prior := owners[id]
	if prior != "" && prior != path.Base(dir) {
		renameErr := e.markRenamed(path.Dir(dir), prior, path.Base(dir))
		if renameErr != nil {
			return "", renameErr
		}
	} else {
		prior = ""
	}

	cleared := *current
	cleared.RenamedTo = ""
	cleared.RenamedAt = time.Time{}

	err = e.writeIdentity(e.store.Join(dir, identityFileName), &cleared)
	if err != nil {
		return "", err
	}

	owners[id] = path.Base(dir)

	return prior, nil
}

// markRenamed stamps the RenamedTo breadcrumb on the identity of the old
// directory oldBase under parent, recording that its object now archives under
// newBase. The stamp also gates the caller's rename warning to fire once
// rather than on every later run.
func (e *Env) markRenamed(parent, oldBase, newBase string) error {
	relPath := e.store.Join(parent, oldBase, identityFileName)

	old, err := e.readIdentity(relPath)
	if err != nil {
		return fmt.Errorf("read renamed identity %q: %w", relPath, err)
	}

	if old == nil {
		return fmt.Errorf("read renamed identity %q: %w", relPath, fs.ErrNotExist)
	}

	old.RenamedTo = newBase
	old.RenamedAt = time.Now().UTC()

	return e.writeIdentity(relPath, old)
}

// readIdentity reads and parses the identity sidecar at relPath, returning nil
// with no error when none exists.
func (e *Env) readIdentity(relPath string) (*Identity, error) {
	//nolint:gosec // The path is store-confined and derived from listed names.
	data, err := os.ReadFile(e.store.AbsPath(relPath))

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil //nolint:nilnil // Absence is a distinct, valid outcome.
	case err != nil:
		return nil, err //nolint:wrapcheck // The caller adds the path context.
	}

	var out Identity

	err = json.Unmarshal(data, &out)
	if err != nil {
		return nil, err //nolint:wrapcheck // The caller adds the path context.
	}

	if out.ID == "" {
		return nil, errors.New("identity file has no id")
	}

	return &out, nil
}

// writeIdentity atomically writes the identity sidecar at relPath.
func (e *Env) writeIdentity(relPath string, ident *Identity) error {
	data, err := json.MarshalIndent(ident, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal identity %q: %w", relPath, err)
	}

	_, err = e.store.WriteBytes(relPath, data)
	if err != nil {
		return fmt.Errorf("write identity %q: %w", relPath, err)
	}

	return nil
}

// ownersUnderLocked maps each owner id recorded by the children of parent to
// the child directory's name, scanning the disk once per parent per run and
// serving later claims from the cache. The scan is best-effort about its
// children: a child with no identity or an unreadable one is skipped, since its
// own claim fails closed when it is visited. A child carrying a RenamedTo
// breadcrumb is also skipped -- it is a kept-for-history orphan, and letting it
// into the map could shadow the directory that actually holds the object's
// latest history. A directory that does not yet
// exist caches an empty map. A real read fault on the parent itself is neither
// cached nor masked as an empty map -- it returns an error, so the caller fails
// its claim closed rather than proceeding as if the parent had no siblings and
// missing a rename.
func (e *Env) ownersUnderLocked(parent string) (map[string]string, error) {
	if owners, ok := e.idOwners[parent]; ok {
		return owners, nil
	}

	owners := make(map[string]string)

	entries, err := os.ReadDir(e.store.AbsPath(parent))
	switch {
	case err == nil:
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}

			ident, rerr := e.readIdentity(e.store.Join(parent, entry.Name(), identityFileName))
			if rerr != nil || ident == nil || ident.RenamedTo != "" {
				continue
			}

			owners[ident.ID] = entry.Name()
		}

	case !errors.Is(err, fs.ErrNotExist):
		// A real read fault (a permission or I/O error, not a fresh parent that
		// has no siblings yet) must not be cached, nor served as an empty map: an
		// empty map would silently disable rename detection under this parent and
		// commit the directory, so no later run re-scans it. Report it so the
		// claim fails closed and a later run re-scans before committing.
		return nil, fmt.Errorf("scan owners under %q: %w", parent, err)
	}

	e.idOwners[parent] = owners

	return owners, nil
}
