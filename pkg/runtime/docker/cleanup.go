package docker

import (
	"context"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	dockerclient "github.com/docker/docker/client"
	"go.uber.org/zap"
)

// CleanupOrphanedContainers stops and removes any Docker containers left behind
// by a previous dicode session. Containers are identified by the "dicode.run-id"
// label which is set on every container created by the Docker runtime.
//
// Call once at startup before any tasks are registered. If Docker is unavailable
// the function logs at debug level and returns — it is not a fatal error.
func CleanupOrphanedContainers(ctx context.Context, log *zap.Logger) {
	dc, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		log.Debug("docker unavailable, skipping orphan cleanup", zap.Error(err))
		return
	}
	defer dc.Close()

	orphans, err := dc.ContainerList(ctx, container.ListOptions{
		All:     true, // include stopped containers
		Filters: filters.NewArgs(filters.Arg("label", "dicode.run-id")),
	})
	if err != nil {
		log.Warn("failed to list orphaned docker containers", zap.Error(err))
		return
	}
	if len(orphans) == 0 {
		return
	}

	log.Info("removing orphaned docker containers from previous session", zap.Int("count", len(orphans)))
	for _, c := range orphans {
		runID := c.Labels["dicode.run-id"]
		taskID := c.Labels["dicode.task-id"]
		shortID := c.ID
		if len(shortID) > 12 {
			shortID = shortID[:12]
		}
		log.Info("removing orphaned container",
			zap.String("container", shortID),
			zap.String("task", taskID),
			zap.String("run", runID),
			zap.String("state", c.State),
		)
		if c.State == "running" {
			stopTimeout := 5
			if err := dc.ContainerStop(ctx, c.ID, container.StopOptions{Timeout: &stopTimeout}); err != nil {
				log.Warn("could not stop orphaned container", zap.String("container", shortID), zap.Error(err))
			}
		}
		if err := dc.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true}); err != nil {
			log.Warn("could not remove orphaned container", zap.String("container", shortID), zap.Error(err))
		}
	}
}
