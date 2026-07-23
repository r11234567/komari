package tasks

import (
	"context"
	"time"

	"github.com/komari-monitor/komari/cmd/flags"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"gorm.io/gorm"
)

func CreateTask(taskId string, clients []string, command string) error {
	db := dbcore.GetDBInstance()
	// Create a new task in the database with clients as JSON array
	task := models.Task{
		TaskId:  taskId,
		Clients: models.StringArray(clients),
		Command: command,
	}
	if err := db.Create(&task).Error; err != nil {
		return err
	}
	var taskResults []models.TaskResult
	for _, client := range clients {
		taskResults = append(taskResults, models.TaskResult{
			TaskId:     taskId,
			Client:     client,
			Result:     "",
			ExitCode:   nil,
			FinishedAt: nil,
			CreatedAt:  models.FromTime(time.Now()),
		})
	}
	if len(taskResults) > 0 {
		return db.Create(&taskResults).Error
	}
	return nil
}
func GetTaskByTaskId(taskId string) (*models.Task, error) {
	var task models.Task
	if err := dbcore.GetDBInstance().Where("task_id = ?", taskId).First(&task).Error; err != nil {
		return nil, err
	}
	return &task, nil
}
func GetTasksByClientId(clientId string) ([]models.Task, error) {
	var tasks []models.Task
	if err := dbcore.GetDBInstance().Where("clients LIKE ?", "%"+clientId+"%").Find(&tasks).Error; err != nil {
		return nil, err
	}
	return tasks, nil
}

func GetSpecificTaskResult(taskId, clientId string) (*models.TaskResult, error) {
	var result models.TaskResult
	if err := dbcore.GetDBInstance().Where("task_id = ? AND client = ?", taskId, clientId).First(&result).Error; err != nil {
		return nil, err
	}
	return &result, nil
}

func GetAllTasksResultByUUID(uuid string) ([]models.TaskResult, error) {
	var results []models.TaskResult
	if err := dbcore.GetDBInstance().Where("client = ?", uuid).Find(&results).Error; err != nil {
		return nil, err
	}
	return results, nil
}
func GetAllTasks() ([]models.Task, error) {
	var tasks []models.Task
	if err := dbcore.GetDBInstance().Find(&tasks).Error; err != nil {
		return nil, err
	}
	return tasks, nil
}

func GetTaskResultsByTaskId(taskId string) ([]models.TaskResult, error) {
	var results []models.TaskResult
	if err := dbcore.GetDBInstance().Where("task_id = ?", taskId).Find(&results).Error; err != nil {
		return nil, err
	}
	return results, nil
}
func DeleteTaskByTaskId(taskId string) error {
	return dbcore.GetDBInstance().Where("task_id = ?", taskId).Delete(&models.Task{}).Error
}

func SaveTaskResult(taskId, clientId, result string, exitCode int, timestamp models.LocalTime) error {
	return dbcore.Write(context.Background(), func(db *gorm.DB) error {
		return db.Model(&models.TaskResult{}).
			Where("task_id = ? AND client = ?", taskId, clientId).
			Updates(map[string]interface{}{
				"result":      result,
				"exit_code":   exitCode,
				"finished_at": timestamp,
			}).Error
	})
}

func ClearTaskResultsByTimeBefore(before time.Time) error {
	_, err := dbcore.TryMaintenance(context.Background(), func(db *gorm.DB) error {
		if flags.IsSQLite() {
			return db.Exec("DELETE FROM task_results WHERE rowid IN (SELECT rowid FROM task_results WHERE created_at < ? LIMIT 5000)", before.Format(time.RFC3339)).Error
		}
		return db.Where("created_at < ?", before.Format(time.RFC3339)).Delete(&models.TaskResult{}).Error
	})
	return err
}
