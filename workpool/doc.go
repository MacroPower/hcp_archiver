// Package workpool provides a resizable pool of worker slots and a controller
// that scales it from a rate-limit signal.
//
// A [Pool] is a counting semaphore whose capacity can move while work is in
// flight: every unit of work acquires a slot up front and releases it when
// done, so capacity is the number of units running at once rather than a
// fixed set of worker goroutines. That decouples "how much runs in parallel"
// from "how the work is structured": any number of producers can feed the
// same pool, and the slots flow to whatever work is ready.
//
// A [Controller] closes the loop between the pool and the server being
// called. It watches a cumulative counter of rate-limited responses and
// scales the pool the way TCP finds a path's capacity: doubling on clean
// sampling windows until rate limiting is first observed (slow start), then
// additive-increase, multiplicative-decrease: any rate limiting in a window
// halves the pool, and a clean window grows it by one, within configured
// bounds. The archiver starts the pool at one worker and points the counter
// at its API client's 429 observations, so every run finds its own
// parallelism: it ramps quickly while the server is silent, sheds the moment
// it pushes back, and probes gently back up.
package workpool
