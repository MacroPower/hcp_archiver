package view

import (
	"archive/zip"
	"context"
	"io"
	"time"
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

// ExtractJob aliases the unexported job type so the external test package can
// carry plans between the plan and run shims.
type ExtractJob = extractJob

// ExtractEvent aliases the unexported event type; its fields are exported, so
// tests assert on them directly.
type ExtractEvent = extractEvent

// PlanWorkspaceExtractForTest exposes [Org.planWorkspaceExtract] to tests.
func PlanWorkspaceExtractForTest(o *Org, ws *Workspace) ([]ExtractJob, error) {
	return o.planWorkspaceExtract(ws)
}

// PlanProjectExtractForTest exposes [Org.planProjectExtract] to tests.
func PlanProjectExtractForTest(o *Org, project string) ([]ExtractJob, error) {
	return o.planProjectExtract(project)
}

// RunExtractForTest drives [runExtract] synchronously, returning the per-file
// events and the terminal summary (zero when the run was canceled before it
// finished).
func RunExtractForTest(
	ctx context.Context,
	org *Org,
	target string,
	jobs []ExtractJob,
) ([]ExtractEvent, ExtractSummary) {
	events := make(chan extractEvent)

	go runExtract(ctx, org, target, jobs, events)

	var (
		perFile []ExtractEvent
		summary ExtractSummary
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

// WriteExtractedForTest exposes [writeExtracted] to tests, streaming data as the
// object's bytes.
func WriteExtractedForTest(ctx context.Context, target, org, rel string, data []byte) error {
	_, err := writeExtracted(ctx, target, org, rel, func(_ context.Context, _ string, w io.Writer) (int64, error) {
		n, err := w.Write(data)

		return int64(n), err //nolint:wrapcheck // A test shim over one write.
	})

	return err
}

// WithListNoticeGraceForTest shortens how long a listing runs before
// [WithListNotice]'s callback fires, so a test exercises the notice without
// the wall-clock wait the real grace period imposes.
func WithListNoticeGraceForTest(d time.Duration) ArchiveOption {
	return func(o *archiveOptions) { o.noticeGrace = d }
}
