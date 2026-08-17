package main

import (
	"context"
	"strings"
	"time"

	"dagger/ci/internal/dagger"
)

// Build runs GoReleaser in snapshot mode, producing binaries for all
// platforms. Returns the dist/ directory. Docker, Homebrew, Nix, signing, and
// SBOM stages are skipped here: images are built natively via Dagger in
// [Ci.BuildImages], package publishing only happens during a real release,
// and signing requires OIDC credentials only available during a real release.
func (m *Ci) Build(ctx context.Context) (*dagger.Directory, error) {
	ctr, err := m.releaserBase(ctx)
	if err != nil {
		return nil, err
	}
	return ctr.
		WithExec([]string{
			"goreleaser", "release", "--snapshot", "--clean",
			"--skip=docker,homebrew,nix,sign,sbom",
			"--parallelism=0",
		}).
		Directory("/src/dist"), nil
}

// BinarySnapshot builds the hcp_archiver binary for a single platform via GoReleaser
// in snapshot mode.
func (m *Ci) BinarySnapshot(
	ctx context.Context,
	// Target build platform (e.g. "linux/arm64").
	platform dagger.Platform,
) (*dagger.File, error) {
	ctr, err := m.releaserBase(ctx)
	if err != nil {
		return nil, err
	}
	goos, goarch, variant := parsePlatform(platform)
	ctr = ctr.
		WithEnvVariable("GOOS", goos).
		WithEnvVariable("GOARCH", goarch)
	// A variant-qualified platform (e.g. "linux/arm/v7", "linux/arm64/v8.0")
	// must not fold the variant into GOARCH; map it to the microarchitecture
	// env var Go expects instead.
	switch {
	case variant != "" && goarch == "arm":
		ctr = ctr.WithEnvVariable("GOARM", strings.TrimPrefix(variant, "v"))
	case variant != "" && goarch == "arm64":
		ctr = ctr.WithEnvVariable("GOARM64", variant)
	}
	return ctr.
		// GoReleaser does not create the --output parent directory.
		WithDirectory("/out", dag.Directory()).
		WithExec([]string{
			"goreleaser", "build", "--snapshot", "--clean",
			"--single-target", "--output", "/out/hcp_archiver",
		}).
		File("/out/hcp_archiver"), nil
}

// parsePlatform splits a dagger.Platform ("os/arch" or "os/arch/variant") into
// its GOOS, GOARCH, and optional variant, so a variant-qualified platform does
// not fold the variant into GOARCH.
func parsePlatform(platform dagger.Platform) (goos, goarch, variant string) {
	parts := strings.SplitN(string(platform), "/", 3)
	goos = parts[0]
	if len(parts) > 1 {
		goarch = parts[1]
	}
	if len(parts) > 2 {
		variant = parts[2]
	}
	return goos, goarch, variant
}

// BuildImages builds multi-arch runtime container images from a GoReleaser
// dist directory. If no dist is provided, a snapshot build is run.
func (m *Ci) BuildImages(
	ctx context.Context,
	// Version label for OCI metadata.
	// +default="snapshot"
	version string,
	// Pre-built GoReleaser dist directory. If not provided, runs a snapshot build.
	// +optional
	dist *dagger.Directory,
) ([]*dagger.Container, error) {
	if dist == nil {
		var err error
		dist, err = m.Build(ctx)
		if err != nil {
			return nil, err
		}
	}

	return runtimeImages(dist, version, ociCreated())
}

// ociCreated renders the current time for the
// org.opencontainers.image.created annotation.
func ociCreated() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// platformDistDir maps a target platform to the GoReleaser dist
// subdirectory holding its hcp_archiver binary.
type platformDistDir struct {
	platform dagger.Platform
	distDir  string
}

// platformDistDirs is the release platform matrix. Both runtimeImages
// and [Ci.ReleaseDryRun] range over it so the images that ship
// and the dry-run that verifies them cover the same platforms.
var platformDistDirs = []platformDistDir{
	{platform: "linux/amd64", distDir: "hcp_archiver_linux_amd64_v1"},
	{platform: "linux/arm64", distDir: "hcp_archiver_linux_arm64_v8.0"},
}

// runtimeImages builds a multi-arch set of runtime container images from a
// pre-built GoReleaser dist/ directory.
//
// The images are scratch plus the statically linked binary and the root
// certificate bundle. The bundle is the one thing the binary cannot supply
// itself: every HCP Terraform and object-store call is HTTPS, and with no
// system trust store Go rejects each one as signed by an unknown authority.
func runtimeImages(dist *dagger.Directory, version, created string) ([]*dagger.Container, error) {
	containers := make([]*dagger.Container, len(platformDistDirs))
	// The bundle is architecture-independent, so one pull serves every
	// platform image.
	caBundle := dag.Container().From(certsImage).File(caBundlePath)

	for i, p := range platformDistDirs {
		containers[i] = withOCILabels(dag.Container(dagger.ContainerOpts{Platform: p.platform})).
			WithLabel("org.opencontainers.image.version", version).
			WithLabel("org.opencontainers.image.created", created).
			WithAnnotation("org.opencontainers.image.version", version).
			WithAnnotation("org.opencontainers.image.created", created).
			WithFile(caBundlePath, caBundle).
			WithFile("/usr/local/bin/hcp_archiver", dist.File(p.distDir+"/hcp_archiver")).
			WithEntrypoint([]string{"hcp_archiver"})
	}

	return containers, nil
}

// withOCILabels applies the static OCI labels and annotations.
func withOCILabels(ctr *dagger.Container) *dagger.Container {
	return ctr.
		WithLabel("org.opencontainers.image.title", "hcp_archiver").
		WithLabel("org.opencontainers.image.source", "https://github.com/MacroPower/hcp_archiver").
		WithLabel("org.opencontainers.image.url", "https://github.com/MacroPower/hcp_archiver").
		WithLabel("org.opencontainers.image.licenses", "Apache-2.0").
		WithAnnotation("org.opencontainers.image.title", "hcp_archiver").
		WithAnnotation("org.opencontainers.image.source", "https://github.com/MacroPower/hcp_archiver")
}

// releaserBase builds the full release toolset: the shared GoReleaser base
// (the Go build base plus the goreleaser binary, from the [Goreleaser]
// toolchain) extended with cosign, syft, project source, and a bootstrapped
// git repo (everything goreleaser release needs for signing and SBOMs).
// cosign and syft are folded into the goreleaser toolchain, so its
// WithCosign/WithSyft install those binaries for GoReleaser's sign and sbom
// steps. Config-only validation goes through the [Goreleaser] toolchain
// directly; see [Ci.LintReleaser].
func (m *Ci) releaserBase(_ context.Context) (*dagger.Container, error) {
	// WithCosign/WithSyft take and return a container, so they are applied as
	// statements rather than chained.
	ctr := m.Goreleaser.GoreleaserBase()
	ctr = m.Goreleaser.WithCosign(ctr)
	ctr = m.Goreleaser.WithSyft(ctr)
	ctr = ctr.
		// Env vars used by GoReleaser ldflags and templates.
		WithEnvVariable("HOSTNAME", "dagger").
		WithEnvVariable("USER", "dagger").
		// Mount source after all tools so that source changes only invalidate
		// layers from here onward, preserving the tool installation layers above.
		WithMountedDirectory("/src", m.Source)
	return m.Goreleaser.EnsureGitRepo(ctr, dagger.GoreleaserEnsureGitRepoOpts{RemoteURL: cloneURL}), nil
}
