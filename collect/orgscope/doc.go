// Package orgscope archives the objects an organization owns directly,
// independent of any single project. It runs once per organization and treats
// this metadata as mutable: it re-reads the metadata and overwrites the stored
// copy when the payload changes, rather than tracking a watermark.
//
// The surface covers the organization record itself; teams with their
// organization-access matrix, members, and SSO/SCIM linkage, plus their team-
// scoped notification configurations; the organization roster, with each
// membership's team reference; and VCS connections. Those connections are OAuth
// clients (each with its access tokens) and GitHub App
// installations; the installations are user- and token-scoped rather than
// org-scoped, so their completeness depends on the archiving identity.
//
// It also captures the governance objects: policy sets with their current and
// newest version and their parameters, each policy's raw source alongside its
// metadata, variable sets with their variables, and
// organization run-task definitions. The remaining org-level
// configuration rounds it out: agent pools with their allowed and excluded
// workspaces and allowed projects, per-token-type max-TTL policies, and reserved
// tag keys. When the corresponding scope toggle is on, it also captures
// hold-your-own-key configurations with their OIDC configuration and customer
// key versions.
//
// Several of these relations are hydrated by a list include yet would collapse
// to a bare id reference on their parent, discarding the attributes the include
// already fetched. Each is instead archived as its own record so those
// attributes survive: a policy set's current and newest version, a HYOK
// configuration's OIDC configuration and key versions, and an OAuth client's
// tokens. Users are the sharpest case: go-tfe exposes no user listing, so the
// users a team and the roster reference are the only capture of who belongs to
// the org, and each is archived from the membership and team reads that hydrate
// them.
//
// Two related objects are deliberately reduced to metadata or skipped: every
// token's secret is write-only and comes back blank, so only its existence and
// metadata are kept, and SSH keys expose only an id and name on read (the
// private key lives solely on the write-only create options), so there is no key
// material to archive.
package orgscope
