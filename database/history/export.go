package history

import (
	"context"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/config"
)

const exportTTL = 48 * time.Hour

var ErrExportQueueFull = errors.New("history export queue is full")

type ExportJob struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	Status     string    `json:"status"`
	Progress   int       `json:"progress"`
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Error      string    `json:"error,omitempty"`
	file       string
	clientName string
	request    QueryRequest
	cancel     context.CancelFunc
}

var exportState = struct {
	sync.RWMutex
	jobs  map[string]*ExportJob
	queue chan *ExportJob
	once  sync.Once
}{
	jobs:  make(map[string]*ExportJob),
	queue: make(chan *ExportJob, 8),
}

// ExportRetentionHours returns the configured retention periods for resource and
// ping history in hours, falling back to sensible defaults when settings are
// unavailable.
func ExportRetentionHours() (resource, ping int) {
	settings, err := config.GetManyAs[config.Settings]()
	if err != nil || settings == nil {
		return 720, 24 // 30d / 1d
	}
	r := settings.RecordPreserveTime
	if r <= 0 {
		r = 720
	}
	p := settings.PingRecordPreserveTime
	if p <= 0 {
		p = 24
	}
	return r, p
}

// parseExportRange resolves a QueryRequest to concrete start/end times while
// enforcing maxHours instead of the interactive 90-day cap used in parseRequest.
func parseExportRange(req QueryRequest, maxHours int) (start, end time.Time, err error) {
	end = time.Now()
	if req.End != "" {
		if end, err = time.Parse(time.RFC3339, req.End); err != nil {
			return
		}
	}
	if req.Start != "" {
		if start, err = time.Parse(time.RFC3339, req.Start); err != nil {
			return
		}
	} else if req.Hours > 0 {
		if req.Hours > maxHours {
			err = fmt.Errorf("export range %d h exceeds retention limit %d h", req.Hours, maxHours)
			return
		}
		start = end.Add(-time.Duration(req.Hours) * time.Hour)
	} else {
		start = end.Add(-4 * time.Hour)
	}
	if !end.After(start) {
		err = errors.New("end must be after start")
		return
	}
	maxDur := time.Duration(maxHours) * time.Hour
	if end.Sub(start) > maxDur {
		err = fmt.Errorf("export range exceeds retention limit %d h", maxHours)
	}
	return
}

var unsafeFilenameRe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// sanitizeFilename replaces characters unsafe for filesystem names with underscores.
func sanitizeFilename(s string) string {
	s = strings.TrimSpace(s)
	s = unsafeFilenameRe.ReplaceAllString(s, "_")
	s = strings.Trim(s, "_")
	if s == "" {
		s = "unknown"
	}
	return s
}

// buildExportFilename produces a descriptive CSV filename.
// Format: {node}_{YYYYMMDD}_{YYYYMMDD}_{type}.csv
func buildExportFilename(clientName, jobType string, start, end time.Time) string {
	name := sanitizeFilename(clientName)
	typeTag := "resource"
	if jobType == "ping" {
		typeTag = "ping"
	}
	return fmt.Sprintf("%s_%s_%s_%s.csv",
		name,
		start.Format("20060102"),
		end.Format("20060102"),
		typeTag,
	)
}

// lookupClientNames returns a map from UUID → display name for the given UUIDs.
// Missing entries fall back to the UUID itself.
func lookupClientNames(uuids []string) map[string]string {
	result := make(map[string]string, len(uuids))
	for _, u := range uuids {
		result[u] = u // fallback
	}
	if len(uuids) == 0 {
		return result
	}
	var clients []models.Client
	if err := dbcore.GetReadDBInstance().
		Select("uuid, name").
		Where("uuid IN ?", uuids).
		Find(&clients).Error; err == nil {
		for _, c := range clients {
			if c.Name != "" {
				result[c.UUID] = c.Name
			}
		}
	}
	return result
}

func StartExport(req QueryRequest, dir string) (*ExportJob, error) {
	if req.Type != "load" && req.Type != "ping" {
		return nil, ErrInvalidQuery
	}
	resourceHours, pingHours := ExportRetentionHours()
	maxHours := resourceHours
	if req.Type == "ping" {
		maxHours = pingHours
	}

	start, end, err := parseExportRange(req, maxHours)
	if err != nil {
		return nil, err
	}
	req.Start = start.Format(time.RFC3339)
	req.End = end.Format(time.RFC3339)
	req.Hours = 0

	// Resolve the client display name for the filename.
	clientName := req.UUID
	if req.UUID != "" {
		names := lookupClientNames([]string{req.UUID})
		if n := names[req.UUID]; n != "" {
			clientName = n
		}
	}

	idBytes := make([]byte, 12)
	if _, err := rand.Read(idBytes); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(idBytes)
	now := time.Now()
	filename := buildExportFilename(clientName, req.Type, start, end)
	job := &ExportJob{
		ID:         id,
		Type:       req.Type,
		Status:     "queued",
		Start:      start,
		End:        end,
		CreatedAt:  now,
		ExpiresAt:  now.Add(exportTTL),
		clientName: clientName,
		request:    req,
		file:       filepath.Join(dir, id+".csv"),
	}
	// Store the intended download filename alongside the temp file.
	job.file = filepath.Join(dir, id+".csv")
	_ = filename // used in DownloadHistoryExport via ExportFilename

	exportState.Lock()
	exportState.jobs[job.ID] = job
	exportState.Unlock()
	exportState.once.Do(func() {
		cleanupExportDir(dir)
		go exportWorker()
		go func() {
			ticker := time.NewTicker(time.Hour)
			defer ticker.Stop()
			for range ticker.C {
				cleanupExports()
			}
		}()
	})

	select {
	case exportState.queue <- job:
		return GetExport(job.ID), nil
	default:
		exportState.Lock()
		delete(exportState.jobs, job.ID)
		exportState.Unlock()
		return nil, ErrExportQueueFull
	}
}

func GetExport(id string) *ExportJob {
	exportState.RLock()
	defer exportState.RUnlock()
	return cloneJob(exportState.jobs[id])
}

// ExportFilename returns the disk path and the user-facing download filename for
// a finished export job.  The second return value is false when the job is not
// yet done.
func ExportFilename(id string) (diskPath, downloadName string, ok bool) {
	exportState.RLock()
	defer exportState.RUnlock()
	job := exportState.jobs[id]
	if job == nil || job.Status != "done" {
		return "", "", false
	}
	download := buildExportFilename(job.clientName, job.Type, job.Start, job.End)
	return job.file, download, true
}

// ExportFile is kept for compatibility; prefer ExportFilename.
func ExportFile(id string) (string, bool) {
	path, _, ok := ExportFilename(id)
	return path, ok
}

func CancelExport(id string) bool {
	exportState.Lock()
	defer exportState.Unlock()
	job := exportState.jobs[id]
	if job == nil {
		return false
	}
	if job.cancel != nil {
		job.cancel()
	}
	if job.Status == "queued" {
		job.Status = "cancelled"
	}
	return true
}

func cloneJob(job *ExportJob) *ExportJob {
	if job == nil {
		return nil
	}
	copy := *job
	copy.file = ""
	copy.clientName = ""
	copy.request = QueryRequest{}
	copy.cancel = nil
	return &copy
}

func exportWorker() {
	for job := range exportState.queue {
		exportState.Lock()
		if job.Status == "cancelled" {
			exportState.Unlock()
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		job.cancel = cancel
		job.Status = "running"
		exportState.Unlock()

		err := runExport(ctx, job)
		exportState.Lock()
		job.cancel = nil
		switch {
		case errors.Is(err, context.Canceled):
			job.Status = "cancelled"
			_ = os.Remove(job.file)
		case err != nil:
			job.Status = "failed"
			job.Error = err.Error()
			_ = os.Remove(job.file)
		default:
			job.Status = "done"
			job.Progress = 100
		}
		exportState.Unlock()
		cleanupExports()
	}
}

func runExport(ctx context.Context, job *ExportJob) error {
	if err := os.MkdirAll(filepath.Dir(job.file), 0o750); err != nil {
		return err
	}
	partial := job.file + ".part"
	file, err := os.OpenFile(partial, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	writer := csv.NewWriter(file)
	if job.Type == "ping" {
		err = exportPing(ctx, job, writer)
	} else {
		err = exportLoad(ctx, job, writer)
	}
	writer.Flush()
	if flushErr := writer.Error(); err == nil {
		err = flushErr
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(partial)
		return err
	}
	return os.Rename(partial, job.file)
}

func exportWindows(ctx context.Context, job *ExportJob, fn func(time.Time, time.Time) error) error {
	start, _ := time.Parse(time.RFC3339, job.request.Start)
	end, _ := time.Parse(time.RFC3339, job.request.End)
	total := end.Sub(start)
	for cursor := start; cursor.Before(end); {
		if err := ctx.Err(); err != nil {
			return err
		}
		next := cursor.Add(15 * time.Minute)
		if next.After(end) {
			next = end
		}
		if err := fn(cursor, next); err != nil {
			return err
		}
		exportState.Lock()
		job.Progress = int(next.Sub(start) * 100 / total)
		exportState.Unlock()
		cursor = next

		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
	return nil
}

// exportPing writes a human-readable Ping CSV.
// Columns: Client, Time, Ping (ms), Loss (%)
// Each row represents one raw measurement; loss is 100.00 when value < 0.
func exportPing(ctx context.Context, job *ExportJob, writer *csv.Writer) error {
	if err := writer.Write([]string{"Client", "Time", "Ping (ms)", "Loss (%)"}); err != nil {
		return err
	}

	// Pre-load a name cache for all clients we might encounter.
	nameCache := make(map[string]string)
	if job.request.UUID != "" {
		names := lookupClientNames([]string{job.request.UUID})
		for k, v := range names {
			nameCache[k] = v
		}
	}

	return exportWindows(ctx, job, func(start, end time.Time) error {
		db := dbcore.GetReadDBInstance().WithContext(ctx).Table("ping_records").
			Select("client, task_id, time, value").Where("time >= ? AND time < ?", start, end)
		if job.request.UUID != "" {
			db = db.Where("client = ?", job.request.UUID)
		}
		if job.request.TaskID != nil {
			db = db.Where("task_id = ?", *job.request.TaskID)
		}
		rows, err := db.Order("time ASC").Rows()
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var clientUUID string
			var task uint
			var recorded models.LocalTime
			var value int
			if err := rows.Scan(&clientUUID, &task, &recorded, &value); err != nil {
				return err
			}

			// Resolve display name on demand and cache.
			displayName, cached := nameCache[clientUUID]
			if !cached {
				m := lookupClientNames([]string{clientUUID})
				displayName = m[clientUUID]
				nameCache[clientUUID] = displayName
			}

			var pingMS, lossStr string
			if value < 0 {
				pingMS = ""
				lossStr = "100.00"
			} else {
				pingMS = strconv.Itoa(value)
				lossStr = "0.00"
			}

			if err := writer.Write([]string{
				displayName,
				recorded.ToTime().Format(time.RFC3339),
				pingMS,
				lossStr,
			}); err != nil {
				return err
			}
		}
		return rows.Err()
	})
}

// formatGiB converts a byte count to a GiB string with 3 decimal places.
func formatGiB(bytes int64) string {
	return strconv.FormatFloat(float64(bytes)/math.Pow(1024, 3), 'f', 3, 64)
}

// formatKiBs converts a bytes-per-second value to KiB/s with 2 decimal places.
func formatKiBs(bytesPerSec int64) string {
	return strconv.FormatFloat(float64(bytesPerSec)/1024, 'f', 2, 64)
}

// loadHeaders returns the column headers for the resource CSV (source column removed).
var loadHeaders = []string{
	"Client", "Time",
	"CPU (%)", "GPU (%)",
	"RAM (GiB)", "RAM Total (GiB)",
	"Swap (GiB)", "Swap Total (GiB)",
	"Load", "Temperature (°C)",
	"Disk (GiB)", "Disk Total (GiB)",
	"Net In (KiB/s)", "Net Out (KiB/s)",
	"Net Total Up (GiB)", "Net Total Down (GiB)",
	"Traffic Up (GiB)", "Traffic Down (GiB)",
	"Processes", "TCP Connections", "UDP Connections",
}

// loadCSVRowValues converts a Record into formatted column values aligned with
// loadHeaders (index 0 = Client, 1 = Time, …).
func loadCSVRowValues(clientName string, record models.Record) []string {
	t := record.Time.ToTime().Format(time.RFC3339)
	return []string{
		clientName,
		t,
		fmt.Sprintf("%.2f", record.Cpu),
		fmt.Sprintf("%.2f", record.Gpu),
		formatGiB(record.Ram),
		formatGiB(record.RamTotal),
		formatGiB(record.Swap),
		formatGiB(record.SwapTotal),
		fmt.Sprintf("%.2f", record.Load),
		fmt.Sprintf("%.1f", record.Temp),
		formatGiB(record.Disk),
		formatGiB(record.DiskTotal),
		formatKiBs(record.NetIn),
		formatKiBs(record.NetOut),
		formatGiB(record.NetTotalUp),
		formatGiB(record.NetTotalDown),
		formatGiB(record.TrafficUp),
		formatGiB(record.TrafficDown),
		strconv.Itoa(record.Process),
		strconv.Itoa(record.Connections),
		strconv.Itoa(record.ConnectionsUdp),
	}
}

// isZeroValue returns true when the formatted value represents a zero quantity.
func isZeroValue(s string) bool {
	f, err := strconv.ParseFloat(s, 64)
	return err == nil && f == 0
}

// exportLoad writes a human-readable resource CSV with:
//   - "source" column removed
//   - client UUID replaced by display name
//   - byte values expressed in GiB / KiB/s
//   - columns that are all-zero across every row omitted
func exportLoad(ctx context.Context, job *ExportJob, writer *csv.Writer) error {
	if job.request.UUID == "" {
		return errors.New("load export requires uuid")
	}

	// Look up the display name once.
	names := lookupClientNames([]string{job.request.UUID})
	clientName := names[job.request.UUID]

	// Buffer all data rows so we can determine which columns are all-zero.
	var rows [][]string
	collectErr := exportWindows(ctx, job, func(start, end time.Time) error {
		for _, table := range []string{"records_long_term", "records"} {
			db := dbcore.GetReadDBInstance().WithContext(ctx).Table(table).
				Where("client = ? AND time >= ? AND time < ?", job.request.UUID, start, end)
			dbRows, err := db.Order("time ASC").Rows()
			if err != nil {
				return err
			}
			for dbRows.Next() {
				var record models.Record
				if err := db.ScanRows(dbRows, &record); err != nil {
					dbRows.Close()
					return err
				}
				rows = append(rows, loadCSVRowValues(clientName, record))
			}
			if err := dbRows.Err(); err != nil {
				dbRows.Close()
				return err
			}
			dbRows.Close()
		}
		return nil
	})
	if collectErr != nil {
		return collectErr
	}

	// Determine which columns (beyond Client and Time) are all-zero so we can drop them.
	const fixedCols = 2 // Client, Time are always kept
	totalCols := len(loadHeaders)
	zeroCol := make([]bool, totalCols)
	for i := fixedCols; i < totalCols; i++ {
		zeroCol[i] = true // assume zero until proven otherwise
	}
	for _, row := range rows {
		for i := fixedCols; i < totalCols; i++ {
			if i < len(row) && !isZeroValue(row[i]) {
				zeroCol[i] = false
			}
		}
	}

	// Build the filtered header.
	header := make([]string, 0, totalCols)
	keepCol := make([]bool, totalCols)
	for i, h := range loadHeaders {
		if i < fixedCols || !zeroCol[i] {
			header = append(header, h)
			keepCol[i] = true
		}
	}
	if err := writer.Write(header); err != nil {
		return err
	}

	// Write data rows.
	for _, row := range rows {
		filtered := make([]string, 0, len(header))
		for i, v := range row {
			if i < totalCols && keepCol[i] {
				filtered = append(filtered, v)
			}
		}
		if err := writer.Write(filtered); err != nil {
			return err
		}
	}
	return nil
}

func cleanupExports() {
	now := time.Now()
	exportState.Lock()
	defer exportState.Unlock()
	for id, job := range exportState.jobs {
		if now.After(job.ExpiresAt) {
			if job.file != "" {
				_ = os.Remove(job.file)
			}
			delete(exportState.jobs, id)
		}
	}
}

func cleanupExportDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-exportTTL)
	for _, entry := range entries {
		info, err := entry.Info()
		if err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
}
