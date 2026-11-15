package historyexport

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
)

// v1 keeps the named human-unit columns consumed by older clients. v2 uses
// metric/tag/base-unit columns; changing the unit never silently reuses v1.
type legacyColumn struct {
	metric, label, group string
	divisor              float64
	decimals             int
	tagged               bool
	task                 string
}

var legacyMetricColumns = []legacyColumn{
	{"cpu.usage", "CPU usage (%)", "Resource", 1, 2, false, ""},
	{"disk.used", "Disk used (GiB)", "Resource", 1 << 30, 3, false, ""},
	{"gpu.device.usage", "GPU device usage (%)", "Resource", 1, 2, true, ""},
	{"gpu.memory.total", "GPU memory total (GiB)", "Resource", 1 << 30, 3, true, ""},
	{"gpu.memory.used", "GPU memory used (GiB)", "Resource", 1 << 30, 3, true, ""},
	{"gpu.temperature", "GPU temperature (C)", "Resource", 1, 1, true, ""},
	{"gpu.usage", "GPU usage (%)", "Resource", 1, 2, false, ""},
	{"load.average", "Load average", "Resource", 1, 2, false, ""},
	{"memory.used", "Memory used (GiB)", "Resource", 1 << 30, 3, false, ""},
	{"process.count", "Process count", "Resource", 1, 0, false, ""},
	{"swap.used", "Swap used (GiB)", "Resource", 1 << 30, 3, false, ""},
	{"connections.tcp", "TCP connections", "Network", 1, 0, false, ""},
	{"connections.udp", "UDP connections", "Network", 1, 0, false, ""},
	{"net.in.rate", "Network inbound (KiB/s)", "Network", 1024, 2, false, ""},
	{"net.out.rate", "Network outbound (KiB/s)", "Network", 1024, 2, false, ""},
	{"net.total.down", "Total downloaded (GiB)", "Network", 1 << 30, 3, false, ""},
	{"net.total.up", "Total uploaded (GiB)", "Network", 1 << 30, 3, false, ""},
	{"traffic.down", "Download delta (MiB)", "Network", 1 << 20, 3, false, ""},
	{"traffic.up", "Upload delta (MiB)", "Network", 1 << 20, 3, false, ""},
}

func legacyColumns(ctx context.Context, spool *sql.DB, j *Job) ([]legacyColumn, bool, error) {
	metrics := map[string]bool{}
	for _, name := range j.Metrics {
		metrics[name] = true
	}
	var columns []legacyColumn
	for _, c := range legacyMetricColumns {
		if metrics[c.metric] {
			columns = append(columns, c)
		}
	}
	tasks := map[string]string{}
	for id, name := range j.PingNames {
		tasks[id] = name
	}
	rows, e := spool.QueryContext(ctx, "SELECT DISTINCT tags FROM samples WHERE metric IN ('ping.latency_ms','ping.loss') LIMIT 1025")
	if e != nil {
		return nil, false, e
	}
	for rows.Next() {
		var encoded string
		if e = rows.Scan(&encoded); e != nil {
			break
		}
		var tags map[string]string
		if e = json.Unmarshal([]byte(encoded), &tags); e != nil {
			break
		}
		id := tags["task_id"]
		if id != "" {
			if _, ok := tasks[id]; !ok {
				tasks[id] = "Deleted ping task " + id
			}
		}
	}
	re := rows.Err()
	rows.Close()
	if e != nil {
		return nil, false, e
	}
	if re != nil {
		return nil, false, re
	}
	if len(tasks) > 1024 {
		return nil, false, errors.New("legacy ping CSV exceeds column budget")
	}
	ids := make([]string, 0, len(tasks))
	for id := range tasks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		name := tasks[id]
		if name == "" {
			name = "Ping task " + id
		}
		columns = append(columns, legacyColumn{"ping.latency_ms", "Ping (ms)", name, 1, 2, false, id}, legacyColumn{"ping.loss", "Loss (%)", name, .01, 2, false, id})
	}
	return columns, len(ids) > 0, nil
}
func (c legacyColumn) value(v float64, tags map[string]string) string {
	if math.IsNaN(v) || math.IsInf(v, 0) || (c.metric == "ping.latency_ms" && v < 0) {
		return ""
	}
	if c.decimals == 0 {
		v = math.Round(v)
	}
	result := strconv.FormatFloat(v/c.divisor, 'f', c.decimals, 64)
	if c.tagged {
		name := tags["device_name"]
		if name == "" {
			name = "GPU " + tags["device_index"]
		}
		result = name + ": " + result
	}
	return result
}
