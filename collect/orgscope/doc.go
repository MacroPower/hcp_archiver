// Package orgscope archives the objects an organization owns directly,
// independent of any single project. It runs once per organization and treats
// this metadata as mutable: it re-reads the metadata and overwrites the stored
// copy when the payload changes, rather than tracking a watermark.
//
// The surface covers the organization record itself; teams with their
// organization-access matrix, members, and SSO/SCIM linkage, plus their team-
// scoped notification configurations; the organization roster, with each
// membership's user and team references; and VCS connections. Those connections
// are OAuth clients (with their tokens, secret redacted) and GitHub App
// installations; the installations are user- and token-scoped rather than
// org-scoped, so their completeness depends on the archiving identity.
//
// It also captures the governance objects: policy sets with their versions and
// parameters, the Sentinel or OPA policy source alongside each policy's
// metadata, variable sets with their variables, and organization run-task
// definitions (HMAC key redacted). The remaining org-level configuration rounds
// it out: agent pools with their allowed and excluded workspaces and allowed
// projects, per-token-type max-TTL policies, and reserved tag keys. When the
// corresponding scope toggle is on, it also captures hold-your-own-key
// configurations with their OIDC configuration and customer key versions.
//
// Two related objects are deliberately reduced to metadata or skipped: every
// token's secret is write-only and comes back blank, so only its existence and
// metadata are kept, and SSH keys expose only an id and name on read (the
// private key lives solely on the write-only create options), so there is no key
// material to archive.
package orgscope
