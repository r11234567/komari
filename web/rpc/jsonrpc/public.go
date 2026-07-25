package jsonrpc

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/komari-monitor/komari/database"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/history"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/database/tasks"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/utils"
	report_cache "github.com/komari-monitor/komari/web/report"
)

// public.go
// 公开（guest 可访问）的只读 RPC2 方法。命名空间 public:* 对 guest 开放。
// 这些方法保持与原 REST 接口完全一致的响应形状。

func init() {
	rpc.Allow("public:*", rpc.RoleGuest)
	regPublic("getMe", publicGetMe, "Get current user info (guest-aware)")
	regPublic("getNodesInformation", publicGetNodesInformation, "List visible nodes (basic info)")
	regPublic("getPublicSettings", publicGetPublicSettings, "Get public site settings")
	regPublic("getVersion", publicGetVersion, "Get server version")
	regPublic("getClientRecentRecords", publicGetClientRecentRecords, "Get a client's recent records")
	regPublic("getRecordsByUUID", publicGetRecordsByUUID, "Get load records for a client")
	regPublic("getPingRecords", publicGetPingRecords, "Get ping records")
	regPublic("queryHistory", publicQueryHistory, "Query bounded load or ping history")
	regPublic("getPublicPingTasks", publicGetPublicPingTasks, "List public ping tasks")
}

func regPublic(name string, h rpc.Handler, summary string) {
	RegisterWithGroupAndMeta(name, "public", h, &rpc.MethodMeta{Name: "public:" + name, Summary: summary})
}

// isLoginFromCtx 依据 meta 判断是否为已登录管理员。
func isLoginFromCtx(ctx context.Context) bool {
	if meta := rpc.MetaFromContext(ctx); meta != nil {
		return meta.Principal != nil && meta.Principal.HasRole(rpc.RoleAdmin)
	}
	return false
}

func publicGetNodesInformation(ctx context.Context, _ *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	clientList, err := clients.GetAllClientBasicInfo()
	if err != nil {
		return nil, rpc.MakeError(rpc.InternalError, "Failed to retrieve client information: "+err.Error(), nil)
	}
	isLogin := isLoginFromCtx(ctx)
	j := 0
	for i := 0; i < len(clientList); i++ {
		if clientList[i].Hidden && !isLogin {
			continue
		}
		clientList[i].IPv4 = ""
		clientList[i].IPv6 = ""
		clientList[i].Remark = ""
		clientList[i].Version = ""
		clientList[i].Token = ""
		clientList[j] = clientList[i]
		j++
	}
	clientList = clientList[:j]
	return clientList, nil
}

func publicGetPublicSettings(ctx context.Context, _ *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	p, e := database.GetPublicInfo()
	if e != nil {
		return nil, rpc.MakeError(rpc.InternalError, e.Error(), nil)
	}
	// 临时访问许可由 transport 层在 meta 标注；此处沿用原逻辑判断 temp_key。
	if meta := rpc.MetaFromContext(ctx); meta != nil && meta.TempShareValid {
		p["private_site"] = false
	}
	return p, nil
}

func publicGetVersion(_ context.Context, _ *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	return map[string]any{
		"version": utils.CurrentVersion,
		"hash":    utils.VersionHash,
	}, nil
}

// publicGetMe 返回当前用户信息；未登录时返回 Guest 占位，保持原 /api/me 的扁平形状。
func publicGetMe(ctx context.Context, _ *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	guest := map[string]any{"username": "Guest", "logged_in": false}
	meta := rpc.MetaFromContext(ctx)
	if meta == nil || meta.User == nil {
		return guest, nil
	}
	u := meta.User
	return map[string]any{
		"username":    u.Username,
		"logged_in":   true,
		"uuid":        u.UUID,
		"sso_type":    u.SSOType,
		"sso_id":      u.SSOID,
		"2fa_enabled": u.TwoFactor != "",
	}, nil
}

func publicGetClientRecentRecords(ctx context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	var params struct {
		UUID string `json:"uuid"`
	}
	req.BindParams(&params)
	if params.UUID == "" {
		return nil, rpc.MakeError(rpc.InvalidParams, "UUID is required", nil)
	}
	if !isLoginFromCtx(ctx) && isHiddenClient(params.UUID) {
		return nil, rpc.MakeError(rpc.InvalidParams, "UUID is required", nil) // 防止未登录获取隐藏客户端
	}
	recs, _ := report_cache.Records.Get(params.UUID)
	return recs, nil
}

// isHiddenClient 查询指定 uuid 是否为隐藏节点。
func isHiddenClient(uuid string) bool {
	var hiddenClients []models.Client
	db := dbcore.GetDBInstance()
	_ = db.Select("uuid").Where("hidden = ?", true).Find(&hiddenClients).Error
	for _, cli := range hiddenClients {
		if cli.UUID == uuid {
			return true
		}
	}
	return false
}

func publicGetRecordsByUUID(ctx context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	var params struct {
		UUID      string `json:"uuid"`
		LoadType  string `json:"load_type"`
		Hours     string `json:"hours"`
		MaxPoints string `json:"max_points"`
	}
	req.BindParams(&params)
	isLogin := isLoginFromCtx(ctx)
	if !isLogin && params.UUID != "" && isHiddenClient(params.UUID) {
		return nil, rpc.MakeError(rpc.InvalidParams, "UUID is required", nil)
	}
	if params.UUID == "" {
		return nil, rpc.MakeError(rpc.InvalidParams, "UUID is required", nil)
	}
	hours := params.Hours
	if hours == "" {
		hours = "4"
	}
	hoursInt, err := strconv.Atoi(hours)
	if err != nil {
		return nil, rpc.MakeError(rpc.InvalidParams, "Invalid hours parameter", nil)
	}
	validLoadTypes := map[string]bool{
		"cpu": true, "gpu": true, "ram": true, "swap": true,
		"load": true, "temp": true, "disk": true, "network": true,
		"process": true, "connections": true, "all": true, "": true,
	}
	if !validLoadTypes[params.LoadType] {
		return nil, rpc.MakeError(rpc.InvalidParams, "Invalid load_type parameter", nil)
	}
	maxPoints := history.DefaultMaxPoints
	if params.MaxPoints != "" {
		parsed, parseErr := strconv.Atoi(params.MaxPoints)
		if parseErr != nil {
			return nil, rpc.MakeError(rpc.InvalidParams, "Invalid max_points parameter", nil)
		}
		maxPoints = parsed
	}
	query, err := history.Query(ctx, history.QueryRequest{Type: "load", UUID: params.UUID, Hours: hoursInt, MaxPoints: maxPoints})
	if err != nil {
		return nil, historyRPCError(err, "Failed to fetch records")
	}
	clientRecords := historyLoadRecords(query, params.UUID)
	response := map[string]any{
		"records": clientRecords,
		"count":   len(clientRecords),
	}
	if params.LoadType != "" && params.LoadType != "all" {
		filtered := filterPublicRecordsByLoadType(clientRecords, params.LoadType)
		response = map[string]any{
			"records":   filtered,
			"count":     len(filtered),
			"load_type": params.LoadType,
		}
	}
	response["resolution"] = query.Resolution
	response["raw_count"] = query.RawCount
	response["sampled"] = query.Sampled
	gpuDevices := make(map[string]any)
	for _, series := range query.Series {
		if series.Kind != "gpu" {
			continue
		}
		records := make([]models.GPURecord, 0, len(series.Points))
		for _, point := range series.Points {
			records = append(records, models.GPURecord{
				Client: series.Client, Time: models.FromTime(point.Time), DeviceIndex: series.DeviceIndex,
				DeviceName: series.DeviceName, MemTotal: int64(point.Metrics["mem_total"]),
				MemUsed: int64(point.Metrics["mem_used"]), Utilization: float32(point.Metrics["utilization"]),
				Temperature: int(point.Metrics["temperature"]),
			})
		}
		gpuDevices[strconv.Itoa(series.DeviceIndex)] = map[string]any{
			"device_index": series.DeviceIndex, "device_name": series.DeviceName, "records": records,
		}
	}
	response["gpu_devices"] = gpuDevices
	response["has_gpu_data"] = len(gpuDevices) > 0
	return response, nil
}

func historyLoadRecords(query *history.Response, uuid string) []models.Record {
	if query == nil {
		return []models.Record{}
	}
	var loadSeries *history.Series
	for index := range query.Series {
		if query.Series[index].Kind == "load" {
			loadSeries = &query.Series[index]
			break
		}
	}
	if loadSeries == nil {
		return []models.Record{}
	}
	records := make([]models.Record, 0, len(loadSeries.Points))
	for _, point := range loadSeries.Points {
		m := point.Metrics
		record := models.Record{Client: uuid, Time: models.FromTime(point.Time), Cpu: float32(m["cpu"]), Gpu: float32(m["gpu"]), Ram: int64(m["ram"]), RamTotal: int64(m["ram_total"]), Swap: int64(m["swap"]), SwapTotal: int64(m["swap_total"]), Load: float32(m["load"]), Temp: float32(m["temp"]), Disk: int64(m["disk"]), DiskTotal: int64(m["disk_total"]), NetIn: int64(m["net_in"]), NetOut: int64(m["net_out"]), NetTotalUp: int64(m["net_total_up"]), NetTotalDown: int64(m["net_total_down"]), TrafficUp: int64(m["traffic_up"]), TrafficDown: int64(m["traffic_down"]), Process: int(m["process"]), Connections: int(m["connections"]), ConnectionsUdp: int(m["connections_udp"])}
		records = append(records, record)
	}
	return records
}

func publicQueryHistory(ctx context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	var params history.QueryRequest
	if err := req.BindParams(&params); err != nil {
		return nil, rpc.MakeError(rpc.InvalidParams, "invalid history request: "+err.Error(), nil)
	}
	isLogin := isLoginFromCtx(ctx)
	hidden := map[string]bool{}
	if !isLogin {
		var hiddenClients []models.Client
		if err := dbcore.GetDBInstance().WithContext(ctx).Select("uuid").Where("hidden = ?", true).Find(&hiddenClients).Error; err != nil {
			return nil, historyRPCError(err, "Failed to check client visibility")
		}
		for _, client := range hiddenClients {
			hidden[client.UUID] = true
		}
		if params.UUID != "" && hidden[params.UUID] {
			return nil, rpc.MakeError(rpc.NotFound, "client not found", nil)
		}
	}
	result, err := history.Query(ctx, params)
	if err != nil {
		return nil, historyRPCError(err, "History query failed")
	}
	if !isLogin {
		visible := result.Series[:0]
		for _, series := range result.Series {
			if !hidden[series.Client] {
				visible = append(visible, series)
			}
		}
		result.Series = visible
		result.ReturnedPoints = 0
		result.RawCount = 0
		for _, series := range visible {
			result.ReturnedPoints += len(series.Points)
			if series.PingSummary != nil {
				result.RawCount += int64(series.PingSummary.TotalCount)
				continue
			}
			for _, point := range series.Points {
				result.RawCount += int64(point.TotalCount)
			}
		}
		result.Sampled = result.RawCount > int64(result.ReturnedPoints)
	}
	return result, nil
}

func historyRPCError(err error, message string) *rpc.JsonRpcError {
	code := rpc.InternalError
	if errors.Is(err, history.ErrInvalidQuery) || errors.Is(err, history.ErrRangeTooLarge) {
		code = rpc.InvalidParams
	}
	if errors.Is(err, context.DeadlineExceeded) {
		code = rpc.DeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		code = rpc.Cancelled
	}
	return rpc.MakeError(code, message+": "+err.Error(), nil)
}

func publicGetPublicPingTasks(_ context.Context, _ *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	pingTasks, err := tasks.GetAllPingTasks()
	if err != nil {
		return nil, rpc.MakeError(rpc.InternalError, err.Error(), nil)
	}
	type publicPingTask struct {
		Id        uint     `json:"id"`
		Weight    int      `json:"weight"`
		Name      string   `json:"name"`
		Clients   []string `json:"clients"`
		DefaultOn bool     `json:"default_on"`
		Type      string   `json:"type"`
		Interval  int      `json:"interval"`
	}
	out := make([]publicPingTask, len(pingTasks))
	for i, task := range pingTasks {
		out[i] = publicPingTask{
			Id:        task.Id,
			Weight:    task.Weight,
			Name:      task.Name,
			Clients:   task.Clients,
			DefaultOn: task.DefaultOn,
			Type:      task.Type,
			Interval:  task.Interval,
		}
	}
	return out, nil
}

// filterPublicRecordsByLoadType 复刻原 public 接口的字段投影逻辑。
func filterPublicRecordsByLoadType(recs []models.Record, loadType string) []map[string]any {
	out := make([]map[string]any, 0, len(recs))
	for _, r := range recs {
		record := map[string]any{"client": r.Client, "time": r.Time}
		switch loadType {
		case "cpu":
			record["cpu"] = r.Cpu
		case "gpu":
			record["gpu"] = r.Gpu
		case "ram":
			record["ram"] = r.Ram
			record["ram_total"] = r.RamTotal
			if r.RamTotal > 0 {
				record["ram_percent"] = float32(r.Ram) / float32(r.RamTotal) * 100
			}
		case "swap":
			record["swap"] = r.Swap
			record["swap_total"] = r.SwapTotal
			if r.SwapTotal > 0 {
				record["swap_percent"] = float32(r.Swap) / float32(r.SwapTotal) * 100
			}
		case "load":
			record["load"] = r.Load
		case "temp":
			record["temp"] = r.Temp
		case "disk":
			record["disk"] = r.Disk
			record["disk_total"] = r.DiskTotal
			if r.DiskTotal > 0 {
				record["disk_percent"] = float32(r.Disk) / float32(r.DiskTotal) * 100
			}
		case "network":
			record["net_in"] = r.NetIn
			record["net_out"] = r.NetOut
			record["net_total_up"] = r.NetTotalUp
			record["net_total_down"] = r.NetTotalDown
		case "process":
			record["process"] = r.Process
		case "connections":
			record["connections"] = r.Connections
			record["connections_udp"] = r.ConnectionsUdp
			record["connections_tcp"] = r.Connections - r.ConnectionsUdp
		}
		out = append(out, record)
	}
	return out
}

type pingHistoryStats struct {
	total int
	loss  int
	min   int
	max   int
	sum   float64
	valid int
}

func ensureClientStats(items map[string]*pingHistoryStats, key string) *pingHistoryStats {
	item := items[key]
	if item == nil {
		item = &pingHistoryStats{}
		items[key] = item
	}
	return item
}

func ensureTaskStats(items map[uint]*pingHistoryStats, key uint) *pingHistoryStats {
	item := items[key]
	if item == nil {
		item = &pingHistoryStats{}
		items[key] = item
	}
	return item
}

func publicGetPingRecords(ctx context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	var params struct {
		UUID      string `json:"uuid"`
		TaskID    string `json:"task_id"`
		Hours     string `json:"hours"`
		MaxPoints string `json:"max_points"`
	}
	req.BindParams(&params)
	if params.UUID == "" && params.TaskID == "" {
		return nil, rpc.MakeError(rpc.InvalidParams, "UUID or task_id is required", nil)
	}
	isLogin := isLoginFromCtx(ctx)

	type recordsResp struct {
		TaskId uint   `json:"task_id,omitempty"`
		Time   string `json:"time"`
		Value  int    `json:"value"`
		Client string `json:"client,omitempty"`
	}
	type clientBasicInfo struct {
		Client string  `json:"client"`
		Loss   float64 `json:"loss"`
		Min    int     `json:"min"`
		Max    int     `json:"max"`
	}
	type responseBody struct {
		Count      int               `json:"count"`
		BasicInfo  []clientBasicInfo `json:"basic_info,omitempty"`
		Records    []recordsResp     `json:"records"`
		Tasks      []map[string]any  `json:"tasks,omitempty"`
		Resolution string            `json:"resolution,omitempty"`
		RawCount   int64             `json:"raw_count,omitempty"`
		Sampled    bool              `json:"sampled"`
	}

	hiddenMap := map[string]bool{}
	response := &responseBody{Records: []recordsResp{}}

	if !isLogin {
		var hiddenClients []models.Client
		db := dbcore.GetDBInstance()
		_ = db.Select("uuid").Where("hidden = ?", true).Find(&hiddenClients).Error
		for _, cli := range hiddenClients {
			hiddenMap[cli.UUID] = true
		}
		if params.UUID != "" && hiddenMap[params.UUID] {
			return response, nil // 对尝试获取隐藏 uuid 返回空
		}
	}

	hours := params.Hours
	if hours == "" {
		hours = "4"
	}
	hoursInt, err := strconv.Atoi(hours)
	if err != nil {
		return nil, rpc.MakeError(rpc.InvalidParams, "Invalid hours parameter", nil)
	}
	var taskID *uint
	if params.TaskID != "" {
		parsed, parseErr := strconv.ParseUint(params.TaskID, 10, 64)
		if parseErr != nil {
			return nil, rpc.MakeError(rpc.InvalidParams, "Invalid task_id parameter", nil)
		}
		value := uint(parsed)
		taskID = &value
	}

	maxPoints := history.DefaultMaxPoints
	if params.MaxPoints != "" {
		parsed, parseErr := strconv.Atoi(params.MaxPoints)
		if parseErr != nil {
			return nil, rpc.MakeError(rpc.InvalidParams, "Invalid max_points parameter", nil)
		}
		maxPoints = parsed
	}
	result, err := history.Query(ctx, history.QueryRequest{Type: "ping", UUID: params.UUID, TaskID: taskID, Hours: hoursInt, MaxPoints: maxPoints})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, rpc.MakeError(rpc.Cancelled, "Ping history request cancelled", nil)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, rpc.MakeError(rpc.DeadlineExceeded, "Ping history query timed out", nil)
		}
		return nil, rpc.MakeError(rpc.InternalError, "Failed to fetch ping history: "+err.Error(), nil)
	}
	response.Resolution = result.Resolution

	clientStats := make(map[string]*pingHistoryStats)
	taskStats := make(map[uint]*pingHistoryStats)
	var visibleRawCount int64
	for _, series := range result.Series {
		if series.Client != "" && !isLogin && hiddenMap[series.Client] {
			continue
		}
		if summary := series.PingSummary; summary != nil {
			visibleRawCount += int64(summary.TotalCount)
			for _, target := range []*pingHistoryStats{ensureClientStats(clientStats, series.Client), ensureTaskStats(taskStats, series.TaskID)} {
				hadValid := target.valid > 0
				target.total += summary.TotalCount
				target.loss += summary.LossCount
				target.valid += summary.ValidCount
				target.sum += summary.Avg * float64(summary.ValidCount)
				min, max := int(summary.Min), int(summary.Max)
				if summary.ValidCount > 0 && (!hadValid || min < target.min) {
					target.min = min
				}
				if summary.ValidCount > 0 && (!hadValid || max > target.max) {
					target.max = max
				}
			}
		}
		for _, point := range series.Points {
			value := int(point.Avg)
			if point.TotalCount == point.LossCount {
				value = -1
			}
			response.Records = append(response.Records, recordsResp{TaskId: series.TaskID, Time: point.Time.Format(time.RFC3339), Value: value, Client: series.Client})
		}
	}
	response.RawCount = visibleRawCount
	response.Sampled = visibleRawCount > int64(len(response.Records))

	if len(clientStats) > 0 {
		response.BasicInfo = make([]clientBasicInfo, 0, len(clientStats))
		for client, item := range clientStats {
			if client != "" && !isLogin && hiddenMap[client] {
				continue
			}
			loss := float64(0)
			if item.total > 0 {
				loss = float64(item.loss) / float64(item.total) * 100
			}
			response.BasicInfo = append(response.BasicInfo, clientBasicInfo{Client: client, Loss: loss, Min: item.min, Max: item.max})
		}
	}

	if params.UUID != "" || taskID != nil {
		pingTasks, err := tasks.GetAllPingTasks()
		if err != nil {
			return nil, rpc.MakeError(rpc.InternalError, "Failed to fetch ping tasks: "+err.Error(), nil)
		}
		tasksList := make([]map[string]any, 0, len(pingTasks))
		for _, t := range pingTasks {
			if taskID != nil && t.Id != *taskID {
				continue
			}
			if params.UUID != "" && !t.AppliesToClient(params.UUID) {
				continue
			}
			item := taskStats[t.Id]
			if item == nil {
				item = &pingHistoryStats{}
			}
			lossRate := float64(0)
			if item.total > 0 {
				lossRate = float64(item.loss) / float64(item.total) * 100
			}
			avgLatency := 0
			if item.valid > 0 {
				avgLatency = int(item.sum / float64(item.valid))
			}
			taskInfo := map[string]any{
				"id": t.Id, "name": t.Name, "type": t.Type, "interval": t.Interval,
				"default_on": t.DefaultOn, "loss": lossRate, "min": item.min,
				"max": item.max, "avg": avgLatency, "total": item.total,
			}
			if params.UUID == "" && taskID != nil {
				taskInfo["clients"] = t.Clients
			}
			tasksList = append(tasksList, taskInfo)
		}
		response.Tasks = tasksList
	}

	response.Count = len(response.Records)
	return response, nil
}
