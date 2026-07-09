package registry

import (
	"context"
	"fmt"

	"github.com/hashicorp/go-tfe"

	"github.com/MacroPower/tfc_archiver/tfeclient"
)

// collectGPGKeys archives the private-registry GPG signing keys scoped to the
// organization's namespace.
//
// The listing is namespaced rather than a flat org list, so it is scoped to the
// organization name; each key is written under its own namespace and key id.
func (c *Collector) collectGPGKeys(ctx context.Context) error {
	keys, err := tfeclient.Paginate(
		ctx,
		c.env.Client(),
		func(ctx context.Context, tc *tfe.Client, o tfe.ListOptions) ([]*tfe.GPGKey, *tfe.Pagination, error) {
			list, e := tc.GPGKeys.ListPrivate(ctx, tfe.GPGKeyListOptions{
				ListOptions: o,
				Namespaces:  []string{c.org},
			})
			if e != nil {
				return nil, nil, fmt.Errorf("list registry gpg keys: %w", e)
			}

			return list.Items, list.Pagination, nil
		},
	)
	if err != nil {
		return c.listFailed(ctx, "gpg-keys")
	}

	for _, key := range keys {
		archiveErr := c.archiveGPGKey(ctx, key)
		if archiveErr != nil {
			return archiveErr
		}
	}

	return nil
}

// archiveGPGKey writes a single GPG key's mutable metadata, including its
// ascii-armored public key material.
func (c *Collector) archiveGPGKey(ctx context.Context, key *tfe.GPGKey) error {
	st := c.env.Store()
	path := st.RegistryGPGKey(key.Namespace, key.KeyID)

	return wrap("archive registry gpg key", c.env.Mutable(ctx, path, func(_ context.Context) (any, error) {
		return key, nil
	}))
}
