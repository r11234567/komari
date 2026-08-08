package notifier

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	messageevent "github.com/komari-monitor/komari/database/models/messageEvent"
	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/pkg/corn"
	"github.com/komari-monitor/komari/utils"
	"github.com/komari-monitor/komari/utils/messageSender"
	"gorm.io/gorm"
)

// InitTrafficReportSchedule 注册三个定时任务：日报、周报、月报
func InitTrafficReportSchedule() {
	// 日报：每天凌晨 0 点
	if err := corn.AddFunc("traffic-report-daily", "0 0 0 * * *", func() {
		runScheduledTrafficReport(TrafficReportDaily)
	}); err != nil {
		log.Println("Failed to register daily traffic report job:", err)
	}

	// 周报：每周一凌晨 0 点 (dow=1)
	if err := corn.AddFunc("traffic-report-weekly", "0 0 0 * * 1", func() {
		runScheduledTrafficReport(TrafficReportWeekly)
	}); err != nil {
		log.Println("Failed to register weekly traffic report job:", err)
	}

	// 月报：每月 1 日凌晨 0 点
	if err := corn.AddFunc("traffic-report-monthly", "0 0 0 1 * *", func() {
		runScheduledTrafficReport(TrafficReportMonthly)
	}); err != nil {
		log.Println("Failed to register monthly traffic report job:", err)
	}

	log.Println("Traffic report schedules registered: daily, weekly, monthly")
}

type TrafficReportCadence string

const (
	TrafficReportDaily   TrafficReportCadence = "daily"
	TrafficReportWeekly  TrafficReportCadence = "weekly"
	TrafficReportMonthly TrafficReportCadence = "monthly"
)

func (cadence TrafficReportCadence) Valid() bool {
	switch cadence {
	case TrafficReportDaily, TrafficReportWeekly, TrafficReportMonthly:
		return true
	default:
		return false
	}
}

type TrafficReportResult struct {
	Sent          bool                 `json:"sent"`
	Cadence       TrafficReportCadence `json:"cadence"`
	Start         time.Time            `json:"start"`
	End           time.Time            `json:"end"`
	ClientCount   int                  `json:"client_count"`
	FailedClients int                  `json:"failed_clients"`
	Message       string               `json:"message,omitempty"`
	Reason        string               `json:"reason,omitempty"`
	Warnings      []string             `json:"warnings,omitempty"`
}

func runScheduledTrafficReport(cadence TrafficReportCadence) {
	result, err := SendTrafficReport(cadence)
	if err != nil {
		log.Printf("Failed to send %s traffic report: %v", cadence, err)
		return
	}
	for _, warning := range result.Warnings {
		log.Printf("Traffic report warning (%s): %s", cadence, warning)
	}
}

// SendTrafficReport runs the same report path used by the scheduler. It is
// exported so an authenticated admin RPC can trigger and verify one report.
func SendTrafficReport(cadence TrafficReportCadence) (TrafficReportResult, error) {
	result := TrafficReportResult{Cadence: cadence}
	if !cadence.Valid() {
		return result, fmt.Errorf("unsupported traffic report cadence %q", cadence)
	}

	// 检查全局通知开关
	enabled, err := config.GetAs[bool](config.NotificationEnabledKey, false)
	if err != nil {
		return result, fmt.Errorf("read notification setting: %w", err)
	}
	if !enabled {
		result.Reason = "notifications are disabled"
		return result, nil
	}

	db := dbcore.GetReadDBInstance()
	now := time.Now()

	// 计算时间范围
	var start, end time.Time
	var eventType, suffix, emoji string

	switch cadence {
	case TrafficReportDaily:
		yesterday := now.AddDate(0, 0, -1)
		start = time.Date(yesterday.Year(), yesterday.Month(), yesterday.Day(), 0, 0, 0, 0, yesterday.Location())
		end = start.AddDate(0, 0, 1)
		eventType = messageevent.DReport
		suffix = fmt.Sprintf("（%s）流量", start.Format("2006-01-02"))
		emoji = "📊"
	case TrafficReportWeekly:
		weekday := int(now.Weekday())
		if weekday == 0 {
			weekday = 7
		}
		lastMonday := now.AddDate(0, 0, -(weekday-1)-7)
		lastSunday := lastMonday.AddDate(0, 0, 6)
		start = time.Date(lastMonday.Year(), lastMonday.Month(), lastMonday.Day(), 0, 0, 0, 0, lastMonday.Location())
		end = time.Date(lastSunday.Year(), lastSunday.Month(), lastSunday.Day(), 0, 0, 0, 0, lastSunday.Location()).AddDate(0, 0, 1)
		eventType = messageevent.WReport
		suffix = fmt.Sprintf("（%s 至 %s）流量", start.Format("2006-01-02"), end.Add(-time.Nanosecond).Format("2006-01-02"))
		emoji = "📈"
	case TrafficReportMonthly:
		firstOfThisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
		firstOfLastMonth := firstOfThisMonth.AddDate(0, -1, 0)
		start = firstOfLastMonth
		end = firstOfThisMonth
		eventType = messageevent.MReport
		suffix = fmt.Sprintf("（%s）流量", start.Format("2006-01"))
		emoji = "📅"
	default:
		return result, fmt.Errorf("unsupported traffic report cadence %q", cadence)
	}
	result.Start, result.End = start, end

	// 查询所有启用该类型报告的服务器配置
	var notifications []models.TrafficReportNotification
	query := db.Model(&models.TrafficReportNotification{}).Where("enable = ?", true)
	switch cadence {
	case TrafficReportDaily:
		query = query.Where("daily = ?", true)
	case TrafficReportWeekly:
		query = query.Where("weekly = ?", true)
	case TrafficReportMonthly:
		query = query.Where("monthly = ?", true)
	}
	if err := query.Find(&notifications).Error; err != nil {
		return result, fmt.Errorf("query %s traffic report notifications: %w", cadence, err)
	}
	if len(notifications) == 0 {
		result.Reason = "no clients enabled for this cadence"
		return result, nil
	}

	// 获取客户端信息
	clientUUIDs := make([]string, 0, len(notifications))
	for _, n := range notifications {
		clientUUIDs = append(clientUUIDs, n.Client)
	}
	var clientList []models.Client
	if err := db.Where("uuid IN ?", clientUUIDs).Find(&clientList).Error; err != nil {
		return result, fmt.Errorf("query clients for %s traffic report: %w", cadence, err)
	}
	clientMap := make(map[string]models.Client, len(clientList))
	for _, c := range clientList {
		clientMap[c.UUID] = c
	}

	// 为每个服务器统计流量并拼接消息
	var lines []string
	eventClients := make([]models.Client, 0, len(notifications))
	for _, n := range notifications {
		c, ok := clientMap[n.Client]
		if !ok {
			result.FailedClients++
			result.Warnings = append(result.Warnings, fmt.Sprintf("client %s: configuration not found", n.Client))
			continue
		}

		used, err := getClientTrafficInRange(n.Client, c.TrafficLimitType, start, end)
		if err != nil {
			result.FailedClients++
			result.Warnings = append(result.Warnings, fmt.Sprintf("client %s: %v", n.Client, err))
			continue
		}

		lines = append(lines, fmt.Sprintf("%s%s：%s", c.Name, suffix, humanBytes(used)))
		eventClients = append(eventClients, c)
	}
	result.ClientCount = len(eventClients)

	if len(lines) == 0 {
		result.Reason = "no client traffic could be computed"
		return result, nil
	}

	message := strings.Join(lines, "\n")
	result.Message = message

	if err := messageSender.SendNotification(models.EventMessage{
		Event:   eventType,
		Clients: eventClients,
		Time:    now,
		Emoji:   emoji,
		Message: message,
	}); err != nil {
		return result, fmt.Errorf("send %s traffic report: %w", cadence, err)
	}
	result.Sent = true
	return result, nil
}

// getClientTrafficInRange 查询某客户端在指定时间段内的流量增量
// 通过累加持久化的精确流量增量字段计算用量
func getClientTrafficInRange(clientUUID string, trafficType string, start, end time.Time) (int64, error) {
	return getClientTrafficInRangeWithDB(dbcore.GetReadDBInstance(), clientUUID, trafficType, start, end)
}

type trafficDeltaRecord struct {
	Time         models.LocalTime `gorm:"column:time"`
	NetTotalUp   int64            `gorm:"column:net_total_up"`
	NetTotalDown int64            `gorm:"column:net_total_down"`
	TrafficUp    int64            `gorm:"column:traffic_up"`
	TrafficDown  int64            `gorm:"column:traffic_down"`
}

func getClientTrafficInRangeWithDB(db *gorm.DB, clientUUID string, trafficType string, start, end time.Time) (int64, error) {
	var recentRecords []trafficDeltaRecord
	if err := db.Table("records").
		Select("time, net_total_up, net_total_down, traffic_up, traffic_down").
		Where("client = ? AND time >= ? AND time < ?", clientUUID, models.FromTime(start), models.FromTime(end)).
		Find(&recentRecords).Error; err != nil {
		return 0, err
	}

	var longTermRecords []trafficDeltaRecord
	if err := db.Table("records_long_term").
		Select("time, net_total_up, net_total_down, traffic_up, traffic_down").
		Where("client = ? AND time >= ? AND time < ?", clientUUID, models.FromTime(start), models.FromTime(end)).
		Find(&longTermRecords).Error; err != nil {
		return 0, err
	}

	records := mergeTrafficRecords(recentRecords, longTermRecords)

	sort.Slice(records, func(i, j int) bool {
		return records[i].Time.ToTime().Before(records[j].Time.ToTime())
	})

	previous, err := getPreviousTrafficDeltaRecord(db, clientUUID, start)
	if err != nil {
		return 0, err
	}

	totalUp, totalDown := sumTrafficDeltas(records, previous)
	return computeUsedByType(strings.ToLower(trafficType), totalUp, totalDown), nil
}

func mergeTrafficRecords(recentRecords, longTermRecords []trafficDeltaRecord) []trafficDeltaRecord {
	recentRecords = deduplicateTrafficRecords(recentRecords)
	longTermRecords = deduplicateTrafficRecords(longTermRecords)

	rawSlots := make(map[time.Time]struct{}, len(recentRecords))
	for _, record := range recentRecords {
		rawSlots[record.Time.ToTime().Truncate(15*time.Minute)] = struct{}{}
	}

	longTermSlots := make(map[time.Time]struct{}, len(longTermRecords))
	records := make([]trafficDeltaRecord, 0, len(longTermRecords)+len(recentRecords))
	for _, record := range longTermRecords {
		slot := record.Time.ToTime().Truncate(15 * time.Minute)
		if _, hasRawSlot := rawSlots[slot]; hasRawSlot && record.TrafficUp == 0 && record.TrafficDown == 0 {
			continue
		}
		longTermSlots[slot] = struct{}{}
		records = append(records, record)
	}

	for _, record := range recentRecords {
		slot := record.Time.ToTime().Truncate(15 * time.Minute)
		if _, exists := longTermSlots[slot]; exists {
			continue
		}
		records = append(records, record)
	}

	return records
}

// Old compaction versions could insert the same client/time slot more than
// once because the legacy history table has no composite unique key. Reports
// must count a persisted slot once even when such rows are still present.
func deduplicateTrafficRecords(records []trafficDeltaRecord) []trafficDeltaRecord {
	byTime := make(map[int64]trafficDeltaRecord, len(records))
	for _, record := range records {
		key := record.Time.ToTime().UnixNano()
		existing, ok := byTime[key]
		if !ok || trafficRecordMagnitude(record) > trafficRecordMagnitude(existing) {
			byTime[key] = record
		}
	}
	result := make([]trafficDeltaRecord, 0, len(byTime))
	for _, record := range byTime {
		result = append(result, record)
	}
	return result
}

func trafficRecordMagnitude(record trafficDeltaRecord) int64 {
	return max(record.TrafficUp, 0) + max(record.TrafficDown, 0)
}

func getPreviousTrafficDeltaRecord(db *gorm.DB, clientUUID string, before time.Time) (*trafficDeltaRecord, error) {
	record, err := latestTrafficDeltaRecordBefore(db.Table("records"), clientUUID, before)
	if err != nil {
		return nil, err
	}

	longTermRecord, err := latestTrafficDeltaRecordBefore(db.Table("records_long_term"), clientUUID, before)
	if err != nil {
		return nil, err
	}

	if record == nil {
		return longTermRecord, nil
	}
	if longTermRecord == nil {
		return record, nil
	}
	if longTermRecord.Time.ToTime().After(record.Time.ToTime()) {
		return longTermRecord, nil
	}
	return record, nil
}

func latestTrafficDeltaRecordBefore(query *gorm.DB, clientUUID string, before time.Time) (*trafficDeltaRecord, error) {
	var record trafficDeltaRecord
	err := query.
		Select("time, net_total_up, net_total_down, traffic_up, traffic_down").
		Where("client = ? AND time < ?", clientUUID, models.FromTime(before)).
		Order("time DESC").
		First(&record).Error
	if err == nil {
		return &record, nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return nil, err
}

func sumTrafficDeltas(records []trafficDeltaRecord, previous *trafficDeltaRecord) (int64, int64) {
	var totalUp int64
	var totalDown int64

	for i := range records {
		up := records[i].TrafficUp
		down := records[i].TrafficDown
		if previous != nil {
			up = trafficDeltaOrFallback(up, records[i].NetTotalUp, previous.NetTotalUp)
			down = trafficDeltaOrFallback(down, records[i].NetTotalDown, previous.NetTotalDown)
		}
		totalUp += up
		totalDown += down
		previous = &records[i]
	}

	return totalUp, totalDown
}

func trafficDeltaOrFallback(storedDelta, currentTotal, previousTotal int64) int64 {
	if storedDelta > 0 {
		return storedDelta
	}
	return utils.ComputeTrafficDelta(currentTotal, previousTotal)
}
