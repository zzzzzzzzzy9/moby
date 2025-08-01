package cluster

import (
	"context"

	"github.com/moby/moby/api/types/filters"
	types "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/v2/daemon/cluster/convert"
	swarmapi "github.com/moby/swarmkit/v2/api"
	"google.golang.org/grpc"
)

// GetTasks returns a list of tasks matching the filter options.
func (c *Cluster) GetTasks(options types.TaskListOptions) ([]types.Task, error) {
	var r *swarmapi.ListTasksResponse

	err := c.lockedManagerAction(func(ctx context.Context, state nodeState) error {
		filterTransform := func(filter filters.Args) error {
			if filter.Contains("service") {
				serviceFilters := filter.Get("service")
				for _, serviceFilter := range serviceFilters {
					service, err := getService(ctx, state.controlClient, serviceFilter, false)
					if err != nil {
						return err
					}
					filter.Del("service", serviceFilter)
					filter.Add("service", service.ID)
				}
			}
			if filter.Contains("node") {
				nodeFilters := filter.Get("node")
				for _, nodeFilter := range nodeFilters {
					node, err := getNode(ctx, state.controlClient, nodeFilter)
					if err != nil {
						return err
					}
					filter.Del("node", nodeFilter)
					filter.Add("node", node.ID)
				}
			}
			if !filter.Contains("runtime") {
				// default to only showing container tasks
				filter.Add("runtime", "container")
				filter.Add("runtime", "")
			}
			return nil
		}

		f, err := newListTasksFilters(options.Filters, filterTransform)
		if err != nil {
			return err
		}

		r, err = state.controlClient.ListTasks(
			ctx,
			&swarmapi.ListTasksRequest{Filters: f},
			grpc.MaxCallRecvMsgSize(defaultRecvSizeForListResponse),
		)
		return err
	})
	if err != nil {
		return nil, err
	}

	tasks := make([]types.Task, 0, len(r.Tasks))
	for _, task := range r.Tasks {
		t, err := convert.TaskFromGRPC(*task)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, nil
}

// GetTask returns a task by an ID.
func (c *Cluster) GetTask(input string) (types.Task, error) {
	var task *swarmapi.Task
	err := c.lockedManagerAction(func(ctx context.Context, state nodeState) error {
		t, err := getTask(ctx, state.controlClient, input)
		if err != nil {
			return err
		}
		task = t
		return nil
	})
	if err != nil {
		return types.Task{}, err
	}
	return convert.TaskFromGRPC(*task)
}
