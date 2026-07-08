package schemavalidate

import (
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
