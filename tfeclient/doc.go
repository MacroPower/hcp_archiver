// Package tfeclient is the single point of contact with HCP Terraform.
//
// It wraps the go-tfe client behind a worker-safe surface so that every other
// package can treat network access as an already-throttled, already-classified
// capability. The adaptive rate governors are shared across all concurrent
// workers: because N workers each paginating and downloading multiply the
// request rate, per-request retry alone is not enough, so exactly one client is
// constructed and shared. There is one governor per server-side rate bucket: a
// general one for most endpoints, and a far slower one for the two runs list
// endpoints, which the server meters separately at 30 requests per minute (see
// [DefaultRunsListRateLimit]). Pushback in one bucket never slows the other.
// The governors are enforced at the HTTP transport, so every attempt that
// leaves the process pays a token; the pages go-tfe's ListAll methods fetch
// internally, the fresh requests its chunked log readers make mid-stream, and
// every in-client retry cannot outrun the aggregate rate. Each governor is its
// bucket's single rate authority, adapting to the server's feedback: it
// launches at a configured ceiling, and a rate-limited (429) response halves
// the rate and pauses every launch in the bucket until the server's advertised
// reset. The pause spans the whole bucket because the server counts rejected
// requests against its window too, so no rate is slow enough to pace into a
// blown window; once it lifts, a clean stretch creeps the rate back up toward
// the ceiling. The go-tfe client's own rate-limit machinery is kept dormant:
// its server-error retry stays disabled, no 429 is ever surfaced to it, and the
// X-RateLimit-Limit header its internal limiter configures itself from is
// stripped off every response. An optional [Gate] bounds how many requests are
// in flight at once; the gate caps concurrency only and leaves the launch rate
// to the governors. The package also walks paginated list endpoints (advancing
// the page number while the response reports a next page) and follows the
// short-lived signed-URL download flow for state blobs, configuration
// tarballs, and plan/apply logs.
//
// Failures are classified so a resume can tell a temporary blip from a
// permanent absence: transient (a network timeout, context cancellation or
// deadline, or rate limiting past the bounded retries), terminal (a permanent
// absence such as a 404), or forbidden (an access denial). Server (5xx),
// transport, and rate-limited (429) failures are all retried by this client's
// own bounded transport, each under its own budget; a 429 that exhausts its
// budget surfaces as an error wrapping [ErrRateLimited]. If a failure still
// surfaces unrecognized structurally it classifies as unknown, which callers
// also treat as retryable. Recording that distinction keeps a resume from
// mistaking a blip for a permanently-gone object. A handful of endpoints do not
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
