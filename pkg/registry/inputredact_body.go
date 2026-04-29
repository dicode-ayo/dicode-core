package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/url"
	"strings"
)

// redactBody applies content-type-aware redaction to an HTTP body. Returns
// a partially-populated PersistedInput with the BodyKind/Body/BodyHash/
// BodyParts fields set; the caller fills in the rest of the struct.
//
// Behavior by media type:
//   - application/json: walk like Params; persist post-redaction JSON.
//     Falls back to "binary" with hash if the body fails to parse.
//   - application/x-www-form-urlencoded: parse, walk values, re-encode;
//     stored as a JSON string for shape consistency. Falls back to
//     "binary" if parsing fails.
//   - multipart/form-data: parse, store BodyParts metadata only; values
//     are NEVER persisted. Falls back to "binary" if parsing fails.
//   - text/*: omit by default; if bodyFullTextual=true, persist verbatim
//     wrapped in a JSON string. Footgun documented at the call site (Task 14).
//   - everything else: omit; sha256 hash for forensic comparison.
//
// BodyHash is always sha256(raw) for forensic comparison even when the body
// is also persisted (json/form).
func redactBody(raw []byte, contentType string, bodyFullTextual bool, redacted *[]string) PersistedInput {
	out := PersistedInput{}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])

	mediaType, params, _ := mime.ParseMediaType(contentType)

	switch {
	case mediaType == "application/json":
		out.BodyKind = "json"
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			out.BodyKind = "binary"
			out.BodyHash = hash
			return out
		}
		walked := redactParams(v, "body", redacted)
		marshalled, err := json.Marshal(walked)
		if err != nil {
			out.BodyKind = "binary"
			out.BodyHash = hash
			return out
		}
		out.Body = marshalled
		out.BodyHash = hash

	case mediaType == "application/x-www-form-urlencoded":
		out.BodyKind = "form"
		vals, err := url.ParseQuery(string(raw))
		if err != nil {
			out.BodyKind = "binary"
			out.BodyHash = hash
			return out
		}
		for k := range vals {
			if shouldRedactName(k) {
				for i := range vals[k] {
					vals[k][i] = redactPlaceholder
				}
				*redacted = append(*redacted, "body."+k)
			}
		}
		// Persist as JSON-string-wrapped form encoding for shape consistency.
		jsonForm, err := json.Marshal(vals.Encode())
		if err != nil {
			out.BodyKind = "binary"
			out.BodyHash = hash
			return out
		}
		out.Body = jsonForm
		out.BodyHash = hash

	case mediaType == "multipart/form-data":
		out.BodyKind = "multipart"
		out.BodyHash = hash
		boundary, ok := params["boundary"]
		if !ok {
			return out
		}
		mr := multipart.NewReader(strings.NewReader(string(raw)), boundary)
		var parts []PartMeta
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return out
			}
			meta := PartMeta{
				Name:        p.FormName(),
				ContentType: p.Header.Get("Content-Type"),
			}
			if filename := p.FileName(); filename != "" {
				meta.Kind = "file"
				if shouldRedactName(meta.Name) {
					meta.Filename = redactPlaceholder
					*redacted = append(*redacted, "body_parts."+meta.Name+".filename")
				} else {
					meta.Filename = filename
				}
			} else {
				meta.Kind = "field"
			}
			body, _ := io.ReadAll(p)
			meta.Size = int64(len(body))
			parts = append(parts, meta)
		}
		out.BodyParts = parts

	case strings.HasPrefix(mediaType, "text/"):
		out.BodyKind = "text"
		if bodyFullTextual {
			j, err := json.Marshal(string(raw))
			if err != nil {
				out.BodyKind = "binary"
				out.BodyHash = hash
				return out
			}
			out.Body = j
		} else {
			out.BodyHash = hash
		}

	default:
		out.BodyKind = "binary"
		out.BodyHash = hash
	}
	return out
}
