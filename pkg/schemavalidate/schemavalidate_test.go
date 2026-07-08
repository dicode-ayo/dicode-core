package schemavalidate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const objSchema = `{
  "type": "object",
  "properties": {
    "project_name": { "type": "string" },
    "count": { "type": "integer" },
    "env": { "type": "string", "enum": ["staging", "prod"] },
    "notify": { "type": "boolean" }
  },
  "required": ["project_name", "env"]
}`

func TestValidate_Valid(t *testing.T) {
	err := Validate([]byte(objSchema), []byte(`{"project_name":"api","env":"prod","count":3,"notify":true}`))
	if err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
}

func TestValidate_EmptySchemaAllowsAnything(t *testing.T) {
	if err := Validate(nil, []byte(`{"whatever": 1}`)); err != nil {
		t.Fatalf("empty schema should impose no constraint: %v", err)
	}
	if err := Validate([]byte("   "), []byte(`{"x":1}`)); err != nil {
		t.Fatalf("blank schema should impose no constraint: %v", err)
	}
}

func TestValidate_MissingRequired(t *testing.T) {
	err := Validate([]byte(objSchema), []byte(`{"project_name":"api"}`))
	if err == nil {
		t.Fatal("expected error for missing required field env")
	}
	if !strings.Contains(err.Error(), "env") {
		t.Fatalf("error should name the missing field env, got: %v", err)
	}
}

func TestValidate_EnumViolation(t *testing.T) {
	err := Validate([]byte(objSchema), []byte(`{"project_name":"api","env":"dev"}`))
	if err == nil {
		t.Fatal("expected enum violation error")
	}
	if !strings.Contains(err.Error(), "env") {
		t.Fatalf("error should reference env, got: %v", err)
	}
}

func TestValidate_TypeMismatch(t *testing.T) {
	err := Validate([]byte(objSchema), []byte(`{"project_name":"api","env":"prod","count":"three"}`))
	if err == nil {
		t.Fatal("expected type error for count")
	}
	if !strings.Contains(err.Error(), "count") {
		t.Fatalf("error should reference count, got: %v", err)
	}
}

func TestValidate_EmptyInputAgainstRequired(t *testing.T) {
	err := Validate([]byte(objSchema), nil)
	if err == nil {
		t.Fatal("expected error: empty input misses required fields")
	}
}

func TestValidate_BadSchema(t *testing.T) {
	if err := Validate([]byte(`{not json`), []byte(`{}`)); err == nil {
		t.Fatal("expected error for malformed schema")
	}
}

// A task-authored schema must not be able to turn a $ref into a local-file
// read by the privileged daemon. The file:// $ref must be denied at compile
// time, and the secret contents of the referenced file must never surface in
// the error (no content/existence oracle).
func TestValidate_FileRefDeniedAndNotLeaked(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "secret.json")
	const sentinel = "TOP-SECRET-SENTINEL-9182"
	if err := os.WriteFile(secretPath, []byte(`{"type":"string","description":"`+sentinel+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	schema := []byte(`{"type":"object","properties":{"x":{"$ref":"file://` + secretPath + `"}}}`)

	err := Validate(schema, []byte(`{"x":"hi"}`))
	if err == nil {
		t.Fatal("expected an error: an external file:// $ref must be denied")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Fatalf("error leaked referenced file contents: %v", err)
	}
	if !strings.Contains(err.Error(), "external $ref not allowed") {
		t.Fatalf("error should explain the denied ref; got: %v", err)
	}
}

// A nonexistent file:// $ref must be denied identically — the loader never
// touches the filesystem, so it cannot be used as an existence oracle.
func TestValidate_MissingFileRefDeniedSameAsPresent(t *testing.T) {
	schema := []byte(`{"$ref":"file:///nonexistent/does-not-exist-9182.json"}`)
	err := Validate(schema, []byte(`{}`))
	if err == nil {
		t.Fatal("expected an error for a file:// $ref regardless of existence")
	}
	if !strings.Contains(err.Error(), "external $ref not allowed") {
		t.Fatalf("error should be the deny-loader message, not a file-not-found oracle; got: %v", err)
	}
}

// A self-contained schema that resolves an internal $ref against $defs (no
// external load) still compiles and validates normally.
func TestValidate_InternalRefStillWorks(t *testing.T) {
	schema := []byte(`{
      "type": "object",
      "properties": { "env": { "$ref": "#/$defs/envName" } },
      "required": ["env"],
      "$defs": { "envName": { "type": "string", "enum": ["staging", "prod"] } }
    }`)
	if err := Validate(schema, []byte(`{"env":"prod"}`)); err != nil {
		t.Fatalf("internal $ref schema should validate: %v", err)
	}
	if err := Validate(schema, []byte(`{"env":"dev"}`)); err == nil {
		t.Fatal("internal $ref enum should still reject an out-of-set value")
	}
}

func TestCompileReuse(t *testing.T) {
	sch, err := Compile([]byte(objSchema))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if err := ValidateCompiled(sch, []byte(`{"project_name":"a","env":"prod"}`)); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
	if err := ValidateCompiled(sch, []byte(`{"env":"prod"}`)); err == nil {
		t.Fatal("expected missing project_name error")
	}
}
