package registry

import (
	"context"
	"fmt"

	"github.com/hashicorp/go-tfe"
	"golang.org/x/sync/errgroup"

	"go.jacobcolvin.com/hcp_archiver/tfeclient"
)

// collectProviders enumerates the organization's registry providers, hydrating
// each provider's versions as id references, and archives them.
func (c *Collector) collectProviders(ctx context.Context) error {
	providers, err := tfeclient.Paginate(
		ctx,
		c.env.Client(),
		func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.RegistryProvider, *tfe.Pagination, error) {
			include := []tfe.RegistryProviderIncludeOps{tfe.RegistryProviderVersionsInclude}

			list, e := tc.RegistryProviders.List(ctx, c.org, &tfe.RegistryProviderListOptions{
				ListOptions: o,
				Include:     &include,
			})
			if e != nil {
				return nil, nil, fmt.Errorf("list registry providers: %w", e)
			}

			return list.Items, list.Pagination, nil
		},
	)
	if err != nil {
		return c.listFailed(ctx, "providers", err)
	}

	// The providers archive concurrently, each under its own namespace/name
	// paths; under detail each fans into per-version reads of its own, so the
	// client's gate, not this loop, bounds the real parallelism, and the
	// fan-out is capped at the environment's ceiling so a huge registry cannot
	// park thousands of goroutines on the gate. An archive returns non-nil only
	// on a cancellation, which cancels the group.
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(c.env.Concurrency())

	for _, prov := range providers {
		g.Go(func() error {
			return c.archiveProvider(gctx, prov)
		})
	}

	return g.Wait() //nolint:wrapcheck // Archive errors already carry their context.
}

// archiveProvider writes a provider's mutable metadata and, when detail is
// enabled and the provider is private, its versions and their platforms.
func (c *Collector) archiveProvider(ctx context.Context, prov *tfe.RegistryProvider) error {
	st := c.env.Store()
	path := st.RegistryProviderFile(prov.Namespace, prov.Name, providerFile)

	err := c.env.Mutable(ctx, path, func(_ context.Context) (any, error) {
		return prov, nil
	})
	if err != nil {
		return wrap("archive registry provider", err)
	}

	if !c.detail || prov.RegistryName == tfe.PublicRegistry {
		return nil
	}

	return c.archiveProviderDetail(ctx, prov)
}

// archiveProviderDetail lists a private provider's versions and archives each
// version's frozen metadata together with its platform list.
func (c *Collector) archiveProviderDetail(ctx context.Context, prov *tfe.RegistryProvider) error {
	pid := tfe.RegistryProviderID{
		OrganizationName: c.org,
		RegistryName:     prov.RegistryName,
		Namespace:        prov.Namespace,
		Name:             prov.Name,
	}

	versions, err := tfeclient.Paginate(
		ctx,
		c.env.Client(),
		func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.RegistryProviderVersion, *tfe.Pagination, error) {
			list, e := tc.RegistryProviderVersions.List(ctx, pid, &tfe.RegistryProviderVersionListOptions{
				ListOptions: o,
			})
			if e != nil {
				return nil, nil, fmt.Errorf("list registry provider versions: %w", e)
			}

			return list.Items, list.Pagination, nil
		},
	)
	if err != nil {
		return c.listFailed(ctx, "provider-versions", err)
	}

	// Each version is a frozen write plus one platform list at its own paths,
	// and a provider accumulates them for as long as it publishes, so they
	// fetch concurrently, capped at the environment's ceiling; the settled
	// versions skip inside Object.
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(c.env.Concurrency())

	for _, ver := range versions {
		g.Go(func() error {
			return c.archiveProviderVersion(gctx, prov, pid, ver)
		})
	}

	return g.Wait() //nolint:wrapcheck // Archive errors already carry their context.
}

// archiveProviderVersion writes a provider version's frozen metadata and its
// platform list, keeping the per-platform shasums that stand in for the
// undownloadable binaries.
func (c *Collector) archiveProviderVersion(
	ctx context.Context,
	prov *tfe.RegistryProvider,
	pid tfe.RegistryProviderID,
	ver *tfe.RegistryProviderVersion,
) error {
	st := c.env.Store()
	versionPath := st.RegistryProviderFile(prov.Namespace, prov.Name, versionFilename(ver.Version))

	err := c.env.Object(ctx, versionPath, func(_ context.Context) (any, error) {
		return ver, nil
	})
	if err != nil {
		return wrap("archive registry provider version", err)
	}

	vid := tfe.RegistryProviderVersionID{
		RegistryProviderID: pid,
		Version:            ver.Version,
	}
	platformsPath := st.RegistryProviderFile(prov.Namespace, prov.Name, providerPlatformsFilename(ver.Version))

	fetch := func(ctx context.Context) (any, error) {
		out, e := tfeclient.Paginate(
			ctx,
			c.env.Client(),
			func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.RegistryProviderPlatform, *tfe.Pagination, error) {
				list, le := tc.RegistryProviderPlatforms.List(ctx, vid, &tfe.RegistryProviderPlatformListOptions{
					ListOptions: o,
				})
				if le != nil {
					return nil, nil, fmt.Errorf("list registry provider platforms: %w", le)
				}

				return list.Items, list.Pagination, nil
			},
		)

		return out, wrap("fetch registry provider platforms", e)
	}

	return wrap("archive registry provider platforms", c.env.Object(ctx, platformsPath, fetch))
}
