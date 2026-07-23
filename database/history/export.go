package history

import (
	"context"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
)

const exportTTL = 48 * time.Hour

var ErrExportQueueFull = errors.New("history export queue is full")

type ExportJob struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Status    string    `json:"status"`
	Progress  int       `json:"progress"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Error     string    `json:"error,omitempty"`
	file      string
	request   QueryRequest
	cancel    context.CancelFunc
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

func StartExport(req QueryRequest, dir string) (*ExportJob, error) {
	start, end, _, _, err := parseRequest(req)
	if err != nil {
		return nil, err
	}
	req.Start = start.Format(time.RFC3339)
	req.End = end.Format(time.RFC3339)
	req.Hours = 0

	idBytes := make([]byte, 12)
	if _, err := rand.Read(idBytes); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(idBytes)
	now := time.Now()
	job := &ExportJob{
		ID: id, Type: req.Type, Status: "queued", Start: start, End: end, CreatedAt: now,
		ExpiresAt: now.Add(exportTTL), request: req, file: filepath.Join(dir, id+".csv"),
	}
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

func ExportFile(id string) (string, bool) {
	exportState.RLock()
	defer exportState.RUnlock()
	job := exportState.jobs[id]
	if job == nil || job.Status != "done" {
		return "", false
	}
	return job.file, true
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

func exportPing(ctx context.Context, job *ExportJob, writer *csv.Writer) error {
	if err := writer.Write([]string{"client", "task_id", "time", "value"}); err != nil {
		return err
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
			var client string
			var task uint
			var recorded models.LocalTime
			var value int
			if err := rows.Scan(&client, &task, &recorded, &value); err != nil {
				return err
			}
			if err := writer.Write([]string{
				client, strconv.FormatUint(uint64(task), 10),
				recorded.ToTime().Format(time.RFC3339Nano), strconv.Itoa(value),
			}); err != nil {
				return err
			}
		}
		return rows.Err()
	})
}

func exportLoad(ctx context.Context, job *ExportJob, writer *csv.Writer) error {
	if job.request.UUID == "" {
		return errors.New("load export requires uuid")
	}
	header := []string{
		"source", "client", "time", "cpu", "gpu", "ram", "ram_total", "swap", "swap_total",
		"load", "temp", "disk", "disk_total", "net_in", "net_out", "net_total_up", "net_total_down",
		"traffic_up", "traffic_down", "process", "connections", "connections_udp",
	}
	if err := writer.Write(header); err != nil {
		return err
	}
	return exportWindows(ctx, job, func(start, end time.Time) error {
		for _, table := range []string{"records_long_term", "records"} {
			db := dbcore.GetReadDBInstance().WithContext(ctx).Table(table).
				Where("client = ? AND time >= ? AND time < ?", job.request.UUID, start, end)
			rows, err := db.Order("time ASC").Rows()
			if err != nil {
				return err
			}
			for rows.Next() {
				var record models.Record
				if err := db.ScanRows(rows, &record); err != nil {
					rows.Close()
					return err
				}
				if err := writer.Write(loadCSVRow(table, record)); err != nil {
					rows.Close()
					return err
				}
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return err
			}
			rows.Close()
		}
		return nil
	})
}

func loadCSVRow(table string, record models.Record) []string {
	return []string{
		table, record.Client, record.Time.ToTime().Format(time.RFC3339Nano),
		fmt.Sprint(record.Cpu), fmt.Sprint(record.Gpu), strconv.FormatInt(record.Ram, 10),
		strconv.FormatInt(record.RamTotal, 10), strconv.FormatInt(record.Swap, 10),
		strconv.FormatInt(record.SwapTotal, 10), fmt.Sprint(record.Load), fmt.Sprint(record.Temp),
		strconv.FormatInt(record.Disk, 10), strconv.FormatInt(record.DiskTotal, 10),
		strconv.FormatInt(record.NetIn, 10), strconv.FormatInt(record.NetOut, 10),
		strconv.FormatInt(record.NetTotalUp, 10), strconv.FormatInt(record.NetTotalDown, 10),
		strconv.FormatInt(record.TrafficUp, 10), strconv.FormatInt(record.TrafficDown, 10),
		strconv.Itoa(record.Process), strconv.Itoa(record.Connections), strconv.Itoa(record.ConnectionsUdp),
	}
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
