package tasks

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"gorm.io/gorm"
)

const (
	pingSpoolDir = "data/ingest-spool/ping"
	pingSpoolMax = 256 << 20
)

var (
	pingSpoolMu      sync.Mutex
	pingSpoolOnce    sync.Once
	ErrPingSpoolFull = errors.New("ping persistence spool is full")
)

type spooledPing struct {
	Client string `json:"client"`
	TaskID uint   `json:"task_id"`
	Time   string `json:"time"`
	Value  int    `json:"value"`
}

func StartPingSpool() {
	pingSpoolOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(3 * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				if err := drainPingSpool(); err != nil && !dbcore.IsBusyError(err) {
					log.Printf("Failed to drain ping spool: %v", err)
				}
			}
		}()
	})
}

func spoolPing(record models.PingRecord) error {
	StartPingSpool()
	pingSpoolMu.Lock()
	defer pingSpoolMu.Unlock()

	if err := os.MkdirAll(pingSpoolDir, 0o750); err != nil {
		return err
	}
	entries, _ := os.ReadDir(pingSpoolDir)
	var size int64
	for _, entry := range entries {
		if info, err := entry.Info(); err == nil {
			size += info.Size()
		}
	}
	if size >= pingSpoolMax {
		return ErrPingSpoolFull
	}

	file, err := os.OpenFile(filepath.Join(pingSpoolDir, "current.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	item := spooledPing{
		Client: record.Client,
		TaskID: record.TaskId,
		Time:   record.Time.ToTime().Format(time.RFC3339Nano),
		Value:  record.Value,
	}
	err = json.NewEncoder(file).Encode(item)
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

func drainPingSpool() error {
	pingSpoolMu.Lock()
	if err := os.MkdirAll(pingSpoolDir, 0o750); err != nil {
		pingSpoolMu.Unlock()
		return err
	}
	entries, err := os.ReadDir(pingSpoolDir)
	if err != nil {
		pingSpoolMu.Unlock()
		return err
	}
	hasSegment := false
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "segment-") && strings.HasSuffix(entry.Name(), ".jsonl") {
			hasSegment = true
			break
		}
	}
	current := filepath.Join(pingSpoolDir, "current.jsonl")
	// Keep at most one waiting segment while SQLite is unavailable. Otherwise a
	// multi-day lock incident creates hundreds of thousands of tiny files.
	if !hasSegment {
		if info, statErr := os.Stat(current); statErr == nil && info.Size() > 0 {
			segment := filepath.Join(pingSpoolDir, fmt.Sprintf("segment-%d.jsonl", time.Now().UnixNano()))
			if err := os.Rename(current, segment); err != nil {
				pingSpoolMu.Unlock()
				return err
			}
		}
	}
	pingSpoolMu.Unlock()

	entries, err = os.ReadDir(pingSpoolDir)
	if err != nil {
		return err
	}
	paths := make([]string, 0)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "segment-") && strings.HasSuffix(entry.Name(), ".jsonl") {
			paths = append(paths, filepath.Join(pingSpoolDir, entry.Name()))
		}
	}
	sort.Strings(paths)
	for _, path := range paths {
		if err := drainPingSegment(path); err != nil {
			return err
		}
	}
	return nil
}

func drainPingSegment(path string) error {
	offsetPath := path + ".offset"
	for {
		offset, err := readPingSpoolOffset(offsetPath)
		if err != nil {
			return err
		}
		batch, nextOffset, done, err := readPingSpoolBatch(path, offset, 500)
		if err != nil {
			return err
		}
		if len(batch) == 0 && done {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			_ = os.Remove(offsetPath)
			return nil
		}

		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		err = dbcore.Write(ctx, func(db *gorm.DB) error {
			return db.Transaction(func(tx *gorm.DB) error {
				return tx.CreateInBatches(batch, 500).Error
			})
		})
		cancel()
		if err != nil {
			return err
		}
		if err := writePingSpoolOffset(offsetPath, nextOffset); err != nil {
			return err
		}
		if done {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			_ = os.Remove(offsetPath)
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readPingSpoolOffset(path string) (int64, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	offset, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || offset < 0 {
		return 0, fmt.Errorf("invalid ping spool offset %q", strings.TrimSpace(string(raw)))
	}
	return offset, nil
}

func readPingSpoolBatch(path string, offset int64, limit int) ([]models.PingRecord, int64, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, offset, false, err
	}
	defer file.Close()
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, false, err
	}

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	batch := make([]models.PingRecord, 0, limit)
	nextOffset := offset
	for len(batch) < limit && scanner.Scan() {
		line := scanner.Bytes()
		nextOffset += int64(len(line) + 1)
		var item spooledPing
		if err := json.Unmarshal(line, &item); err != nil {
			return nil, offset, false, err
		}
		recorded, err := time.Parse(time.RFC3339Nano, item.Time)
		if err != nil {
			return nil, offset, false, err
		}
		batch = append(batch, models.PingRecord{
			Client: item.Client,
			TaskId: item.TaskID,
			Time:   models.FromTime(recorded),
			Value:  item.Value,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, offset, false, err
	}
	info, err := file.Stat()
	if err != nil {
		return nil, offset, false, err
	}
	return batch, nextOffset, nextOffset >= info.Size(), nil
}

func writePingSpoolOffset(path string, offset int64) error {
	temporary := path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	_, err = file.WriteString(strconv.FormatInt(offset, 10))
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, path); err == nil {
		return nil
	}
	_ = os.Remove(path)
	return os.Rename(temporary, path)
}
