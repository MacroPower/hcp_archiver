package main

import (
	"dagger/ci/internal/dagger"
)

// Schemas assembles the GitHub Pages site directory published on release:
// the config JSON schema, regenerated from source, placed at the site root
// so it serves at <pages-base>/config.schema.json.
func (m *Ci) Schemas() *dagger.Directory {
	schema := m.env().
		WithExec([]string{"devbox", "run", "--", "go", "generate", "./pkg/config"}).
		File("pkg/config/config.schema.json")

	return dag.Directory().WithFile("config.schema.json", schema)
}
