package admin

import (
	"context"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
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

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/pkg/metric"
)

const historyExportTTL = 48 * time.Hour

type historyExportRequest struct {
	Type  string `json:"type"`
	UUID  string `json:"uuid"`
	Hours int    `json:"hours"`
	Start string `json:"start"`
	End   string `json:"end"`
}

type historyExportJob struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	UUID      string    `json:"uuid"`
	Status    string    `json:"status"`
	Progress  int       `json:"progress"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Error     string    `json:"error,omitempty"`
	Path      string    `json:"-"`
	Filename  string    `json:"-"`
	cancel    context.CancelFunc
}

var historyExports = struct {
	sync.Mutex
	jobs map[string]*historyExportJob
}{jobs: make(map[string]*historyExportJob)}

var resourceExportMetrics = []string{
	metricstore.MetricCPU,
	metricstore.MetricGPU,
	metricstore.MetricRAM,
	metricstore.MetricSwap,
	metricstore.MetricLoad,
	metricstore.MetricDisk,
	metricstore.MetricNetIn,
	metricstore.MetricNetOut,
	metricstore.MetricNetTotalUp,
	metricstore.MetricNetTotalDown,
	metricstore.MetricTrafficUp,
	metricstore.MetricTrafficDown,
	metricstore.MetricProcess,
	metricstore.MetricConnections,
	metricstore.MetricConnectionsUDP,
}

func RegisterHistoryExportRoutes(group *gin.RouterGroup) {
	group.GET("/history/export/retention", getHistoryExportRetention)
	group.POST("/history/export", startHistoryExport)
	group.GET("/history/export/:id", getHistoryExport)
	group.GET("/history/export/:id/download", downloadHistoryExport)
	group.DELETE("/history/export/:id", cancelHistoryExport)
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
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

func startHistoryExport(c *gin.Context) {
	var request historyExportRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": err.Error()})
		return
	}
	request.Type = strings.ToLower(strings.TrimSpace(request.Type))
	request.UUID = strings.TrimSpace(request.UUID)
	if request.Type != "load" && request.Type != "ping" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "type must be load or ping"})
		return
	}
	if request.UUID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "uuid is required"})
		return
	}
	start, end, err := parseHistoryExportRange(request)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": err.Error()})
		return
	}

	historyExports.Lock()
	pruneHistoryExportsLocked(time.Now().UTC())
	for _, existing := range historyExports.jobs {
		if existing.Status == "queued" || existing.Status == "running" {
			historyExports.Unlock()
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "error", "message": "another history export is running"})
			return
		}
	}
	now := time.Now().UTC()
	job := &historyExportJob{
		ID: newHistoryExportID(), Type: request.Type, UUID: request.UUID,
		Status: "queued", Start: start, End: end, CreatedAt: now, ExpiresAt: now.Add(historyExportTTL),
	}
	ctx, cancel := context.WithCancel(context.Background())
	job.cancel = cancel
	historyExports.jobs[job.ID] = job
	historyExports.Unlock()
	go runHistoryExport(ctx, job.ID)
	c.JSON(http.StatusAccepted, gin.H{"status": "success", "data": cloneHistoryExportJob(job)})
}

func runHistoryExport(ctx context.Context, id string) {
	historyExports.Lock()
	job := historyExports.jobs[id]
	if job == nil {
		historyExports.Unlock()
		return
	}
	job.Status = "running"
	job.Progress = 1
	historyExports.Unlock()

	directory := filepath.Join("data", "exports")
	err := os.MkdirAll(directory, 0750)
	path := filepath.Join(directory, id+".csv")
	if err == nil {
		err = writeHistoryExport(ctx, id, path)
	}

	historyExports.Lock()
	defer historyExports.Unlock()
	job = historyExports.jobs[id]
	if job == nil {
		_ = os.Remove(path)
		return
	}
	switch {
	case errors.Is(err, context.Canceled):
		job.Status = "cancelled"
		_ = os.Remove(path)
	case err != nil:
		job.Status = "failed"
		job.Error = err.Error()
		_ = os.Remove(path)
	default:
		job.Status = "done"
		job.Progress = 100
		job.Path = path
		client, lookupErr := clients.GetClientByUUID(job.UUID)
		name := job.UUID
		if lookupErr == nil && strings.TrimSpace(client.Name) != "" {
			name = client.Name
		}
		job.Filename = sanitizeExportName(fmt.Sprintf("komari-%s-%s-%s.csv", name, job.Type, job.Start.Format("20060102-1504")))
	}
}

func writeHistoryExport(ctx context.Context, id, path string) error {
	historyExports.Lock()
	job := cloneHistoryExportJob(historyExports.jobs[id])
	historyExports.Unlock()
	if job == nil {
		return fmt.Errorf("export job not found")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
		return err
	}
	writer := csv.NewWriter(file)
	if job.Type == "ping" {
		err = exportPingMetrics(ctx, writer, job)
	} else {
		err = exportResourceMetrics(ctx, writer, job)
	}
	writer.Flush()
	if err == nil {
		err = writer.Error()
	}
	return err
}

func exportResourceMetrics(ctx context.Context, writer *csv.Writer, job *historyExportJob) error {
	queries := make([]metric.Query, 0, len(resourceExportMetrics))
	for _, name := range resourceExportMetrics {
		queries = append(queries, metric.Query{
			MetricName: name, EntityID: job.UUID, Start: job.Start, End: job.End, Order: metric.OrderAsc,
		})
	}
	store := metricstore.GetStore()
	if store == nil {
		return fmt.Errorf("metric store is not initialized")
	}
	series, err := store.QueryBatch(ctx, queries)
	if err != nil {
		return err
	}
	type resourceRow struct {
		time   time.Time
		values map[string]float64
	}
	rows := make(map[int64]*resourceRow)
	for index, points := range series {
		for _, point := range points {
			key := point.Timestamp.UnixNano()
			row := rows[key]
			if row == nil {
				row = &resourceRow{time: point.Timestamp, values: make(map[string]float64)}
				rows[key] = row
			}
			row.values[resourceExportMetrics[index]] = point.Value
		}
		setHistoryExportProgress(job.ID, 5+(index+1)*75/len(series))
	}
	keys := make([]int64, 0, len(rows))
	for key := range rows {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	header := append([]string{"Client", "Time"}, resourceExportMetrics...)
	if err := writer.Write(header); err != nil {
		return err
	}
	for index, key := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		row := rows[key]
		record := []string{job.UUID, row.time.UTC().Format(time.RFC3339Nano)}
		for _, name := range resourceExportMetrics {
			value, ok := row.values[name]
			if !ok {
				record = append(record, "")
				continue
			}
			record = append(record, formatExportFloat(value))
		}
		if err := writer.Write(record); err != nil {
			return err
		}
		if index%1024 == 0 && len(keys) > 0 {
			setHistoryExportProgress(job.ID, 80+(index+1)*19/len(keys))
		}
	}
	return nil
}

func exportPingMetrics(ctx context.Context, writer *csv.Writer, job *historyExportJob) error {
	store := metricstore.GetStore()
	if store == nil {
		return fmt.Errorf("metric store is not initialized")
	}
	series, err := store.QueryBatch(ctx, []metric.Query{
		{
			MetricName: metricstore.MetricPingLatency, EntityID: job.UUID,
			Start: job.Start, End: job.End, Order: metric.OrderAsc,
		},
		{
			MetricName: metricstore.MetricPingLoss, EntityID: job.UUID,
			Start: job.Start, End: job.End, Order: metric.OrderAsc,
		},
	})
	if err != nil {
		return err
	}
	type pingRow struct {
		time       time.Time
		task       string
		latency    float64
		loss       float64
		hasLatency bool
		hasLoss    bool
	}
	rows := make(map[string]*pingRow)
	for seriesIndex, points := range series {
		for _, point := range points {
			task := point.Tags["task_id"]
			key := task + "\x00" + strconv.FormatInt(point.Timestamp.UnixNano(), 10)
			row := rows[key]
			if row == nil {
				row = &pingRow{time: point.Timestamp, task: task}
				rows[key] = row
			}
			if seriesIndex == 0 {
				row.latency = point.Value
				row.hasLatency = true
			} else {
				row.loss = point.Value
				row.hasLoss = true
			}
		}
	}
	ordered := make([]*pingRow, 0, len(rows))
	for _, row := range rows {
		ordered = append(ordered, row)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].time.Equal(ordered[j].time) {
			return ordered[i].task < ordered[j].task
		}
		return ordered[i].time.Before(ordered[j].time)
	})
	if err := writer.Write([]string{"Client", "Task", "Time", "Ping(ms)", "Loss(%)"}); err != nil {
		return err
	}
	for index, row := range ordered {
		if err := ctx.Err(); err != nil {
			return err
		}
		latency, loss := "", ""
		if row.hasLatency && row.latency >= 0 {
			latency = formatExportFloat(row.latency)
		}
		if row.hasLoss {
			loss = formatExportFloat(row.loss * 100)
		} else if row.hasLatency {
			loss = "0"
			if row.latency < 0 {
				loss = "100"
			}
		}
		if err := writer.Write([]string{job.UUID, row.task, row.time.UTC().Format(time.RFC3339Nano), latency, loss}); err != nil {
			return err
		}
		if index%2048 == 0 && len(ordered) > 0 {
			setHistoryExportProgress(job.ID, 5+(index+1)*94/len(ordered))
		}
	}
	return nil
}

func formatExportFloat(value float64) string {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return ""
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func setHistoryExportProgress(id string, progress int) {
	historyExports.Lock()
	if job := historyExports.jobs[id]; job != nil && job.Status == "running" && progress > job.Progress {
		job.Progress = progress
	}
	historyExports.Unlock()
}

func getHistoryExport(c *gin.Context) {
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
	store := metricstore.GetStore()
	resourceHours, pingHours := 24, 24
	if store != nil {
		if definitions, err := store.ListMetrics(c.Request.Context()); err == nil {
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
	c.JSON(http.StatusOK, gin.H{"status": "success", "data": gin.H{"resource_hours": resourceHours, "ping_hours": pingHours}})
}

func cloneHistoryExportJob(job *historyExportJob) *historyExportJob {
	if job == nil {
		return nil
	}
	clone := *job
	clone.cancel = nil
	return &clone
}

func pruneHistoryExportsLocked(now time.Time) {
	for id, job := range historyExports.jobs {
		if now.Before(job.ExpiresAt) {
			continue
		}
		if job.cancel != nil {
			job.cancel()
		}
		if job.Path != "" {
			_ = os.Remove(job.Path)
		}
		delete(historyExports.jobs, id)
	}
}

func sanitizeExportName(name string) string {
	return strings.Map(func(value rune) rune {
		if value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' || strings.ContainsRune("-_.", value) {
			return value
		}
		return '_'
	}, name)
}
