package tasks

import (
	"time"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"gorm.io/gorm"
)

type CommandTask struct {
	ID      string
	Client  string
	Command string
}

func CreateTask(taskId string, clients []string, command string) error {
	items := make([]CommandTask, 0, len(clients))
	for _, client := range clients {
		items = append(items, CommandTask{ID: taskId, Client: client, Command: command})
	}
	if len(items) == 1 {
		return CreateTaskBatch(items)
	}
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
			CreatedAt:  time.Now().UTC(),
		})
	}
	if len(taskResults) > 0 {
		return db.Create(&taskResults).Error
	}
	return nil
}

// CreateTaskBatch atomically creates independent single-client command tasks.
// Typed execution uses one task ID per client so streams and cancellation have
// unambiguous ownership, while legacy multi-client tasks keep CreateTask.
func CreateTaskBatch(items []CommandTask) error {
	if len(items) == 0 {
		return nil
	}
	now := time.Now().UTC()
	return dbcore.GetDBInstance().Transaction(func(tx *gorm.DB) error {
		for _, item := range items {
			task := models.Task{TaskId: item.ID, Clients: models.StringArray{item.Client}, Command: item.Command}
			if err := tx.Create(&task).Error; err != nil {
				return err
			}
			result := models.TaskResult{TaskId: item.ID, Client: item.Client, CreatedAt: now}
			if err := tx.Create(&result).Error; err != nil {
				return err
			}
		}
		return nil
	})
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
func SaveTaskResult(taskId, clientId, result string, exitCode int, timestamp time.Time) error {
	return dbcore.GetDBInstance().
		Model(&models.TaskResult{}).
		Where("task_id = ? AND client = ?", taskId, clientId).
		Updates(map[string]interface{}{
			"result":      result,
			"exit_code":   exitCode,
			"finished_at": timestamp.UTC(),
		}).Error
}

func ClearTaskResultsByTimeBefore(before time.Time) error {
	return dbcore.GetDBInstance().Where("created_at < ?", before.UTC()).Delete(&models.TaskResult{}).Error
}
