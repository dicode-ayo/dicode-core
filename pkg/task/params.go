package task

import (
	"fmt"
	"strconv"
)

// ParamError describes a single per-field problem encountered while validating
// a params payload against a task's declared params schema. The Field name
// matches the declared Param.Name; Message is suitable for surfacing to a
// human or an HTTP API caller.
type ParamError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ParamErrors is a multi-field aggregate. Implements error so callers can
// return it from validation paths and still have errors.Is / errors.As
// behave naturally; consumers that need per-field detail should type-assert.
type ParamErrors []ParamError

func (e ParamErrors) Error() string {
	switch len(e) {
	case 0:
		return "params: no errors"
	case 1:
		return fmt.Sprintf("params: %s: %s", e[0].Field, e[0].Message)
	default:
		return fmt.Sprintf("params: %d field error(s): %s: %s (and %d more)",
			len(e), e[0].Field, e[0].Message, len(e)-1)
	}
}

// ValidateParams checks an inbound params map against the task's declared
// Params schema. It returns a coerced map (string-typed values for the
// runtime, since params are always serialised as strings on the wire to
// the Deno/Python SDK) and a non-nil ParamErrors when one or more fields
// fail validation.
//
// Unknown keys (i.e. keys present in input but not declared in spec.Params)
// are reported as errors — the schema is intentionally closed so that typos
// in a calling agent's payload surface immediately rather than being
// silently dropped at runtime.
//
// Missing-but-required fields are flagged. Fields with a declared default
// fall back to that default when absent.
//
// Type coercion rules:
//   - "string" / "" — accepts JSON string, number, or boolean (stringified).
//   - "number"       — accepts JSON number, or a string that parses as float64.
//   - "boolean"      — accepts JSON boolean, or "true"/"false" strings (case-insensitive).
//   - "cron"         — accepts a non-empty string; cron expression validity is
//     not checked here (the trigger engine owns that).
//
// Other type names are accepted as-is — unknown types are not the validator's
// job to police; they're already covered by the loader's spec validation.
func ValidateParams(declared Params, input map[string]any) (map[string]string, ParamErrors) {
	out := make(map[string]string, len(declared))
	var errs ParamErrors

	declaredByName := make(map[string]Param, len(declared))
	for _, p := range declared {
		declaredByName[p.Name] = p
	}

	// Catch unknown keys early so the caller sees the full picture in one shot.
	for k := range input {
		if _, ok := declaredByName[k]; !ok {
			errs = append(errs, ParamError{Field: k, Message: "unknown parameter"})
		}
	}

	for _, p := range declared {
		raw, present := input[p.Name]
		if !present {
			if p.Default != "" {
				out[p.Name] = p.Default
				continue
			}
			if p.Required {
				errs = append(errs, ParamError{Field: p.Name, Message: "required"})
			}
			continue
		}
		coerced, err := coerceParam(p.Type, raw)
		if err != "" {
			errs = append(errs, ParamError{Field: p.Name, Message: err})
			continue
		}
		out[p.Name] = coerced
	}

	if len(errs) == 0 {
		return out, nil
	}
	return out, errs
}

// coerceParam converts a single value to its string wire form, applying the
// declared type's accept rules. Returns the coerced string and an empty
// error message on success; an empty coerced string and a non-empty error
// on rejection.
func coerceParam(declaredType string, raw any) (string, string) {
	switch declaredType {
	case "string", "":
		switch v := raw.(type) {
		case string:
			return v, ""
		case bool:
			return strconv.FormatBool(v), ""
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64), ""
		case int:
			return strconv.Itoa(v), ""
		case int64:
			return strconv.FormatInt(v, 10), ""
		default:
			return "", fmt.Sprintf("expected string, got %T", raw)
		}
	case "number":
		switch v := raw.(type) {
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64), ""
		case int:
			return strconv.Itoa(v), ""
		case int64:
			return strconv.FormatInt(v, 10), ""
		case string:
			if _, err := strconv.ParseFloat(v, 64); err != nil {
				return "", fmt.Sprintf("expected number, got %q", v)
			}
			return v, ""
		default:
			return "", fmt.Sprintf("expected number, got %T", raw)
		}
	case "boolean":
		switch v := raw.(type) {
		case bool:
			return strconv.FormatBool(v), ""
		case string:
			b, err := strconv.ParseBool(v)
			if err != nil {
				return "", fmt.Sprintf("expected boolean, got %q", v)
			}
			return strconv.FormatBool(b), ""
		default:
			return "", fmt.Sprintf("expected boolean, got %T", raw)
		}
	case "cron":
		s, ok := raw.(string)
		if !ok {
			return "", fmt.Sprintf("expected cron string, got %T", raw)
		}
		if s == "" {
			return "", "cron expression must not be empty"
		}
		return s, ""
	default:
		// Unknown declared types fall back to a permissive string coercion;
		// see ValidateParams godoc for rationale.
		if s, ok := raw.(string); ok {
			return s, ""
		}
		return fmt.Sprintf("%v", raw), ""
	}
}
