package historyexport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"github.com/komari-monitor/komari/pkg/metric"
)

type Worker struct {
	Queue          *Queue
	InstanceCgroup string
	// Fault and Verify are process-boundary test hooks, never HTTP settings.
	Fault            func(string) error
	Verify           func() (ResourceProof, error)
	lastOwner        string
	IOBytesPerSecond int64
	io               *ioBudget
	activeJob        string
	RequirePolicy    bool
	resources        resourceSampler
}
type Availability struct {
	Available     bool           `json:"available"`
	Error         string         `json:"error,omitempty"`
	At            time.Time      `json:"at"`
	CPUCoreLimit  float64        `json:"cpu_core_limit"`
	CPUShareLimit float64        `json:"cpu_share_limit"`
	Proof         ResourceProof  `json:"proof"`
	ActiveJob     string         `json:"active_job,omitempty"`
	Resources     *ResourceUsage `json:"resources,omitempty"`
}

const maxExportInput = 2 << 30
const maxExportOutput = 8 << 30

func atomicJSON(path string, value any) error {
	b, e := json.Marshal(value)
	if e != nil {
		return e
	}
	f, e := os.OpenFile(path+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	_, e = f.Write(b)
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = os.Rename(path+".tmp", path); e != nil {
		return e
	}
	return syncDir(filepath.Dir(path))
}
func syncDir(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func (q *Queue) Availability() Availability {
	var a Availability
	b, e := os.ReadFile(filepath.Join(q.Dir, "worker.json"))
	if e == nil {
		e = json.Unmarshal(b, &a)
	}
	if e != nil || time.Since(a.At) > 15*time.Second {
		a.Available = false
		a.Error = "strict background export worker unavailable; configure the isolated Linux worker with a verified CPU cap"
	}
	return a
}
func (w *Worker) proof() (ResourceProof, error) {
	if w.Verify != nil {
		return w.Verify()
	}
	self, e := SelfCgroup()
	if e != nil {
		return ResourceProof{}, e
	}
	return VerifyResources(self, w.InstanceCgroup)
}
func (w *Worker) fault(s string) error {
	if w.Fault != nil {
		return w.Fault(s)
	}
	return nil
}
func (w *Worker) heartbeat(ctx context.Context) error {
	release, err := w.Queue.guard(ctx)
	if err != nil {
		return err
	}
	defer release()
	p, e := w.proof()
	a := Availability{Available: e == nil, At: time.Now().UTC(), CPUCoreLimit: p.WorkerCPUs, CPUShareLimit: .1, Proof: p, ActiveJob: w.activeJob}
	if e == nil {
		a.Resources = w.observeResources(p, a.At)
	}
	if e != nil {
		a.Error = e.Error()
	}
	if err := atomicJSON(filepath.Join(w.Queue.Dir, "worker.json"), a); err != nil {
		return err
	}
	return e
}

func (w *Worker) Run(ctx context.Context) error {
	if _, e := w.proof(); e != nil {
		return e
	}
	release, e := lockWorker(w.Queue.Dir)
	if e != nil {
		return e
	}
	defer release()
	runtime.GOMAXPROCS(1)
	debug.SetMemoryLimit(90 << 20)
	if e = w.Queue.Recover(ctx); e != nil {
		return e
	}
	lastPrune := time.Time{}
	for {
		if e = ctx.Err(); e != nil {
			return e
		}
		if e = w.heartbeat(ctx); e != nil {
			return e
		}
		if time.Since(lastPrune) > time.Hour {
			if e = w.Queue.Prune(ctx, time.Now()); e != nil {
				return e
			}
			lastPrune = time.Now()
		}
		job, err := w.Queue.Next(ctx, w.lastOwner)
		if err != nil {
			return err
		}
		if job == nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		}
		w.lastOwner = job.Owner
		if err = w.Step(ctx, job); err != nil {
			return err
		}
	}
}

// Step executes one immutable input unit, then gives another account a turn.
// The complete process, including planning and checkpoints, shares one cgroup.
func (w *Worker) Step(parent context.Context, j *Job) error {
	release, lockErr := w.Queue.guard(parent)
	if lockErr != nil {
		return lockErr
	}
	defer release()
	if _, e := w.proof(); e != nil {
		return e
	}
	w.activeJob = j.ID
	defer func() { w.activeJob = "" }()
	control, e := w.Queue.Desired(parent, j.ID)
	if e != nil {
		return e
	}
	if control == "pause" || control == "cancel" {
		j.Status = "paused"
		j.WaitReason = "pause"
		if control == "cancel" {
			j.Status = "cancelled"
			j.Phase = "cancelled"
			j.WaitReason = ""
			w.removeWork(j)
		}
		return w.Queue.Save(parent, j)
	}
	if err := w.checkPolicy(parent, j); err != nil {
		var pause *resourcePause
		if errors.As(err, &pause) {
			j.Status = "paused"
			j.WaitReason = pause.reason
		} else {
			j.Status = "cancelled"
			j.Phase = "cancelled"
			j.Error = err.Error()
			w.removeWork(j)
		}
		if e = w.Queue.Save(parent, j); e != nil {
			return e
		}
		select {
		case <-parent.Done():
			return parent.Err()
		case <-time.After(time.Second):
			return nil
		}
	}
	ctx, cancel := context.WithCancelCause(parent)
	if w.Verify == nil {
		if w.io == nil {
			rate := w.IOBytesPerSecond
			if rate == 0 {
				rate = 8 << 20
			}
			if rate < 64<<10 || rate > 64<<20 {
				cancel(nil)
				return errors.New("I/O budget must be between 64 KiB/s and 64 MiB/s")
			}
			w.io = &ioBudget{rate: rate}
		}
		ctx = metric.WithTelemetryWorkYield(ctx, w.io.wait)
	}
	defer cancel(nil)
	var reasonMu sync.Mutex
	reason := ""
	monitorDone := make(chan struct{})
	defer func() { cancel(nil); <-monitorDone }()
	go func() {
		defer close(monitorDone)
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		lastProof := time.Time{}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				control, e := w.Queue.Desired(ctx, j.ID)
				if e != nil {
					cancel(e)
					return
				}
				if control == "pause" || control == "cancel" {
					reasonMu.Lock()
					reason = control
					reasonMu.Unlock()
					cancel(context.Canceled)
					return
				}
				if time.Since(lastProof) > time.Second {
					if e = w.checkPolicy(ctx, j); e != nil {
						reasonMu.Lock()
						reason = "source_invalidated"
						var pause *resourcePause
						if errors.As(e, &pause) {
							reason = pause.reason
						}
						reasonMu.Unlock()
						cancel(e)
						return
					}
					if e = w.heartbeat(ctx); e != nil {
						reasonMu.Lock()
						reason = "quota_configuration"
						reasonMu.Unlock()
						cancel(e)
						return
					}
					lastProof = time.Now()
				}
			}
		}
	}()
	recovering := j.Status == "recovering" || j.Status == "retry_wait"
	if !recovering && j.Status != "publishing" {
		j.Status = "running"
	}
	j.WaitReason = ""
	j.Error = ""
	if j.StartedAt.IsZero() {
		j.StartedAt = time.Now().UTC()
	}
	j.Heartbeat = time.Now().UTC()
	if e := w.Queue.Save(ctx, j); e != nil {
		return e
	}
	var err error
	if recovering {
		err = w.verifyCheckpoint(ctx, j)
		if err == nil {
			j.Status, j.WaitReason = "running", ""
			err = w.Queue.Save(ctx, j)
		}
	}
	if err == nil {
		err = w.step(ctx, j)
	}
	if err == nil {
		return nil
	}
	reasonMu.Lock()
	why := reason
	reasonMu.Unlock()
	if errors.Is(err, errExportInvalidated) {
		why = "source_invalidated"
	}
	var resource *resourcePause
	if why == "" && errors.As(err, &resource) {
		why = resource.reason
	}
	// A final publication fence can observe control before the monitor tick.
	// Resolve it synchronously so a successful cancellation never becomes failure.
	if why == "" && j.Status != "publishing" {
		if desired, e := w.Queue.Desired(context.Background(), j.ID); e == nil && (desired == "pause" || desired == "cancel") {
			why = desired
		}
	}
	if parent.Err() != nil {
		// Stop/restart is not task failure. The committed output survives and
		// the next exclusive worker truncates the unconfirmed tail.
		return parent.Err()
	}
	switch why {
	case "pause", "quota_configuration", "instance_state_unavailable", "foreground_latency", "ingestion_backlog", "telemetry_backlog", "storage_unavailable", "disk_space":
		j.Status = "paused"
		j.WaitReason = why
	case "cancel":
		j.Status = "cancelled"
		j.Phase = "cancelled"
		j.WaitReason = ""
		w.removeWork(j)
	case "source_invalidated":
		j.Status = "cancelled"
		j.Phase = "cancelled"
		j.Error = errExportInvalidated.Error()
		w.removeWork(j)
	default:
		j.Status = "failed"
		j.Error = err.Error()
		j.WaitReason = ""
		if transientDatabaseBusy(err) && j.AutomaticRetries < 3 {
			j.Status, j.WaitReason, j.ProgressKind = "retry_wait", "database_busy", "indeterminate"
			j.AutomaticRetries++
			j.RetryAt = time.Now().UTC().Add(time.Duration(5*(1<<uint(j.AutomaticRetries-1))) * time.Second)
		}
	}
	return w.Queue.Save(context.Background(), j)
}
func (w *Worker) removeWork(j *Job) {
	for _, suffix := range []string{".csv.part", ".snapshot", ".spool.db", ".spool.db-wal", ".spool.db-shm"} {
		p := w.Queue.Path(j.ID, suffix)
		if suffix == ".snapshot" {
			_ = os.RemoveAll(p)
		} else {
			_ = os.Remove(p)
		}
	}
}
func (w *Worker) space(j *Job) error {
	free, e := diskAvailable(w.Queue.Dir)
	if e != nil {
		return e
	}
	if free < 256<<20 {
		return &resourcePause{reason: "disk_space"}
	}
	if j.OutputBytes > maxExportOutput {
		return errors.New("export output exceeds 8 GiB; split the requested range")
	}
	var temporary int64
	for _, suffix := range []string{".spool.db", ".spool.db-wal", ".spool.db-shm"} {
		if info, e := os.Stat(w.Queue.Path(j.ID, suffix)); e == nil {
			temporary += info.Size()
		} else if !os.IsNotExist(e) {
			return e
		}
	}
	if temporary > 4<<30 {
		return errors.New("sample spool exceeds 4 GiB; split the requested range")
	}
	return nil
}
func (w *Worker) phase(ctx context.Context, j *Job, phase, kind string, total int64) error {
	if j.Phase != phase || j.PhaseTotal != total {
		j.PhaseCompleted = 0
	}
	j.Phase = phase
	j.ProgressKind = kind
	j.PhaseTotal = total
	if kind != "input_units" {
		j.CurrentInput = nil
	}
	j.Heartbeat = time.Now().UTC()
	return w.Queue.Save(ctx, j)
}
func (w *Worker) snapshot(ctx context.Context, j *Job) (metric.ExportSnapshot, error) {
	dir := w.Queue.Path(j.ID, ".snapshot")
	path := filepath.Join(dir, "manifest.json")
	var snapshot metric.ExportSnapshot
	if info, e := os.Lstat(path); e == nil && (!info.Mode().IsRegular() || info.Size() > 16<<20) {
		return snapshot, errors.New("invalid manifest size/type")
	}
	if b, e := os.ReadFile(path); e == nil {
		if len(b) > 16<<20 {
			return snapshot, errors.New("manifest exceeds budget")
		}
		if e = json.Unmarshal(b, &snapshot); e != nil {
			return snapshot, e
		}
		if snapshot.Version != 1 {
			return snapshot, errors.New("unsupported manifest version")
		}
		if w.Verify == nil && (snapshot.Instance != j.Source.Instance || snapshot.Epoch != j.Source.Epoch) {
			return snapshot, errExportInvalidated
		}
		h := sha256.Sum256(b)
		j.Generation = hex.EncodeToString(h[:])
		return snapshot, nil
	} else if !os.IsNotExist(e) {
		return snapshot, e
	}
	if e := w.phase(ctx, j, "planning", "indeterminate", 0); e != nil {
		return snapshot, e
	}
	// A planning attempt is not acknowledged input. Interrupted partial freezes
	// are discarded; committed manifests and output checkpoints are preserved.
	if e := os.RemoveAll(dir); e != nil {
		return snapshot, e
	}
	if e := os.MkdirAll(dir, 0700); e != nil {
		return snapshot, e
	}
	if e := w.space(j); e != nil {
		return snapshot, e
	}
	cfg := metric.SQLite(j.Source.DSN, metric.WithAutoMigrate(false), metric.WithRetiredLegacyStorage(), metric.WithTablePrefix(j.Source.Prefix), metric.WithMaxOpenConns(1), metric.WithSQLiteProfile(metric.SQLiteProfileDurable), metric.WithSQLiteCacheSizeKB(2048), metric.WithSQLiteReadCacheSizeKB(2048), metric.WithSQLiteReadPool(1), metric.WithSQLiteMMapSize(0))
	s, e := metric.Open(ctx, cfg)
	if e != nil {
		return snapshot, e
	}
	defer s.Close()
	telemetry, e := s.OpenTelemetry(ctx, j.Source.Telemetry)
	if e != nil {
		return snapshot, e
	}
	defer telemetry.Close()
	state, e := telemetry.ExportState(ctx)
	if e != nil {
		return snapshot, e
	}
	if w.Verify == nil && (state.Instance != j.Source.Instance || state.Epoch != j.Source.Epoch) {
		return snapshot, errExportInvalidated
	}
	ctx = metric.WithTelemetryEntities(ctx, []string{j.UUID})
	queries := make([]metric.Query, 0, len(j.Metrics))
	for _, name := range j.Metrics {
		queries = append(queries, metric.Query{MetricName: name, EntityID: j.UUID, Start: j.Start, End: j.End})
	}
	if j.Mode == "summary" {
		qs := make([]metric.AggregateQuery, len(queries))
		for i, q := range queries {
			qs[i] = metric.AggregateQuery{Query: q, Aggregation: j.Aggregation, Interval: j.Interval, PreserveSeries: true}
		}
		r, e := telemetry.ChartBatchBounded(ctx, qs, time.Now(), 20000, 8<<20)
		if e != nil {
			return snapshot, e
		}
		pointCount := 0
		for _, series := range r {
			pointCount += len(series.Points)
		}
		if pointCount > 20000 {
			return snapshot, errors.New("summary manifest exceeds 20000 points; use a larger interval")
		}
		snapshot = metric.ExportSnapshot{Version: 1, Queries: queries, Summary: r, UnitsByMetric: map[string]string{}, Instance: state.Instance, Epoch: state.Epoch}
		defs, e := s.ListMetrics(ctx)
		if e != nil {
			return snapshot, e
		}
		for _, d := range defs {
			snapshot.UnitsByMetric[d.Name] = d.Unit
		}
	} else {
		freezeCtx, cancel := context.WithTimeout(ctx, 9*time.Minute)
		defer cancel()
		snapshot, e = telemetry.FreezeExport(freezeCtx, queries, dir, maxExportInput)
		if e != nil {
			return snapshot, e
		}
	}
	if w.Verify == nil && (snapshot.Instance != j.Source.Instance || snapshot.Epoch != j.Source.Epoch) {
		return snapshot, errExportInvalidated
	}
	if e = w.fault("snapshot-before-manifest"); e != nil {
		return snapshot, e
	}
	if e = atomicJSON(path, snapshot); e != nil {
		return snapshot, e
	}
	h := sha256.New()
	b, _ := json.Marshal(snapshot)
	h.Write(b)
	j.Generation = hex.EncodeToString(h.Sum(nil))
	j.LastProgress = time.Now().UTC()
	if e = w.Queue.Save(ctx, j); e != nil {
		return snapshot, e
	}
	return snapshot, nil
}

type outputWriter struct {
	f          *os.File
	h          hash.Hash
	n          int64
	sampleSlot int64
	ctx        context.Context
	budget     *ioBudget
}

// Validate only the durable prefix on process recovery, before truncating any
// unconfirmed tail. Missing bytes and same-size corruption must never become a
// downloadable CSV. This read shares the worker's CPU and logical I/O budgets.
func (w *Worker) verifyCheckpoint(ctx context.Context, j *Job) error {
	if j.Committed.Offset == 0 {
		return nil
	}
	suffix := ".csv.part"
	if j.Phase == "publishing" {
		if _, e := os.Stat(w.Queue.Path(j.ID, ".csv")); e == nil {
			suffix = ".csv"
		}
	}
	f, err := os.Open(w.Queue.Path(j.ID, suffix))
	if err != nil {
		return fmt.Errorf("checkpoint output unavailable: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() < j.Committed.Offset || (suffix == ".csv" && info.Size() != j.Committed.Offset) {
		return errors.New("checkpoint output size mismatch")
	}
	expected := sha256.New()
	if len(j.Committed.Hash) == 0 {
		return errors.New("checkpoint hash missing")
	}
	if err = expected.(encoding.BinaryUnmarshaler).UnmarshalBinary(j.Committed.Hash); err != nil {
		return err
	}
	h := sha256.New()
	buf := make([]byte, 64<<10)
	remaining := j.Committed.Offset
	for remaining > 0 {
		if err = ctx.Err(); err != nil {
			return err
		}
		n, e := io.ReadFull(f, buf[:min(int64(len(buf)), remaining)])
		if e != nil {
			return e
		}
		h.Write(buf[:n])
		remaining -= int64(n)
		if w.io != nil {
			if err = w.io.wait(ctx); err != nil {
				return err
			}
		}
	}
	if !bytes.Equal(h.Sum(nil), expected.Sum(nil)) {
		return errors.New("checkpoint output checksum mismatch")
	}
	return nil
}

func (o *outputWriter) Write(b []byte) (int, error) {
	if o.budget != nil {
		if e := o.budget.wait(o.ctx); e != nil {
			return 0, e
		}
	}
	if o.n+int64(len(b)) > maxExportOutput {
		return 0, errors.New("CSV output exceeds 8 GiB budget")
	}
	n, e := o.f.Write(b)
	if n > 0 {
		o.h.Write(b[:n])
		o.n += int64(n)
	}
	return n, e
}
func (w *Worker) output(ctx context.Context, j *Job) (*outputWriter, *csv.Writer, error) {
	f, e := os.OpenFile(w.Queue.Path(j.ID, ".csv.part"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, nil, e
	}
	info, e := f.Stat()
	if e != nil || info.Size() < j.Committed.Offset {
		f.Close()
		if e != nil {
			return nil, nil, e
		}
		return nil, nil, errors.New("export checkpoint output is missing or truncated; retry from a new snapshot")
	}
	if e = f.Truncate(j.Committed.Offset); e != nil {
		f.Close()
		return nil, nil, e
	}
	if _, e = f.Seek(j.Committed.Offset, io.SeekStart); e != nil {
		f.Close()
		return nil, nil, e
	}
	h := sha256.New()
	if len(j.Committed.Hash) > 0 {
		if e = h.(encoding.BinaryUnmarshaler).UnmarshalBinary(j.Committed.Hash); e != nil {
			f.Close()
			return nil, nil, e
		}
	}
	j.OutputBytes = j.Committed.Offset
	j.WrittenRows = j.Committed.Rows
	o := &outputWriter{f: f, h: h, n: j.Committed.Offset, sampleSlot: j.Committed.Slot, ctx: ctx, budget: w.io}
	return o, csv.NewWriter(o), nil
}
func (w *Worker) checkpoint(ctx context.Context, j *Job, o *outputWriter, c *csv.Writer, unit int) error {
	c.Flush()
	if e := c.Error(); e != nil {
		return e
	}
	if e := w.fault("checkpoint-before-sync"); e != nil {
		return e
	}
	if e := o.f.Sync(); e != nil {
		return e
	}
	if e := w.fault("checkpoint-after-sync"); e != nil {
		return e
	}
	b, e := o.h.(encoding.BinaryMarshaler).MarshalBinary()
	if e != nil {
		return e
	}
	j.Committed = Commit{Unit: unit, Offset: o.n, Rows: j.WrittenRows, Decoded: j.DecodedRecords, Hash: b, SampleOutput: j.Committed.SampleOutput, Slot: o.sampleSlot}
	j.Checkpoint = unit
	j.OutputBytes = o.n
	j.LastProgress = time.Now().UTC()
	j.Heartbeat = j.LastProgress
	if e = w.Queue.Save(ctx, j); e != nil {
		return e
	}
	return w.fault("checkpoint-after-commit")
}
func (w *Worker) step(ctx context.Context, j *Job) error {
	if e := w.space(j); e != nil {
		return e
	}
	// Recover a crash after durable rename but before ready state commit. Only
	// this worker can write published files in the private queue directory.
	if j.Phase == "publishing" {
		if info, e := os.Stat(w.Queue.Path(j.ID, ".csv")); e == nil && info.Size() == j.Committed.Offset {
			return w.ready(ctx, j)
		}
	}
	snapshot, e := w.snapshot(ctx, j)
	if e != nil {
		return e
	}
	j.CompletedInputBytes = 0
	completed := j.Committed.Unit
	if j.Mode == "sample30s" {
		completed = j.Checkpoint
	}
	for i := 0; i < completed && i < len(snapshot.Units); i++ {
		j.CompletedInputBytes += snapshot.Units[i].Segment.FileBytes
	}
	if j.Mode == "sample30s" {
		return w.sample(ctx, j, snapshot)
	}
	o, c, e := w.output(ctx, j)
	if e != nil {
		return e
	}
	defer o.f.Close()
	if j.Committed.Offset == 0 {
		if _, e = o.Write([]byte{0xef, 0xbb, 0xbf}); e != nil {
			return e
		}
		header := []string{"observation_id", "observed_at", "received_at", "entity_id", "metric", "tags", "labels", "value", "unit"}
		if j.Mode == "summary" {
			header = []string{"bucket_start", "bucket_end", "entity_id", "metric", "tags", "value", "unit", "aggregation", "sample_count", "output_interval_seconds", "source_precision", "coverage", "coverage_metadata"}
		}
		if e = c.Write(header); e != nil {
			return e
		}
		if e = w.checkpoint(ctx, j, o, c, 0); e != nil {
			return e
		}
	}
	if j.Mode == "summary" {
		if e = w.phase(ctx, j, "generating_summary", "units", int64(len(snapshot.Summary))); e != nil {
			return e
		}
		if j.Committed.Unit < len(snapshot.Summary) {
			i := j.Committed.Unit
			r := snapshot.Summary[i]
			metadata, _ := json.Marshal(struct {
				Sources, Gaps        []metric.TelemetrySourceSpan
				Accepted, Summarized int64
			}{r.Sources, r.Gaps, r.AcceptedThrough, r.AccountedThrough})
			for _, p := range r.Points {
				if e = ctx.Err(); e != nil {
					return e
				}
				tags, _ := json.Marshal(p.Tags)
				coverage := "complete"
				if len(r.Gaps) > 0 {
					coverage = "partial"
				}
				if e = c.Write([]string{p.Bucket.UTC().Format(time.RFC3339Nano), p.Bucket.Add(j.Interval).UTC().Format(time.RFC3339Nano), p.EntityID, p.MetricName, string(tags), strconv.FormatFloat(p.Value, 'g', -1, 64), snapshot.UnitsByMetric[p.MetricName], string(j.Aggregation), strconv.Itoa(p.Count), strconv.FormatFloat(j.Interval.Seconds(), 'f', 0, 64), r.Resolution.String(), coverage, string(metadata)}); e != nil {
					return e
				}
				j.WrittenRows++
			}
			j.PhaseCompleted = int64(i + 1)
			if e = w.checkpoint(ctx, j, o, c, i+1); e != nil {
				return e
			}
			if i+1 < len(snapshot.Summary) {
				return nil
			}
		}
	} else {
		j.inputProgress(snapshot, j.Committed.Unit)
		if e = w.phase(ctx, j, "decoding_and_generating", "input_units", int64(len(snapshot.Units))); e != nil {
			return e
		}
		j.PhaseCompleted = int64(j.Committed.Unit)
		if j.Committed.Unit < len(snapshot.Units) {
			i := j.Committed.Unit
			lastSave := time.Now()
			e = metric.VisitExportInputProgress(metric.WithTelemetryEntities(ctx, []string{j.UUID}), w.Queue.Path(j.ID, ".snapshot"), snapshot, i, func(records, bytes int64) { j.DecodedRecords += records; j.ScannedInputBytes += bytes }, func(ob metric.ExportObservation) error {
				if e := ctx.Err(); e != nil {
					return e
				}
				p := ob.Point
				tags, _ := json.Marshal(p.Tags)
				labels, _ := json.Marshal(p.Labels)
				received := ""
				if !ob.Received.IsZero() {
					received = ob.Received.UTC().Format(time.RFC3339Nano)
				}
				if e := c.Write([]string{ob.Identity, p.Timestamp.UTC().Format(time.RFC3339Nano), received, p.EntityID, p.MetricName, string(tags), string(labels), strconv.FormatFloat(p.Value, 'g', -1, 64), ob.Unit}); e != nil {
					return e
				}
				j.WrittenRows++
				if time.Since(lastSave) > 5*time.Second {
					c.Flush()
					if e := c.Error(); e != nil {
						return e
					}
					j.OutputBytes = o.n
					j.Heartbeat = time.Now().UTC()
					j.LastProgress = j.Heartbeat
					if e := w.Queue.Save(ctx, j); e != nil {
						return e
					}
					if e := w.space(j); e != nil {
						return e
					}
					lastSave = time.Now()
				}
				return nil
			})
			if e != nil {
				return e
			}
			j.CompletedInputBytes += snapshot.Units[i].Segment.FileBytes
			j.PhaseCompleted = int64(i + 1)
			if e = w.checkpoint(ctx, j, o, c, i+1); e != nil {
				return e
			}
			if i+1 < len(snapshot.Units) {
				return nil
			}
		}
	}
	return w.publish(ctx, j, o, c)
}
func (w *Worker) publish(ctx context.Context, j *Job, o *outputWriter, c *csv.Writer) (e error) {
	if e = wPhase(w, ctx, j, "flushing"); e != nil {
		return e
	}
	if e := w.checkpoint(ctx, j, o, c, j.Committed.Unit); e != nil {
		return e
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	if e = wPhase(w, ctx, j, "syncing"); e != nil {
		return e
	}
	if e := o.f.Sync(); e != nil {
		return e
	}
	j.SHA256 = hex.EncodeToString(o.h.Sum(nil))
	if e := o.f.Close(); e != nil {
		return e
	}
	if e = w.phase(ctx, j, "publishing", "indeterminate", 0); e != nil {
		return e
	}
	if e = w.fault("publish-before-rename"); e != nil {
		return e
	}
	if e = w.Queue.FencePublication(ctx, j); e != nil {
		return e
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	if e = os.Rename(w.Queue.Path(j.ID, ".csv.part"), w.Queue.Path(j.ID, ".csv")); e != nil {
		return e
	}
	if e = syncDir(w.Queue.Dir); e != nil {
		return e
	}
	if e = w.fault("publish-after-rename"); e != nil {
		return e
	}
	return w.ready(ctx, j)
}
func wPhase(w *Worker, ctx context.Context, j *Job, phase string) error {
	return w.phase(ctx, j, phase, "indeterminate", 0)
}
func (w *Worker) ready(ctx context.Context, j *Job) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	j.Status = "done"
	j.Phase = "ready"
	j.ProgressKind = "complete"
	j.ReadyAt = time.Now().UTC()
	j.ExpiresAt = j.ReadyAt.Add(TTL)
	j.Filename = j.MakeFilename()
	j.OutputBytes = j.Committed.Offset
	j.LastProgress = j.ReadyAt
	j.Heartbeat = j.ReadyAt
	if e := w.Queue.Save(ctx, j); e != nil {
		return e
	}
	w.removeWork(j)
	return nil
}

// Compatibility sampling uses a disk-backed, bounded-cache spool. All inputs
// are decoded once; output is assembled once with a fixed header. Ties select
// the earliest actual observation, then ingest/ordinal, never an average.
func (w *Worker) sample(ctx context.Context, j *Job, snapshot metric.ExportSnapshot) error {
	spool, e := sql.Open("sqlite3", "file:"+filepath.ToSlash(w.Queue.Path(j.ID, ".spool.db"))+"?_busy_timeout=1000&_journal_mode=WAL&_synchronous=FULL&_cache_size=-2048")
	if e != nil {
		return e
	}
	defer spool.Close()
	spool.SetMaxOpenConns(1)
	if _, e = spool.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS samples(slot INTEGER,metric TEXT,tags TEXT,observed INTEGER,ingest INTEGER,ordinal INTEGER,value REAL,unit TEXT,PRIMARY KEY(slot,metric,tags)) WITHOUT ROWID; PRAGMA temp_store=FILE;`); e != nil {
		return e
	}
	if j.Checkpoint < len(snapshot.Units) {
		j.inputProgress(snapshot, j.Checkpoint)
		if e = w.phase(ctx, j, "decoding_and_sampling", "input_units", int64(len(snapshot.Units))); e != nil {
			return e
		}
		j.PhaseCompleted = int64(j.Checkpoint)
		i := j.Checkpoint
		tx, e := spool.BeginTx(ctx, nil)
		if e != nil {
			return e
		}
		defer func() {
			if tx != nil {
				tx.Rollback()
			}
		}()
		pending := 0
		e = metric.VisitExportInputProgress(metric.WithTelemetryEntities(ctx, []string{j.UUID}), w.Queue.Path(j.ID, ".snapshot"), snapshot, i, func(records, bytes int64) { j.DecodedRecords += records; j.ScannedInputBytes += bytes }, func(ob metric.ExportObservation) error {
			if e := ctx.Err(); e != nil {
				return e
			}
			p := ob.Point
			tags, _ := json.Marshal(p.Tags)
			slot := metric.TelemetryBucketStart(p.Timestamp, 30*time.Second).UnixNano()
			_, e := tx.ExecContext(ctx, `INSERT INTO samples(slot,metric,tags,observed,ingest,ordinal,value,unit) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(slot,metric,tags) DO UPDATE SET observed=excluded.observed,ingest=excluded.ingest,ordinal=excluded.ordinal,value=excluded.value,unit=excluded.unit WHERE (excluded.observed,excluded.ingest,excluded.ordinal)<(samples.observed,samples.ingest,samples.ordinal)`, slot, p.MetricName, string(tags), p.Timestamp.UnixNano(), ob.Ingest, ob.Ordinal, p.Value, ob.Unit)
			if e != nil {
				return e
			}
			pending++
			if pending >= 128 {
				if e = w.space(j); e != nil {
					return e
				}
				if w.io != nil {
					if e = w.io.wait(ctx); e != nil {
						return e
					}
				}
				pending = 0
			}
			return nil
		})
		if e != nil {
			return e
		}
		// Do not commit candidates from a unit whose checksum is unverified.
		// This transaction touches only the private spool, never the live DB.
		if e = w.fault("sample-unit-before-commit"); e != nil {
			return e
		}
		if e = tx.Commit(); e != nil {
			return e
		}
		if e = w.fault("sample-unit-after-commit"); e != nil {
			return e
		}
		j.Checkpoint = i + 1
		j.PhaseCompleted = int64(i + 1)
		j.CompletedInputBytes += snapshot.Units[i].Segment.FileBytes
		j.LastProgress = time.Now().UTC()
		if e = w.Queue.Save(ctx, j); e != nil {
			return e
		}
		if i+1 < len(snapshot.Units) {
			return nil
		}
	}
	if e = w.phase(ctx, j, "assembling_sampled_rows", "indeterminate", 0); e != nil {
		return e
	}
	j.CurrentInput = nil
	// Re-assembly after interruption starts at a deterministic spool boundary;
	// it never decodes the archive again and never appends duplicate CSV rows.
	if !j.Committed.SampleOutput {
		j.Committed = Commit{SampleOutput: true, Slot: -1}
	}
	o, c, e := w.output(ctx, j)
	if e != nil {
		return e
	}
	defer o.f.Close()
	if j.Committed.Offset == 0 {
		if _, e = o.Write([]byte{0xef, 0xbb, 0xbf}); e != nil {
			return e
		}
	}
	rows, e := spool.QueryContext(ctx, "SELECT DISTINCT metric,tags,unit FROM samples ORDER BY metric,tags LIMIT 2049")
	if e != nil {
		return e
	}
	keys := []string{}
	header := []string{"Server", "Time (UTC)"}
	positions := map[string]int{}
	var legacy []legacyColumn
	for rows.Next() {
		var name, tags, unit string
		if e = rows.Scan(&name, &tags, &unit); e != nil {
			break
		}
		key := name + "\x00" + tags
		positions[key] = len(header)
		keys = append(keys, key)
		header = append(header, name+" "+tags+" ("+unit+")")
	}
	re := rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	if re != nil {
		return re
	}
	if len(keys) > 2048 {
		return errors.New("sampled wide CSV exceeds 2048-column budget")
	}
	if j.FormatVersion == 1 {
		var grouped bool
		legacy, grouped, e = legacyColumns(ctx, spool, j)
		if e != nil {
			return e
		}
		header = []string{"Server", "Time (UTC)"}
		groups := []string{"", ""}
		for _, column := range legacy {
			header = append(header, column.label)
			groups = append(groups, column.group)
		}
		if len(legacy) > 2048 {
			return errors.New("legacy CSV exceeds 2048-column budget")
		}
		if grouped && j.Committed.Offset == 0 {
			if e = c.Write(groups); e != nil {
				return e
			}
		}
	}
	if j.Committed.Offset == 0 {
		if e = c.Write(header); e != nil {
			return e
		}
		if e = w.checkpoint(ctx, j, o, c, len(snapshot.Units)); e != nil {
			return e
		}
	}
	order := "slot,metric,tags"
	if j.FormatVersion == 1 {
		order = "slot,observed,ingest,ordinal,metric,tags"
	}
	rows, e = spool.QueryContext(ctx, "SELECT slot,metric,tags,value FROM samples WHERE slot>? ORDER BY "+order, j.Committed.Slot)
	if e != nil {
		return e
	}
	defer rows.Close()
	current := int64(-1)
	record := make([]string, len(header))
	yield := errors.New("sample output checkpoint confirmed")
	flush := func() error {
		if current < 0 {
			return nil
		}
		if e := c.Write(record); e != nil {
			return e
		}
		j.WrittenRows++
		o.sampleSlot = current
		if j.WrittenRows%128 == 0 {
			if e = w.fault("sample-row-before-checkpoint"); e != nil {
				return e
			}
			if e = w.checkpoint(ctx, j, o, c, len(snapshot.Units)); e != nil {
				return e
			}
			if e = w.space(j); e != nil {
				return e
			}
			if e = w.fault("sample-row-after-checkpoint"); e != nil {
				return e
			}
			return yield // Rotate accounts during wide-row assembly as well.
		}
		return nil
	}
	for rows.Next() {
		if e = ctx.Err(); e != nil {
			return e
		}
		var slot int64
		var name, tags string
		var value float64
		if e = rows.Scan(&slot, &name, &tags, &value); e != nil {
			return e
		}
		if slot != current {
			if e = flush(); e != nil {
				if errors.Is(e, yield) {
					return nil
				}
				return e
			}
			record = make([]string, len(header))
			record[0] = j.NodeName
			record[1] = time.Unix(0, slot).UTC().Format(time.RFC3339Nano)
			current = slot
		}
		if j.FormatVersion == 1 {
			var decoded map[string]string
			if e = json.Unmarshal([]byte(tags), &decoded); e != nil {
				return e
			}
			for i, column := range legacy {
				if column.metric == name && (column.task == "" || column.task == decoded["task_id"]) && record[i+2] == "" {
					record[i+2] = column.value(value, decoded)
				}
			}
		} else {
			record[positions[name+"\x00"+tags]] = strconv.FormatFloat(value, 'g', -1, 64)
		}
	}
	if e = rows.Err(); e != nil {
		return e
	}
	if e = flush(); e != nil {
		if errors.Is(e, yield) {
			return nil
		}
		return e
	}
	rows.Close()
	if e = w.checkpoint(ctx, j, o, c, len(snapshot.Units)); e != nil {
		return e
	}
	return w.publish(ctx, j, o, c)
}
