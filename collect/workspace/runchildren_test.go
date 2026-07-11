package workspace_test

import (
	"io"
	"net/http"
	"os"
	"testing"

	"github.com/hashicorp/go-tfe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.jacobcolvin.com/hcp_archiver/manifest"
)

func TestArchiveConfigurationVersionWithIngress(t *testing.T) {
	t.Parallel()

	cv := &tfe.ConfigurationVersion{
		ID:     "cv-1",
		Status: tfe.ConfigurationUploaded,
		IngressAttributes: &tfe.IngressAttributes{
			ID:        "ia-1",
			Branch:    "main",
			CommitSHA: "abc123",
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/configuration-versions/cv-1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONAPI(t, w, marshalJSONAPI(t, cv))
	})

	f := newWSFixture(t, mux)
	st := f.store

	// The tarball is deduped org-wide and downloaded separately; settle it so the
	// test isolates on the record read and its split into two files.
	f.preSettle(st.ConfigVersionTarball("cv-1"))

	run := &tfe.Run{ID: "run-1", ConfigurationVersion: &tfe.ConfigurationVersion{ID: "cv-1"}}
	require.NoError(t, f.collector.ArchiveConfigurationVersion(t.Context(), "proj", "ws", run))

	f.attrs(t, st.RunFile("proj", "ws", "run-1", "config-version.json"), "configuration-versions", "cv-1")

	ingPath := st.RunFile("proj", "ws", "run-1", "config-version-ingress.json")
	ing := f.attrs(t, ingPath, "ingress-attributes", "ia-1")
	assert.Equal(t, "abc123", ing["commit-sha"])
	assert.Equal(t, "main", ing["branch"])
}

func TestArchiveConfigurationVersionVCSLess(t *testing.T) {
	t.Parallel()

	// A VCS-less upload has no ingress attributes: the record is archived and the
	// ingress path settles a not-applicable gap rather than a null file.
	cv := &tfe.ConfigurationVersion{ID: "cv-2", Status: tfe.ConfigurationUploaded}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/configuration-versions/cv-2", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONAPI(t, w, marshalJSONAPI(t, cv))
	})

	f := newWSFixture(t, mux)
	st := f.store

	f.preSettle(st.ConfigVersionTarball("cv-2"))

	run := &tfe.Run{ID: "run-2", ConfigurationVersion: &tfe.ConfigurationVersion{ID: "cv-2"}}
	require.NoError(t, f.collector.ArchiveConfigurationVersion(t.Context(), "proj", "ws", run))

	f.attrs(t, st.RunFile("proj", "ws", "run-2", "config-version.json"), "configuration-versions", "cv-2")

	ingPath := st.RunFile("proj", "ws", "run-2", "config-version-ingress.json")
	assert.Equal(t, manifest.StatusNotApplicable, f.status(ingPath))

	exists, err := st.Exists(ingPath)
	require.NoError(t, err)
	assert.False(t, exists, "a VCS-less config version writes no ingress file")
}

func TestArchiveConfigurationVersionRetryGate(t *testing.T) {
	t.Parallel()

	cv := &tfe.ConfigurationVersion{
		ID:                "cv-3",
		IngressAttributes: &tfe.IngressAttributes{ID: "ia-3", CommitSHA: "def456"},
	}

	var hits int

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/configuration-versions/cv-3", func(w http.ResponseWriter, _ *http.Request) {
		hits++

		writeJSONAPI(t, w, marshalJSONAPI(t, cv))
	})

	f := newWSFixture(t, mux)
	st := f.store

	cvPath := st.RunFile("proj", "ws", "run-3", "config-version.json")
	ingPath := st.RunFile("proj", "ws", "run-3", "config-version-ingress.json")

	// The record settled on an earlier pass but its ingress write did not: the
	// read must still happen because the gate opens on either derived file, and
	// the already-settled record must not regress.
	f.preSettle(cvPath)
	f.preSettle(st.ConfigVersionTarball("cv-3"))

	run := &tfe.Run{ID: "run-3", ConfigurationVersion: &tfe.ConfigurationVersion{ID: "cv-3"}}
	require.NoError(t, f.collector.ArchiveConfigurationVersion(t.Context(), "proj", "ws", run))

	assert.Equal(t, 1, hits, "the record is re-read while the ingress gap remains")
	f.attrs(t, ingPath, "ingress-attributes", "ia-3")
	assert.Equal(t, manifest.StatusDone, f.status(cvPath), "the settled record is left untouched")
}

func TestArchiveConfigurationVersionTarball(t *testing.T) {
	t.Parallel()

	// The tarball is streamed from its download endpoint, not preSettled: this
	// locks in byte-identity for the newly-streamed blob end to end.
	const tarball = "\x1f\x8b\x08 fake tar.gz bytes \x00\x01\x02"

	cv := &tfe.ConfigurationVersion{ID: "cv-1", Status: tfe.ConfigurationUploaded}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/configuration-versions/cv-1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONAPI(t, w, marshalJSONAPI(t, cv))
	})
	mux.HandleFunc("/api/v2/configuration-versions/cv-1/download", func(w http.ResponseWriter, _ *http.Request) {
		_, werr := io.WriteString(w, tarball)
		if werr != nil {
			return
		}
	})

	f := newWSFixture(t, mux)
	st := f.store

	run := &tfe.Run{ID: "run-1", ConfigurationVersion: &tfe.ConfigurationVersion{ID: "cv-1"}}
	require.NoError(t, f.collector.ArchiveConfigurationVersion(t.Context(), "proj", "ws", run))

	tarballPath := st.ConfigVersionTarball("cv-1")
	assert.Equal(t, manifest.StatusDone, f.status(tarballPath))

	got, err := os.ReadFile(st.AbsPath(tarballPath))
	require.NoError(t, err)
	assert.Equal(t, tarball, string(got), "the tarball streams to disk byte-identically")
}

func TestArchiveConfigurationVersionTarballAbsent(t *testing.T) {
	t.Parallel()

	// An expired configuration version's tarball 404s on a normal lifecycle. The
	// streaming DoRaw path discards the SDK's typed error, so this guards that the
	// status translation still classifies the 404 terminal: the object must settle
	// absent, never re-fetched and re-errored on every later run.
	cv := &tfe.ConfigurationVersion{ID: "cv-1", Status: tfe.ConfigurationUploaded}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/configuration-versions/cv-1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONAPI(t, w, marshalJSONAPI(t, cv))
	})
	mux.HandleFunc("/api/v2/configuration-versions/cv-1/download", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	f := newWSFixture(t, mux)
	st := f.store

	run := &tfe.Run{ID: "run-1", ConfigurationVersion: &tfe.ConfigurationVersion{ID: "cv-1"}}
	require.NoError(t, f.collector.ArchiveConfigurationVersion(t.Context(), "proj", "ws", run))

	tarballPath := st.ConfigVersionTarball("cv-1")
	assert.Equal(t, manifest.StatusAbsentPermanently, f.status(tarballPath))

	exists, err := st.Exists(tarballPath)
	require.NoError(t, err)
	assert.False(t, exists, "an absent tarball writes no file")
}

func TestArchivePlanJSON(t *testing.T) {
	t.Parallel()

	// The structured plan is streamed from json-output, not preSettled, locking in
	// byte-identity for the newly-streamed blob end to end. The embedded NUL proves
	// the stream is byte-exact, arbitrary bytes and all, rather than re-encoded.
	const planBody = "structured plan output\x00\x01 bytes"

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/plans/plan-1/json-output", func(w http.ResponseWriter, _ *http.Request) {
		_, werr := io.WriteString(w, planBody)
		if werr != nil {
			return
		}
	})

	f := newWSFixture(t, mux)
	st := f.store

	// Settle the log so the test isolates on the structured JSON stream.
	f.preSettle(st.RunFile("proj", "ws", "run-1", "plan.log"))

	run := &tfe.Run{ID: "run-1", Plan: &tfe.Plan{ID: "plan-1"}}
	require.NoError(t, f.collector.ArchivePlan(t.Context(), "proj", "ws", run))

	jsonPath := st.RunFile("proj", "ws", "run-1", "plan.json")
	assert.Equal(t, manifest.StatusDone, f.status(jsonPath))

	got, err := os.ReadFile(st.AbsPath(jsonPath))
	require.NoError(t, err)
	assert.Equal(t, planBody, string(got), "the structured plan streams to disk byte-identically")
}

func TestArchivePlanJSONAbsent(t *testing.T) {
	t.Parallel()

	// A plan whose structured output has expired 404s; the object must settle
	// absent rather than re-fetch and re-error every run (the section-3 regression
	// guard for the plan.json blob).
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/plans/plan-1/json-output", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	f := newWSFixture(t, mux)
	st := f.store

	f.preSettle(st.RunFile("proj", "ws", "run-1", "plan.log"))

	run := &tfe.Run{ID: "run-1", Plan: &tfe.Plan{ID: "plan-1"}}
	require.NoError(t, f.collector.ArchivePlan(t.Context(), "proj", "ws", run))

	jsonPath := st.RunFile("proj", "ws", "run-1", "plan.json")
	assert.Equal(t, manifest.StatusAbsentPermanently, f.status(jsonPath))

	exists, err := st.Exists(jsonPath)
	require.NoError(t, err)
	assert.False(t, exists, "an absent structured plan writes no file")
}

func TestArchivePlanSummary(t *testing.T) {
	t.Parallel()

	f := newWSFixture(t, http.NewServeMux())
	st := f.store

	// The plan summary is value-in-hand; settle the heavy log and JSON so no fetch
	// is attempted and the test isolates on the summary write.
	f.preSettle(st.RunFile("proj", "ws", "run-1", "plan.log"))
	f.preSettle(st.RunFile("proj", "ws", "run-1", "plan.json"))

	run := &tfe.Run{
		ID:   "run-1",
		Plan: &tfe.Plan{ID: "plan-1", HasChanges: true, ResourceAdditions: 3, ResourceDestructions: 1},
	}
	require.NoError(t, f.collector.ArchivePlan(t.Context(), "proj", "ws", run))

	attrs := f.attrs(t, st.RunFile("proj", "ws", "run-1", "plan-summary.json"), "plans", "plan-1")
	assert.Equal(t, true, attrs["has-changes"])
	assert.InDelta(t, 3, attrs["resource-additions"], 0)
	assert.InDelta(t, 1, attrs["resource-destructions"], 0)
}

func TestArchivePlanLog(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/plans/plan-1", func(w http.ResponseWriter, r *http.Request) {
		writeJSONAPI(t, w, marshalJSONAPI(t, &tfe.Plan{
			ID:         "plan-1",
			LogReadURL: "http://" + r.Host + "/artifact/plan-1-log",
		}))
	})
	mux.HandleFunc("/artifact/plan-1-log", func(w http.ResponseWriter, _ *http.Request) {
		_, werr := io.WriteString(w, "\x02plan output\x03")
		if werr != nil {
			return
		}
	})

	f := newWSFixture(t, mux)
	st := f.store

	// Settle the structured JSON so the test isolates on the log download.
	f.preSettle(st.RunFile("proj", "ws", "run-1", "plan.json"))

	run := &tfe.Run{ID: "run-1", Plan: &tfe.Plan{ID: "plan-1"}}
	require.NoError(t, f.collector.ArchivePlan(t.Context(), "proj", "ws", run))

	logPath := st.RunFile("proj", "ws", "run-1", "plan.log")
	assert.Equal(t, manifest.StatusDone, f.status(logPath))

	got, err := os.ReadFile(st.AbsPath(logPath))
	require.NoError(t, err)
	assert.Equal(t, "plan output", string(got), "the log downloads whole with its STX/ETX framing trimmed")
}

func TestArchiveApplyLog(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/applies/apply-1", func(w http.ResponseWriter, r *http.Request) {
		writeJSONAPI(t, w, marshalJSONAPI(t, &tfe.Apply{
			ID:         "apply-1",
			LogReadURL: "http://" + r.Host + "/artifact/apply-1-log",
		}))
	})
	mux.HandleFunc("/artifact/apply-1-log", func(w http.ResponseWriter, _ *http.Request) {
		_, werr := io.WriteString(w, "\x02apply output\x03")
		if werr != nil {
			return
		}
	})

	f := newWSFixture(t, mux)
	st := f.store

	run := &tfe.Run{ID: "run-1", Apply: &tfe.Apply{ID: "apply-1"}}
	require.NoError(t, f.collector.ArchiveApply(t.Context(), "proj", "ws", run))

	logPath := st.RunFile("proj", "ws", "run-1", "apply.log")
	assert.Equal(t, manifest.StatusDone, f.status(logPath))

	got, err := os.ReadFile(st.AbsPath(logPath))
	require.NoError(t, err)
	assert.Equal(t, "apply output", string(got), "the log downloads whole with its STX/ETX framing trimmed")
}

func TestArchiveApplySummary(t *testing.T) {
	t.Parallel()

	f := newWSFixture(t, http.NewServeMux())
	st := f.store

	f.preSettle(st.RunFile("proj", "ws", "run-1", "apply.log"))

	run := &tfe.Run{
		ID:    "run-1",
		Apply: &tfe.Apply{ID: "apply-1", ResourceAdditions: 5, ResourceChanges: 2},
	}
	require.NoError(t, f.collector.ArchiveApply(t.Context(), "proj", "ws", run))

	attrs := f.attrs(t, st.RunFile("proj", "ws", "run-1", "apply-summary.json"), "applies", "apply-1")
	assert.InDelta(t, 5, attrs["resource-additions"], 0)
	assert.InDelta(t, 2, attrs["resource-changes"], 0)
}

func TestArchiveTFPolicyOutcomes(t *testing.T) {
	t.Parallel()

	hydrated := &tfe.Run{
		ID:                  "run-1",
		TFPolicyEvaluations: []*tfe.TFPolicyEvaluation{{ID: "eval-1", Status: tfe.TFPolicyEvaluationStatusPassed}},
	}
	outcome := &tfe.TFPolicySetOutcome{ID: "out-1", PolicySetName: "prod-guardrails"}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/runs/run-1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONAPI(t, w, marshalJSONAPI(t, hydrated))
	})
	mux.HandleFunc("/api/v2/tf-policy-evaluations/eval-1/tf-policy-set-outcomes",
		func(w http.ResponseWriter, _ *http.Request) {
			writeJSONAPI(t, w, marshalJSONAPI(t, []*tfe.TFPolicySetOutcome{outcome}))
		})

	f := newWSFixture(t, mux)
	st := f.store

	require.NoError(t, f.collector.ArchiveTFPolicyOutcomes(t.Context(), "proj", "ws", &tfe.Run{ID: "run-1"}))

	// The hydrated evaluations are archived as their own file, not dropped as bare
	// id refs, alongside their aggregated set outcomes.
	assert.Equal(t, []string{"eval-1"}, f.dataIDs(t, st.RunFile("proj", "ws", "run-1", "tf-policy-evaluations.json")))
	assert.Equal(t, []string{"out-1"}, f.dataIDs(t, st.RunFile("proj", "ws", "run-1", "tf-policy-outcomes.json")))
}

func TestArchiveTFPolicyOutcomesNoEvaluations(t *testing.T) {
	t.Parallel()

	// The common no-policy run has no native evaluations: both files settle a
	// not-applicable gap rather than writing an empty {"data":[]} outcomes list.
	hydrated := &tfe.Run{ID: "run-2"}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/runs/run-2", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONAPI(t, w, marshalJSONAPI(t, hydrated))
	})

	f := newWSFixture(t, mux)
	st := f.store

	require.NoError(t, f.collector.ArchiveTFPolicyOutcomes(t.Context(), "proj", "ws", &tfe.Run{ID: "run-2"}))

	evalPath := st.RunFile("proj", "ws", "run-2", "tf-policy-evaluations.json")
	outPath := st.RunFile("proj", "ws", "run-2", "tf-policy-outcomes.json")

	assert.Equal(t, manifest.StatusNotApplicable, f.status(evalPath))
	assert.Equal(t, manifest.StatusNotApplicable, f.status(outPath))

	for _, p := range []string{evalPath, outPath} {
		exists, err := st.Exists(p)
		require.NoError(t, err)
		assert.False(t, exists, "%s should not be written for a no-policy run", p)
	}
}
