// Integration tests for the [Ci] module. Individual tests are annotated
// with +check so `dagger check -m ci/tests` runs them all concurrently.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"dagger/tests/internal/dagger"

	"golang.org/x/sync/errgroup"
)

// caBundlePath is where the runtime images carry the root certificates. It
// mirrors the constant of the same name in the `ci` module, which this module
// cannot import.
const caBundlePath = "/etc/ssl/certs/ca-certificates.crt"

// Tests provides integration tests for the [Ci] module. Create instances
// with [New].
type Tests struct{}

// All runs all tests in parallel.
func (m *Tests) All(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error { return m.TestBuildDist(ctx) })
	g.Go(func() error { return m.TestBuildImageMetadata(ctx) })
	g.Go(func() error { return m.TestLintReleaserClean(ctx) })
	g.Go(func() error { return m.TestBinary(ctx) })
	g.Go(func() error { return m.TestLintActionsClean(ctx) })
	g.Go(func() error { return m.TestSchemas(ctx) })

	return g.Wait()
}

// TestBuildDist verifies that [Ci.Build] returns a dist directory containing
// expected entries (checksums and at least one platform archive).
//
// +check
func (m *Tests) TestBuildDist(ctx context.Context) error {
	entries, err := dag.Ci().Build().Entries(ctx)
	if err != nil {
		return fmt.Errorf("list dist entries: %w", err)
	}

	hasChecksums := false
	hasArchive := false
	for _, entry := range entries {
		if strings.Contains(entry, "checksums") {
			hasChecksums = true
		}
		if strings.Contains(entry, "linux_amd64") || strings.Contains(entry, "linux_arm64") {
			hasArchive = true
		}
	}

	if !hasChecksums {
		return fmt.Errorf("dist missing checksums file (entries: %v)", entries)
	}
	if !hasArchive {
		return fmt.Errorf("dist missing platform archive (entries: %v)", entries)
	}
	return nil
}

// TestBuildImageMetadata verifies that [Ci.BuildImages] produces
// containers with expected OCI labels and entrypoint for each platform.
//
// +check
func (m *Tests) TestBuildImageMetadata(ctx context.Context) error {
	dist := dag.Ci().Build()

	containers, err := dag.Ci().BuildImages(ctx, dagger.CiBuildImagesOpts{
		Version: "v0.0.0-test",
		Dist:    dist,
	})
	if err != nil {
		return fmt.Errorf("build images: %w", err)
	}
	if len(containers) != 2 {
		return fmt.Errorf("expected 2 platform containers, got %d", len(containers))
	}

	for i, ctr := range containers {
		version, err := ctr.Label(ctx, "org.opencontainers.image.version")
		if err != nil {
			return fmt.Errorf("[%d]: version label: %w", i, err)
		}
		if version != "v0.0.0-test" {
			return fmt.Errorf("[%d]: version label = %q, want %q", i, version, "v0.0.0-test")
		}

		title, err := ctr.Label(ctx, "org.opencontainers.image.title")
		if err != nil {
			return fmt.Errorf("[%d]: title label: %w", i, err)
		}
		if title != "hcp_archiver" {
			return fmt.Errorf("[%d]: title label = %q, want %q", i, title, "hcp_archiver")
		}

		created, err := ctr.Label(ctx, "org.opencontainers.image.created")
		if err != nil {
			return fmt.Errorf("[%d]: created label: %w", i, err)
		}
		if created == "" {
			return fmt.Errorf("[%d]: created label is empty", i)
		}

		ep, err := ctr.Entrypoint(ctx)
		if err != nil {
			return fmt.Errorf("[%d]: entrypoint: %w", i, err)
		}
		if len(ep) != 1 || ep[0] != "hcp_archiver" {
			return fmt.Errorf("[%d]: entrypoint = %v, want [hcp_archiver]", i, ep)
		}

		// Without the bundle every HTTPS call fails on an unknown authority,
		// which a scratch image gives no other sign of until it runs.
		size, err := ctr.File(caBundlePath).Size(ctx)
		if err != nil {
			return fmt.Errorf("[%d]: ca bundle %s: %w", i, caBundlePath, err)
		}
		if size == 0 {
			return fmt.Errorf("[%d]: ca bundle %s is empty", i, caBundlePath)
		}
	}

	return nil
}

// TestLintReleaserClean verifies that the GoReleaser configuration passes
// validation. This exercises the [Ci.LintReleaser] check.
//
// +check
func (m *Tests) TestLintReleaserClean(ctx context.Context) error {
	return dag.Ci().LintReleaser(ctx)
}

// TestBinary verifies that [Ci.Binary] compiles the hcp_archiver binary.
//
// +check
func (m *Tests) TestBinary(ctx context.Context) error {
	size, err := dag.Ci().Binary().Size(ctx)
	if err != nil {
		return fmt.Errorf("binary: %w", err)
	}
	if size == 0 {
		return fmt.Errorf("binary has zero size")
	}
	return nil
}

// TestLintActionsClean verifies that the GitHub Actions workflows pass
// zizmor linting. This exercises the [Ci.LintActions] check and catches
// workflow security or syntax issues.
//
// +check
func (m *Tests) TestLintActionsClean(ctx context.Context) error {
	return dag.Ci().LintActions(ctx)
}

// TestSchemas verifies that [Ci.Schemas] produces the Pages site directory
// with a valid config schema at the published path.
//
// +check
func (m *Tests) TestSchemas(ctx context.Context) error {
	contents, err := dag.Ci().Schemas().File("schemas/config.schema.json").Contents(ctx)
	if err != nil {
		return fmt.Errorf("read schema from site directory: %w", err)
	}

	var schema map[string]any
	if err := json.Unmarshal([]byte(contents), &schema); err != nil {
		return fmt.Errorf("schema is not valid JSON: %w", err)
	}
	if _, ok := schema["$schema"]; !ok {
		return fmt.Errorf("schema missing $schema key")
	}
	return nil
}
