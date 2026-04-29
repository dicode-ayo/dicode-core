package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func TestRedactBody_JSON(t *testing.T) {
	body := []byte(`{"user":"alice","password":"secret","token":"abc"}`)
	redacted := []string{}
	out := redactBody(body, "application/json", false, &redacted)

	if out.BodyKind != "json" {
		t.Errorf("BodyKind = %q, want json", out.BodyKind)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Body, &got); err != nil {
		t.Fatalf("unmarshal redacted body: %v", err)
	}
	if got["user"] != "alice" {
		t.Errorf("user mutated: %v", got["user"])
	}
	if got["password"] != redactPlaceholder {
		t.Errorf("password not redacted: %v", got["password"])
	}
	if got["token"] != redactPlaceholder {
		t.Errorf("token not redacted: %v", got["token"])
	}
}

func TestRedactBody_JSON_InvalidFallsBackToBinary(t *testing.T) {
	body := []byte(`{this is not json`)
	redacted := []string{}
	out := redactBody(body, "application/json", false, &redacted)
	if out.BodyKind != "binary" {
		t.Errorf("invalid JSON should fall back to binary; got %q", out.BodyKind)
	}
	if out.BodyHash == "" {
		t.Error("BodyHash should be set on fallback")
	}
}

func TestRedactBody_FormURLEncoded(t *testing.T) {
	body := []byte("user=alice&password=secret&token=abc")
	redacted := []string{}
	out := redactBody(body, "application/x-www-form-urlencoded", false, &redacted)

	if out.BodyKind != "form" {
		t.Errorf("BodyKind = %q, want form", out.BodyKind)
	}
	// Body is stored as a JSON string of the re-encoded form. Unmarshal first.
	var encoded string
	if err := json.Unmarshal(out.Body, &encoded); err != nil {
		t.Fatalf("body should be JSON string: %v", err)
	}
	parsed, err := url.ParseQuery(encoded)
	if err != nil {
		t.Fatalf("re-encoded form should parse: %v", err)
	}
	if parsed.Get("user") != "alice" {
		t.Errorf("user mutated: %q", parsed.Get("user"))
	}
	if parsed.Get("password") != redactPlaceholder {
		t.Errorf("password not redacted: %q", parsed.Get("password"))
	}
	if parsed.Get("token") != redactPlaceholder {
		t.Errorf("token not redacted: %q", parsed.Get("token"))
	}
}

func TestRedactBody_BinaryDefaultOmitted(t *testing.T) {
	body := []byte{0x00, 0x01, 0x02, 0x03}
	redacted := []string{}
	out := redactBody(body, "application/octet-stream", false, &redacted)

	if out.BodyKind != "binary" {
		t.Errorf("BodyKind = %q, want binary", out.BodyKind)
	}
	if len(out.Body) != 0 {
		t.Errorf("body should be omitted for binary; got %d bytes", len(out.Body))
	}
	sum := sha256.Sum256(body)
	if out.BodyHash != hex.EncodeToString(sum[:]) {
		t.Errorf("BodyHash = %q, want %s", out.BodyHash, hex.EncodeToString(sum[:]))
	}
}

func TestRedactBody_TextDefaultOmitted(t *testing.T) {
	body := []byte("some opaque text payload")
	redacted := []string{}
	out := redactBody(body, "text/plain", false, &redacted)

	if out.BodyKind != "text" {
		t.Errorf("BodyKind = %q, want text", out.BodyKind)
	}
	if len(out.Body) != 0 {
		t.Errorf("body should be omitted for text by default")
	}
	if out.BodyHash == "" {
		t.Error("BodyHash should be set")
	}
}

func TestRedactBody_TextFullTextualOptIn(t *testing.T) {
	body := []byte("some opaque text payload")
	redacted := []string{}
	out := redactBody(body, "text/plain", true, &redacted)

	if out.BodyKind != "text" {
		t.Errorf("BodyKind = %q, want text", out.BodyKind)
	}
	var got string
	if err := json.Unmarshal(out.Body, &got); err != nil {
		t.Fatalf("body should be JSON-string-wrapped: %v", err)
	}
	if got != string(body) {
		t.Errorf("body should be persisted verbatim; got %q want %q", got, string(body))
	}
}

func TestRedactBody_Multipart(t *testing.T) {
	body := []byte("--BOUNDARY\r\n" +
		"Content-Disposition: form-data; name=\"username\"\r\n\r\n" +
		"alice\r\n" +
		"--BOUNDARY\r\n" +
		"Content-Disposition: form-data; name=\"avatar\"; filename=\"face.png\"\r\n" +
		"Content-Type: image/png\r\n\r\n" +
		"\x89PNG\r\n\x1a\n" +
		"\r\n--BOUNDARY--\r\n")
	redacted := []string{}
	out := redactBody(body, "multipart/form-data; boundary=BOUNDARY", false, &redacted)

	if out.BodyKind != "multipart" {
		t.Errorf("BodyKind = %q, want multipart", out.BodyKind)
	}
	if len(out.BodyParts) != 2 {
		t.Fatalf("BodyParts len = %d, want 2; parts = %#v", len(out.BodyParts), out.BodyParts)
	}
	if out.BodyParts[0].Name != "username" || out.BodyParts[0].Kind != "field" {
		t.Errorf("part[0] = %+v", out.BodyParts[0])
	}
	if out.BodyParts[1].Name != "avatar" || out.BodyParts[1].Kind != "file" || out.BodyParts[1].Filename != "face.png" {
		t.Errorf("part[1] = %+v", out.BodyParts[1])
	}
	if len(out.Body) != 0 {
		t.Errorf("multipart body should be omitted; values are not stored")
	}
	if out.BodyHash == "" {
		t.Error("BodyHash should be set for multipart")
	}
}

// Sanity: an empty body should not panic.
func TestRedactBody_EmptyBody(t *testing.T) {
	redacted := []string{}
	out := redactBody([]byte{}, "application/json", false, &redacted)
	// Either kind is acceptable here as long as no panic; document with comment.
	_ = out
	// Not a strict assertion — just don't crash. (json.Unmarshal of empty bytes errors,
	// so we expect the binary fallback.)
	if !strings.HasPrefix(out.BodyKind, "binary") && out.BodyKind != "json" {
		t.Logf("empty body redaction produced BodyKind=%q (informational; either is acceptable)", out.BodyKind)
	}
}
