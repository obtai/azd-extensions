// Command schemagen writes the JSON Schema for preview.yaml.
//
// Generated from internal/config.Config rather than hand-maintained, so the
// schema cannot drift from the struct that actually parses the file — and the
// doc comments on that struct, which are the real documentation, become the
// descriptions an editor shows on hover.
//
// Run it with `go generate ./...`.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/invopop/jsonschema"

	"github.com/obtai/azd-extensions/preview/internal/config"
)

const (
	modulePath = "github.com/obtai/azd-extensions/preview"
	outputPath = "preview.schema.json"

	// Where the schema answers from once the repo is public. Consumers put this
	// in a `# yaml-language-server: $schema=` modeline, so it has to match the
	// file's committed location.
	schemaID = "https://raw.githubusercontent.com/obtai/azd-extensions/main/preview/preview.schema.json"
)

func main() {
	reflector := &jsonschema.Reflector{
		// Read `yaml:"..."` tags, not `json:"..."` — this struct is only ever
		// unmarshalled from YAML.
		FieldNameTag: "yaml",

		// Without this, invopop applies JSON tag semantics to the yaml tags and
		// treats every field lacking `,omitempty` as required — which would be
		// all of them. Requiredness is stated explicitly instead, with
		// `jsonschema:"required"`.
		RequiredFromJSONSchemaTags: true,

		// One flat schema rather than a $defs/$ref pair. It is a five-field
		// file; indirection earns nothing and makes the committed artefact
		// harder to read in a diff.
		ExpandedStruct: true,
		DoNotReference: true,
	}

	// Turns the Go doc comments into schema descriptions. Needs the source
	// tree, which is why this runs from the module root.
	if err := reflector.AddGoComments(modulePath, "./internal/config"); err != nil {
		fail("reading doc comments: %v", err)
	}

	schema := reflector.Reflect(&config.Config{})
	schema.ID = jsonschema.ID(schemaID)
	schema.Title = "azd preview configuration"
	schema.Description = "Per-pull-request preview environments, read by the obtai.preview azd extension."

	encoded, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		fail("encoding schema: %v", err)
	}

	if err := os.WriteFile(outputPath, append(encoded, '\n'), 0o644); err != nil {
		fail("writing %s: %v", outputPath, err)
	}

	fmt.Printf("wrote %s\n", outputPath)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "schemagen: "+format+"\n", args...)
	os.Exit(1)
}
