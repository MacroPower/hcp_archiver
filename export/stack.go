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

	r := one(decodeTolerant(org.ReadFile(path.Join(stackDir, "stack.json"))))

	pageCtx := StackPage{Title: titleOr(r, name)}

	if r != nil {
		pageCtx.Info = addKV(pageCtx.Info, labelID, r.ID)
		pageCtx.Info = addKV(pageCtx.Info, labelDescription, r.String("description"))
		pageCtx.Info = addKV(pageCtx.Info, labelCreated, fmtTime(r.Time("created-at")))
		pageCtx.Info = addKV(pageCtx.Info, "Updated", fmtTime(r.Time("updated-at")))
	}

	deployments := subdirNames(org, path.Join(stackDir, "deployments"))

	if len(deployments) > 0 {
		pageCtx.Info = addKV(pageCtx.Info, "Deployments", strconv.Itoa(len(deployments)))
	}

	for _, dep := range deployments {
		row := Deployment{Name: dep}

		rel := path.Join(stackDir, "deployments", dep, "deployment.json")
		if depRes := one(decodeTolerant(org.ReadFile(rel))); depRes != nil {
			row.Created = fmtTime(depRes.Time("created-at"))
		}

		pageCtx.Deployments = append(pageCtx.Deployments, row)
	}

	data, err := e.renderPage("stack.md.tmpl", pageCtx)
	if err != nil {
		return err
	}

	return e.write(path.Join(org.Name, stackDir, indexFile), data)
}
