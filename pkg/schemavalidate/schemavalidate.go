// Package schemavalidate compiles a stored JSON Schema (draft 2020-12) and
// validates a submitted input object against it. It is the single validator
// shared by the WebUI resume endpoint and the CLI resume control method so a
// suspended task can trust that ctx.resume_input conforms to the schema it
// declared via dicode.suspend({ schema }).
package schemavalidate

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// enPrinter localizes constraint-failure messages to English; the validator's
// kind messages are rendered through it.
var enPrinter = message.NewPrinter(language.English)

// denyLoader is a jsonschema URLLoader that refuses every external reference. It
// replaces the library's default FileLoader so a task's suspend schema can
// never turn a `$ref` into a local-file read (or any other fetch) by the
// privileged daemon. The error names the offending url without ever opening it.
type denyLoader struct{}

func (denyLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("external $ref not allowed in a suspend schema: %s", url)
}

// Validate checks input (a JSON document) against schema (a JSON Schema
// document). A nil/empty schema imposes no constraints. A returned error
// carries a concise, per-field message suitable for a 400 body or a CLI line.
func Validate(schema, input []byte) error {
	if len(bytes.TrimSpace(schema)) == 0 {
		return nil
	}
	sch, err := Compile(schema)
	if err != nil {
		return err
	}
	return ValidateCompiled(sch, input)
}

// Compile parses and compiles a JSON Schema document. Reuse the result across
// many Validate calls when validating repeatedly against the same schema.
func Compile(schema []byte) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	if err != nil {
		return nil, fmt.Errorf("invalid schema JSON: %w", err)
	}
	c := jsonschema.NewCompiler()
	// The compiler installs a default FileLoader that resolves file:// $refs off
	// the daemon's own filesystem — a task-authored schema with
	// {"$ref":"file:///etc/passwd"} would be read by the privileged daemon,
	// outside the task's sandbox. A suspend schema is self-contained (internal
	// $ref/$defs are resolved in-document, no load), so deny every external ref.
	c.UseLoader(denyLoader{})
	if err := c.AddResource("schema.json", doc); err != nil {
		return nil, fmt.Errorf("invalid schema: %w", err)
	}
	sch, err := c.Compile("schema.json")
	if err != nil {
		return nil, fmt.Errorf("invalid schema: %w", err)
	}
	return sch, nil
}

// ValidateCompiled validates input against an already-compiled schema.
func ValidateCompiled(sch *jsonschema.Schema, input []byte) error {
	if len(bytes.TrimSpace(input)) == 0 {
		input = []byte("{}")
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(input))
	if err != nil {
		return fmt.Errorf("invalid input JSON: %w", err)
	}
	if err := sch.Validate(inst); err != nil {
		var ve *jsonschema.ValidationError
		if errors.As(err, &ve) {
			return errors.New(fieldMessage(ve))
		}
		return err
	}
	return nil
}

// message renders a ValidationError tree into one concise line. It walks to the
// leaf causes (the concrete failures) and formats each as `field: reason`,
// keyed off the instance location so the caller learns which property is at
// fault. Falls back to the library's own message for a rootless failure.
func fieldMessage(ve *jsonschema.ValidationError) string {
	leaves := leafErrors(ve)
	parts := make([]string, 0, len(leaves))
	seen := map[string]bool{}
	for _, leaf := range leaves {
		field := strings.Trim(strings.Join(leaf.InstanceLocation, "/"), "/")
		reason := leaf.ErrorKind.LocalizedString(enPrinter)
		line := reason
		if field != "" {
			line = field + ": " + reason
		}
		if seen[line] {
			continue
		}
		seen[line] = true
		parts = append(parts, line)
	}
	if len(parts) == 0 {
		return "input does not satisfy the schema"
	}
	return strings.Join(parts, "; ")
}

// leafErrors collects the deepest causes — the ones without further causes are
// the concrete constraint failures; the intermediate nodes only carry schema
// location context.
func leafErrors(ve *jsonschema.ValidationError) []*jsonschema.ValidationError {
	if len(ve.Causes) == 0 {
		return []*jsonschema.ValidationError{ve}
	}
	var out []*jsonschema.ValidationError
	for _, c := range ve.Causes {
		out = append(out, leafErrors(c)...)
	}
	return out
}
