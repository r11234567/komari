package connectapi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/pkg/metric"
	"github.com/komari-monitor/komari/pkg/rpc"
	metricsv1 "github.com/r11234567/komari-proto/gen/go/komari/metrics/v1"
)

func (s *metricsService) SubmitTrafficSamples(ctx context.Context, req *connect.Request[metricsv1.SubmitTrafficSamplesRequest]) (*connect.Response[metricsv1.SubmitTrafficSamplesResponse], error) {
	input := req.Msg
	source, err := requireAgent(rpc.MetaFromContext(ctx), input.AgentId)
	if err != nil {
		return nil, err
	}
	store, release, err := metricstore.AcquireStore(ctx)
	if err != nil {
		return nil, connectError(connect.CodeUnavailable, err)
	}
	defer release()
	telemetry := store.Telemetry()
	if telemetry == nil {
		return nil, connectError(connect.CodeUnimplemented, errors.New("traffic samples require the telemetry store"))
	}
	if metricstore.EntityWritesBlocked(source) {
		return nil, connectError(connect.CodeUnavailable, metricstore.ErrMetricWriteBlocked)
	}
	response := &metricsv1.SubmitTrafficSamplesResponse{}
	if len(input.Samples) == 0 {
		return connect.NewResponse(response), nil
	}
	stream, err := uuid.Parse(input.StreamId)
	if err != nil || stream == uuid.Nil || len(input.Samples) > 32 {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("invalid traffic stream or batch size"))
	}
	now := time.Now().UTC()
	var previous uint64
	definitions, err := store.ListMetrics(ctx)
	if err != nil {
		return nil, connectError(connect.CodeUnavailable, err)
	}
	var trafficMetrics []string
	for _, definition := range definitions {
		if definition.RetentionDays > 0 && (definition.Name == metricstore.MetricTrafficUp || definition.Name == metricstore.MetricTrafficDown) {
			trafficMetrics = append(trafficMetrics, definition.Name)
		}
	}
	for _, inputSample := range input.Samples {
		sample, err := decodeTrafficSample(inputSample, stream.String(), now, telemetry.TrafficSampleWindow())
		if err != nil {
			return nil, connectError(connect.CodeInvalidArgument, err)
		}
		if inputSample.Sequence <= previous {
			return nil, connectError(connect.CodeInvalidArgument, errors.New("traffic batch is not in sequence order"))
		}
		previous = inputSample.Sequence
		batch := metric.TelemetryBatch{
			Source: source, Identity: fmt.Sprintf("traffic/%s/%d", stream, sample.Sequence), TrafficSample: &sample,
		}
		for _, name := range trafficMetrics {
			batch.Points = append(batch.Points, metric.Point{MetricName: name, EntityID: source, Timestamp: time.Unix(0, sample.Observed).UTC()})
		}
		if _, err := telemetry.Accept(ctx, batch, now); err != nil {
			switch {
			case errors.Is(err, metric.ErrInvalidArgument):
				return nil, connectError(connect.CodeInvalidArgument, err)
			case errors.Is(err, metric.ErrTelemetryConflict):
				return nil, connectError(connect.CodeFailedPrecondition, err)
			default:
				if response.AcceptedSequence != 0 {
					return connect.NewResponse(response), nil
				}
				return nil, connectError(connect.CodeUnavailable, err)
			}
		}
		response.AcceptedSequence = previous
	}
	return connect.NewResponse(response), nil
}

func decodeTrafficSample(input *metricsv1.TrafficSample, stream string, now time.Time, window time.Duration) (metric.TrafficSample, error) {
	result := metric.TrafficSample{Stream: stream}
	if input == nil || input.Sequence == 0 || input.Sequence > math.MaxInt64 || input.ObservedAt == nil || input.ObservedAt.CheckValid() != nil || len(input.Interfaces) > 64 || len(input.BootId) > 128 {
		return result, errors.New("invalid traffic sample")
	}
	observed := input.ObservedAt.AsTime()
	if observed.Before(now.Add(-window)) || observed.After(now.Add(30*time.Second)) {
		return result, errors.New("traffic sample is outside the acceptance window")
	}
	result.Sequence, result.Observed, result.BootID = int64(input.Sequence), observed.UnixNano(), input.BootId
	if input.BootedAt != nil {
		if input.BootedAt.CheckValid() != nil || input.BootedAt.AsTime().After(observed) || input.BootedAt.Seconds < 0 {
			return result, errors.New("invalid traffic boot time")
		}
		result.Booted = input.BootedAt.AsTime().UnixNano()
	}
	if input.GapStartedAt != nil {
		if input.GapStartedAt.CheckValid() != nil || input.GapStartedAt.AsTime().After(observed) || input.GapStartedAt.Seconds < 0 {
			return result, errors.New("invalid traffic gap time")
		}
		result.GapStart = input.GapStartedAt.AsTime().UnixNano()
	}
	result.Interfaces = make(map[string]metric.TrafficCounters, len(input.Interfaces))
	for _, nic := range input.Interfaces {
		if nic == nil || nic.InterfaceId == "" || len(nic.InterfaceId) > 128 || nic.SentBytes > math.MaxInt64 || nic.ReceivedBytes > math.MaxInt64 {
			return result, errors.New("invalid traffic interface sample")
		}
		if _, duplicate := result.Interfaces[nic.InterfaceId]; duplicate {
			return result, errors.New("duplicate traffic interface")
		}
		result.Interfaces[nic.InterfaceId] = metric.TrafficCounters{Sent: int64(nic.SentBytes), Received: int64(nic.ReceivedBytes)}
	}
	return result, nil
}
