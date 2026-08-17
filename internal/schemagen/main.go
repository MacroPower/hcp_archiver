// Command schemagen generates the JSON schema for the hcp_archiver YAML
// configuration file from [config.File] and writes it to disk.
//
// It runs via go generate; see the directive in the config package. The
// generated schema is embedded by the config package for validation and is
// referenced by the yaml-language-server directive in example configurations
// for editor integration.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"

	"go.jacobcolvin.com/x/jsonschema"

	"go.jacobcolvin.com/hcp_archiver/pkg/config"
)

// outFile is the path the generated schema is written to.
var outFile = flag.String("o", "config.schema.json", "output file for the generated schema")

func main() {
	flag.Parse()

	js, err := jsonschema.GenerateFor[config.File](
		context.Background(),
		jsonschema.WithDescriptionProvider(jsonschema.NewGoCommentProvider()),
		jsonschema.WithRootTitle(true),
	)
	if err != nil {
		log.Fatalf("generate JSON schema: %v", err)
	}

	data, err := json.MarshalIndent(js, "", "  ")
	if err != nil {
		log.Fatalf("marshal JSON schema: %v", err)
	}

	err = os.WriteFile(*outFile, append(data, '\n'), 0o600)
	if err != nil {
		log.Fatalf("write schema file: %v", err)
	}
}
