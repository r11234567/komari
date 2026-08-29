package admin

import (
	"context"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
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
	historyExportQueueDepth = 8
	historyExportDirectory  = "data/exports"
	historyExportBatchWork  = 600 * time.Minute
	historyExportMinWindow  = 2 * time.Minute
	historyExportMaxWindow  = 2 * time.Hour
	historyExportSampleStep = 30 * time.Second
	// Kept for the legacy batching helpers exercised by older tests. The
	// production export path uses historyExportSampleStep directly.
	historyExportPingBatchGap = 10 * time.Second
	historyExportPingJoinGap  = time.Minute
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
	latency []float64
	loss    []float64
}

type exportPingObservation struct {
	timestamp time.Time
	taskID    string
	value     float64
	loss      bool
}

type exportPingBatch struct {
	first time.Time
	last  time.Time
	ping  map[string]*exportPingValue
}

type exportRow struct {
	timestamp time.Time
	values    map[string][]string
	ping      map[string]*exportPingValue
}

type historyExportScheduler struct {
	window     time.Duration
	total      time.Duration
	lastSample historyExportSystemSample
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
	columns, err := historyExportColumnsForJob(job)
	if err != nil {
		return err
	}
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
	if err := compactHistoryExportCSV(partial, len(pingTasks) > 0); err != nil {
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

// historyExportColumnsForJob omits the complete GPU family when the node says
// it has no GPU device. Agents still report a zero-valued node GPU metric on
// such machines, so raw point existence is not a reliable capability signal.
func historyExportColumnsForJob(job *historyExportJob) ([]exportMetricColumn, error) {
	columns := historyExportColumns(job.Category)
	if job.Category != "resource" && job.Category != "all" {
		return columns, nil
	}
	client, err := clients.GetClientByUUID(job.UUID)
	if err != nil {
		return nil, fmt.Errorf("check GPU export capability: %w", err)
	}
	if hasGPUDevice(client.GpuName) {
		return columns, nil
	}
	filtered := make([]exportMetricColumn, 0, len(columns)-5)
	for _, column := range columns {
		if !isGPUExportColumn(column) {
			filtered = append(filtered, column)
		}
	}
	return filtered, nil
}

func hasGPUDevice(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "none", "n/a", "unknown", "not available":
		return false
	default:
		return true
	}
}

// compactHistoryExportCSV removes columns and rows that contain no samples.
// It runs on the completed temporary file so the streaming query path can keep
// its bounded memory usage while the final CSV still reflects sparse metrics.
func compactHistoryExportCSV(path string, hasPing bool) error {
	input, err := os.Open(path)
	if err != nil {
		return err
	}
	defer input.Close()

	reader := csv.NewReader(input)
	reader.FieldsPerRecord = -1
	first, err := reader.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	if len(first) > 0 {
		first[0] = strings.TrimPrefix(first[0], "\ufeff")
	}
	headerRows := [][]string{first}
	if hasPing {
		second, readErr := reader.Read()
		if readErr != nil {
			return readErr
		}
		headerRows = append(headerRows, second)
	}

	dataRows := make([][]string, 0)
	active := make([]bool, len(first))
	for {
		record, readErr := reader.Read()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
		rowHasSample := false
		for index := 2; index < len(record); index++ {
			if strings.TrimSpace(record[index]) == "" {
				continue
			}
			if index >= len(active) {
				grown := make([]bool, index+1)
				copy(grown, active)
				active = grown
			}
			active[index] = true
			rowHasSample = true
		}
		if rowHasSample {
			dataRows = append(dataRows, record)
		}
	}

	keep := []int{0, 1}
	for index := 2; index < len(first); index++ {
		if index < len(active) && active[index] {
			keep = append(keep, index)
		}
	}
	compactPath := path + ".compact"
	output, err := os.OpenFile(compactPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	succeeded := false
	defer func() {
		_ = output.Close()
		if !succeeded {
			_ = os.Remove(compactPath)
		}
	}()
	if _, err := output.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
		return err
	}
	writer := csv.NewWriter(output)
	for _, header := range headerRows {
		if err := writer.Write(historyExportProjectCSVRecord(header, keep)); err != nil {
			return err
		}
	}
	for _, record := range dataRows {
		if err := writer.Write(historyExportProjectCSVRecord(record, keep)); err != nil {
			return err
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	if err := os.Rename(compactPath, path); err != nil {
		return err
	}
	succeeded = true
	return nil
}

func historyExportProjectCSVRecord(record []string, keep []int) []string {
	projected := make([]string, len(keep))
	for index, source := range keep {
		if source < len(record) {
			projected[index] = record[source]
		}
	}
	return projected
}

func isGPUExportColumn(column exportMetricColumn) bool {
	switch column.name {
	case metricstore.MetricGPUDeviceUsage, metricstore.MetricGPUMemTotal, metricstore.MetricGPUMem, metricstore.MetricGPUTemp, metricstore.MetricGPU:
		return true
	default:
		return false
	}
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
	queryCount := len(columns)
	if len(pingTasks) > 0 {
		queryCount += 2
	}
	scheduler := newHistoryExportScheduler(total, queryCount)
	for cursor := job.Start; cursor.Before(job.End); {
		chunkStarted := time.Now()
		if err := ctx.Err(); err != nil {
			return err
		}
		next := cursor.Add(scheduler.window)
		if next.After(job.End) {
			next = job.End
		} else if aligned := next.UTC().Truncate(historyExportSampleStep); aligned.After(cursor) {
			// Rows are keyed by 30-second slots. Align every chunk boundary to the
			// same boundary so one slot cannot be emitted by two adjacent chunks.
			next = aligned
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
		pointCount := 0
		for _, points := range series {
			pointCount += len(points)
		}
		rowFor := func(timestamp time.Time) *exportRow {
			timestamp = timestamp.UTC().Truncate(historyExportSampleStep)
			key := timestamp.UnixNano()
			row := rows[key]
			if row == nil {
				row = &exportRow{timestamp: timestamp, values: make(map[string][]string), ping: make(map[string]*exportPingValue)}
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
				if len(row.values[column.name]) == 0 {
					row.values[column.name] = []string{value}
				}
			}
		}
		if len(pingTasks) > 0 {
			observations := make([]exportPingObservation, 0, len(series[pingOffset])+len(series[pingOffset+1]))
			for seriesIndex := 0; seriesIndex < 2; seriesIndex++ {
				for _, point := range series[pingOffset+seriesIndex] {
					observations = append(observations, exportPingObservation{
						timestamp: point.Timestamp.UTC(), taskID: point.Tags["task_id"], value: point.Value, loss: seriesIndex == 1,
					})
				}
			}
			attachHistoryExportObservations(rows, observations, rowFor)
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
		delay := scheduler.completeChunk(pointCount, time.Since(chunkStarted))
		if cursor.Before(job.End) {
			if delay <= 0 {
				runtime.Gosched()
			} else {
				timer := time.NewTimer(delay)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				}
			}
		}
	}
	return nil
}

func historyExportPingBatches(observations []exportPingObservation) []*exportPingBatch {
	sort.SliceStable(observations, func(i, j int) bool {
		return observations[i].timestamp.Before(observations[j].timestamp)
	})
	batches := make([]*exportPingBatch, 0)
	var current *exportPingBatch
	for _, observation := range observations {
		value := (*exportPingValue)(nil)
		if current != nil {
			value = current.ping[observation.taskID]
		}
		repeated := value != nil && ((!observation.loss && len(value.latency) > 0) || (observation.loss && len(value.loss) > 0))
		if current == nil || repeated || observation.timestamp.Sub(current.last) > historyExportPingBatchGap {
			current = &exportPingBatch{
				first: observation.timestamp, last: observation.timestamp, ping: make(map[string]*exportPingValue),
			}
			batches = append(batches, current)
			value = nil
		}
		if value == nil {
			value = &exportPingValue{}
			current.ping[observation.taskID] = value
		}
		if observation.loss {
			value.loss = append(value.loss, observation.value)
		} else {
			value.latency = append(value.latency, observation.value)
		}
		current.last = observation.timestamp
	}
	return batches
}

func attachHistoryExportPingBatches(rows map[int64]*exportRow, batches []*exportPingBatch, rowFor func(time.Time) *exportRow) {
	for _, batch := range batches {
		var target *exportRow
		bestDistance := historyExportPingJoinGap + time.Nanosecond
		midpoint := batch.first.Add(batch.last.Sub(batch.first) / 2)
		for _, row := range rows {
			if len(row.values) == 0 {
				continue
			}
			distance := row.timestamp.Sub(midpoint)
			if distance < 0 {
				distance = -distance
			}
			if distance <= historyExportPingJoinGap && distance < bestDistance {
				target, bestDistance = row, distance
			}
		}
		if target == nil {
			target = rowFor(batch.first)
		}
		for taskID, value := range batch.ping {
			existing := target.ping[taskID]
			if existing == nil {
				target.ping[taskID] = value
				continue
			}
			existing.latency = append(existing.latency, value.latency...)
			existing.loss = append(existing.loss, value.loss...)
		}
	}
}

// attachHistoryExportObservations places each raw ping observation into its
// own 30-second slot. A task can contribute at most one latency and one loss
// value to a cell; extra samples in the same slot are intentionally ignored.
func attachHistoryExportObservations(rows map[int64]*exportRow, observations []exportPingObservation, rowFor func(time.Time) *exportRow) {
	for _, observation := range observations {
		row := rowFor(observation.timestamp)
		value := row.ping[observation.taskID]
		if value == nil {
			value = &exportPingValue{}
			row.ping[observation.taskID] = value
		}
		if observation.loss {
			if len(value.loss) == 0 {
				value.loss = append(value.loss, observation.value)
			}
		} else if len(value.latency) == 0 {
			value.latency = append(value.latency, observation.value)
		}
	}
}

func newHistoryExportScheduler(total time.Duration, queryCount int) *historyExportScheduler {
	if queryCount < 1 {
		queryCount = 1
	}
	window := historyExportBatchWork / time.Duration(queryCount)
	if window > time.Hour {
		window = time.Hour
	}
	sample := sampleHistoryExportSystem()
	memoryRatio := sample.memoryAvailableRatio()
	switch {
	case sample.memoryAvailable > 0 && (sample.memoryAvailable < 512*1024*1024 || memoryRatio > 0 && memoryRatio < 0.15):
		window /= 4
	case sample.loadRatio >= 0.90:
		window /= 4
	case sample.memoryAvailable > 0 && sample.memoryAvailable < 1024*1024*1024:
		window /= 2
	case sample.loadRatio >= 0.75:
		window /= 2
	case sample.loadRatio >= 0 && sample.loadRatio < 0.35 && (memoryRatio == 0 || memoryRatio >= 0.30):
		window = window * 3 / 2
	}
	window = clampHistoryExportWindow(window, total)
	return &historyExportScheduler{window: window, total: total, lastSample: sample}
}

func clampHistoryExportWindow(window, total time.Duration) time.Duration {
	if window < historyExportMinWindow {
		window = historyExportMinWindow
	} else if window > historyExportMaxWindow {
		window = historyExportMaxWindow
	}
	if total > 0 && total < window {
		return total
	}
	return window
}

func (scheduler *historyExportScheduler) completeChunk(pointCount int, chunkDuration time.Duration) time.Duration {
	current := sampleHistoryExportSystem()
	cpuUsage := historyExportCPUUsage(scheduler.lastSample, current)
	scheduler.lastSample = current

	targetPoints := 40_000
	memoryRatio := current.memoryAvailableRatio()
	switch {
	case current.memoryAvailable > 0 && (current.memoryAvailable < 512*1024*1024 || memoryRatio > 0 && memoryRatio < 0.15):
		targetPoints = 12_000
	case current.memoryAvailable > 0 && (current.memoryAvailable < 1024*1024*1024 || memoryRatio > 0 && memoryRatio < 0.25):
		targetPoints = 25_000
	case current.memoryAvailable >= 4*1024*1024*1024 && memoryRatio >= 0.40:
		targetPoints = 120_000
	case current.memoryAvailable >= 2*1024*1024*1024 && memoryRatio >= 0.30:
		targetPoints = 75_000
	}
	switch {
	case cpuUsage >= 0.90:
		targetPoints /= 4
	case cpuUsage >= 0.75:
		targetPoints /= 2
	case cpuUsage >= 0 && cpuUsage < 0.35 && (memoryRatio == 0 || memoryRatio >= 0.30):
		targetPoints = targetPoints * 3 / 2
	}
	if targetPoints < 4_000 {
		targetPoints = 4_000
	} else if targetPoints > 150_000 {
		targetPoints = 150_000
	}

	if pointCount > 0 {
		desired := time.Duration(float64(scheduler.window) * float64(targetPoints) / float64(pointCount))
		if desired < scheduler.window/2 {
			desired = scheduler.window / 2
		} else if desired > scheduler.window*2 {
			desired = scheduler.window * 2
		}
		if current.memoryAvailable > 0 && (current.memoryAvailable < 512*1024*1024 || memoryRatio > 0 && memoryRatio < 0.15) {
			desired = scheduler.window / 2
		} else if cpuUsage >= 0.90 {
			desired = scheduler.window / 2
		} else if cpuUsage >= 0.75 && desired > scheduler.window*3/4 {
			desired = scheduler.window * 3 / 4
		}
		scheduler.window = clampHistoryExportWindow((scheduler.window*2+desired)/3, scheduler.total)
	} else {
		scheduler.window = clampHistoryExportWindow(scheduler.window*2, scheduler.total)
	}

	memoryTight := current.memoryAvailable > 0 && (current.memoryAvailable < 512*1024*1024 || memoryRatio > 0 && memoryRatio < 0.15)
	switch {
	case memoryTight || cpuUsage >= 0.90:
		return clampHistoryExportDelay(chunkDuration/2, 10*time.Millisecond, 250*time.Millisecond)
	case cpuUsage >= 0.75:
		return clampHistoryExportDelay(chunkDuration/5, 5*time.Millisecond, 100*time.Millisecond)
	case cpuUsage >= 0.50:
		return clampHistoryExportDelay(chunkDuration/20, time.Millisecond, 25*time.Millisecond)
	default:
		return 0
	}
}

func historyExportCPUUsage(previous, current historyExportSystemSample) float64 {
	if current.cpuTotal > previous.cpuTotal {
		total := current.cpuTotal - previous.cpuTotal
		idle := uint64(0)
		if current.cpuIdle >= previous.cpuIdle {
			idle = current.cpuIdle - previous.cpuIdle
		}
		if idle <= total {
			return 1 - float64(idle)/float64(total)
		}
	}
	return current.loadRatio
}

func clampHistoryExportDelay(value, minimum, maximum time.Duration) time.Duration {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}

func writeHistoryExportRow(writer *csv.Writer, nodeName string, row *exportRow, columns []exportMetricColumn, pingTasks []exportPingTask) error {
	record := []string{nodeName, row.timestamp.UTC().Format(time.RFC3339Nano)}
	for _, column := range columns {
		values := row.values[column.name]
		if len(values) > 1 {
			// A 30-second cell represents one raw sample. Older exports joined
			// every sample in the minute with "; ", creating multi-value cells.
			// Keep the first deterministic sample and discard the rest.
			values = values[:1]
		}
		record = append(record, strings.Join(values, ""))
	}
	for _, task := range pingTasks {
		latency, loss := "", ""
		if value := row.ping[task.id]; value != nil {
			latencies := make([]string, 0, len(value.latency))
			for _, point := range value.latency {
				if point >= 0 {
					latencies = append(latencies, strconv.FormatFloat(point, 'f', 2, 64))
				}
			}
			if len(latencies) > 0 {
				latency = latencies[0]
			}
			losses := make([]string, 0, len(value.loss))
			if len(value.loss) > 0 {
				for _, point := range value.loss {
					losses = append(losses, strconv.FormatFloat(point*100, 'f', 2, 64))
				}
			} else {
				for _, point := range value.latency {
					if point < 0 {
						losses = append(losses, "100.00")
					} else {
						losses = append(losses, "0.00")
					}
				}
			}
			if len(losses) > 0 {
				loss = losses[0]
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
