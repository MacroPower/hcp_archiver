package archiver_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/hashicorp/go-tfe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/archiver"
	"go.jacobcolvin.com/hcp_archiver/collect"
	"go.jacobcolvin.com/hcp_archiver/config"
	"go.jacobcolvin.com/hcp_archiver/manifest"
	"go.jacobcolvin.com/hcp_archiver/remote"
	"go.jacobcolvin.com/hcp_archiver/remote/remotetest"
	"go.jacobcolvin.com/hcp_archiver/store"
)

func TestLogFailures(t *testing.T) {
	t.Parallel()

	ledger, err := manifest.Load(t.TempDir())
	require.NoError(t, err)

	ledger.StartRun()
	ledger.RecordDone("ok.json", manifest.Signature{Size: 1})

	longErr := "list github app installations: list github app installations: " +
		"bad request no github app oauth token, the token must be created by the github app owner"
	ledger.RecordErrored("github-app-installations.json", errors.New(longErr), false)

	buf := &bytes.Buffer{}
	a := archiver.New(
		&config.Config{},
		archiver.WithLogger(slog.New(slog.NewTextHandler(buf, nil))),
	)

	archiver.LogFailures(a, t.Context(), "acme", ledger)

	out := buf.String()
	assert.Contains(t, out, "object_archive_error")
	assert.Contains(t, out, "path=github-app-installations.json")
	assert.Contains(t, out, "status=errored")
	assert.Contains(t, out, longErr, "the full error text survives, untruncated")
}

func TestOrgIncomplete(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		tally manifest.Tally
		want  bool
	}{
		"empty org is complete": {
			tally: manifest.Tally{},
			want:  false,
		},
		"clean run is complete": {
			tally: manifest.Tally{Done: 10},
			want:  false,
		},
		"partial per-object failures stay complete": {
			tally: manifest.Tally{Done: 10, Errored: 2, Forbidden: 1},
			want:  false,
		},
		"wholly failed org is incomplete": {
			tally: manifest.Tally{Errored: 3},
			want:  true,
		},
		"forbidden-only org is incomplete": {
			tally: manifest.Tally{Forbidden: 1},
			want:  true,
		},
		"a dropped surface is incomplete even with work done": {
			tally: manifest.Tally{Done: 100, SurfacesDropped: 1},
			want:  true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, archiver.OrgIncomplete(tc.tally))
		})
	}
}

func TestNew(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Token:        "secret",
		Address:      config.DefaultAddress,
		OutputDir:    t.TempDir(),
		ProgressMode: config.ProgressModeQuiet,
	}

	buf := &bytes.Buffer{}

	a := archiver.New(cfg, archiver.WithWriter(buf))
	require.NotNil(t, a)
}

func TestRunInvalidConfig(t *testing.T) {
	t.Parallel()

	// A config missing its output directory fails validation before any I/O.
	cfg := &config.Config{
		Token:        "secret",
		Address:      config.DefaultAddress,
		ProgressMode: config.ProgressModeQuiet,
	}

	a := archiver.New(cfg, archiver.WithWriter(&bytes.Buffer{}))

	err := a.Run(t.Context())
	require.ErrorIs(t, err, config.ErrMissingOutputDir)
}

func TestResolveOrgs(t *testing.T) {
	t.Parallel()

	errList := errors.New("list called")

	tests := map[string]struct {
		orgs      []string
		listOrgs  []string
		listErr   error
		want      []string
		wantErr   error
		wantCalls int
	}{
		"named orgs skip listing": {
			orgs:      []string{"acme", "globex"},
			listErr:   errList,
			want:      []string{"acme", "globex"},
			wantCalls: 0,
		},
		"empty list enumerates every visible org": {
			orgs:      nil,
			listOrgs:  []string{"one", "two"},
			want:      []string{"one", "two"},
			wantCalls: 1,
		},
		"list error propagates": {
			orgs:      nil,
			listErr:   errList,
			wantErr:   errList,
			wantCalls: 1,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			calls := 0
			list := func(_ context.Context) ([]string, error) {
				calls++

				return tc.listOrgs, tc.listErr
			}

			got, err := archiver.ResolveOrgs(t.Context(), tc.orgs, list)

			assert.Equal(t, tc.wantCalls, calls)

			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestProjectNameFor(t *testing.T) {
	t.Parallel()

	names := map[string]string{
		"prj-1": "networking",
		"prj-2": "",
	}

	tests := map[string]struct {
		ws   *tfe.Workspace
		want string
	}{
		"resolves from the map by id": {
			ws:   &tfe.Workspace{Project: &tfe.Project{ID: "prj-1"}},
			want: "networking",
		},
		"nil project falls back to default": {
			ws:   &tfe.Workspace{Project: nil},
			want: archiver.DefaultProjectName,
		},
		"unknown id falls back to the relation name": {
			ws:   &tfe.Workspace{Project: &tfe.Project{ID: "prj-9", Name: "hydrated"}},
			want: "hydrated",
		},
		"blank map entry falls back to the relation name": {
			ws:   &tfe.Workspace{Project: &tfe.Project{ID: "prj-2", Name: "from-relation"}},
			want: "from-relation",
		},
		"unknown id and no relation name falls back to default": {
			ws:   &tfe.Workspace{Project: &tfe.Project{ID: "prj-9"}},
			want: archiver.DefaultProjectName,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := archiver.ProjectNameFor(names, tc.ws)
			assert.Equal(t, tc.want, got)
		})
	}
}

// newSyncOrgFixture builds an archiver with an injected fake-backed remote
// client plus a collect environment over a temp store, so the close sweep can
// be driven directly.
func newSyncOrgFixture(t *testing.T, buf *bytes.Buffer) (*archiver.Archiver, *collect.Env, *remotetest.Fake) {
	t.Helper()

	root := t.TempDir()
	st := store.New(root)

	ledger, err := manifest.Load(root)
	require.NoError(t, err)

	fake := remotetest.New()
	cfg := remote.Config{Prefix: "hcp"}

	client, err := remote.New(t.Context(), cfg,
		remote.WithBucket(fake.Bucket()), remote.WithRetry(0, 0))
	require.NoError(t, err)

	logger := slog.New(slog.NewTextHandler(buf, nil))

	env := collect.NewEnv(nil, st, ledger,
		collect.WithRemote(client, cfg, "acme"),
		collect.WithLogger(logger),
	)

	_, err = st.WriteBytes("org.json", []byte(`{"org":"acme"}`))
	require.NoError(t, err)

	a := archiver.New(
		&config.Config{Remote: &config.RemoteConfig{Prefix: "hcp"}},
		archiver.WithLogger(logger),
	)
	a.SetRemote(client)

	return a, env, fake
}

func TestSyncOrgCanceledContextSkipsSweep(t *testing.T) {
	t.Parallel()

	buf := &bytes.Buffer{}
	a, env, fake := newSyncOrgFixture(t, buf)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	archiver.SyncOrg(a, ctx, env, "acme")

	assert.Empty(t, fake.Keys(), "an interrupted run uploads nothing; the next run sweeps")
	assert.NotContains(t, buf.String(), "remote_sync_complete")
}

func TestSyncOrgFailureWarnsAndReportsFailed(t *testing.T) {
	t.Parallel()

	buf := &bytes.Buffer{}
	a, env, fake := newSyncOrgFixture(t, buf)
	fake.PutErr = assert.AnError

	stats := archiver.SyncOrg(a, t.Context(), env, "acme")

	assert.Equal(t, 1, stats.Failed,
		"the sweep's failures come back to the run loop, which marks the run incomplete")

	out := buf.String()
	assert.Contains(t, out, "remote_sync_complete")
	assert.Contains(t, out, "level=WARN")
	assert.Contains(t, out, "failed=1")
}

func TestSyncOrgLogsSummary(t *testing.T) {
	t.Parallel()

	buf := &bytes.Buffer{}
	a, env, fake := newSyncOrgFixture(t, buf)

	archiver.SyncOrg(a, t.Context(), env, "acme")

	assert.Contains(t, fake.Keys(), "hcp/acme/org.json")

	out := buf.String()
	assert.Contains(t, out, "remote_sync_complete")
	assert.Contains(t, out, "uploaded=1")
	assert.Contains(t, out, "eager_failed=0")
}

func TestSyncOrgEagerFailureIsVisibilityOnly(t *testing.T) {
	t.Parallel()

	buf := &bytes.Buffer{}
	a, env, fake := newSyncOrgFixture(t, buf)

	// One eager as-written upload fails; the close sweep retries it cleanly,
	// so the run stays complete (only the sweep's own Failed marks it).
	fake.PutErr = assert.AnError
	fake.PutErrN = 1

	require.NoError(t, env.Bytes(t.Context(), "users/user-1.json",
		func(context.Context) ([]byte, error) { return []byte(`{"id":"user-1"}`), nil }))
	require.Equal(t, 1, env.EagerFailures())

	stats := archiver.SyncOrg(a, t.Context(), env, "acme")

	assert.Zero(t, stats.Failed, "a retried eager failure never marks the run incomplete")
	assert.Contains(t, fake.Keys(), "hcp/acme/users/user-1.json")
	assert.Contains(t, buf.String(), "eager_failed=1")

	// The run-wide tally the reporter renders (via WithRemoteStats) spans both
	// motions: the failed eager upload and the sweep's two retried uploads.
	tally := env.RemoteTally()
	assert.Equal(t, 2, tally.Uploaded)
	assert.Equal(t, 1, tally.Failed)
	assert.Zero(t, tally.Evicted)
	assert.Positive(t, tally.UploadedBytes)
}

func TestWriteRemoteMarkerMirrorsEagerly(t *testing.T) {
	t.Parallel()

	buf := &bytes.Buffer{}
	a, env, fake := newSyncOrgFixture(t, buf)
	st := env.Store()

	require.NoError(t, archiver.WriteRemoteMarker(a, t.Context(), st, "acme"))

	local, err := st.Exists(remote.MarkerName)
	require.NoError(t, err)
	assert.True(t, local, "the marker lands at the archive root")

	_, ok := fake.Object("hcp/acme/" + remote.MarkerName)
	assert.True(t, ok, "the marker mirrors immediately, not at the close sweep")
}

func TestWriteRemoteMarkerMirrorFailureWarnsOnly(t *testing.T) {
	t.Parallel()

	buf := &bytes.Buffer{}
	a, env, fake := newSyncOrgFixture(t, buf)
	st := env.Store()

	fake.PutErr = assert.AnError

	require.NoError(t, archiver.WriteRemoteMarker(a, t.Context(), st, "acme"),
		"a marker mirror failure defers to the close sweep")

	local, err := st.Exists(remote.MarkerName)
	require.NoError(t, err)
	assert.True(t, local, "the local marker write still succeeds")

	assert.Contains(t, buf.String(), "remote_marker_sync_error")
}
