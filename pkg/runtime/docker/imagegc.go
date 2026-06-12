package docker

import (
	"context"
	"strings"

	"github.com/dicode/dicode/pkg/runtime/imagegc"
	"github.com/dicode/dicode/pkg/task"
	"github.com/docker/docker/api/types/image"
	dockerclient "github.com/docker/docker/client"
	"go.uber.org/zap"
)

// ReclaimOrphanedImages removes locally built dicode-<taskID>:<hash> images
// that no longer correspond to a registered task with that Dockerfile hash
// (issue #380 / TODO in pkg/task/spec.go). Best effort: if Docker is
// unavailable or an image is still in use, the function logs and moves on.
// specs should be the registry's current task list (registry.All()).
func ReclaimOrphanedImages(ctx context.Context, log *zap.Logger, specs []*task.Spec) {
	dc, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		log.Debug("docker unavailable, skipping image GC", zap.Error(err))
		return
	}
	defer dc.Close()

	list, err := dc.ImageList(ctx, image.ListOptions{})
	if err != nil {
		log.Debug("docker image list failed, skipping image GC", zap.Error(err))
		return
	}

	var candidates []imagegc.Candidate
	for _, summary := range list {
		for _, repoTag := range summary.RepoTags {
			repo, tag, ok := splitRepoTag(repoTag)
			if !ok {
				continue
			}
			candidates = append(candidates, imagegc.Candidate{Repository: repo, Tag: tag})
		}
	}

	current, skip := imagegc.CurrentTags(specs)
	orphans := imagegc.SelectOrphans(candidates, current, skip)
	for _, ref := range orphans {
		// No Force: an image backing a running container fails removal,
		// which is exactly the safe behaviour we want.
		if _, err := dc.ImageRemove(ctx, ref, image.RemoveOptions{PruneChildren: true}); err != nil {
			log.Debug("could not remove orphaned dicode image", zap.String("image", ref), zap.Error(err))
			continue
		}
		log.Info("reclaimed orphaned dicode image", zap.String("image", ref))
	}
}

// splitRepoTag splits "repo:tag" at the last colon that belongs to the tag
// (a colon followed by a "/" is a registry port, not a tag separator).
func splitRepoTag(ref string) (repo, tag string, ok bool) {
	i := strings.LastIndex(ref, ":")
	if i < 0 || strings.Contains(ref[i+1:], "/") {
		return "", "", false
	}
	return ref[:i], ref[i+1:], true
}
