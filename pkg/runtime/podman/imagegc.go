package podman

import (
	"context"
	"os/exec"
	"strings"
	"time"

	podmanpkg "github.com/dicode/dicode/pkg/podman"
	"github.com/dicode/dicode/pkg/runtime/imagegc"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// ReclaimOrphanedImages removes locally built dicode-<taskID>:<hash> images
// that no longer correspond to a registered task with that Dockerfile hash
// (issue #380 / the long-standing build-cache TODO). Best effort: if podman
// is unavailable or an image is still in use, the function logs and moves
// on. specs should be the registry's current task list (registry.All()).
func ReclaimOrphanedImages(ctx context.Context, log *zap.Logger, specs []*task.Spec) {
	podmanPath, err := podmanpkg.BinaryPath()
	if err != nil {
		log.Debug("podman unavailable, skipping image GC", zap.Error(err))
		return
	}

	listCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(listCtx, podmanPath, "images", //nolint:gosec
		"--format", "{{.Repository}}|{{.Tag}}",
	).Output()
	if err != nil {
		log.Debug("podman image list failed, skipping image GC", zap.Error(err))
		return
	}

	var candidates []imagegc.Candidate
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		repo, tag, ok := strings.Cut(strings.TrimSpace(line), "|")
		if !ok || repo == "" {
			continue
		}
		candidates = append(candidates, imagegc.Candidate{Repository: repo, Tag: tag})
	}

	current, skip := imagegc.CurrentTags(specs)
	for _, ref := range imagegc.SelectOrphans(candidates, current, skip) {
		rmCtx, rmCancel := context.WithTimeout(ctx, 30*time.Second)
		// No -f: an image backing a running container fails removal, which
		// is exactly the safe behaviour we want.
		err := exec.CommandContext(rmCtx, podmanPath, "rmi", ref).Run() //nolint:gosec
		rmCancel()
		if err != nil {
			log.Debug("could not remove orphaned dicode image", zap.String("image", ref), zap.Error(err))
			continue
		}
		log.Info("reclaimed orphaned dicode image", zap.String("image", ref))
	}
}
