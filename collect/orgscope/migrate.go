package orgscope

import (
	"encoding/json"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"go.jacobcolvin.com/hcp_archiver/manifest"
	"go.jacobcolvin.com/hcp_archiver/store"
)

var (
	// The extensions a policy source file is archived under (see policyExt),
	// keyed for the migration's name matching.
	policySourceExts = map[string]bool{
		"sentinel": true,
		"rego":     true,
		"tf":       true,
		"policy":   true,
	}

	// The stamp policyRevision composes into a stamped source name's
	// extension.
	revisionStampPattern = regexp.MustCompile(`\.\d{8}T\d{6}Z$`)
)

// plainPolicySourceID returns the policy id a leaf under the policies
// directory names, and whether the leaf is a plain (unstamped) policy source
// file. The policy metadata's .json leaf and the stamped revision names both
// answer false: only the plain source entry carries the baseline
// [collect.Env.RevisionPath] reads.
func plainPolicySourceID(leaf string) (string, bool) {
	ext := path.Ext(leaf)
	if ext == "" || !policySourceExts[ext[1:]] {
		return "", false
	}

	id := strings.TrimSuffix(leaf, ext)
	if revisionStampPattern.MatchString(id) {
		return "", false
	}

	return id, true
}

// LedgerMigration returns the [manifest.Migration] backfilling the server-side
// updated-at onto pre-v2 plain policy source entries, read from the archived
// policy metadata beside them. Pass it to [manifest.Load] via
// [manifest.WithMigrations].
//
// A pre-v2 entry carries no server stamp, so [collect.Env.RevisionPath] would
// fall back to baselining new revisions against the plain capture's local
// fetch time, the weak cross-clock comparison that silently loses a revision
// uploaded between a run's download and its ledger stamp. The archived
// metadata's updated-at is the reading the pre-v2 releases compared against (a
// completed run's last recorded revision), so carrying it onto the entry
// preserves their baseline exactly. An entry whose metadata is unreadable or
// carries no updated-at stays unchanged and keeps the fetch-time fallback.
func LedgerMigration(st *store.Store) manifest.Migration {
	prefix := path.Dir(st.Policy("probe", "json")) + "/"

	return func(relPath string, _ int, e manifest.Entry) (manifest.Entry, bool) {
		if e.Status != manifest.StatusDone || !e.UpdatedAt.IsZero() {
			return e, false
		}

		if !strings.HasPrefix(relPath, prefix) {
			return e, false
		}

		id, ok := plainPolicySourceID(path.Base(relPath))
		if !ok {
			return e, false
		}

		updatedAt, ok := archivedPolicyUpdatedAt(st, id)
		if !ok {
			return e, false
		}

		e.UpdatedAt = updatedAt

		return e, true
	}
}

// archivedPolicyUpdatedAt reads the updated-at recorded in the archived
// metadata of the policy with the given id, and whether one is available: it
// is absent when the metadata was never archived, cannot be read, or carries
// no updated-at.
func archivedPolicyUpdatedAt(st *store.Store, id string) (time.Time, bool) {
	//nolint:gosec // The path is composed by the store from its archive root.
	data, err := os.ReadFile(st.AbsPath(st.Policy(id, "json")))
	if err != nil {
		return time.Time{}, false
	}

	var doc struct {
		Data struct {
			Attributes struct {
				UpdatedAt time.Time `json:"updated-at"`
			} `json:"attributes"`
		} `json:"data"`
	}

	err = json.Unmarshal(data, &doc)
	if err != nil || doc.Data.Attributes.UpdatedAt.IsZero() {
		return time.Time{}, false
	}

	return doc.Data.Attributes.UpdatedAt, true
}
