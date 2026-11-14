package metric

import "context"

// These counters have raw CSV or independent traffic-ledger consumers only.
// They are not part of the resource/Ping chart storage contract.
func telemetryChartMetric(name string) bool {
	switch name {
	case "net.total.up", "net.total.down", "traffic.up", "traffic.down", "process.count", "connections.tcp", "connections.udp":
		return false
	}
	return true
}

func (t *Telemetry) simplifyScalarSchema(ctx context.Context) error {
	probe, err := t.s.db.QueryContext(ctx, "SELECT first_offset FROM "+t.table("rollup")+" LIMIT 0")
	if err == nil {
		probe.Close()
		return nil
	}
	tx, err := t.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	name := t.table("rollup_small")
	suffix := ""
	if t.s.cfg.Driver == DriverSQLite {
		suffix = " WITHOUT ROWID"
	}
	if _, err = tx.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS "+name+" (series_id BIGINT NOT NULL,resolution INTEGER NOT NULL,bucket BIGINT NOT NULL,n BIGINT NOT NULL,loss BIGINT NOT NULL,total DOUBLE PRECISION NOT NULL,minimum DOUBLE PRECISION NOT NULL,maximum DOUBLE PRECISION NOT NULL,last_value DOUBLE PRECISION NOT NULL,first_offset BIGINT NOT NULL,last_offset BIGINT NOT NULL,PRIMARY KEY(series_id,resolution,bucket))"+suffix); err != nil {
		return err
	}
	statement := "INSERT INTO " + name + " (series_id,resolution,bucket," + scalarStatColumns + ") SELECT series_id,resolution,bucket,n,loss,total,minimum,maximum,last_value,first_ns-bucket*1000000000,last_ns-bucket*1000000000 FROM " + t.table("rollup")
	if t.s.cfg.Driver == DriverMySQL {
		statement += " ON DUPLICATE KEY UPDATE n=VALUES(n)"
	}
	if _, err = tx.ExecContext(ctx, statement); err != nil {
		return err
	}
	if t.s.cfg.Driver == DriverMySQL {
		old := t.table("rollup_retired")
		if _, err = tx.ExecContext(ctx, "RENAME TABLE "+t.table("rollup")+" TO "+old+", "+name+" TO "+t.table("rollup")); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "DROP TABLE "+old)
	} else {
		if _, err = tx.ExecContext(ctx, "DROP TABLE "+t.table("rollup")); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "ALTER TABLE "+name+" RENAME TO "+t.table("rollup"))
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (t *Telemetry) discardNonChartRollups(ctx context.Context) error {
	tx, err := t.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	where := "series_id IN (SELECT k.id FROM " + t.table("rollup_keys") + " k JOIN " + t.table("series") + " s ON s.id=k.source_id WHERE s.metric IN ('net.total.up','net.total.down','traffic.up','traffic.down','process.count','connections.tcp','connections.udp'))"
	for _, suffix := range []string{"rollup", "rollup_due"} {
		if _, err = tx.ExecContext(ctx, "DELETE FROM "+t.table(suffix)+" WHERE "+where); err != nil {
			return err
		}
	}
	return tx.Commit()
}
