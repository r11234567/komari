package metric

import (
	"context"
	"database/sql"
)

type TelemetryExportState struct {
	Instance       string
	Epoch, Pending int64
}

// ExportState is a bounded metadata read. Raw data, receipt counts and cold
// archives are not scanned to decide whether background work may continue.
func (t *Telemetry) ExportState(ctx context.Context) (TelemetryExportState, error) {
	out := TelemetryExportState{Instance: t.instance}
	values := map[string]int64{}
	rows, e := t.s.reader().QueryContext(ctx, "SELECT task,value FROM "+t.table("progress")+" WHERE task IN ('export_epoch','ingest','summary','traffic')")
	if e != nil {
		return out, e
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var value int64
		if e = rows.Scan(&key, &value); e != nil {
			return out, e
		}
		values[key] = value
	}
	if e = rows.Err(); e != nil {
		return out, e
	}
	rows.Close()
	out.Epoch = values["export_epoch"]
	out.Pending = values["ingest"] - values["summary"]
	var pending int64
	if e = t.s.reader().QueryRowContext(ctx, "SELECT COUNT(*) FROM (SELECT ingest_id FROM "+t.table("outbox")+" WHERE delivered=0 LIMIT 1025) pending_export_pressure").Scan(&pending); e != nil {
		return out, e
	}
	out.Pending = max(out.Pending, pending)
	return out, nil
}
func (t *Telemetry) bumpExportEpoch(ctx context.Context, tx *sql.Tx) error {
	_, e := tx.ExecContext(ctx, "UPDATE "+t.table("progress")+" SET value=value+1 WHERE task='export_epoch'")
	return e
}
