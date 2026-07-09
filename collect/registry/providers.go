package registry

import (
	"context"
	"fmt"

	"github.com/hashicorp/go-tfe"

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
		return c.listFailed(ctx, "providers")
	}

	for _, prov := range providers {
		archiveErr := c.archiveProvider(ctx, prov)
		if archiveErr != nil {
			return archiveErr
		}
	}

	return nil
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
		return c.listFailed(ctx, "provider-versions")
	}

	for _, ver := range versions {
		versionErr := c.archiveProviderVersion(ctx, prov, pid, ver)
		if versionErr != nil {
			return versionErr
		}
	}

	return nil
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
	versionPath := st.RegistryProviderFile(prov.Namespace, prov.Name, providerVersionFilename(ver.Version))

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
