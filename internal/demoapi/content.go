package demoapi

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"strings"

	"github.com/hashicorp/go-tfe"
)

// planLog renders the plan output a run's plan.log holds, ending on whatever
// settled that run: a plan that errored stops on the provider's message, and
// the rest print the usual summary line.
func planLog(ws *workspaceSpec, spec runSpec) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Terraform v%s\non linux_amd64\n\n", ws.tfVersion)
	b.WriteString("Initializing plugins and modules...\n")
	b.WriteString("Terraform used the selected providers to generate the following execution\n")
	b.WriteString("plan. Resource actions are indicated with the following symbols:\n")
	b.WriteString("  + create\n  ~ update in-place\n  - destroy\n\n")
	b.WriteString("Terraform will perform the following actions:\n\n")

	b.WriteString(`  # aws_flow_log.vpc will be updated in-place
  ~ resource "aws_flow_log" "vpc" {
        id                       = "fl-0b41d6c9a7e2f8c31"
      ~ max_aggregation_interval = 600 -> 60
        tags                     = {
            "environment" = "` + environmentOf(ws.name) + `"
        }
        # (8 unchanged attributes hidden)
    }

  # aws_cloudwatch_log_group.flow_logs will be created
  + resource "aws_cloudwatch_log_group" "flow_logs" {
      + arn               = (known after apply)
      + id                = (known after apply)
      + name              = "/vpc/flow-logs/` + ws.name + `"
      + retention_in_days = 90
      + skip_destroy      = false
    }

`)

	if spec.status == tfe.RunErrored {
		b.WriteString("Plan: 1 to add, 1 to change, 0 to destroy.\n\n")
		b.WriteString("Error: creating CloudWatch Logs Log Group (/vpc/flow-logs/" + ws.name + "):\n")
		b.WriteString("operation error CloudWatch Logs: CreateLogGroup, https response error\n")
		b.WriteString("StatusCode: 400, api error InvalidParameterException: Retention period\n")
		b.WriteString("must be one of the allowed values.\n\n")
		b.WriteString("  with aws_cloudwatch_log_group.flow_logs,\n")
		b.WriteString("  on main.tf line 42, in resource \"aws_cloudwatch_log_group\" \"flow_logs\":\n")
		b.WriteString("  42: resource \"aws_cloudwatch_log_group\" \"flow_logs\" {\n")

		return b.String()
	}

	if spec.status == tfe.RunPlannedAndFinished {
		b.WriteString("No changes. Your infrastructure matches the configuration.\n")

		return b.String()
	}

	b.WriteString("Plan: 1 to add, 1 to change, 0 to destroy.\n")

	return b.String()
}

// applyLog renders the apply output a run that applied left behind.
func applyLog(ws *workspaceSpec) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Terraform v%s\non linux_amd64\n\n", ws.tfVersion)
	b.WriteString("aws_flow_log.vpc: Modifying... [id=fl-0b41d6c9a7e2f8c31]\n")
	b.WriteString("aws_cloudwatch_log_group.flow_logs: Creating...\n")
	b.WriteString("aws_cloudwatch_log_group.flow_logs: Creation complete after 2s\n")
	b.WriteString("aws_flow_log.vpc: Modifications complete after 4s [id=fl-0b41d6c9a7e2f8c31]\n\n")
	b.WriteString("Apply complete! Resources: 1 added, 1 changed, 0 destroyed.\n\n")
	b.WriteString("Outputs:\n\n")
	fmt.Fprintf(&b, "flow_log_group = \"/vpc/flow-logs/%s\"\n", ws.name)

	return b.String()
}

// planJSON renders the machine-readable plan a recent Terraform version emits
// beside the log, which is the artifact a later audit actually parses.
func planJSON(ws *workspaceSpec, spec runSpec) string {
	actions := `["update"]`
	if spec.status == tfe.RunPlannedAndFinished {
		actions = `["no-op"]`
	}

	return fmt.Sprintf(`{
  "format_version": "1.2",
  "terraform_version": "%s",
  "resource_changes": [
    {
      "address": "aws_flow_log.vpc",
      "mode": "managed",
      "type": "aws_flow_log",
      "name": "vpc",
      "provider_name": "registry.terraform.io/hashicorp/aws",
      "change": {
        "actions": %s,
        "before": {
          "id": "fl-0b41d6c9a7e2f8c31",
          "max_aggregation_interval": 600
        },
        "after": {
          "id": "fl-0b41d6c9a7e2f8c31",
          "max_aggregation_interval": 60
        }
      }
    },
    {
      "address": "aws_cloudwatch_log_group.flow_logs",
      "mode": "managed",
      "type": "aws_cloudwatch_log_group",
      "name": "flow_logs",
      "provider_name": "registry.terraform.io/hashicorp/aws",
      "change": {
        "actions": ["create"],
        "before": null,
        "after": {
          "name": "/vpc/flow-logs/%s",
          "retention_in_days": 90
        }
      }
    }
  ]
}
`, ws.tfVersion, actions, ws.name)
}

// stateResource is one managed resource a state document records.
type stateResource struct {
	kind string
	name string
	id   string
	cidr string
}

// arn renders the resource's ARN, which names the resource by its short kind.
func (r stateResource) arn() string {
	kind := strings.ReplaceAll(strings.TrimPrefix(r.kind, "aws_"), "_", "-")

	return "arn:aws:ec2:us-east-2:123456789012:" + kind + "/" + r.id
}

// cidrLine renders the resource's cidr_block attribute at the given
// indentation, or nothing for a resource that has no CIDR to record.
func (r stateResource) cidrLine(indent string) string {
	if r.cidr == "" {
		return ""
	}

	return indent + `"cidr_block": "` + r.cidr + `",` + "\n"
}

// stateResourceKinds are the resource types the demo organization's networking
// is built from, cycled through to fill out a state document.
var stateResourceKinds = []string{
	"aws_subnet",
	"aws_route_table",
	"aws_security_group",
	"aws_network_acl",
}

// stateResourceCount is how many networking resources a state document records
// beside the flow log, which gives the document the bulk a real one has: a
// state carrying a single resource would understate both what the browser
// scrolls through and what an extract moves.
const stateResourceCount = 24

// stateResources renders the managed resources a workspace's state holds: the
// flow log every workspace owns, then a run of networking resources whose
// identifiers and CIDRs derive from the workspace name.
func stateResources(ws *workspaceSpec) []stateResource {
	resources := []stateResource{{
		kind: "aws_flow_log",
		name: "vpc",
		id:   "fl-0b41d6c9a7e2f8c31",
	}}

	for i := range stateResourceCount {
		kind := stateResourceKinds[i%len(stateResourceKinds)]
		name := fmt.Sprintf("%s_%02d", strings.TrimPrefix(kind, "aws_"), i/len(stateResourceKinds)+1)

		resources = append(resources, stateResource{
			kind: kind,
			name: name,
			id:   awsID(strings.TrimPrefix(kind, "aws_"), ws.name+"/"+name),
			cidr: fmt.Sprintf("10.%d.%d.0/24", len(ws.name)%256, i),
		})
	}

	return resources
}

// awsID renders a plausible AWS resource identifier: the resource's short kind
// and seventeen hex characters derived from seed.
func awsID(kind, seed string) string {
	digest := hash(kind + ":" + seed)

	return fmt.Sprintf("%s-0%016x", kind, digest)
}

// rawState renders the .tfstate document a state version holds: the document
// whose cleartext values are why the archive is sensitive at rest.
func rawState(ws *workspaceSpec, serial int64) string {
	var b strings.Builder

	fmt.Fprintf(&b, `{
  "version": 4,
  "terraform_version": "%s",
  "serial": %d,
  "lineage": "%s",
  "outputs": {
    "flow_log_group": {
      "value": "/vpc/flow-logs/%s",
      "type": "string"
    }
  },
  "resources": [
`, ws.tfVersion, serial, lineage(ws.name), ws.name)

	for i, r := range stateResources(ws) {
		if i > 0 {
			b.WriteString(",\n")
		}

		fmt.Fprintf(&b, `    {
      "mode": "managed",
      "type": "%s",
      "name": "%s",
      "provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
      "instances": [
        {
          "schema_version": 0,
          "attributes": {
            "id": "%s",
            "arn": "%s",
%s            "vpc_id": "%s",
            "tags": {
              "Name": "%s",
              "environment": "%s"
            }
          }
        }
      ]
    }`, r.kind, r.name, r.id, r.arn(), r.cidrLine("            "), awsID("vpc", ws.name), r.name, environmentOf(ws.name))
	}

	b.WriteString("\n  ]\n}\n")

	return b.String()
}

// jsonState renders the JSON-format state the platform stores beside the raw
// document: the same resources, rendered for machines rather than for
// Terraform.
func jsonState(ws *workspaceSpec, serial int64) string {
	var b strings.Builder

	fmt.Fprintf(&b, `{
  "format_version": "1.0",
  "terraform_version": "%s",
  "serial": %d,
  "values": {
    "outputs": {
      "flow_log_group": {
        "sensitive": false,
        "value": "/vpc/flow-logs/%s"
      }
    },
    "root_module": {
      "resources": [
`, ws.tfVersion, serial, ws.name)

	for i, r := range stateResources(ws) {
		if i > 0 {
			b.WriteString(",\n")
		}

		fmt.Fprintf(&b, `        {
          "address": "%s.%s",
          "mode": "managed",
          "type": "%s",
          "name": "%s",
          "provider_name": "registry.terraform.io/hashicorp/aws",
          "values": {
            "id": "%s",
            "arn": "%s",
%s            "vpc_id": "%s",
            "tags": {
              "Name": "%s",
              "environment": "%s"
            }
          }
        }`, r.kind, r.name, r.kind, r.name, r.id, r.arn(), r.cidrLine("            "),
			awsID("vpc", ws.name), r.name, environmentOf(ws.name))
	}

	b.WriteString("\n      ]\n    }\n  }\n}\n")

	return b.String()
}

// lineage renders the UUID a state's lineage carries, derived from the
// workspace name so every version of one workspace's state shares it.
func lineage(ws string) string {
	digest := hash("lineage:" + ws)

	var b strings.Builder

	for b.Len() < 32 {
		fmt.Fprintf(&b, "%016x", digest)

		digest = digest*6364136223846793005 + 1442695040888963407
	}

	hex := b.String()

	return hex[0:8] + "-" + hex[8:12] + "-" + hex[12:16] + "-" + hex[16:20] + "-" + hex[20:32]
}

// readme renders the workspace overview the platform serves as raw markdown,
// which the archiver stores as readme.md.
func readme(ws *workspaceSpec) string {
	return fmt.Sprintf(`# %s

%s

| Setting | Value |
| -- | -- |
| Repository | `+"`%s`"+` |
| Working directory | `+"`%s`"+` |
| Terraform | `+"`%s`"+` |
| Environment | `+"`%s`"+` |

Runs are triggered by merges to `+"`main`"+`; queue one by hand only to
correct drift the scheduled plan did not catch.
`, ws.name, ws.description, ws.repo, ws.dir, ws.tfVersion, environmentOf(ws.name))
}

// policySource renders the Sentinel source a policy's download endpoint serves.
func policySource(name string) string {
	return fmt.Sprintf(`# %s
#
# Every production resource carries an environment tag, so an audit can tell
# what a resource belongs to years after whoever created it has moved on.

import "tfplan/v2" as tfplan

required_tags = ["environment", "owner"]

mandatory_tags = rule {
  all tfplan.resource_changes as _, rc {
    all required_tags as tag {
      rc.change.after.tags contains tag
    }
  }
}

main = rule { mandatory_tags }
`, name)
}

// configTarball renders one workspace's configuration as the gzipped tar the
// platform stores: the Terraform files the run planned from.
func configTarball(ws *workspaceSpec) ([]byte, error) {
	files := map[string]string{
		"main.tf": fmt.Sprintf(`terraform {
  required_version = "~> %s"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.60"
    }
  }
}

module "%s" {
  source = "../../modules/%s"

  environment = var.environment
  tags        = var.tags
}
`, ws.tfVersion, strings.ReplaceAll(ws.name, "-", "_"), strings.SplitN(ws.name, "-", 2)[0]),
		"variables.tf": `variable "environment" {
  type        = string
  description = "The environment this workspace manages."
}

variable "tags" {
  type        = map(string)
  description = "Tags applied to every resource."
  default     = {}
}
`,
	}

	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for _, name := range []string{"main.tf", "variables.tf"} {
		body := files[name]

		err := tw.WriteHeader(&tar.Header{
			Name:    name,
			Mode:    0o644,
			Size:    int64(len(body)),
			ModTime: epoch,
		})
		if err != nil {
			return nil, fmt.Errorf("write tar header %q: %w", name, err)
		}

		_, err = tw.Write([]byte(body))
		if err != nil {
			return nil, fmt.Errorf("write tar body %q: %w", name, err)
		}
	}

	err := tw.Close()
	if err != nil {
		return nil, fmt.Errorf("close tar writer: %w", err)
	}

	err = gz.Close()
	if err != nil {
		return nil, fmt.Errorf("close gzip writer: %w", err)
	}

	return buf.Bytes(), nil
}
