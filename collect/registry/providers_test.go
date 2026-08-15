package registry_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/jsonapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tfe "github.com/hashicorp/go-tfe/v2"

	"go.jacobcolvin.com/hcp_archiver/collect"
	"go.jacobcolvin.com/hcp_archiver/collect/registry"
	"go.jacobcolvin.com/hcp_archiver/manifest"
	"go.jacobcolvin.com/hcp_archiver/store"
	"go.jacobcolvin.com/hcp_archiver/tfeclient"
)

const (
	providerOrg       = "acme"
	providerNamespace = "acme"
	providerName      = "aws"
	providerVersion   = "1.0.0"
)

// providerFixture drives the provider-detail archive against a fake registry
// endpoint whose one private version's publish state the test flips between
// runs, with a real store and ledger reused across the runs so the second run
// sees exactly what a re-run would.
type providerFixture struct {
	collector *registry.Collector
	store     *store.Store

	mu        sync.Mutex
	sigDone   bool
	platforms []*tfe.RegistryProviderPlatform
}

// newProviderFixture serves one private provider with one version whose
// signature flag and platform list are read from the fixture state under the
// lock, so a run reflects whatever the test last set.
func newProviderFixture(t *testing.T) *providerFixture {
	t.Helper()

	f := &providerFixture{}

	const base = "/api/v2/organizations/" + providerOrg + "/registry-providers"

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/ping", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc(base, func(w http.ResponseWriter, _ *http.Request) {
		writeJSONAPIList(t, w, []*tfe.RegistryProvider{{
			ID:           "prov-1",
			Name:         providerName,
			Namespace:    providerNamespace,
			RegistryName: tfe.PrivateRegistry,
		}})
	})
	mux.HandleFunc(base+"/private/"+providerNamespace+"/"+providerName+"/versions",
		func(w http.ResponseWriter, _ *http.Request) {
			sig, _ := f.state()

			writeJSONAPIList(t, w, []*tfe.RegistryProviderVersion{{
				ID:                 "pv-1",
				Version:            providerVersion,
				ShasumsUploaded:    true,
				ShasumsSigUploaded: sig,
			}})
		})
	mux.HandleFunc(base+"/private/"+providerNamespace+"/"+providerName+"/versions/"+providerVersion+"/platforms",
		func(w http.ResponseWriter, _ *http.Request) {
			_, platforms := f.state()

			if platforms == nil {
				platforms = []*tfe.RegistryProviderPlatform{}
			}

			writeJSONAPIList(t, w, platforms)
		})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client, err := tfeclient.New(tfeclient.WithToken("test-token"), tfeclient.WithAddress(srv.URL))
	require.NoError(t, err)

	st := store.New(t.TempDir())

	ledger, err := manifest.Load(st.Root())
	require.NoError(t, err)

	ledger.StartRun()

	// A zero confirm delay keeps the 404-confirming re-probe from sleeping.
	env := collect.NewEnv(client, st, ledger, collect.WithAbsentConfirm(0))

	f.collector = registry.New(env, providerOrg, registry.WithDetail(true))
	f.store = st

	return f
}

// setState fixes the version's signature flag and platform list served by the
// next run.
func (f *providerFixture) setState(sigDone bool, platforms []*tfe.RegistryProviderPlatform) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.sigDone = sigDone
	f.platforms = platforms
}

// state reads the version's signature flag and platform list under the lock.
func (f *providerFixture) state() (bool, []*tfe.RegistryProviderPlatform) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.sigDone, f.platforms
}

// versionAttrs decodes the archived version file's jsonapi attributes.
func (f *providerFixture) versionAttrs(t *testing.T) map[string]any {
	t.Helper()

	relPath := f.store.RegistryProviderFile(
		string(tfe.PrivateRegistry),
		providerNamespace,
		providerName,
		"version-"+providerVersion+".json",
	)

	raw, err := os.ReadFile(f.store.AbsPath(relPath))
	require.NoError(t, err, "%s should be written", relPath)

	var doc struct {
		Data struct {
			Attributes map[string]any `json:"attributes"`
		} `json:"data"`
	}

	require.NoError(t, json.Unmarshal(raw, &doc))

	return doc.Data.Attributes
}

// platformCount returns how many platforms the archived platform file records.
func (f *providerFixture) platformCount(t *testing.T) int {
	t.Helper()

	relPath := f.store.RegistryProviderFile(
		string(tfe.PrivateRegistry),
		providerNamespace,
		providerName,
		"platforms-"+providerVersion+".json",
	)

	raw, err := os.ReadFile(f.store.AbsPath(relPath))
	require.NoError(t, err, "%s should be written", relPath)

	var doc struct {
		Data []json.RawMessage `json:"data"`
	}

	require.NoError(t, json.Unmarshal(raw, &doc))

	return len(doc.Data)
}

// versionModTime reports the archived version file's modification time, the
// signal a rewrite would move.
func (f *providerFixture) versionModTime(t *testing.T) time.Time {
	t.Helper()

	relPath := f.store.RegistryProviderFile(
		string(tfe.PrivateRegistry),
		providerNamespace,
		providerName,
		"version-"+providerVersion+".json",
	)

	info, err := os.Stat(f.store.AbsPath(relPath))
	require.NoError(t, err)

	return info.ModTime()
}

// writeJSONAPIList writes models as a JSON:API list document.
func writeJSONAPIList(t *testing.T, w http.ResponseWriter, models any) {
	t.Helper()

	w.Header().Set("Content-Type", "application/vnd.api+json")

	require.NoError(t, jsonapi.MarshalPayload(w, models))
}

func TestCollectProvidersRefreshesAnInFlightVersion(t *testing.T) {
	t.Parallel()

	f := newProviderFixture(t)

	// Run 1: the version is still publishing -- its signature is not yet
	// uploaded and no platform has landed.
	f.setState(false, nil)
	require.NoError(t, f.collector.CollectProviders(t.Context()))

	require.Equal(t, false, f.versionAttrs(t)["shasums-sig-uploaded"],
		"the in-flight version is captured unsigned on the first run")
	require.Zero(t, f.platformCount(t))

	// Run 2: publication completed -- the signature flipped and a platform was
	// added after the version first listed. Mutable re-reads both; Object would
	// freeze the run-1 snapshot forever.
	f.setState(true, []*tfe.RegistryProviderPlatform{{
		ID: "plat-1", OS: "linux", Arch: "amd64", ProviderBinaryUploaded: true,
	}})
	require.NoError(t, f.collector.CollectProviders(t.Context()))

	assert.Equal(t, true, f.versionAttrs(t)["shasums-sig-uploaded"],
		"the mid-flight version is refreshed once it finishes publishing")
	assert.Equal(t, 1, f.platformCount(t),
		"a platform added after the version listed is captured on the next run")
}

func TestCollectProvidersDoesNotRewriteAnUnchangedVersion(t *testing.T) {
	t.Parallel()

	f := newProviderFixture(t)
	f.setState(true, []*tfe.RegistryProviderPlatform{{
		ID: "plat-1", OS: "linux", Arch: "amd64", ProviderBinaryUploaded: true,
	}})

	require.NoError(t, f.collector.CollectProviders(t.Context()))

	before := f.versionModTime(t)

	// A second run over identical published state re-reads but must not rewrite
	// the byte-identical file.
	require.NoError(t, f.collector.CollectProviders(t.Context()))

	assert.Equal(t, before, f.versionModTime(t),
		"Mutable skips the on-disk write when the payload is unchanged")
}

func TestCollectProvidersCancellationPropagates(t *testing.T) {
	t.Parallel()

	f := newProviderFixture(t)
	f.setState(true, nil)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := f.collector.CollectProviders(ctx)
	require.ErrorIs(t, err, context.Canceled)
}
