package export

import (
	"path"
	"strconv"

	"go.jacobcolvin.com/hcp_archiver/view"
)

// renderStack writes one stack's page: its allowlisted settings and its
// deployments. Stacks are never sealed, so every read is a plain org-relative
// one.
func (e *Exporter) renderStack(org *view.Org, project, name string) error {
	stackDir := path.Join("projects", project, "stacks", name)

	p := &page{}

	r := one(decodeTolerant(org.ReadFile(path.Join(stackDir, "stack.json"))))

	var kv [][2]string

	if r != nil {
		kv = [][2]string{
			{labelID, escapeCell(r.ID)},
			{labelDescription, escapeCell(r.String("description"))},
			{labelCreated, fmtTime(r.Time("created-at"))},
			{"Updated", fmtTime(r.Time("updated-at"))},
		}
	}

	deployments := subdirNames(org, path.Join(stackDir, "deployments"))

	if len(deployments) > 0 {
		kv = append(kv, [2]string{"Deployments", strconv.Itoa(len(deployments))})
	}

	p.h1(titleOr(r, name))
	p.kv(kv)

	if len(deployments) > 0 {
		rows := make([][]string, 0, len(deployments))

		for _, dep := range deployments {
			row := []string{escapeCell(dep), ""}

			rel := path.Join(stackDir, "deployments", dep, "deployment.json")
			if dep := one(decodeTolerant(org.ReadFile(rel))); dep != nil {
				row[1] = fmtTime(dep.Time("created-at"))
			}

			rows = append(rows, row)
		}

		p.h2("Deployments")
		p.table([]string{"Deployment", labelCreated}, rows)
	}

	return e.write(path.Join(org.Name, stackDir, indexFile), p.bytes())
}
