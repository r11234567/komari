package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/metricstore"
	dbtasks "github.com/komari-monitor/komari/database/tasks"
	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/pkg/historyexport"
	"github.com/komari-monitor/komari/pkg/metric"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/api"
)

type backgroundExportAPI struct {
	queue *historyexport.Queue
	err   error
}
type backgroundExportRequest struct {
	UUID, Category, Type, Mode, Start, End, Aggregation string
	Hours                                               int
	IntervalSeconds                                     int `json:"interval_seconds"`
	FormatVersion                                       int `json:"format_version"`
}

func RegisterHistoryExportRoutes(group *gin.RouterGroup) {
	q, e := historyexport.Open("./data/exports-v2")
	a := &backgroundExportAPI{queue: q, err: e}
	group.GET("/history/export/retention", a.retention)
	group.GET("/history/export", a.list)
	group.POST("/history/export", a.create)
	group.GET("/history/export/:id", a.get)
	group.GET("/history/export/:id/download", a.download)
	group.DELETE("/history/export/:id", a.cancel)
	group.POST("/history/export/:id/:action", a.control)
}
func exportError(c *gin.Context, status int, e error) {
	c.JSON(status, gin.H{"status": "error", "message": e.Error()})
}
func (a *backgroundExportAPI) owner(c *gin.Context) (string, bool) {
	c.Header("Cache-Control", "private, no-store")
	if a.err != nil {
		exportError(c, 503, a.err)
		return "", false
	}
	p := api.GetPrincipal(c)
	if p == nil {
		p = api.IdentifyPrincipal(c)
	}
	if p.Type != rpc.PrincipalUser || p.UserUUID == "" || !p.HasRole(rpc.RoleAdmin) {
		exportError(c, 403, errors.New("background exports require an authenticated administrator account"))
		return "", false
	}
	return p.UserUUID, true
}
func backgroundCategory(r backgroundExportRequest) string {
	category := strings.ToLower(strings.TrimSpace(r.Category))
	if category == "" {
		category = strings.ToLower(strings.TrimSpace(r.Type))
	}
	if category == "load" {
		category = "resource"
	}
	if category == "ping" {
		category = "latency"
	}
	return category
}
func backgroundMetrics(category string, gpu bool) []string {
	resource := []string{metricstore.MetricCPU, metricstore.MetricDisk, metricstore.MetricLoad, metricstore.MetricRAM, metricstore.MetricProcess, metricstore.MetricSwap}
	if gpu {
		resource = append(resource, metricstore.MetricGPUDeviceUsage, metricstore.MetricGPUMemTotal, metricstore.MetricGPUMem, metricstore.MetricGPUTemp, metricstore.MetricGPU)
	}
	network := []string{metricstore.MetricConnections, metricstore.MetricConnectionsUDP, metricstore.MetricNetIn, metricstore.MetricNetOut, metricstore.MetricNetTotalDown, metricstore.MetricNetTotalUp, metricstore.MetricTrafficDown, metricstore.MetricTrafficUp}
	var names []string
	if category == "resource" || category == "all" {
		names = append(names, resource...)
	}
	if category == "network" || category == "all" {
		names = append(names, network...)
	}
	if category == "latency" || category == "all" {
		names = append(names, metricstore.MetricPingLatency, metricstore.MetricPingLoss)
	}
	sort.Strings(names)
	return names
}
func backgroundHasGPU(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "none", "n/a", "unknown", "not available":
		return false
	}
	return true
}
func backgroundSource(cfg *metricstore.MetricStoreConfig) (historyexport.Source, error) {
	if metricstore.ResolveDriverFromConfig(cfg.Driver, cfg.DSN) != metric.DriverSQLite {
		return historyexport.Source{}, errors.New("strict export is unavailable for external SQL: database-side CPU isolation is not verified")
	}
	path := strings.TrimPrefix(cfg.DSN, "file:")
	if i := strings.IndexByte(path, '?'); i >= 0 {
		params, e := url.ParseQuery(path[i+1:])
		if e != nil {
			return historyexport.Source{}, e
		}
		if params.Get("mode") == "memory" {
			return historyexport.Source{}, errors.New("background export requires a persistent SQLite file")
		}
		path = path[:i]
	}
	if path == "" {
		path = "./data/metrics.db"
	}
	if decoded, e := url.PathUnescape(path); e == nil {
		path = decoded
	}
	if path == ":memory:" {
		return historyexport.Source{}, errors.New("background export requires a persistent SQLite file")
	}
	path, e := filepath.Abs(path)
	if e != nil {
		return historyexport.Source{}, e
	}
	archive := cfg.ArchiveDirectory
	if archive == "" {
		archive = "./data/telemetry"
	}
	archive, e = filepath.Abs(archive)
	if e != nil {
		return historyexport.Source{}, e
	}
	prefix := cfg.TablePrefix
	if prefix == "" {
		prefix = "metric_"
	}
	hot, segment, budget := cfg.HotWindowMinutes, cfg.ArchiveSegmentMinutes, cfg.HotBudgetMiB
	if hot <= 0 {
		hot = 15
	}
	if segment <= 0 {
		segment = 15
	}
	if budget <= 0 {
		budget = 1024
	}
	return historyexport.Source{DSN: path, Prefix: prefix, Telemetry: metric.TelemetryConfig{ArchiveDirectory: archive, RawWindow: time.Duration(hot) * time.Minute, SegmentDuration: time.Duration(segment) * time.Minute, MaxHotBytes: int64(budget) << 20}}, nil
}
func (a *backgroundExportAPI) create(c *gin.Context) {
	owner, ok := a.owner(c)
	if !ok {
		return
	}
	var r backgroundExportRequest
	if e := c.ShouldBindJSON(&r); e != nil {
		exportError(c, 400, e)
		return
	}
	category := backgroundCategory(r)
	names := backgroundMetrics(category, true)
	if len(names) == 0 {
		exportError(c, 400, errors.New("category must be resource, network, latency or all"))
		return
	}
	client, e := clients.GetClientByUUID(strings.TrimSpace(r.UUID))
	if e != nil {
		exportError(c, 404, errors.New("node not found"))
		return
	}
	names = backgroundMetrics(category, backgroundHasGPU(client.GpuName))
	end := time.Now().UTC()
	if r.End != "" {
		end, e = time.Parse(time.RFC3339, r.End)
		if e != nil {
			exportError(c, 400, e)
			return
		}
	}
	var start time.Time
	if r.Start != "" {
		start, e = time.Parse(time.RFC3339, r.Start)
		if e != nil {
			exportError(c, 400, e)
			return
		}
	} else if r.Hours > 0 && r.Hours <= 24*3650 {
		start = end.Add(-time.Duration(r.Hours) * time.Hour)
	}
	if start.IsZero() || !start.Before(end) || end.After(time.Now().Add(time.Minute)) {
		exportError(c, 400, errors.New("invalid export time range"))
		return
	}
	if r.Mode == "" {
		r.Mode = "sample30s"
		if r.FormatVersion == 0 {
			r.FormatVersion = 1
		}
	}
	pingNames := map[string]string{}
	if r.FormatVersion == 1 && (category == "latency" || category == "all") {
		tasks, e := dbtasks.GetAllPingTasks()
		if e != nil {
			exportError(c, 503, e)
			return
		}
		if len(tasks) > 1024 {
			exportError(c, 400, errors.New("too many ping tasks for legacy CSV"))
			return
		}
		for _, task := range tasks {
			pingNames[strconv.FormatUint(uint64(task.Id), 10)] = task.Name
		}
	}
	aggregation := metric.Aggregation(r.Aggregation)
	if aggregation == "" {
		aggregation = metric.AggAvg
	}
	interval := metric.TelemetryChartInterval(end.Sub(start), 360)
	if r.IntervalSeconds > 0 {
		interval = time.Duration(r.IntervalSeconds) * time.Second
	}
	if r.Mode == "summary" && (interval < 10*time.Second || end.Sub(start)/interval > 1000) {
		exportError(c, 400, errors.New("summary interval must be at least 10 seconds and no more than 1000 output buckets"))
		return
	}
	cfg, e := config.GetManyAs[metricstore.MetricStoreConfig]()
	if e != nil {
		exportError(c, 503, e)
		return
	}
	source, e := backgroundSource(cfg)
	if e != nil {
		exportError(c, 503, e)
		return
	}
	store, release, e := metricstore.AcquireStore(c.Request.Context())
	if e != nil {
		exportError(c, 503, e)
		return
	}
	if store.Telemetry() == nil {
		release()
		exportError(c, 503, errors.New("telemetry not activated"))
		return
	}
	state, e := store.Telemetry().ExportState(c.Request.Context())
	release()
	if e != nil {
		exportError(c, 503, e)
		return
	}
	source.Instance, source.Epoch = state.Instance, state.Epoch
	availability := a.queue.Availability()
	if !availability.Available {
		exportError(c, 503, errors.New(availability.Error))
		return
	}
	j, e := a.queue.Create(c.Request.Context(), historyexport.Request{Owner: owner, UUID: client.UUID, NodeName: client.Name, Category: category, Mode: r.Mode, Start: start.UTC(), End: end.UTC(), Metrics: names, Aggregation: aggregation, Interval: interval, Source: source, CSVVersion: r.FormatVersion, PingNames: pingNames})
	if e != nil {
		status := 400
		if errors.Is(e, historyexport.ErrQueueFull) {
			status = 429
		}
		exportError(c, status, e)
		return
	}
	c.JSON(202, gin.H{"status": "success", "data": j})
}
func (a *backgroundExportAPI) visible(c *gin.Context) (*historyexport.Job, bool) {
	owner, ok := a.owner(c)
	if !ok {
		return nil, false
	}
	j, e := a.queue.Get(c.Request.Context(), owner, c.Param("id"))
	if e != nil {
		exportError(c, 404, historyexport.ErrNotFound)
		return nil, false
	}
	if _, e = clients.GetClientByUUID(j.UUID); e != nil {
		exportError(c, 404, historyexport.ErrNotFound)
		return nil, false
	}
	return j, true
}
func (a *backgroundExportAPI) list(c *gin.Context) {
	owner, ok := a.owner(c)
	if !ok {
		return
	}
	jobs, e := a.queue.List(c.Request.Context(), owner)
	if e != nil {
		exportError(c, 503, e)
		return
	}
	visible := make([]*historyexport.Job, 0, len(jobs))
	for _, j := range jobs {
		if _, e = clients.GetClientByUUID(j.UUID); e == nil {
			visible = append(visible, j)
		}
	}
	b, _ := json.Marshal(visible)
	h := sha256.Sum256(b)
	etag := "\"" + hex.EncodeToString(h[:]) + "\""
	c.Header("ETag", etag)
	if c.GetHeader("If-None-Match") == etag {
		c.Status(304)
		return
	}
	c.JSON(200, gin.H{"status": "success", "data": visible})
}
func (a *backgroundExportAPI) get(c *gin.Context) {
	j, ok := a.visible(c)
	if ok {
		c.JSON(200, gin.H{"status": "success", "data": j})
	}
}
func (a *backgroundExportAPI) control(c *gin.Context) {
	j, ok := a.visible(c)
	if !ok {
		return
	}
	if e := a.queue.Control(c.Request.Context(), j.Owner, j.ID, c.Param("action")); e != nil {
		exportError(c, 409, e)
		return
	}
	c.JSON(202, gin.H{"status": "success", "data": gin.H{"accepted": true}})
}
func (a *backgroundExportAPI) cancel(c *gin.Context) {
	c.Params = append(c.Params, gin.Param{Key: "action", Value: "cancel"})
	a.control(c)
}
func (a *backgroundExportAPI) download(c *gin.Context) {
	j, ok := a.visible(c)
	if !ok {
		return
	}
	if j.Status != "done" || !time.Now().Before(j.ExpiresAt) {
		exportError(c, 409, errors.New("background export is not ready or has expired"))
		return
	}
	store, release, e := metricstore.AcquireStore(c.Request.Context())
	if e != nil {
		exportError(c, 503, e)
		return
	}
	if store.Telemetry() == nil {
		release()
		exportError(c, 409, errors.New("source generation unavailable"))
		return
	}
	state, e := store.Telemetry().ExportState(c.Request.Context())
	release()
	if e != nil || state.Instance != j.Source.Instance || state.Epoch != j.Source.Epoch {
		exportError(c, 409, errors.New("source generation or history changed; create a new export"))
		return
	}
	c.Header("X-Content-SHA256", j.SHA256)
	c.Header("X-Export-Format-Version", strconv.Itoa(j.FormatVersion))
	c.FileAttachment(a.queue.Path(j.ID, ".csv"), j.Filename)
}
func (a *backgroundExportAPI) retention(c *gin.Context) {
	if _, ok := a.owner(c); !ok {
		return
	}
	resource, ping := 24, 24
	store, release, e := metricstore.AcquireStore(c.Request.Context())
	if e == nil {
		defer release()
		defs, e := store.ListMetrics(c.Request.Context())
		if e == nil {
			for _, d := range defs {
				if d.Name == metricstore.MetricPingLatency || d.Name == metricstore.MetricPingLoss {
					ping = max(ping, d.RetentionDays*24)
				} else {
					resource = max(resource, d.RetentionDays*24)
				}
			}
		}
	}
	available := a.queue.Availability()
	if cfg, e := config.GetManyAs[metricstore.MetricStoreConfig](); e == nil {
		if _, e = backgroundSource(cfg); e != nil {
			available.Available = false
			available.Error = e.Error()
		}
	} else {
		available.Available = false
		available.Error = e.Error()
	}
	c.JSON(200, gin.H{"status": "success", "data": gin.H{"resource_hours": resource, "ping_hours": ping, "worker_available": available.Available, "worker_error": available.Error, "cpu_core_limit": available.CPUCoreLimit, "cpu_share_limit": .1, "format_version": historyexport.FormatVersion, "snapshot_policy": "fixed when background planning starts"}})
}
