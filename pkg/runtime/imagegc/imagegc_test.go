package imagegc

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dicode/dicode/pkg/task"
)

func TestTag_Format(t *testing.T) {
	dockerfile := []byte("FROM alpine\n")
	tag := Tag("mytask", dockerfile)
	want := Repository("mytask") + ":" + TagSuffix(dockerfile)
	if tag != want {
		t.Errorf("Tag = %q, want %q", tag, want)
	}
	if Repository("mytask") != "dicode-mytask" {
		t.Errorf("Repository = %q", Repository("mytask"))
	}
	if len(TagSuffix(dockerfile)) != 12 { // 6 bytes hex-encoded
		t.Errorf("TagSuffix length = %d, want 12", len(TagSuffix(dockerfile)))
	}
	// Same content → same tag; different content → different tag.
	if Tag("mytask", dockerfile) != tag {
		t.Errorf("Tag not deterministic")
	}
	if Tag("mytask", []byte("FROM debian\n")) == tag {
		t.Errorf("different Dockerfile content must yield a different tag")
	}
}

func TestSelectOrphans(t *testing.T) {
	current := map[string]string{
		"dicode-alive":   "aaaaaaaaaaaa",
		"dicode-rotated": "bbbbbbbbbbbb",
	}
	skip := map[string]bool{
		"dicode-unreadable": true,
	}
	images := []Candidate{
		{Repository: "dicode-alive", Tag: "aaaaaaaaaaaa"},             // in use → keep
		{Repository: "dicode-alive", Tag: "000000000000"},             // stale hash → orphan
		{Repository: "localhost/dicode-rotated", Tag: "cccccc"},       // stale hash, podman prefix → orphan
		{Repository: "localhost/dicode-rotated", Tag: "bbbbbbbbbbbb"}, // current, podman prefix → keep
		{Repository: "dicode-removed", Tag: "dddddddddddd"},           // task gone → orphan
		{Repository: "dicode-unreadable", Tag: "eeeeeeeeeeee"},        // Dockerfile unreadable → keep (fail safe)
		{Repository: "dicode-alive", Tag: "<none>"},                   // dangling → leave for prune
		{Repository: "dicode-alive", Tag: ""},                         // malformed → leave
		{Repository: "alpine", Tag: "latest"},                         // not ours → keep
		{Repository: "someorg/dicode-tool", Tag: "v1"},                // registry image with dicode- basename → keep
		{Repository: "docker.io/library/redis", Tag: "7"},             // not ours → keep
	}

	got := SelectOrphans(images, current, skip)
	want := []string{
		"dicode-alive:000000000000",
		"localhost/dicode-rotated:cccccc",
		"dicode-removed:dddddddddddd",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SelectOrphans = %v, want %v", got, want)
	}
}

func TestSelectOrphans_EmptyInputs(t *testing.T) {
	if got := SelectOrphans(nil, nil, nil); len(got) != 0 {
		t.Errorf("expected no orphans for empty input, got %v", got)
	}
	// No registered tasks at all: every dicode-* image is orphaned.
	got := SelectOrphans([]Candidate{{Repository: "dicode-x", Tag: "abc"}}, map[string]string{}, map[string]bool{})
	if len(got) != 1 || got[0] != "dicode-x:abc" {
		t.Errorf("expected dicode-x:abc orphaned, got %v", got)
	}
}

func TestCurrentTags(t *testing.T) {
	dir := t.TempDir()

	buildTask := filepath.Join(dir, "buildtask")
	if err := os.MkdirAll(buildTask, 0o755); err != nil {
		t.Fatal(err)
	}
	dockerfile := []byte("FROM alpine\nRUN echo hi\n")
	if err := os.WriteFile(filepath.Join(buildTask, "Dockerfile"), dockerfile, 0o644); err != nil {
		t.Fatal(err)
	}

	specs := []*task.Spec{
		nil, // tolerated
		{ // docker build task → contributes to current
			ID:      "buildtask",
			Runtime: task.RuntimeDocker,
			TaskDir: buildTask,
			Docker:  &task.DockerConfig{Build: &task.DockerBuild{}},
		},
		{ // podman build task with missing Dockerfile → skip (fail safe)
			ID:      "broken",
			Runtime: task.RuntimePodman,
			TaskDir: filepath.Join(dir, "does-not-exist"),
			Docker:  &task.DockerConfig{Build: &task.DockerBuild{}},
		},
		{ // image-based task → no local build, ignored
			ID:      "pulled",
			Runtime: task.RuntimeDocker,
			Docker:  &task.DockerConfig{Image: "alpine"},
		},
		{ // deno task → ignored
			ID:      "script",
			Runtime: task.RuntimeDeno,
		},
	}

	current, skip := CurrentTags(specs)

	if got, want := current["dicode-buildtask"], TagSuffix(dockerfile); got != want {
		t.Errorf("current[dicode-buildtask] = %q, want %q", got, want)
	}
	if !skip["dicode-broken"] {
		t.Errorf("unreadable Dockerfile must land in skip; skip=%v", skip)
	}
	if _, ok := current["dicode-pulled"]; ok {
		t.Errorf("image-based task must not contribute a current tag")
	}
	if len(current) != 1 {
		t.Errorf("unexpected current map: %v", current)
	}
}

// TestCurrentTags_SelectOrphans_EndToEnd ties the two halves together: after
// a Dockerfile change, the old tag is orphaned and the new tag is kept.
func TestCurrentTags_SelectOrphans_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM alpine:3.20\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := &task.Spec{
		ID:      "web",
		Runtime: task.RuntimePodman,
		TaskDir: dir,
		Docker:  &task.DockerConfig{Build: &task.DockerBuild{}},
	}

	oldTag := TagSuffix([]byte("FROM alpine:3.19\n"))
	newTag := TagSuffix([]byte("FROM alpine:3.20\n"))

	current, skip := CurrentTags([]*task.Spec{spec})
	images := []Candidate{
		{Repository: "localhost/dicode-web", Tag: oldTag},
		{Repository: "localhost/dicode-web", Tag: newTag},
	}
	got := SelectOrphans(images, current, skip)
	want := []string{"localhost/dicode-web:" + oldTag}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SelectOrphans = %v, want %v", got, want)
	}
}
