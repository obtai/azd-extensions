// Command schemagen writes the JSON Schema for preview.yaml from
// internal/config.Config. Run it with `go generate ./...`.
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

	// Must match the file's committed location: consumers point a
	// `# yaml-language-server: $schema=` modeline at it.
	schemaID = "https://raw.githubusercontent.com/obtai/azd-extensions/main/preview/preview.schema.json"
)

func main() {
	reflector := &jsonschema.Reflector{
		FieldNameTag: "yaml",

		// Otherwise every field lacking `,omitempty` is treated as required.
		// Requiredness is stated with `jsonschema:"required"` instead.
		RequiredFromJSONSchemaTags: true,

		ExpandedStruct: true,
		DoNotReference: true,
	}

	// Turns the Go doc comments into schema descriptions.
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
