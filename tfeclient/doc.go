// Package tfeclient is the single point of contact with HCP Terraform.
//
// It wraps the go-tfe client behind a worker-safe surface so that every other
// package can treat network access as an already-throttled, already-classified
// capability. One rate limiter is shared across all concurrent workers: because
// N workers each paginating and downloading multiply the request rate, per-
// request retry alone is not enough, so exactly one client is constructed and
// shared and the limiter bounds the aggregate rate of the whole run. An
// optional [Gate] sits in front of the limiter and bounds how many requests
// are in flight at once; a resizable gate lets the caller scale the run's
// parallelism live, and the client's transport counts rate-limited (429)
// responses into a caller-supplied counter so an adaptive scaler has a
// pressure signal to react to. The package also walks paginated list
// endpoints (advancing the page number while the response reports a next
// page) and follows the short-lived signed-URL download flow for state blobs,
// configuration tarballs, and plan/apply logs.
//
// Failures are classified so a resume can tell a temporary blip from a
// permanent absence: transient (a network timeout, context cancellation or
// deadline, or rate-limiter exhaustion), terminal (a permanent absence such as
// a 404), or forbidden (an access denial). Rate-limited (429) responses are
// retried inside go-tfe, honoring the server's reset time; server (5xx) and
// transport failures are retried by this client's own bounded transport, since
// go-tfe's server-error retry is disabled. If one still surfaces it is not
// recognized structurally and classifies as unknown, which callers also treat
// as retryable. Recording that distinction keeps a resume from mistaking a blip
// for a permanently-gone object. A handful of endpoints do not
// enumerate at all and are reachable only when another object references their
// id (plan exports, HYOK encrypted data keys, the OIDC configuration types, and
// the experimental provider-set and registry-component types); the wrapper
// exposes them as id-addressed reads rather than lists.
//
// # go-tfe version
//
// This package targets the classic go-tfe client, module path
// github.com/hashicorp/go-tfe with no /v2 suffix, pinned to v1.109.0. The pin
// is deliberate: do not float to @latest and do not move to v2.
//
// The library ships two coexisting modules from one repository. The classic v1
// module is the hand-written jsonapi client this archive is built on; it is
// frozen, consolidated into a single file whose header declares it no longer
// tested and not to be extended, but frozen is not sunset. It still takes
// critical and security fixes, has roughly a hundred releases of hardening, and
// v1.109.0 is its last and most complete tag. That makes it the low-risk,
// highest-fidelity surface for a read-only archive.
//
// The /v2 module is a separate, nightly-regenerated Microsoft-Kiota client from
// an OpenAPI spec, still self-labeled beta. It is disqualifying here. Its spec
// omits roughly twenty operations this archive needs today: the entire Stacks
// family, the entire private registry (modules, providers, versions, platforms,
// no-code modules), private GPG key listing, reserved tag keys, run events, the
// organization audit configuration, cost-estimate logs, and native Terraform
// policy-evaluation outcomes. It also regresses two things a bulk archiver leans
// on: runs no longer accept an include sideload (collapsing plan, apply,
// configuration-version, created-by, and cost-estimate hydration into N+1
// walks), and the structured plan-JSON read is a generated no-op. Because the
// two modules have distinct import paths, adopting v2 additively later for a
// genuinely v2-only resource forecloses nothing.
package tfeclient
