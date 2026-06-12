// Package imagegc selects orphaned dicode-built container images for
// garbage collection.
//
// The docker and podman runtimes build local task images tagged
// "dicode-<taskID>:<dockerfile-hash>" (see Tag). When a task is removed or
// its Dockerfile changes, the previously built image lingers forever —
// the TODOs in pkg/task/spec.go and pkg/runtime/podman/runtime.go.
//
// This package holds the pure, unit-testable selection logic; the
// runtime-specific reclaim wiring (docker SDK / podman CLI calls) lives in
// pkg/runtime/docker and pkg/runtime/podman and is best-effort.
package imagegc

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strings"

	"github.com/dicode/dicode/pkg/task"
)

// Repository returns the image repository used for a task's locally built
// image: "dicode-<taskID>".
func Repository(taskID string) string { return "dicode-" + taskID }

// TagSuffix returns the tag component derived from Dockerfile content.
func TagSuffix(dockerfile []byte) string {
	h := sha256.Sum256(dockerfile)
	return fmt.Sprintf("%x", h[:6])
}

// Tag returns the full image reference for a task's locally built image:
// "dicode-<taskID>:<hash>". Both container runtimes use this when building,
// so GC selection and build caching can never disagree on the format.
func Tag(taskID string, dockerfile []byte) string {
	return Repository(taskID) + ":" + TagSuffix(dockerfile)
}

// Candidate is one locally present image, as listed by the container runtime.
type Candidate struct {
	Repository string // e.g. "dicode-mytask" or "localhost/dicode-mytask"
	Tag        string // e.g. "a1b2c3d4e5f6"
}

// CurrentTags computes, for every registered task that builds a local image,
// the repository → expected-tag mapping currently in use.
//
// Tasks whose Dockerfile cannot be read land in skip instead — their images
// are never collected (fail safe: a transient read error must not delete an
// image that may still be in use).
func CurrentTags(specs []*task.Spec) (current map[string]string, skip map[string]bool) {
	current = make(map[string]string)
	skip = make(map[string]bool)
	for _, spec := range specs {
		if spec == nil || spec.Docker == nil || spec.Docker.Build == nil {
			continue
		}
		if spec.Runtime != task.RuntimeDocker && spec.Runtime != task.RuntimePodman {
			continue
		}
		repo := Repository(spec.ID)
		dockerfilePath, _ := spec.Docker.Build.ResolvePaths(spec.TaskDir)
		content, err := os.ReadFile(dockerfilePath) //nolint:gosec
		if err != nil {
			skip[repo] = true
			continue
		}
		current[repo] = TagSuffix(content)
	}
	return current, skip
}

// SelectOrphans returns the references ("repository:tag", as listed) of
// dicode-built images that are orphaned and safe to remove:
//
//   - the repository is "dicode-*" (or "localhost/dicode-*", podman's local
//     prefix) — images from real registries are never touched, even if the
//     basename happens to start with "dicode-";
//   - the repository is not in skip (Dockerfile unreadable → keep all);
//   - no registered task currently expects this exact tag — either the task
//     is gone, or its Dockerfile hash moved on.
//
// Untagged ("<none>") images are left for the runtime's own prune tooling.
func SelectOrphans(images []Candidate, current map[string]string, skip map[string]bool) []string {
	var orphans []string
	for _, img := range images {
		repo := strings.TrimPrefix(img.Repository, "localhost/")
		if !strings.HasPrefix(repo, "dicode-") {
			continue
		}
		if img.Tag == "" || img.Tag == "<none>" {
			continue
		}
		if skip[repo] {
			continue
		}
		if expected, ok := current[repo]; ok && expected == img.Tag {
			continue
		}
		orphans = append(orphans, img.Repository+":"+img.Tag)
	}
	return orphans
}
