package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// ContentHash is a task's content-hash identity: the digest a caller gets
// back from ComputeContentHash or ComputeSpecHash. It is always a
// hex-encoded SHA-256 sum.
type ContentHash string

// ComputeContentHash combines dir's content hash (Hash) with a caller-chosen
// "resolved" value under a versioned, NUL-delimited domain prefix:
//
//	domain \0 dirHash \0 json(resolved)
//
// domain is the caller's own versioned scope tag (e.g.
// "dicode-approval-content-v1") — bump it whenever that caller's folded
// field set or encoding changes, so old digests can never collide with new
// ones; see callers' own domain constants. resolved is whatever the caller
// decides should perturb the hash alongside the directory's file content —
// e.g. only security-relevant resolved fields, or an entire resolved spec —
// this function has no opinion on that choice, only on how the two pieces
// are combined, so every caller shares one combination scheme instead of
// each re-deriving its own.
//
// Returns an error, never a silently degraded hash, when dir's content
// cannot be hashed (see Hash) or resolved cannot be marshalled — the caller
// decides what to do with that error (propagate it, or explicitly and
// loudly fall back — see pkg/taskset/source.go's contentHashFor for a
// documented example of the latter).
func ComputeContentHash(domain, taskID, dir string, resolved any, hashInclude ...string) (ContentHash, error) {
	dirHash, err := Hash(dir, hashInclude...)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(resolved)
	if err != nil {
		return "", fmt.Errorf("hash %s: marshal resolved fields: %w", taskID, err)
	}
	h := sha256.New()
	h.Write([]byte(domain))
	h.Write([]byte{0})
	h.Write([]byte(dirHash))
	h.Write([]byte{0})
	h.Write(b)
	return ContentHash(hex.EncodeToString(h.Sum(nil))), nil
}

// ComputeSpecHash is the dir-less fallback: SHA-256 over v's JSON encoding
// alone, with no directory content and no domain separation — for a task
// with nothing on disk to hash (e.g. an inline taskset entry).
func ComputeSpecHash(taskID string, v any) (ContentHash, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("hash %s: %w", taskID, err)
	}
	sum := sha256.Sum256(b)
	return ContentHash(hex.EncodeToString(sum[:])), nil
}
