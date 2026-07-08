// Package collect provides the machinery every domain collector shares, so that
// each collector carries only the knowledge of which endpoints and includes
// describe its own object family.
//
// It binds the API client, the serializer, the on-disk store, and the ledger
// into one environment that archives a single object end to end: consult the
// ledger to decide whether to fetch, retrieve through the client, serialize and
// redact, write atomically to the object's path, and record the resulting
// status, content signature, and (for append-mostly collections) high-water
// mark. Two behaviors that would otherwise be re-implemented per family live
// here: the newest-first walk that halts as soon as it reaches an already-
// archived object, and the mapping from the client's transient-versus-terminal
// error classification onto a ledger status, so a best-effort run continues past
// a missing object instead of aborting.
//
// A domain collector satisfies this package's collector contract and is
// constructed and scheduled by the orchestrator; it never touches the
// serializer, store, or ledger directly. It reaches them only through the
// shared environment. That keeps the collectors thin and this engine the one
// place the per-object archive policy is enforced.
package collect
