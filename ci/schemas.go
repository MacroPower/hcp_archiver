package main

import (
	"dagger/ci/internal/dagger"
)

// Schemas assembles the GitHub Pages site directory published on release:
// the config JSON schema, regenerated from source, laid out under schemas/
// so it serves at <pages-base>/schemas/config.schema.json.
func (m *Ci) Schemas() *dagger.Directory {
	schema := m.env().
		WithExec([]string{"devbox", "run", "--", "go", "generate", "./config"}).
		File("config/config.schema.json")

	return dag.Directory().WithFile("schemas/config.schema.json", schema)
}
