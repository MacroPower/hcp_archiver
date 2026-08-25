package demoapi

import (
	"fmt"
	"hash/fnv"
	"strings"
)

// idAlphabet is the base62 alphabet HCP Terraform draws its identifiers from.
const idAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// ids mints every identifier the served organization is known by.
//
// Each one derives from a seed string, so two objects referring to the same
// thing agree on its identifier whichever the world builds first, and two
// servers started with the same seed serve the same organization. The seed also
// separates one demo organization from another: the archiver binds a workspace
// directory to the id it first saw there
// ([go.jacobcolvin.com/hcp_archiver/pkg/collect.Env.ClaimDir]), so a changed
// seed wants a fresh archive directory.
//
// Create instances with [newIDs].
type ids struct {
	seed string
}

// newIDs creates a new [ids] salting every identifier with seed.
func newIDs(seed string) *ids {
	return &ids{seed: seed}
}

// id renders an HCP-style identifier: the kind prefix and sixteen base62
// characters derived from name.
func (i *ids) id(prefix, name string) string {
	digest := hash(i.seed + ":" + prefix + ":" + name)

	var b strings.Builder

	b.WriteString(prefix)
	b.WriteByte('-')

	for range 16 {
		b.WriteByte(idAlphabet[digest%uint64(len(idAlphabet))])

		digest = digest*6364136223846793005 + 1442695040888963407
	}

	return b.String()
}

// commitSHA renders a plausible git object name from name.
func (i *ids) commitSHA(name string) string {
	digest := hash(i.seed + ":sha:" + name)

	var b strings.Builder

	for b.Len() < 40 {
		fmt.Fprintf(&b, "%016x", digest)

		digest = digest*6364136223846793005 + 1442695040888963407
	}

	return b.String()[:40]
}

// project returns a project's identifier.
func (i *ids) project(name string) string { return i.id("prj", name) }

// workspace returns a workspace's identifier.
func (i *ids) workspace(project, ws string) string { return i.id("ws", project+"/"+ws) }

// user returns a user's identifier.
func (i *ids) user(username string) string { return i.id("user", username) }

// team returns a team's identifier.
func (i *ids) team(name string) string { return i.id("team", name) }

// run returns the identifier of a workspace's run number n, counting back from
// the newest.
func (i *ids) run(project, ws string, n int) string {
	return i.id("run", fmt.Sprintf("%s/%s/%d", project, ws, n))
}

// stateVersion returns the identifier of a workspace's state version number n,
// counting back from the newest.
func (i *ids) stateVersion(project, ws string, n int) string {
	return i.id("sv", fmt.Sprintf("%s/%s/%d", project, ws, n))
}

// configVersion names the configuration version a workspace's run number n was
// built from. Runs share tarballs the way real ones do (a re-plan of the same
// commit reuses its configuration), so the organization holds fewer
// configuration versions than it has runs.
func (i *ids) configVersion(project, ws string, n int) string {
	return i.id("cv", fmt.Sprintf("%s/%s/%d", project, ws, n/2))
}

// hash returns the FNV-1a digest of name, the deterministic source every
// generated identifier draws from.
func hash(name string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))

	return h.Sum64()
}
