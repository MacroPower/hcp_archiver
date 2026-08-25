package demoapi

import "time"

// Profiles exposed to the external test package, naming the treatments
// [Decide] applies.
const (
	ProfileNone      = profileNone
	ProfileAPI       = profileAPI
	ProfileBlob      = profileBlob
	ProfileRateLimit = profileRateLimit
	ProfileTruncate  = profileTruncate
	ProfileVanish    = profileVanish
)

// Profile exposes profile to the external test package.
type Profile = profile

// Verdict is what [Decide] returned, flattened for assertions.
type Verdict struct {
	Reset    string
	Latency  time.Duration
	Status   int
	Truncate bool
}

// Decide exposes decide to the external test package.
func Decide(seed uint64, key string, attempt int, prof Profile) Verdict {
	v := decide(seed, key, attempt, prof)

	return Verdict{Reset: v.reset, Latency: v.latency, Status: v.status, Truncate: v.truncate}
}

// Window is the pagination object [WindowFor] builds, flattened for
// assertions.
type Window struct {
	CurrentPage  int
	PreviousPage int
	NextPage     int
	TotalCount   int
	TotalPages   int
}

// WindowFor exposes windowFor to the external test package, alongside the page
// of items it names.
func WindowFor(total, number, size int) (Window, []int) {
	items := make([]int, 0, total)
	for i := range total {
		items = append(items, i)
	}

	win := windowFor(total, number, size)

	return Window(win), pageOf(items, win, size)
}

// LatencyBounds exposes the latency bands to the external test package.
var LatencyBounds = map[Profile][2]time.Duration{
	profileAPI:       {apiLatencyMin, apiLatencyMax},
	profileBlob:      {blobLatencyMin, blobLatencyMax},
	profileRateLimit: {apiLatencyMin, apiLatencyMax},
	profileTruncate:  {blobLatencyMin, blobLatencyMax},
	profileVanish:    {apiLatencyMin, apiLatencyMax},
}
