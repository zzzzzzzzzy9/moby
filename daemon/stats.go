package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"time"

	"github.com/containerd/log"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/server/backend"
	"github.com/moby/moby/v2/errdefs"
)

// ContainerStats writes information about the container to the stream
// given in the config object.
func (daemon *Daemon) ContainerStats(ctx context.Context, prefixOrName string, config *backend.ContainerStatsConfig) error {
	ctr, err := daemon.GetContainer(prefixOrName)
	if err != nil {
		return err
	}

	if config.Stream && config.OneShot {
		return errdefs.InvalidParameter(errors.New("cannot have stream=true and one-shot=true"))
	}

	enc := json.NewEncoder(config.OutStream())

	// If the container is either not running or restarting and requires no stream, return an empty stats.
	if (!ctr.IsRunning() || ctr.IsRestarting()) && !config.Stream {
		return enc.Encode(&containertypes.StatsResponse{
			Name: ctr.Name,
			ID:   ctr.ID,
		})
	}

	// Get container stats directly if OneShot is set
	if config.OneShot {
		stats, err := daemon.GetContainerStats(ctr)
		if err != nil {
			return err
		}
		return enc.Encode(stats)
	}

	var preCPUStats containertypes.CPUStats
	var preRead time.Time
	getStatJSON := func(v interface{}) *containertypes.StatsResponse {
		ss := v.(containertypes.StatsResponse)
		ss.Name = ctr.Name
		ss.ID = ctr.ID
		ss.PreCPUStats = preCPUStats
		ss.PreRead = preRead
		preCPUStats = ss.CPUStats
		preRead = ss.Read
		return &ss
	}

	updates := daemon.subscribeToContainerStats(ctr)
	defer daemon.unsubscribeToContainerStats(ctr, updates)

	noStreamFirstFrame := !config.OneShot

	for {
		select {
		case v, ok := <-updates:
			if !ok {
				return nil
			}

			statsJSON := getStatJSON(v)
			if !config.Stream && noStreamFirstFrame {
				// prime the cpu stats so they aren't 0 in the final output
				noStreamFirstFrame = false
				continue
			}

			if err := enc.Encode(statsJSON); err != nil {
				return err
			}

			if !config.Stream {
				return nil
			}
		case <-ctx.Done():
			return nil
		}
	}
}

func (daemon *Daemon) subscribeToContainerStats(c *container.Container) chan interface{} {
	return daemon.statsCollector.Collect(c)
}

func (daemon *Daemon) unsubscribeToContainerStats(c *container.Container, ch chan interface{}) {
	daemon.statsCollector.Unsubscribe(c, ch)
}

// GetContainerStats collects all the stats published by a container
func (daemon *Daemon) GetContainerStats(container *container.Container) (*containertypes.StatsResponse, error) {
	stats, err := daemon.stats(container)
	if err != nil {
		goto done
	}

	// Sample system CPU usage close to container usage to avoid
	// noise in metric calculations.
	// FIXME: move to containerd on Linux (not Windows)
	stats.CPUStats.SystemUsage, stats.CPUStats.OnlineCPUs, err = getSystemCPUUsage()
	if err != nil {
		goto done
	}

	// We already have the network stats on Windows directly from HCS.
	if !container.Config.NetworkDisabled && runtime.GOOS != "windows" {
		stats.Networks, err = daemon.getNetworkStats(container)
	}

done:
	switch err.(type) {
	case nil:
		return stats, nil
	case errdefs.ErrConflict, errdefs.ErrNotFound:
		// return empty stats containing only name and ID if not running or not found
		return &containertypes.StatsResponse{
			Name: container.Name,
			ID:   container.ID,
		}, nil
	default:
		log.G(context.TODO()).Errorf("collecting stats for container %s: %v", container.Name, err)
		return nil, err
	}
}
