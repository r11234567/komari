package admin

import (
	"context"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/metricstore"
	dbtasks "github.com/komari-monitor/komari/database/tasks"
	"github.com/komari-monitor/komari/pkg/metric"
)

const (
	historyExportTTL        = 48 * time.Hour
	historyExportWindow     = 5 * time.Minute
	historyExportQueueDepth = 8
	historyExportDirectory  = "data/exports"
)

type historyExportRequest struct {
	Category string `json:"category"`
	Type     string `json:"type"`
	UUID     string `json:"uuid"`
	Hours    int    `json:"hours"`
	Start    string `json:"start"`
	End      string `json:"end"`
}

type historyExportJob struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Category  string    `json:"category"`
	UUID      string    `json:"uuid,omitempty"`
	NodeName  string    `json:"node_name"`
	Status    string    `json:"status"`
	Progress  int       `json:"progress"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Filename  string    `json:"filename,omitempty"`
	Size      int64     `json:"size,omitempty"`
	Error     string    `json:"error,omitempty"`
	Path      string    `json:"-"`
	cancel    context.CancelFunc
}

type exportValueFormat int

const (
	exportDecimal exportValueFormat = iota
	exportPercent
	exportGiB
	exportMiB
	exportKiBPerSecond
	exportCount
	exportTemperature
)

type exportMetricColumn struct {
	name   string
	label  string
	group  string
	format exportValueFormat
	tagged bool
}

type exportPingTask struct {
	id   string
	name string
}

type exportPingValue struct {
	latency    float64
	loss       float64
	hasLatency bool
	hasLoss    bool
}

type exportRow struct {
	timestamp time.Time
	values    map[string][]string
	ping      map[string]*exportPingValue
}

var resourceExportColumns = []exportMetricColumn{
	{name: metricstore.MetricCPU, label: "CPU usage (%)", group: "Resource", format: exportPercent},
	{name: metricstore.MetricDisk, label: "Disk used (GiB)", group: "Resource", format: exportGiB},
	{name: metricstore.MetricGPUDeviceUsage, label: "GPU device usage (%)", group: "Resource", format: exportPercent, tagged: true},
	{name: metricstore.MetricGPUMemTotal, label: "GPU memory total (GiB)", group: "Resource", format: exportGiB, tagged: true},
	{name: metricstore.MetricGPUMem, label: "GPU memory used (GiB)", group: "Resource", format: exportGiB, tagged: true},
	{name: metricstore.MetricGPUTemp, label: "GPU temperature (C)", group: "Resource", format: exportTemperature, tagged: true},
	{name: metricstore.MetricGPU, label: "GPU usage (%)", group: "Resource", format: exportPercent},
	{name: metricstore.MetricLoad, label: "Load average", group: "Resource", format: exportDecimal},
	{name: metricstore.MetricRAM, label: "Memory used (GiB)", group: "Resource", format: exportGiB},
	{name: metricstore.MetricProcess, label: "Process count", group: "Resource", format: exportCount},
	{name: metricstore.MetricSwap, label: "Swap used (GiB)", group: "Resource", format: exportGiB},
}

var networkExportColumns = []exportMetricColumn{
	{name: metricstore.MetricConnections, label: "TCP connections", group: "Network", format: exportCount},
	{name: metricstore.MetricConnectionsUDP, label: "UDP connections", group: "Network", format: exportCount},
	{name: metricstore.MetricNetIn, label: "Network inbound (KiB/s)", group: "Network", format: exportKiBPerSecond},
	{name: metricstore.MetricNetOut, label: "Network outbound (KiB/s)", group: "Network", format: exportKiBPerSecond},
	{name: metricstore.MetricNetTotalDown, label: "Total downloaded (GiB)", group: "Network", format: exportGiB},
	{name: metricstore.MetricNetTotalUp, label: "Total uploaded (GiB)", group: "Network", format: exportGiB},
	{name: metricstore.MetricTrafficDown, label: "Download delta (MiB)", group: "Network", format: exportMiB},
	{name: metricstore.MetricTrafficUp, label: "Upload delta (MiB)", group: "Network", format: exportMiB},
}

var historyExports = struct {
	sync.Mutex
	once  sync.Once
	jobs  map[string]*historyExportJob
	queue chan string
}{
	jobs:  make(map[string]*historyExportJob),
	queue: make(chan string, historyExportQueueDepth),
}

func RegisterHistoryExportRoutes(group *gin.RouterGroup) {
	initializeHistoryExports()
	group.GET("/history/export/retention", getHistoryExportRetention)
	group.GET("/history/export", listHistoryExports)
	group.POST("/history/export", startHistoryExport)
	group.GET("/history/export/:id", getHistoryExport)
	group.GET("/history/export/:id/download", downloadHistoryExport)
	group.DELETE("/history/export/:id", cancelHistoryExport)
}

func initializeHistoryExports() {
	historyExports.once.Do(func() {
		_ = os.MkdirAll(historyExportDirectory, 0750)
		loadPersistedHistoryExports(time.Now().UTC())
		go historyExportWorker()
		go func() {
			ticker := time.NewTicker(time.Hour)
			defer ticker.Stop()
			for now := range ticker.C {
				historyExports.Lock()
				pruneHistoryExportsLocked(now.UTC())
				historyExports.Unlock()
			}
		}()
	})
}

func normalizeHistoryExportCategory(request historyExportRequest) string {
	category := strings.ToLower(strings.TrimSpace(request.Category))
	if category == "" {
		category = strings.ToLower(strings.TrimSpace(request.Type))
	}
	switch category {
	case "load":
		return "resource"
	case "ping":
		return "latency"
	default:
		return category
	}
}

func validHistoryExportCategory(category string) bool {
	switch category {
	case "resource", "network", "latency", "all":
		return true
	default:
		return false
	}
}

func parseHistoryExportRange(request historyExportRequest) (time.Time, time.Time, error) {
	end := time.Now().UTC()
	start := time.Time{}
	var err error
	if request.End != "" {
		end, err = time.Parse(time.RFC3339, request.End)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid end time")
		}
	}
	if request.Start != "" {
		start, err = time.Parse(time.RFC3339, request.Start)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid start time")
		}
	} else if request.Hours > 0 {
		start = end.Add(-time.Duration(request.Hours) * time.Hour)
	}
	if start.IsZero() || !start.Before(end) {
		return time.Time{}, time.Time{}, fmt.Errorf("start must be before end")
	}
	return start.UTC(), end.UTC(), nil
}

func newHistoryExportID() string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err == nil {
		return hex.EncodeToString(value[:])
	}
	return fmt.Sprintf("%024x", time.Now().UnixNano())
}

func startHistoryExport(c *gin.Context) {
	initializeHistoryExports()
	var request historyExportRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": err.Error()})
		return
	}
	category := normalizeHistoryExportCategory(request)
	request.UUID = strings.TrimSpace(request.UUID)
	if !validHistoryExportCategory(category) {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "category must be resource, network, latency, or all"})
		return
	}
	if request.UUID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "uuid is required"})
		return
	}
	client, err := clients.GetClientByUUID(request.UUID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "node not found"})
		return
	}
	nodeName := strings.TrimSpace(client.Name)
	if nodeName == "" {
		nodeName = "Unnamed server"
	}
	start, end, err := parseHistoryExportRange(request)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": err.Error()})
		return
	}
	resourceHours, pingHours := historyExportRetentionHours(c.Request.Context())
	maxHours := resourceHours
	if category == "latency" {
		maxHours = pingHours
	} else if category == "all" && pingHours < maxHours {
		maxHours = pingHours
	}
	if end.Sub(start) > time.Duration(maxHours)*time.Hour {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": fmt.Sprintf("export range exceeds retention limit of %d hours", maxHours)})
		return
	}

	now := time.Now().UTC()
	job := &historyExportJob{
		ID: newHistoryExportID(), Type: category, Category: category, UUID: request.UUID, NodeName: nodeName,
		Status: "queued", Start: start, End: end, CreatedAt: now, ExpiresAt: now.Add(historyExportTTL),
	}
	_, cancel := context.WithCancel(context.Background())
	job.cancel = cancel
	historyExports.Lock()
	pruneHistoryExportsLocked(now)
	historyExports.jobs[job.ID] = job
	historyExports.Unlock()
	select {
	case historyExports.queue <- job.ID:
		c.JSON(http.StatusAccepted, gin.H{"status": "success", "data": cloneHistoryExportJob(job)})
	default:
		cancel()
		historyExports.Lock()
		delete(historyExports.jobs, job.ID)
		historyExports.Unlock()
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "error", "message": "history export queue is full"})
	}
}

func historyExportWorker() {
	for id := range historyExports.queue {
		historyExports.Lock()
		job := historyExports.jobs[id]
		if job == nil || job.Status == "cancelled" {
			historyExports.Unlock()
			continue
		}
		job.Status = "running"
		job.Progress = 1
		ctx, cancel := context.WithCancel(context.Background())
		if job.cancel != nil {
			job.cancel()
		}
		job.cancel = cancel
		historyExports.Unlock()

		err := writeHistoryExport(ctx, id)
		historyExports.Lock()
		job = historyExports.jobs[id]
		if job == nil {
			historyExports.Unlock()
			continue
		}
		job.cancel = nil
		switch {
		case errors.Is(err, context.Canceled):
			job.Status = "cancelled"
			removeHistoryExportFiles(job.ID)
		case err != nil:
			job.Status = "failed"
			job.Error = err.Error()
			removeHistoryExportFiles(job.ID)
		default:
			job.Status = "done"
			job.Progress = 100
			job.ExpiresAt = time.Now().UTC().Add(historyExportTTL)
			job.Path = historyExportCSVPath(job.ID)
			job.Filename = buildHistoryExportFilename(job)
			if info, statErr := os.Stat(job.Path); statErr == nil {
				job.Size = info.Size()
			}
		}
		snapshot := cloneHistoryExportJob(job)
		historyExports.Unlock()
		if snapshot.Status == "done" {
			_ = persistHistoryExport(snapshot)
		}
	}
}

func writeHistoryExport(ctx context.Context, id string) error {
	historyExports.Lock()
	job := cloneHistoryExportJob(historyExports.jobs[id])
	historyExports.Unlock()
	if job == nil {
		return fmt.Errorf("export job not found")
	}
	store := metricstore.GetStore()
	if store == nil {
		return fmt.Errorf("metric store is not initialized")
	}
	columns := historyExportColumns(job.Category)
	pingTasks, err := historyExportPingTasks(ctx, store, job.Category)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(historyExportDirectory, 0750); err != nil {
		return err
	}
	partial := historyExportCSVPath(job.ID) + ".part"
	file, err := os.OpenFile(partial, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	succeeded := false
	defer func() {
		_ = file.Close()
		if !succeeded {
			_ = os.Remove(partial)
		}
	}()
	if _, err := file.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
		return err
	}
	writer := csv.NewWriter(file)
	if err := writeHistoryExportHeaders(writer, columns, pingTasks); err != nil {
		return err
	}
	if err := streamHistoryExportRows(ctx, writer, store, job, columns, pingTasks); err != nil {
		return err
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(partial, historyExportCSVPath(job.ID)); err != nil {
		return err
	}
	succeeded = true
	return nil
}

func historyExportColumns(category string) []exportMetricColumn {
	columns := make([]exportMetricColumn, 0, len(resourceExportColumns)+len(networkExportColumns))
	if category == "resource" || category == "all" {
		columns = append(columns, resourceExportColumns...)
	}
	if category == "network" || category == "all" {
		columns = append(columns, networkExportColumns...)
	}
	return columns
}

func historyExportPingTasks(ctx context.Context, store *metric.Store, category string) ([]exportPingTask, error) {
	if category != "latency" && category != "all" {
		return nil, nil
	}
	known, err := dbtasks.GetAllPingTasks()
	if err != nil {
		return nil, fmt.Errorf("list ping tasks: %w", err)
	}
	tasks := make([]exportPingTask, 0, len(known))
	seen := make(map[string]struct{}, len(known))
	for _, task := range known {
		id := strconv.FormatUint(uint64(task.Id), 10)
		name := strings.TrimSpace(task.Name)
		if name == "" {
			name = "Ping task " + id
		}
		tasks = append(tasks, exportPingTask{id: id, name: name})
		seen[id] = struct{}{}
	}
	ids, err := store.MetricTagValues(ctx, metricstore.MetricPingLatency, "task_id")
	if err != nil {
		return nil, fmt.Errorf("list retained ping tasks: %w", err)
	}
	unknown := make([]string, 0)
	for _, id := range ids {
		if _, ok := seen[id]; !ok {
			unknown = append(unknown, id)
		}
	}
	sort.Strings(unknown)
	for _, id := range unknown {
		tasks = append(tasks, exportPingTask{id: id, name: "Deleted ping task " + id})
	}
	return tasks, nil
}

func writeHistoryExportHeaders(writer *csv.Writer, columns []exportMetricColumn, pingTasks []exportPingTask) error {
	if len(pingTasks) == 0 {
		header := []string{"Server", "Time (UTC)"}
		for _, column := range columns {
			header = append(header, column.label)
		}
		return writer.Write(header)
	}
	groups := []string{"", ""}
	header := []string{"Server", "Time (UTC)"}
	for _, column := range columns {
		groups = append(groups, column.group)
		header = append(header, column.label)
	}
	for _, task := range pingTasks {
		groups = append(groups, task.name, task.name)
		header = append(header, "Ping (ms)", "Loss (%)")
	}
	if err := writer.Write(groups); err != nil {
		return err
	}
	return writer.Write(header)
}

func streamHistoryExportRows(ctx context.Context, writer *csv.Writer, store *metric.Store, job *historyExportJob, columns []exportMetricColumn, pingTasks []exportPingTask) error {
	total := job.End.Sub(job.Start)
	for cursor := job.Start; cursor.Before(job.End); {
		if err := ctx.Err(); err != nil {
			return err
		}
		next := cursor.Add(historyExportWindow)
		if next.After(job.End) {
			next = job.End
		}
		queryEnd := next
		if next.Before(job.End) {
			queryEnd = next.Add(-time.Nanosecond)
		}
		queries := make([]metric.Query, 0, len(columns)+2)
		for _, column := range columns {
			queries = append(queries, metric.Query{MetricName: column.name, EntityID: job.UUID, Start: cursor, End: queryEnd, Order: metric.OrderAsc})
		}
		pingOffset := len(queries)
		if len(pingTasks) > 0 {
			queries = append(queries,
				metric.Query{MetricName: metricstore.MetricPingLatency, EntityID: job.UUID, Start: cursor, End: queryEnd, Order: metric.OrderAsc},
				metric.Query{MetricName: metricstore.MetricPingLoss, EntityID: job.UUID, Start: cursor, End: queryEnd, Order: metric.OrderAsc},
			)
		}
		series, err := store.QueryRawBatch(ctx, queries)
		if err != nil {
			return err
		}
		rows := make(map[int64]*exportRow)
		rowFor := func(timestamp time.Time) *exportRow {
			key := timestamp.UnixNano()
			row := rows[key]
			if row == nil {
				row = &exportRow{timestamp: timestamp.UTC(), values: make(map[string][]string), ping: make(map[string]*exportPingValue)}
				rows[key] = row
			}
			return row
		}
		for index, column := range columns {
			for _, point := range series[index] {
				value := formatHistoryMetricValue(column, point.Value)
				if value == "" {
					continue
				}
				if column.tagged {
					device := strings.TrimSpace(point.Tags["device_name"])
					if device == "" {
						device = "GPU " + point.Tags["device_index"]
					}
					value = device + ": " + value
				}
				row := rowFor(point.Timestamp)
				row.values[column.name] = append(row.values[column.name], value)
			}
		}
		if len(pingTasks) > 0 {
			for seriesIndex := 0; seriesIndex < 2; seriesIndex++ {
				for _, point := range series[pingOffset+seriesIndex] {
					taskID := point.Tags["task_id"]
					row := rowFor(point.Timestamp)
					value := row.ping[taskID]
					if value == nil {
						value = &exportPingValue{}
						row.ping[taskID] = value
					}
					if seriesIndex == 0 {
						value.latency, value.hasLatency = point.Value, true
					} else {
						value.loss, value.hasLoss = point.Value, true
					}
				}
			}
		}
		keys := make([]int64, 0, len(rows))
		for key := range rows {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		for _, key := range keys {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := writeHistoryExportRow(writer, job.NodeName, rows[key], columns, pingTasks); err != nil {
				return err
			}
		}
		setHistoryExportProgress(job.ID, 1+int(next.Sub(job.Start)*98/total))
		cursor = next
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
	return nil
}

func writeHistoryExportRow(writer *csv.Writer, nodeName string, row *exportRow, columns []exportMetricColumn, pingTasks []exportPingTask) error {
	record := []string{nodeName, row.timestamp.UTC().Format(time.RFC3339Nano)}
	for _, column := range columns {
		values := row.values[column.name]
		sort.Strings(values)
		record = append(record, strings.Join(values, "; "))
	}
	for _, task := range pingTasks {
		latency, loss := "", ""
		if value := row.ping[task.id]; value != nil {
			if value.hasLatency && value.latency >= 0 {
				latency = strconv.FormatFloat(value.latency, 'f', 2, 64)
			}
			if value.hasLoss {
				loss = strconv.FormatFloat(value.loss*100, 'f', 2, 64)
			} else if value.hasLatency {
				if value.latency < 0 {
					loss = "100.00"
				} else {
					loss = "0.00"
				}
			}
		}
		record = append(record, latency, loss)
	}
	return writer.Write(record)
}

func formatHistoryMetricValue(column exportMetricColumn, value float64) string {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return ""
	}
	switch column.format {
	case exportPercent:
		return strconv.FormatFloat(value, 'f', 2, 64)
	case exportGiB:
		return strconv.FormatFloat(value/(1024*1024*1024), 'f', 3, 64)
	case exportMiB:
		return strconv.FormatFloat(value/(1024*1024), 'f', 3, 64)
	case exportKiBPerSecond:
		return strconv.FormatFloat(value/1024, 'f', 2, 64)
	case exportCount:
		return strconv.FormatInt(int64(math.Round(value)), 10)
	case exportTemperature:
		return strconv.FormatFloat(value, 'f', 1, 64)
	default:
		return strconv.FormatFloat(value, 'f', 2, 64)
	}
}

func setHistoryExportProgress(id string, progress int) {
	historyExports.Lock()
	if job := historyExports.jobs[id]; job != nil && job.Status == "running" && progress > job.Progress {
		if progress > 99 {
			progress = 99
		}
		job.Progress = progress
	}
	historyExports.Unlock()
}

func listHistoryExports(c *gin.Context) {
	initializeHistoryExports()
	historyExports.Lock()
	pruneHistoryExportsLocked(time.Now().UTC())
	jobs := make([]*historyExportJob, 0, len(historyExports.jobs))
	for _, job := range historyExports.jobs {
		if job.Status == "done" {
			jobs = append(jobs, cloneHistoryExportJob(job))
		}
	}
	historyExports.Unlock()
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].CreatedAt.After(jobs[j].CreatedAt) })
	c.JSON(http.StatusOK, gin.H{"status": "success", "data": jobs})
}

func getHistoryExport(c *gin.Context) {
	initializeHistoryExports()
	historyExports.Lock()
	job := cloneHistoryExportJob(historyExports.jobs[c.Param("id")])
	historyExports.Unlock()
	if job == nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "export not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "success", "data": job})
}

func downloadHistoryExport(c *gin.Context) {
	initializeHistoryExports()
	historyExports.Lock()
	job := cloneHistoryExportJob(historyExports.jobs[c.Param("id")])
	historyExports.Unlock()
	if job == nil || job.Status != "done" || job.Path == "" {
		c.JSON(http.StatusConflict, gin.H{"status": "error", "message": "export is not ready"})
		return
	}
	c.FileAttachment(job.Path, job.Filename)
}

func cancelHistoryExport(c *gin.Context) {
	initializeHistoryExports()
	historyExports.Lock()
	job := historyExports.jobs[c.Param("id")]
	if job == nil {
		historyExports.Unlock()
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "message": "export not found"})
		return
	}
	if job.cancel != nil {
		job.cancel()
	}
	if job.Status == "queued" || job.Status == "running" {
		job.Status = "cancelled"
	}
	historyExports.Unlock()
	c.JSON(http.StatusOK, gin.H{"status": "success"})
}

func getHistoryExportRetention(c *gin.Context) {
	resourceHours, pingHours := historyExportRetentionHours(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{"status": "success", "data": gin.H{"resource_hours": resourceHours, "ping_hours": pingHours}})
}

func historyExportRetentionHours(ctx context.Context) (int, int) {
	store := metricstore.GetStore()
	resourceHours, pingHours := 24, 24
	if store != nil {
		if definitions, err := store.ListMetrics(ctx); err == nil {
			for _, definition := range definitions {
				hours := definition.RetentionDays * 24
				if definition.Name == metricstore.MetricPingLatency || definition.Name == metricstore.MetricPingLoss {
					if hours > pingHours {
						pingHours = hours
					}
				} else if hours > resourceHours {
					resourceHours = hours
				}
			}
		}
	}
	return resourceHours, pingHours
}

func cloneHistoryExportJob(job *historyExportJob) *historyExportJob {
	if job == nil {
		return nil
	}
	clone := *job
	clone.cancel = nil
	return &clone
}

func historyExportCSVPath(id string) string {
	return filepath.Join(historyExportDirectory, id+".csv")
}

func historyExportMetadataPath(id string) string {
	return filepath.Join(historyExportDirectory, id+".json")
}

func persistHistoryExport(job *historyExportJob) error {
	content, err := json.Marshal(job)
	if err != nil {
		return err
	}
	path := historyExportMetadataPath(job.ID)
	partial := path + ".tmp"
	if err := os.WriteFile(partial, content, 0600); err != nil {
		return err
	}
	return os.Rename(partial, path)
}

func loadPersistedHistoryExports(now time.Time) {
	entries, err := os.ReadDir(historyExportDirectory)
	if err != nil {
		return
	}
	for _, entry := range entries {
		path := filepath.Join(historyExportDirectory, entry.Name())
		if entry.IsDir() {
			continue
		}
		if strings.HasSuffix(entry.Name(), ".part") || strings.HasSuffix(entry.Name(), ".tmp") {
			_ = os.Remove(path)
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			continue
		}
		var job historyExportJob
		if json.Unmarshal(content, &job) != nil || !validHistoryExportID(job.ID) || job.Status != "done" || !now.Before(job.ExpiresAt) {
			_ = os.Remove(path)
			if validHistoryExportID(job.ID) {
				removeHistoryExportFiles(job.ID)
			}
			continue
		}
		job.Path = historyExportCSVPath(job.ID)
		if info, statErr := os.Stat(job.Path); statErr != nil {
			_ = os.Remove(path)
			continue
		} else {
			job.Size = info.Size()
		}
		historyExports.jobs[job.ID] = &job
	}
	pruneOrphanedHistoryExportFiles(now)
}

func pruneHistoryExportsLocked(now time.Time) {
	for id, job := range historyExports.jobs {
		if now.Before(job.ExpiresAt) {
			continue
		}
		if job.cancel != nil {
			job.cancel()
		}
		removeHistoryExportFiles(id)
		delete(historyExports.jobs, id)
	}
	pruneOrphanedHistoryExportFiles(now)
}

func pruneOrphanedHistoryExportFiles(now time.Time) {
	entries, err := os.ReadDir(historyExportDirectory)
	if err != nil {
		return
	}
	cutoff := now.Add(-historyExportTTL)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(historyExportDirectory, entry.Name()))
		}
	}
}

func removeHistoryExportFiles(id string) {
	if !validHistoryExportID(id) {
		return
	}
	_ = os.Remove(historyExportCSVPath(id))
	_ = os.Remove(historyExportCSVPath(id) + ".part")
	_ = os.Remove(historyExportMetadataPath(id))
	_ = os.Remove(historyExportMetadataPath(id) + ".tmp")
}

func validHistoryExportID(id string) bool {
	if len(id) != 24 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func buildHistoryExportFilename(job *historyExportJob) string {
	rangeName := job.Start.UTC().Format("20060102-1504") + "_" + job.End.UTC().Format("20060102-1504")
	return sanitizeExportName(job.NodeName) + "_" + rangeName + "_" + job.Category + ".csv"
}

func sanitizeExportName(name string) string {
	name = strings.TrimSpace(name)
	var result strings.Builder
	for _, value := range name {
		switch {
		case unicode.IsLetter(value), unicode.IsDigit(value), strings.ContainsRune("-_.", value):
			result.WriteRune(value)
		case unicode.IsSpace(value):
			result.WriteByte('_')
		default:
			result.WriteByte('_')
		}
	}
	clean := strings.Trim(result.String(), "_.-")
	if clean == "" {
		return "server"
	}
	return clean
}
