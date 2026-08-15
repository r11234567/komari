package connectapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/auditlog"
	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/pkg/metric"
	"github.com/komari-monitor/komari/pkg/rpc"
	metricsv1 "github.com/r11234567/komari-proto/gen/go/komari/metrics/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (s *metricsService) UpdateMetricDefinition(ctx context.Context, req *connect.Request[metricsv1.UpdateMetricDefinitionRequest]) (*connect.Response[metricsv1.UpdateMetricDefinitionResponse], error) {
	name := strings.TrimSpace(req.Msg.Name)
	if name == "" {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("metric name is required"))
	}
	store := metricstore.GetStore()
	if store == nil {
		return nil, connectError(connect.CodeFailedPrecondition, errors.New("metric store is not initialized"))
	}
	definition, err := store.UpdateMetricRetention(ctx, name, int(req.Msg.RetentionDays))
	if errors.Is(err, metric.ErrNotFound) {
		return nil, connectError(connect.CodeNotFound, fmt.Errorf("metric not found: %s", name))
	}
	if err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	if req.Msg.RetentionDays == 0 {
		metricstore.DeleteMetricDataAsync(name)
	}
	connectAudit(ctx, "update metric definition: "+name, "info")
	return connect.NewResponse(&metricsv1.UpdateMetricDefinitionResponse{Definition: metricDefinitionToProto(definition)}), nil
}

func (s *metricsService) GetDownsamplingPolicy(context.Context, *connect.Request[metricsv1.GetDownsamplingPolicyRequest]) (*connect.Response[metricsv1.GetDownsamplingPolicyResponse], error) {
	policy, err := connectDownsamplingPolicy()
	if err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&metricsv1.GetDownsamplingPolicyResponse{Policy: policy}), nil
}

func (s *metricsService) SetDownsamplingPolicy(ctx context.Context, req *connect.Request[metricsv1.SetDownsamplingPolicyRequest]) (*connect.Response[metricsv1.SetDownsamplingPolicyResponse], error) {
	cfg, err := config.GetManyAs[metricstore.MetricStoreConfig]()
	if err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	cfg.DownsamplingEnabled = req.Msg.Enabled
	cfg.RollupMinuteRetentionMinutes = int(req.Msg.MinuteRetentionMinutes)
	cfg.RollupFiveMinuteRetentionMinutes = int(req.Msg.FiveMinuteRetentionMinutes)
	cfg.RollupHourRetentionHours = int(req.Msg.HourRetentionHours)
	if err := metricstore.ValidateDownsamplingPolicy(cfg); err != nil {
		return nil, connectError(connect.CodeInvalidArgument, err)
	}
	if err := config.SetMany(map[string]any{
		metricstore.MetricDownsamplingEnabledKey:              cfg.DownsamplingEnabled,
		metricstore.MetricRollupMinuteRetentionMinutesKey:     cfg.RollupMinuteRetentionMinutes,
		metricstore.MetricRollupFiveMinuteRetentionMinutesKey: cfg.RollupFiveMinuteRetentionMinutes,
		metricstore.MetricRollupHourRetentionHoursKey:         cfg.RollupHourRetentionHours,
	}); err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	if err := metricstore.Reload(ctx); err != nil {
		return nil, connectError(connect.CodeInternal, fmt.Errorf("policy saved but metric store reload failed: %w", err))
	}
	connectAudit(ctx, fmt.Sprintf("set metric downsampling enabled=%t retention=%dm/%dm/%dh", cfg.DownsamplingEnabled, cfg.RollupMinuteRetentionMinutes, cfg.RollupFiveMinuteRetentionMinutes, cfg.RollupHourRetentionHours), "warn")
	policy, err := connectDownsamplingPolicy()
	if err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&metricsv1.SetDownsamplingPolicyResponse{Policy: policy}), nil
}

func (s *metricsService) GetMetricMigrationStatus(context.Context, *connect.Request[metricsv1.GetMetricMigrationStatusRequest]) (*connect.Response[metricsv1.GetMetricMigrationStatusResponse], error) {
	return connect.NewResponse(&metricsv1.GetMetricMigrationStatusResponse{Migration: metricMigrationToProto()}), nil
}

func (s *metricsService) StartMetricMigration(ctx context.Context, req *connect.Request[metricsv1.StartMetricMigrationRequest]) (*connect.Response[metricsv1.StartMetricMigrationResponse], error) {
	if err := metricstore.StartStoreMigration(strings.TrimSpace(req.Msg.SourceDriver), strings.TrimSpace(req.Msg.SourceDsn)); err != nil {
		return nil, connectError(connect.CodeFailedPrecondition, err)
	}
	connectAudit(ctx, "start metrics store migration", "info")
	return connect.NewResponse(&metricsv1.StartMetricMigrationResponse{Migration: metricMigrationToProto()}), nil
}

func (s *metricsService) CancelMetricMigration(ctx context.Context, _ *connect.Request[metricsv1.CancelMetricMigrationRequest]) (*connect.Response[metricsv1.CancelMetricMigrationResponse], error) {
	if err := metricstore.CancelStoreMigration(); err != nil {
		return nil, connectError(connect.CodeFailedPrecondition, err)
	}
	connectAudit(ctx, "cancel metrics store migration", "warn")
	return connect.NewResponse(&metricsv1.CancelMetricMigrationResponse{Migration: metricMigrationToProto()}), nil
}

func connectDownsamplingPolicy() (*metricsv1.DownsamplingPolicy, error) {
	cfg, err := config.GetManyAs[metricstore.MetricStoreConfig]()
	if err != nil {
		return nil, err
	}
	rawRetention := metricstore.DefaultRollupRawRetention
	if !cfg.DownsamplingEnabled {
		rawRetention = metricstore.DefaultRollupMaterializationDelay
	}
	return &metricsv1.DownsamplingPolicy{
		Enabled:                    cfg.DownsamplingEnabled,
		PreserveRaw:                !cfg.DownsamplingEnabled,
		RawRetention:               rawRetention.String(),
		MinuteRetentionMinutes:     uint32(max(cfg.RollupMinuteRetentionMinutes, 0)),
		FiveMinuteRetentionMinutes: uint32(max(cfg.RollupFiveMinuteRetentionMinutes, 0)),
		HourRetentionHours:         uint32(max(cfg.RollupHourRetentionHours, 0)),
		Tiers: []*metricsv1.DownsamplingTier{
			{Interval: "1m", Retention: (time.Duration(cfg.RollupMinuteRetentionMinutes) * time.Minute).String()},
			{Interval: "5m", Retention: (time.Duration(cfg.RollupFiveMinuteRetentionMinutes) * time.Minute).String()},
			{Interval: "1h", Retention: (time.Duration(cfg.RollupHourRetentionHours) * time.Hour).String()},
		},
	}, nil
}

func metricDefinitionToProto(definition metric.Definition) *metricsv1.MetricDefinition {
	return &metricsv1.MetricDefinition{
		Name:          definition.Name,
		Description:   definition.Description,
		Type:          string(definition.Type),
		Unit:          definition.Unit,
		RetentionDays: uint32(max(definition.RetentionDays, 0)),
		Metadata:      definition.Metadata,
		CreatedAt:     timestamppb.New(definition.CreatedAt),
		UpdatedAt:     timestamppb.New(definition.UpdatedAt),
	}
}

func metricMigrationToProto() *metricsv1.MetricMigration {
	progress := metricstore.GetStoreMigrationProgress()
	result := &metricsv1.MetricMigration{
		State:          metricMigrationState(progress.Status),
		IsRunning:      metricstore.IsStoreMigrationRunning(),
		SourceDriver:   progress.SourceDriver,
		SourceDsn:      progress.SourceDSN,
		TargetDriver:   progress.TargetDriver,
		TargetDsn:      progress.TargetDSN,
		TotalMetrics:   uint32(max(progress.TotalMetrics, 0)),
		MetricsDone:    uint32(max(progress.MetricsDone, 0)),
		CurrentMetric:  progress.CurrentMetric,
		MigratedPoints: uint64(max(progress.MigratedPoints, 0)),
		Error:          progress.Error,
	}
	if !progress.StartTime.IsZero() {
		result.StartedAt = timestamppb.New(progress.StartTime)
	}
	if !progress.EndTime.IsZero() {
		result.FinishedAt = timestamppb.New(progress.EndTime)
	}
	return result
}

func metricMigrationState(status string) metricsv1.MetricMigrationState {
	switch status {
	case "running":
		return metricsv1.MetricMigrationState_METRIC_MIGRATION_STATE_RUNNING
	case "completed":
		return metricsv1.MetricMigrationState_METRIC_MIGRATION_STATE_COMPLETED
	case "failed":
		return metricsv1.MetricMigrationState_METRIC_MIGRATION_STATE_FAILED
	case "canceled":
		return metricsv1.MetricMigrationState_METRIC_MIGRATION_STATE_CANCELED
	default:
		return metricsv1.MetricMigrationState_METRIC_MIGRATION_STATE_IDLE
	}
}

func connectAudit(ctx context.Context, action, level string) {
	meta := rpc.MetaFromContext(ctx)
	if meta == nil {
		return
	}
	auditlog.Log(meta.RemoteIP, meta.UserUUID, action, level)
}
