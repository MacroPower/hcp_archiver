package demoapi

import (
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Latency bands the injector draws a request's delay from: enough to make a run
// look like it is talking to a network, far short of the client's
// time-to-first-byte bound.
const (
	apiLatencyMin  = 20 * time.Millisecond
	apiLatencyMax  = 80 * time.Millisecond
	blobLatencyMin = 40 * time.Millisecond
	blobLatencyMax = 220 * time.Millisecond
)

// rateLimitReset is the cooldown a rate-limited response advertises, in
// seconds. The progress panel renders a pause in whole seconds, so anything
// under one would render as a pause of none at all.
const rateLimitReset = "1.0"

// profile names the treatment one request path is eligible for.
//
// Everything but [profileNone] carries latency; the rest each add one failure,
// and only ever on a path's first request, so the archiver's own retry always
// wins on the second. A profile past latency is therefore assigned only to a
// path the archiver fetches once per run: a path a collector re-lists (a
// listing under a walk, a run's comments, a confirming re-probe) has no stable
// attempt count to hang a one-shot rule on.
type profile int

const (
	// The profileNone treatment leaves a request untouched: the ping every
	// client opens with, whose retry budget a rate-limited answer would spend
	// before the run starts, and the runs listing, whose own bucket already
	// paces it to one request every two seconds.
	profileNone profile = iota
	// The profileAPI treatment delays an ordinary API request.
	profileAPI
	// The profileBlob treatment delays an artifact download, which carries more
	// bytes and so more delay.
	profileBlob
	// The profileRateLimit treatment answers the first request 429, which
	// halves the general governor's rate once and pauses its launches until the
	// advertised reset.
	profileRateLimit
	// The profileTruncate treatment cuts the first request's body short, which
	// the client reads as an unexpected end of stream: a transient failure the
	// blob primitive retries in-run.
	profileTruncate
	// The profileVanish treatment answers the first request 404, which the
	// archive primitives re-probe once before believing.
	profileVanish
)

// verdict is what the injector decided for one request.
type verdict struct {
	// The reset field is the X-RateLimit-Reset header a rate-limited answer
	// carries.
	reset string
	// The latency field is how long to hold the request before answering.
	latency time.Duration
	// The status field is the HTTP status to answer with, or zero to run the
	// handler.
	status int
	// The truncate field asks for the handler's body to be cut short and the
	// connection closed behind it.
	truncate bool
}

// decide is the injector's whole policy: a pure function of the seed, the
// request path, how many times that path has been requested before, and the
// profile the path carries.
func decide(seed uint64, key string, attempt int, prof profile) verdict {
	switch prof {
	case profileNone:
		return verdict{}

	case profileAPI:
		return verdict{latency: jitter(seed, key, apiLatencyMin, apiLatencyMax)}

	case profileBlob:
		return verdict{latency: jitter(seed, key, blobLatencyMin, blobLatencyMax)}

	case profileRateLimit:
		v := verdict{latency: jitter(seed, key, apiLatencyMin, apiLatencyMax)}
		if attempt == 0 {
			v.status = 429
			v.reset = rateLimitReset
		}

		return v

	case profileTruncate:
		v := verdict{latency: jitter(seed, key, blobLatencyMin, blobLatencyMax)}
		v.truncate = attempt == 0

		return v

	case profileVanish:
		v := verdict{latency: jitter(seed, key, apiLatencyMin, apiLatencyMax)}
		if attempt == 0 {
			v.status = 404
		}

		return v

	default:
		return verdict{}
	}
}

// jitter returns a delay in [lo, hi) derived from seed and key, so one path is
// always held for the same time. Both bands are positive constants a few
// hundred milliseconds wide, so neither conversion can overflow.
func jitter(seed uint64, key string, lo, hi time.Duration) time.Duration {
	span := uint64(hi - lo) //nolint:gosec // A positive constant band.

	return lo + time.Duration(mix(seed, key)%span) //nolint:gosec // Bounded by that band.
}

// mix folds seed and key into one digest.
func mix(seed uint64, key string) uint64 {
	return hash(strconv.FormatUint(seed, 16) + ":" + key)
}

// injector applies [decide] to live requests, counting the attempts each path
// has taken so a one-shot failure fires exactly once.
//
// Create instances with [newInjector]. An injector is safe for concurrent use.
type injector struct {
	targets  map[string]profile
	attempts map[string]int
	mu       sync.Mutex
	seed     uint64
	limited  atomic.Int64
	enabled  bool
}

// newInjector creates a new [injector] over the paths in targets, each mapped
// to the profile it carries. A disabled injector returns a zero verdict for
// every request, which is how the browsing recordings collect their archive at
// full speed.
func newInjector(enabled bool, seed string, targets map[string]profile) *injector {
	return &injector{
		enabled:  enabled,
		seed:     hash(seed),
		targets:  targets,
		attempts: map[string]int{},
	}
}

// retarget installs the paths the injector treats specially, which the server
// resolves once the organization it serves has been built.
func (i *injector) retarget(targets map[string]profile) {
	i.mu.Lock()
	defer i.mu.Unlock()

	i.targets = targets
}

// next records one request for path and returns the verdict for it.
func (i *injector) next(path string) verdict {
	if !i.enabled {
		return verdict{}
	}

	i.mu.Lock()

	prof := i.profileFor(path)
	attempt := i.attempts[path]
	i.attempts[path]++
	i.mu.Unlock()

	v := decide(i.seed, path, attempt, prof)
	if v.status == 429 {
		i.limited.Add(1)
	}

	return v
}

// RateLimited reports how many rate-limited answers the injector has issued,
// which is the count the run's progress panel shows as its 429 tally.
func (i *injector) RateLimited() int {
	return int(i.limited.Load())
}

// profileFor returns the treatment path is eligible for. The caller holds mu.
func (i *injector) profileFor(path string) profile {
	if path == pingPath || isRunsListPath(path) {
		return profileNone
	}

	prof, ok := i.targets[path]
	if ok {
		return prof
	}

	if strings.HasPrefix(path, blobPrefix) {
		return profileBlob
	}

	return profileAPI
}

// isRunsListPath reports whether path is the runs listing, which the client
// meters in a bucket of its own at one request every two seconds; a failure
// there costs the run far more wall time than it shows.
func isRunsListPath(path string) bool {
	return strings.HasPrefix(path, apiPrefix+"workspaces/") && strings.HasSuffix(path, "/runs")
}
