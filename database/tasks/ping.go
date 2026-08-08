package tasks

import (
	"context"
	"errors"
	"time"

	"github.com/komari-monitor/komari/cmd/flags"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/utils"
	"gorm.io/gorm"
)

// AddPingTask 创建延迟监测任务。defaultOn 表示新加入的服务器是否自动开启此监测。
func AddPingTask(clients []string, defaultOn bool, name string, target, task_type string, interval int) (uint, error) {
	normalizedClients := normalizePingClients(models.StringArray(clients))
	task := models.PingTask{
		Clients:   normalizedClients,
		DefaultOn: defaultOn,
		Name:      name,
		Type:      task_type,
		Target:    target,
		Interval:  interval,
	}
	err := dbcore.Write(context.Background(), func(db *gorm.DB) error {
		return db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Create(&task).Error; err != nil {
				return err
			}
			result := tx.Model(&models.PingTask{}).Where("id = ?", task.Id).Update("weight", int(task.Id))
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 {
				return gorm.ErrRecordNotFound
			}
			return nil
		})
	})
	if err != nil {
		return 0, err
	}
	ReloadPingSchedule()
	return task.Id, nil
}

func DeletePingTask(id []uint) error {
	var rowsAffected int64
	err := dbcore.Write(context.Background(), func(db *gorm.DB) error {
		result := db.Where("id IN ?", id).Delete(&models.PingTask{})
		rowsAffected = result.RowsAffected
		return result.Error
	})
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return ReloadPingSchedule()
}

// EditPingTask 批量更新延迟监测任务配置。
func EditPingTask(tasks []*models.PingTask) error {
	err := dbcore.Write(context.Background(), func(db *gorm.DB) error {
		return db.Transaction(func(tx *gorm.DB) error {
			for _, task := range tasks {
				task.Clients = normalizePingClients(task.Clients)
				updates := map[string]interface{}{
					"name":        task.Name,
					"clients":     task.Clients,
					"all_clients": task.DefaultOn,
					"type":        task.Type,
					"target":      task.Target,
					"interval":    task.Interval,
				}
				result := tx.Model(&models.PingTask{}).Where("id = ?", task.Id).Updates(updates)
				if result.Error != nil {
					return result.Error
				}
				if result.RowsAffected == 0 {
					return gorm.ErrRecordNotFound
				}
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	return ReloadPingSchedule()
}

// normalizePingClients 保持 clients 字段序列化为 JSON 数组，避免空值变成 null。
func normalizePingClients(clients models.StringArray) models.StringArray {
	if clients == nil {
		return models.StringArray{}
	}
	return clients
}

func GetAllPingTasks() ([]models.PingTask, error) {
	db := dbcore.GetDBInstance()
	var tasks []models.PingTask
	if err := db.Order("weight ASC").Order("id ASC").Find(&tasks).Error; err != nil {
		return nil, err
	}
	return tasks, nil
}

// GetPingTasksByClient 获取指定服务器需要执行的延迟监测任务。
func GetPingTasksByClient(uuid string) []models.PingTask {
	db := dbcore.GetDBInstance()
	var tasks []models.PingTask
	if err := db.Where("clients LIKE ?", `%"`+uuid+`"%`).Find(&tasks).Error; err != nil {
		return nil
	}
	return tasks
}

func UpdatePingTaskOrder(order map[uint]int) error {
	err := dbcore.Write(context.Background(), func(db *gorm.DB) error {
		return db.Transaction(func(tx *gorm.DB) error {
			for id, weight := range order {
				result := tx.Model(&models.PingTask{}).Where("id = ?", id).Update("weight", weight)
				if result.Error != nil {
					return result.Error
				}
				if result.RowsAffected == 0 {
					return gorm.ErrRecordNotFound
				}
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	ReloadPingSchedule()
	return nil
}

func SavePingRecord(record models.PingRecord) error {
	return SavePingRecordContext(context.Background(), record)
}

func SavePingRecordContext(ctx context.Context, record models.PingRecord) error {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	err := dbcore.Write(ctx, func(db *gorm.DB) error {
		return db.Create(&record).Error
	})
	if err == nil {
		return nil
	}
	if dbcore.IsBusyError(err) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return spoolPing(record)
	}
	return err
}

func DeletePingRecordsBefore(cutoff time.Time) error {
	if !flags.IsSQLite() {
		return dbcore.Write(context.Background(), func(db *gorm.DB) error {
			if err := db.Where("time < ?", cutoff).Delete(&models.PingRecord{}).Error; err != nil {
				return err
			}
			return db.Where("time < ?", cutoff).Delete(&models.PingRollup{}).Error
		})
	}
	const sqliteCleanupBatchSize = 1000
	_, err := dbcore.TryMaintenance(context.Background(), func(db *gorm.DB) error {
		for _, table := range []string{"ping_records", "ping_rollups"} {
			if err := db.Exec(
				"DELETE FROM "+table+" WHERE rowid IN (SELECT rowid FROM "+table+" WHERE time < ? LIMIT ?)",
				cutoff,
				sqliteCleanupBatchSize,
			).Error; err != nil {
				return err
			}
		}
		return nil
	})
	return err
}

func DeletePingRecords(id []uint) error {
	var rowsAffected int64
	err := dbcore.Write(context.Background(), func(db *gorm.DB) error {
		rawResult := db.Where("task_id IN ?", id).Delete(&models.PingRecord{})
		if rawResult.Error != nil {
			return rawResult.Error
		}
		rollupResult := db.Where("task_id IN ?", id).Delete(&models.PingRollup{})
		if rollupResult.Error != nil {
			return rollupResult.Error
		}
		rowsAffected = rawResult.RowsAffected + rollupResult.RowsAffected
		return nil
	})
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func DeleteAllPingRecords() error {
	return dbcore.Write(context.Background(), func(db *gorm.DB) error {
		if err := db.Exec("DELETE FROM ping_records").Error; err != nil {
			return err
		}
		return db.Exec("DELETE FROM ping_rollups").Error
	})
}
func ReloadPingSchedule() error {
	db := dbcore.GetDBInstance()
	var pingTasks []models.PingTask
	if err := db.Find(&pingTasks).Error; err != nil {
		return err
	}
	return utils.ReloadPingSchedule(pingTasks)
}

// AddDefaultOnClientUUID 在新客户端注册后，把该 UUID 追加到所有 default_on=true 的任务的 clients 中（去重）。
func AddDefaultOnClientUUID(uuid string) error {
	if uuid == "" {
		return nil
	}
	changed := false
	if err := dbcore.Write(context.Background(), func(db *gorm.DB) error {
		return db.Transaction(func(tx *gorm.DB) error {
			var tasks []models.PingTask
			if err := tx.Where("all_clients = ?", true).Find(&tasks).Error; err != nil {
				return err
			}
			for _, task := range tasks {
				exists := false
				for _, client := range task.Clients {
					if client == uuid {
						exists = true
						break
					}
				}
				if exists {
					continue
				}
				next := append(append(models.StringArray{}, task.Clients...), uuid)
				if err := tx.Model(&models.PingTask{}).Where("id = ?", task.Id).Update("clients", next).Error; err != nil {
					return err
				}
				changed = true
			}
			return nil
		})
	}); err != nil {
		return err
	}
	if changed {
		return ReloadPingSchedule()
	}
	return nil
}

func GetPingRecords(uuid string, taskId int, start, end time.Time) ([]models.PingRecord, error) {
	db := dbcore.GetReadDBInstance()
	var records []models.PingRecord
	dbQuery := db.Model(&models.PingRecord{})
	if uuid != "" {
		dbQuery = dbQuery.Where("client = ?", uuid)
	}
	if taskId >= 0 {
		dbQuery = dbQuery.Where("task_id = ?", uint(taskId))
	}
	if err := dbQuery.Where("time >= ? AND time <= ?", start, end).Order("time DESC").Find(&records).Error; err != nil {
		return nil, err
	}
	return records, nil
}
