package view

import (
	"archive/zip"
	"context"
)

// DeflateMethod is [zip.Deflate], exposed so the external test package can drive
// [DecompressMemberBoundedForTest] without importing archive/zip.
const DeflateMethod = zip.Deflate

// DecompressMemberBoundedForTest exposes [decompressMemberBounded] to tests, so
// the decompression cap can be exercised with a small limit rather than a
// gibibyte-scale fixture.
func DecompressMemberBoundedForTest(method uint16, compressed []byte, limit int64) ([]byte, error) {
	return decompressMemberBounded(method, compressed, limit)
}

// UnsealJob aliases the unexported job type so the external test package can
// carry plans between the plan and run shims.
type UnsealJob = unsealJob

// UnsealEvent aliases the unexported event type; its fields are exported, so
// tests assert on them directly.
type UnsealEvent = unsealEvent

// PlanWorkspaceUnsealForTest exposes [Org.planWorkspaceUnseal] to tests.
func PlanWorkspaceUnsealForTest(o *Org, ws *Workspace) ([]UnsealJob, error) {
	return o.planWorkspaceUnseal(ws)
}

// PlanProjectUnsealForTest exposes [Org.planProjectUnseal] to tests.
func PlanProjectUnsealForTest(o *Org, project string) ([]UnsealJob, error) {
	return o.planProjectUnseal(project)
}

// RunUnsealForTest drives [runUnseal] synchronously, returning the per-file
// events and the terminal summary (zero when the run was canceled before it
// finished).
func RunUnsealForTest(ctx context.Context, org *Org, target string, jobs []UnsealJob) ([]UnsealEvent, UnsealSummary) {
	events := make(chan unsealEvent)

	go runUnseal(ctx, org, target, jobs, events)

	var (
		perFile []UnsealEvent
		summary UnsealSummary
	)

	for ev := range events {
		if ev.Summary != nil {
			summary = *ev.Summary

			continue
		}

		perFile = append(perFile, ev)
	}

	return perFile, summary
}

// WriteUnsealedForTest exposes [writeUnsealed] to tests.
func WriteUnsealedForTest(target, org, rel string, data []byte) error {
	return writeUnsealed(target, org, rel, data)
}
