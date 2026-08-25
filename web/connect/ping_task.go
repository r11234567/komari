package connectapi

// PingTaskService is the production transport for latency probe task
// administration. The matching admin:*PingTask* RPC2 methods stay registered
// for direct /api/rpc2 callers but no longer have REST bridge routes.

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/database/tasks"
	adminv1 "github.com/r11234567/komari-proto/gen/go/komari/admin/v1"
	adminv1connect "github.com/r11234567/komari-proto/gen/go/komari/admin/v1/adminv1connect"
)

type pingTaskService struct {
	adminv1connect.UnimplementedPingTaskServiceHandler
}

func (s *pingTaskService) ListPingTasks(_ context.Context, _ *connect.Request[adminv1.ListPingTasksRequest]) (*connect.Response[adminv1.ListPingTasksResponse], error) {
	stored, err := tasks.GetAllPingTasks()
	if err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	list := make([]*adminv1.PingTask, 0, len(stored))
	for _, task := range stored {
		list = append(list, pingTaskToProto(task))
	}
	return connect.NewResponse(&adminv1.ListPingTasksResponse{Tasks: list}), nil
}

func (s *pingTaskService) CreatePingTask(_ context.Context, req *connect.Request[adminv1.CreatePingTaskRequest]) (*connect.Response[adminv1.CreatePingTaskResponse], error) {
	name := strings.TrimSpace(req.Msg.Name)
	target := strings.TrimSpace(req.Msg.Target)
	taskType := strings.TrimSpace(req.Msg.Type)
	if name == "" || target == "" || taskType == "" || req.Msg.IntervalSeconds == 0 {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("name, target, type and interval are required"))
	}
	// default_on only enrolls agents registered later, so a task with neither
	// default_on nor an explicit client list would probe nothing.
	if !req.Msg.DefaultOn && len(req.Msg.Clients) == 0 {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("clients is required when default_on is false"))
	}
	taskID, err := tasks.AddPingTask(req.Msg.Clients, req.Msg.DefaultOn, name, target, taskType, int(req.Msg.IntervalSeconds))
	if err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&adminv1.CreatePingTaskResponse{TaskId: uint32(taskID)}), nil
}

func (s *pingTaskService) UpdatePingTasks(_ context.Context, req *connect.Request[adminv1.UpdatePingTasksRequest]) (*connect.Response[adminv1.UpdatePingTasksResponse], error) {
	if len(req.Msg.Tasks) == 0 {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("at least one ping task is required"))
	}
	updates := make([]*models.PingTask, 0, len(req.Msg.Tasks))
	for _, task := range req.Msg.Tasks {
		if task == nil {
			return nil, connectError(connect.CodeInvalidArgument, errors.New("ping task entries cannot be empty"))
		}
		updates = append(updates, &models.PingTask{
			Id:        uint(task.Id),
			Weight:    int(task.Weight),
			Name:      task.Name,
			Clients:   task.Clients,
			DefaultOn: task.DefaultOn,
			Type:      task.Type,
			Target:    task.Target,
			Interval:  int(task.IntervalSeconds),
		})
	}
	if err := tasks.EditPingTask(updates); err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&adminv1.UpdatePingTasksResponse{}), nil
}

func (s *pingTaskService) DeletePingTasks(_ context.Context, req *connect.Request[adminv1.DeletePingTasksRequest]) (*connect.Response[adminv1.DeletePingTasksResponse], error) {
	if len(req.Msg.Ids) == 0 {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("ids cannot be empty"))
	}
	ids := make([]uint, 0, len(req.Msg.Ids))
	for _, id := range req.Msg.Ids {
		ids = append(ids, uint(id))
	}
	if err := tasks.DeletePingTask(ids); err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&adminv1.DeletePingTasksResponse{}), nil
}

func (s *pingTaskService) ReorderPingTasks(_ context.Context, req *connect.Request[adminv1.ReorderPingTasksRequest]) (*connect.Response[adminv1.ReorderPingTasksResponse], error) {
	if len(req.Msg.Weights) == 0 {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("weights cannot be empty"))
	}
	order := make(map[uint]int, len(req.Msg.Weights))
	for id, weight := range req.Msg.Weights {
		order[uint(id)] = int(weight)
	}
	if err := tasks.UpdatePingTaskOrder(order); err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&adminv1.ReorderPingTasksResponse{}), nil
}

func pingTaskToProto(task models.PingTask) *adminv1.PingTask {
	return &adminv1.PingTask{
		Id:              uint32(task.Id),
		Name:            task.Name,
		Target:          task.Target,
		Type:            task.Type,
		IntervalSeconds: int32(task.Interval),
		Clients:         task.Clients,
		DefaultOn:       task.DefaultOn,
		Weight:          int32(task.Weight),
	}
}
