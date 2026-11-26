package connectapi

import (
	"context"
	"database/sql"
	"errors"
	"net/netip"
	"regexp"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/auditlog"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/database/trafficevidence"
	"github.com/komari-monitor/komari/pkg/rpc"
	metrics "github.com/r11234567/komari-proto/gen/go/komari/metrics/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gorm.io/gorm"
)

const maxEvidenceBytes = 64 << 10

var (
	errInvalidEvidence = errors.New("invalid traffic evidence record")
	evidenceReason     = regexp.MustCompile(`^(fast|slow|sustained)_unexplained_(ingress|egress|bidirectional)$`)
	evidenceLabel      = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,31}$`)
)

func (s *metricsService) SubmitTrafficEvidence(ctx context.Context, req *connect.Request[metrics.SubmitTrafficEvidenceRequest]) (*connect.Response[metrics.SubmitTrafficEvidenceResponse], error) {
	agent, err := requireAgent(rpc.MetaFromContext(ctx), req.Msg.AgentId)
	if err != nil {
		return nil, err
	}
	if !validEvidenceID(agent) || proto.Size(req.Msg) > maxEvidenceBytes {
		return nil, connectError(connect.CodeInvalidArgument, errInvalidEvidence)
	}
	store := trafficevidence.Current()
	if store == nil || metricstore.EntityWritesBlocked(agent) {
		return nil, connectError(connect.CodeUnavailable, nil)
	}
	now := time.Now().UTC()
	var kind, id string
	var revision, expires time.Time
	var summary, detail []byte
	switch r := req.Msg.Record.(type) {
	case *metrics.SubmitTrafficEvidenceRequest_Hour:
		if !validEvidenceHour(r.Hour, now) {
			return nil, connectError(connect.CodeInvalidArgument, errInvalidEvidence)
		}
		hour := r.Hour.HourUtc.AsTime()
		kind, id = trafficevidence.Hour, hour.Format(time.RFC3339)
		revision, expires = r.Hour.Through.AsTime(), hour.Add(trafficevidence.HourRetention)
		// Build the stored record from known fields only.
		services := make(map[string]*metrics.TrafficEvidenceBytes, len(r.Hour.Services))
		for name, count := range r.Hour.Services {
			services[name] = cleanEvidenceBytes(count)
		}
		summary, err = proto.Marshal(&metrics.TrafficEvidenceHour{
			HourUtc: timestamppb.New(hour), Through: timestamppb.New(revision), Observed: cleanEvidenceBytes(r.Hour.Observed),
			Services: services, Unexplained: cleanEvidenceBytes(r.Hour.Unexplained), Partial: r.Hour.Partial,
		})
	case *metrics.SubmitTrafficEvidenceRequest_Incident:
		event := r.Incident
		if !validEvidenceIncident(event, now) {
			return nil, connectError(connect.CodeInvalidArgument, errInvalidEvidence)
		}
		kind, id = trafficevidence.Incident, event.Id
		revision = event.End.AsTime()
		expires = revision.Add(trafficevidence.IncidentRetention)
		event = cleanEvidenceIncident(event)
		detail, err = proto.Marshal(event)
		if err == nil {
			event.IngressSources, event.EgressDestinations = nil, nil
			summary, err = proto.Marshal(event)
		}
	default:
		return nil, connectError(connect.CodeInvalidArgument, errInvalidEvidence)
	}
	if err != nil {
		return nil, connectError(connect.CodeInternal, nil)
	}
	if expires.After(now) {
		if err := store.Put(ctx, agent, kind, id, revision, expires, summary, detail); err != nil {
			return nil, connectError(connect.CodeUnavailable, nil)
		}
	}
	return connect.NewResponse(&metrics.SubmitTrafficEvidenceResponse{Accepted: true}), nil
}

func trafficEvidenceReader(ctx context.Context, agent string) (*trafficevidence.Store, error) {
	meta := rpc.MetaFromContext(ctx)
	if meta == nil || meta.Principal == nil || meta.Principal.Type != rpc.PrincipalUser || meta.SessionToken == "" {
		return nil, connectError(connect.CodePermissionDenied, errors.New("an administrator browser session is required"))
	}
	if !validEvidenceID(agent) {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("invalid agent ID"))
	}
	if _, err := clients.GetClientByUUID(agent); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, connectError(connect.CodeNotFound, nil)
		}
		return nil, connectError(connect.CodeUnavailable, nil)
	}
	store := trafficevidence.Current()
	if store == nil {
		return nil, connectError(connect.CodeUnavailable, nil)
	}
	return store, nil
}

func (s *metricsService) ListTrafficEvidence(ctx context.Context, req *connect.Request[metrics.ListTrafficEvidenceRequest]) (*connect.Response[metrics.ListTrafficEvidenceResponse], error) {
	store, err := trafficEvidenceReader(ctx, req.Msg.AgentId)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	hours, err := store.List(ctx, req.Msg.AgentId, trafficevidence.Hour, now)
	if err != nil {
		return nil, connectError(connect.CodeUnavailable, nil)
	}
	incidents, err := store.List(ctx, req.Msg.AgentId, trafficevidence.Incident, now)
	if err != nil {
		return nil, connectError(connect.CodeUnavailable, nil)
	}
	result := &metrics.ListTrafficEvidenceResponse{}
	for _, data := range hours {
		record := new(metrics.TrafficEvidenceHour)
		if err := proto.Unmarshal(data, record); err != nil {
			return nil, connectError(connect.CodeInternal, nil)
		}
		result.Hours = append(result.Hours, record)
	}
	for _, data := range incidents {
		record := new(metrics.TrafficEvidenceIncident)
		if err := proto.Unmarshal(data, record); err != nil {
			return nil, connectError(connect.CodeInternal, nil)
		}
		result.Incidents = append(result.Incidents, record)
	}
	return connect.NewResponse(result), nil
}

func (s *metricsService) GetTrafficEvidenceIncident(ctx context.Context, req *connect.Request[metrics.GetTrafficEvidenceIncidentRequest]) (*connect.Response[metrics.GetTrafficEvidenceIncidentResponse], error) {
	store, err := trafficEvidenceReader(ctx, req.Msg.AgentId)
	if err != nil {
		return nil, err
	}
	if !validEvidenceID(req.Msg.IncidentId) {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("invalid incident ID"))
	}
	data, err := store.Detail(ctx, req.Msg.AgentId, req.Msg.IncidentId, time.Now().UTC())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, connectError(connect.CodeNotFound, nil)
	}
	if err != nil {
		return nil, connectError(connect.CodeUnavailable, nil)
	}
	record := new(metrics.TrafficEvidenceIncident)
	if err := proto.Unmarshal(data, record); err != nil {
		return nil, connectError(connect.CodeInternal, nil)
	}
	meta := rpc.MetaFromContext(ctx)
	auditlog.EventLog("info", "Traffic evidence viewed by "+meta.UserUUID+" for agent "+req.Msg.AgentId+" incident "+req.Msg.IncidentId)
	return connect.NewResponse(&metrics.GetTrafficEvidenceIncidentResponse{Incident: record}), nil
}

func validEvidenceHour(hour *metrics.TrafficEvidenceHour, now time.Time) bool {
	if hour == nil || !validEvidenceTime(hour.HourUtc, now) || !validEvidenceTime(hour.Through, now) {
		return false
	}
	start, end := hour.HourUtc.AsTime(), hour.Through.AsTime()
	if !start.Equal(start.Truncate(time.Hour)) || !end.After(start) || end.After(start.Add(time.Hour)) || len(hour.Services) > 4 {
		return false
	}
	for name, value := range hour.Services {
		switch name {
		case "xray", "backup", "system_update", "other_proxy":
			if value == nil {
				return false
			}
		default:
			return false
		}
	}
	return hour.Unexplained.GetRxBytes() <= hour.Observed.GetRxBytes() && hour.Unexplained.GetTxBytes() <= hour.Observed.GetTxBytes()
}

func validEvidenceIncident(event *metrics.TrafficEvidenceIncident, now time.Time) bool {
	if event == nil || !event.Complete || !validEvidenceID(event.Id) || !evidenceReason.MatchString(event.Reason) ||
		!evidenceLabel.MatchString(event.InterfaceName) || !evidenceLabel.MatchString(event.Sampler) ||
		!validEvidenceTime(event.Start, now) || !validEvidenceTime(event.End, now) || !validEvidenceTime(event.TriggeredAt, now) {
		return false
	}
	start, end, triggered := event.Start.AsTime(), event.End.AsTime(), event.TriggeredAt.AsTime()
	if !end.After(start) || end.Sub(start) > time.Hour || triggered.Before(start) || triggered.After(end) {
		return false
	}
	if (event.FirstPacketAt == nil) != (event.LastPacketAt == nil) {
		return false
	}
	if event.FirstPacketAt != nil && (!validEvidenceTime(event.FirstPacketAt, now) || !validEvidenceTime(event.LastPacketAt, now) ||
		event.LastPacketAt.AsTime().Before(event.FirstPacketAt.AsTime())) {
		return false
	}
	return validEvidencePeers(event.IngressSources, event.Other.GetRxBytes(), event.Observed.GetRxBytes()) &&
		validEvidencePeers(event.EgressDestinations, event.Other.GetTxBytes(), event.Observed.GetTxBytes())
}

func validEvidencePeers(peers []*metrics.TrafficEvidencePeer, other, total uint64) bool {
	if len(peers) > 20 || other > total {
		return false
	}
	remaining := total - other
	seen := make(map[netip.Addr]bool, len(peers))
	for _, peer := range peers {
		if peer == nil || peer.Bytes == 0 || peer.Bytes > remaining {
			return false
		}
		ip, err := netip.ParseAddr(peer.Ip)
		if err != nil || ip.Zone() != "" || ip.Unmap().String() != peer.Ip || seen[ip] {
			return false
		}
		seen[ip] = true
		remaining -= peer.Bytes
	}
	return remaining == 0
}

func validEvidenceTime(t *timestamppb.Timestamp, now time.Time) bool {
	return t != nil && t.CheckValid() == nil && t.AsTime().After(time.Unix(0, 0)) && !t.AsTime().After(now.Add(time.Minute))
}

func validEvidenceID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}

func cleanEvidenceBytes(value *metrics.TrafficEvidenceBytes) *metrics.TrafficEvidenceBytes {
	return &metrics.TrafficEvidenceBytes{RxBytes: value.GetRxBytes(), TxBytes: value.GetTxBytes()}
}

func cleanEvidenceIncident(e *metrics.TrafficEvidenceIncident) *metrics.TrafficEvidenceIncident {
	peers := func(entries []*metrics.TrafficEvidencePeer) []*metrics.TrafficEvidencePeer {
		result := make([]*metrics.TrafficEvidencePeer, 0, len(entries))
		for _, entry := range entries {
			result = append(result, &metrics.TrafficEvidencePeer{Ip: entry.Ip, Bytes: entry.Bytes})
		}
		return result
	}
	result := &metrics.TrafficEvidenceIncident{
		Id: e.Id, Reason: e.Reason, InterfaceName: e.InterfaceName, Sampler: e.Sampler, TriggeredAt: timestamppb.New(e.TriggeredAt.AsTime()),
		Start: timestamppb.New(e.Start.AsTime()), End: timestamppb.New(e.End.AsTime()), FlowsSpanSlices: e.FlowsSpanSlices,
		Observed: cleanEvidenceBytes(e.Observed), TriggerUnexplained: cleanEvidenceBytes(e.TriggerUnexplained),
		UnclassifiedBytes: e.UnclassifiedBytes, Partial: e.Partial, Complete: e.Complete,
		IngressSources: peers(e.IngressSources), EgressDestinations: peers(e.EgressDestinations), Other: cleanEvidenceBytes(e.Other),
	}
	if e.FirstPacketAt != nil {
		result.FirstPacketAt = timestamppb.New(e.FirstPacketAt.AsTime())
		result.LastPacketAt = timestamppb.New(e.LastPacketAt.AsTime())
	}
	return result
}
