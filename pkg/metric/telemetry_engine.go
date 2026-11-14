package metric

// The archive engine owns a separate schema. It never rewrites legacy blocks.
// Application routing is deliberately a separate integration checkpoint.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	ErrTelemetryConflict     = errors.New("telemetry batch identity reused with different content")
	ErrTelemetryCold         = errors.New("raw telemetry is outside the online window; use an authorized export")
	ErrTelemetryCoverage     = errors.New("requested telemetry precision or boundary is unavailable")
	ErrTelemetryBackpressure = errors.New("telemetry storage backlog budget exhausted")
)

type TelemetryConfig struct {
	ArchiveDirectory string
	SegmentDuration  time.Duration
	SegmentPoints    int64
	SegmentBytes     int64
	RawWindow        time.Duration
	// RawRetention is ignored and retained only for source compatibility.
	// Charts own their scalar rollups and CSV reads the immutable archive;
	// archived raw SQL rows retire after summary/accounting handoff commits.
	RawRetention time.Duration
	MaxLateness  time.Duration
	MaxHotBytes  int64
	WorkLease    time.Duration
	// Fault is used by integration tests at durable state transitions. It is
	// never configured from HTTP or persisted settings.
	Fault func(string) error `json:"-"`
}

func (c TelemetryConfig) defaults() TelemetryConfig {
	c.RawRetention = 0
	if c.SegmentDuration == 0 {
		c.SegmentDuration = 24 * time.Hour
	}
	if c.SegmentPoints == 0 {
		c.SegmentPoints = 5_000_000
	}
	if c.SegmentBytes == 0 {
		c.SegmentBytes = 4 << 20
	}
	if c.RawWindow == 0 {
		c.RawWindow = 15 * time.Minute
	}
	if c.MaxLateness == 0 {
		c.MaxLateness = 48 * time.Hour
	}
	if c.MaxHotBytes == 0 {
		c.MaxHotBytes = 16 << 20
	}
	if c.WorkLease == 0 {
		c.WorkLease = time.Minute
	}
	return c
}

// Telemetry shares the owning Store's connection and lifetime. A cancellable
// gate bounds writers; SQL CAS/serializable transactions protect other handles.
type Telemetry struct {
	s                *Store
	cfg              TelemetryConfig
	root             *os.Root
	gate             chan struct{}
	archiveGate      chan struct{}
	trafficGate      chan struct{}
	scheduleGate     chan struct{}
	changed          chan struct{}
	capacity         telemetryCapacity
	owner            string
	instance         string
	legacyUntil      int64
	legacyResolution time.Duration
	scalar           bool
	closeOnce        sync.Once
	closeErr         error
}

type TelemetryTraffic struct {
	HasDelta   bool  `json:"has_delta,omitempty"`
	DeltaUp    int64 `json:"delta_up,omitempty"`
	DeltaDown  int64 `json:"delta_down,omitempty"`
	ObservedAt int64 `json:"observed_at_ns,omitempty"`
	TotalUp    int64 `json:"total_up"`
	TotalDown  int64 `json:"total_down"`
	Up         int64 `json:"up"`
	Down       int64 `json:"down"`
}

type TelemetryBatch struct {
	Source string `json:"source"`
	// Identity is supplied by the authenticated adapter. Legacy Agent identity
	// includes sequence AND observed time; process-local sequence alone is unsafe.
	Identity string            `json:"identity"`
	Points   []Point           `json:"points"`
	Traffic  *TelemetryTraffic `json:"traffic,omitempty"`
}

type TelemetryReceipt struct {
	IngestID  int64
	Points    int
	Duplicate bool
}

type telemetrySegment struct {
	ID                                           string
	State                                        string
	First, Last, Points, Bytes, Created, Deleted int64
	Hash                                         string
	FileBytes                                    int64
	StagingOwner                                 string
	ArchiveEntity                                string `json:",omitempty"`
	ArchiveDay                                   int64  `json:",omitempty"`
	ArchiveFile                                  string `json:",omitempty"`
	ArchiveOffset                                int64  `json:",omitempty"`
}

func telemetryID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func (t *Telemetry) table(name string) string { return t.s.cfg.TablePrefix + "telemetry_" + name }
func (t *Telemetry) bind(q string) string {
	if t.s.cfg.Driver != DriverPostgreSQL {
		return q
	}
	i := 0
	return qWithPlaceholders(q, &i)
}
func qWithPlaceholders(q string, i *int) string {
	var b strings.Builder
	for _, r := range q {
		if r == '?' {
			*i++
			fmt.Fprintf(&b, "$%d", *i)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (t *Telemetry) acquire(ctx context.Context) (func(), error) {
	select {
	case t.gate <- struct{}{}:
		return func() { <-t.gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (t *Telemetry) fault(stage string) error {
	if t.cfg.Fault != nil {
		return t.cfg.Fault(stage)
	}
	return nil
}

// OpenTelemetry creates only new tables. It neither converts existing history
// nor changes application routing. The caller must close it before Store.Close.
func (s *Store) OpenTelemetry(ctx context.Context, cfg TelemetryConfig) (*Telemetry, error) {
	if err := s.ensureOpen(); err != nil {
		return nil, err
	}
	cfg = cfg.defaults()
	if cfg.ArchiveDirectory == "" || cfg.SegmentDuration <= 0 || cfg.SegmentPoints <= 0 || cfg.SegmentBytes < 4096 || cfg.RawWindow <= 0 || cfg.RawRetention < 0 || cfg.MaxLateness < cfg.RawWindow || cfg.MaxHotBytes < cfg.SegmentBytes || cfg.WorkLease < 10*time.Millisecond || cfg.WorkLease > 5*time.Minute {
		return nil, fmt.Errorf("%w: telemetry configuration", ErrInvalidArgument)
	}
	if s.cfg.Driver == DriverSQLite {
		// FULL is a connection setting. The SQLite writer pool has one connection;
		// reject a borrowed/multiwriter pool whose durability cannot be guaranteed.
		if s.db.Stats().MaxOpenConnections != 1 || s.cfg.DB != nil || s.cfg.SQLite.PerformanceProfile != SQLiteProfileDurable {
			return nil, errors.New("telemetry requires a single durable SQLite writer connection")
		}
		if _, err := s.db.ExecContext(ctx, "PRAGMA synchronous=FULL"); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(cfg.ArchiveDirectory, 0700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(cfg.ArchiveDirectory)
	if err != nil {
		return nil, err
	}
	t := &Telemetry{s: s, cfg: cfg, root: root, gate: make(chan struct{}, 1), archiveGate: make(chan struct{}, 1), trafficGate: make(chan struct{}, 1), scheduleGate: make(chan struct{}, 1), changed: make(chan struct{}, 1), owner: telemetryID()}
	if err := t.createSchema(ctx); err != nil {
		root.Close()
		return nil, err
	}
	if err := t.initializeCapacity(ctx); err != nil {
		root.Close()
		return nil, err
	}
	return t, nil
}

func (t *Telemetry) Close() error {
	t.closeOnce.Do(func() { t.closeErr = t.root.Close() })
	return t.closeErr
}

// EnableTelemetry selects the replacement engine for this Store. Call only
// before publishing the Store to readers/writers. Runtime reloads publish a new
// Store under the application's exclusive lifecycle lease.
func (s *Store) EnableTelemetry(ctx context.Context, cfg TelemetryConfig) error {
	if s.telemetry.Load() != nil {
		return errors.New("telemetry already enabled")
	}
	t, err := s.OpenTelemetry(ctx, cfg)
	if err != nil {
		return err
	}
	s.telemetry.Store(t)
	return nil
}

// Telemetry returns the active engine. The application must retain its Store
// lease for the entire operation, including file streams and backup snapshots.
func (s *Store) Telemetry() *Telemetry { return s.telemetry.Load() }

func (t *Telemetry) createSchema(ctx context.Context) error {
	progressTable := t.table("progress")
	if _, err := t.s.db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS "+progressTable+" (task VARCHAR(64) PRIMARY KEY, value BIGINT NOT NULL)"); err != nil {
		return err
	}
	var version int64
	err := t.s.db.QueryRowContext(ctx, "SELECT value FROM "+progressTable+" WHERE task='schema_version'").Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		var rows int
		if e := t.s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+progressTable).Scan(&rows); e != nil {
			return e
		}
		if rows != 0 {
			return errors.New("telemetry schema version missing; explicit repair required")
		}
		if _, e := t.s.db.ExecContext(ctx, "INSERT INTO "+progressTable+" (task,value) VALUES ('schema_version',-1)"); e != nil {
			return e
		}
		version = -1
	} else if err != nil {
		return err
	}
	if version != 10 && version != -10 && version != 9 && version != -9 && version != 8 && version != -8 && version != 7 && version != 6 && version != 5 && version != 4 && version != 3 && version != 2 && version != 1 && version != -1 {
		return fmt.Errorf("unsupported telemetry SQL schema version %d", version)
	}
	if version == 9 {
		if _, err = t.s.db.ExecContext(ctx, "UPDATE "+progressTable+" SET value=-10 WHERE task='schema_version'"); err != nil {
			return err
		}
	}
	if version == 8 {
		if _, err := t.s.db.ExecContext(ctx, "UPDATE "+progressTable+" SET value=-9 WHERE task='schema_version'"); err != nil {
			return err
		}
	}
	// Small integer/CHAR keys keep these DDLs portable across SQLite, MySQL and
	// PostgreSQL. JSON payloads use TEXT, not backend-dependent JSON operators.
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS %sprogress (task VARCHAR(64) PRIMARY KEY, value BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS %sreceipts (ingest_id BIGINT PRIMARY KEY, source VARCHAR(255) NOT NULL, identity VARCHAR(256) NOT NULL, observed BIGINT NOT NULL, received BIGINT NOT NULL, points INTEGER NOT NULL, segment_id CHAR(32) NOT NULL, importing INTEGER NOT NULL DEFAULT 0, UNIQUE(source,identity))`,
		`CREATE TABLE IF NOT EXISTS %sseries (id CHAR(64) PRIMARY KEY, metric VARCHAR(255) NOT NULL, entity VARCHAR(255) NOT NULL, tags TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS %shot (ingest_id BIGINT NOT NULL, ordinal INTEGER NOT NULL, series_id CHAR(64) NOT NULL, observed BIGINT NOT NULL, received BIGINT NOT NULL, value DOUBLE PRECISION NOT NULL, labels TEXT NOT NULL, segment_id CHAR(32) NOT NULL, PRIMARY KEY(ingest_id, ordinal))`,
		`CREATE TABLE IF NOT EXISTS %ssegments (id CHAR(32) PRIMARY KEY, state VARCHAR(16) NOT NULL, first_id BIGINT NOT NULL, last_id BIGINT NOT NULL, points BIGINT NOT NULL, bytes BIGINT NOT NULL, created BIGINT NOT NULL, deleted BIGINT NOT NULL, checksum CHAR(64) NOT NULL, file_bytes BIGINT NOT NULL, staging_owner CHAR(32) NOT NULL, summary_version INTEGER NOT NULL DEFAULT 1)`,
		`CREATE TABLE IF NOT EXISTS %ssummary (segment_id CHAR(32) NOT NULL, series_id CHAR(64) NOT NULL, resolution BIGINT NOT NULL, bucket BIGINT NOT NULL, stats TEXT NOT NULL, input_through BIGINT NOT NULL DEFAULT 0, promoted_through BIGINT NOT NULL DEFAULT 0, PRIMARY KEY(segment_id,series_id,resolution,bucket))`,
		`CREATE TABLE IF NOT EXISTS %soutbox (ingest_id BIGINT PRIMARY KEY, source VARCHAR(255) NOT NULL, received BIGINT NOT NULL, payload TEXT NOT NULL, delivered INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS %sleases (id CHAR(32) NOT NULL, segment_id CHAR(32) NOT NULL, expires BIGINT NOT NULL, PRIMARY KEY(id,segment_id))`,
		`CREATE TABLE IF NOT EXISTS %swork (task VARCHAR(64) PRIMARY KEY, owner CHAR(32) NOT NULL, expires BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS %smembers (segment_id CHAR(32) NOT NULL, series_id CHAR(64) NOT NULL, first_observed BIGINT NOT NULL, last_observed BIGINT NOT NULL, points BIGINT NOT NULL, PRIMARY KEY(segment_id,series_id))`,
		`CREATE TABLE IF NOT EXISTS %serasures (series_id CHAR(64) PRIMARY KEY, through_id BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS %sidentity (slot INTEGER PRIMARY KEY, id CHAR(32) NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS %straffic_ingress (source VARCHAR(255) PRIMARY KEY, observed BIGINT NOT NULL, total_up BIGINT NOT NULL, total_down BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS %sgaps (segment_id CHAR(32) NOT NULL, series_id CHAR(64) NOT NULL, first_observed BIGINT NOT NULL, last_observed BIGINT NOT NULL, PRIMARY KEY(segment_id,series_id,first_observed))`,
		`CREATE TABLE IF NOT EXISTS %sschedule (task VARCHAR(64) PRIMARY KEY, due BIGINT NOT NULL, attempts INTEGER NOT NULL, scan_after VARCHAR(255) NOT NULL, owner CHAR(32) NOT NULL, expires BIGINT NOT NULL, last_error TEXT NOT NULL)`,
	}
	prefix := t.s.cfg.TablePrefix + "telemetry_"
	for _, q := range stmts {
		if _, err := t.s.db.ExecContext(ctx, fmt.Sprintf(q, prefix)); err != nil {
			return err
		}
	}
	if t.s.cfg.Driver == DriverSQLite {
		if _, err := t.s.db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS "+t.table("series_keys")+" (id INTEGER PRIMARY KEY,canonical TEXT NOT NULL UNIQUE)"); err != nil {
			return err
		}
	}
	if version == 1 {
		rows, e := t.s.db.QueryContext(ctx, "SELECT staging_owner FROM "+t.table("segments")+" LIMIT 0")
		if e == nil {
			rows.Close()
		} else if _, e = t.s.db.ExecContext(ctx, "ALTER TABLE "+t.table("segments")+" ADD COLUMN staging_owner CHAR(32) NOT NULL DEFAULT ''"); e != nil {
			return e
		}
	}
	// DDL can commit independently on MySQL. Probe each additive column so a
	// crash between ALTERs resumes safely without scanning historical payloads.
	for _, column := range []struct{ table, name, definition string }{
		{"segments", "summary_version", "INTEGER NOT NULL DEFAULT 1"},
		{"segments", "archive_entity", "VARCHAR(255) NOT NULL DEFAULT ''"},
		{"segments", "archive_day", "BIGINT NOT NULL DEFAULT 0"},
		{"segments", "archive_file", "CHAR(32) NOT NULL DEFAULT ''"},
		{"segments", "archive_offset", "BIGINT NOT NULL DEFAULT 0"},
		{"summary", "input_through", "BIGINT NOT NULL DEFAULT 0"},
		{"summary", "promoted_through", "BIGINT NOT NULL DEFAULT 0"},
		{"gaps", "ranges", "VARCHAR(14000) NOT NULL DEFAULT ''"},
	} {
		rows, e := t.s.db.QueryContext(ctx, "SELECT "+column.name+" FROM "+t.table(column.table)+" LIMIT 0")
		if e == nil {
			rows.Close()
		} else if _, e = t.s.db.ExecContext(ctx, "ALTER TABLE "+t.table(column.table)+" ADD COLUMN "+column.name+" "+column.definition); e != nil {
			return e
		}
	}
	if err := t.simplifyReceiptSchema(ctx); err != nil {
		return err
	}
	for _, idx := range []struct{ name, table, cols string }{
		{"hot_series", "hot", "series_id,observed,ingest_id,ordinal"},
		{"hot_segment", "hot", "segment_id,ingest_id,ordinal"},
		{"hot_live_age", "hot", "segment_id,observed,ingest_id,ordinal"},
		{"series_metric", "series", "metric,entity,id"},
		{"series_entity", "series", "entity,id"},
		{"segments_state", "segments", "state,created,id"},
		{"segments_partition", "segments", "state,archive_entity,archive_day,created,id"},
		{"segments_file", "segments", "archive_file,archive_offset"},
		{"outbox_pending", "outbox", "delivered,ingest_id"},
		{"summary_series", "summary", "series_id,resolution,bucket"},
		{"receipt_age", "receipts", "received,ingest_id"},
		{"receipt_segment", "receipts", "segment_id,ingest_id"},
		{"members_series", "members", "series_id,first_observed,last_observed,segment_id"},
		{"gaps_series", "gaps", "series_id,first_observed,last_observed"},
		{"schedule_due", "schedule", "due,expires,task"},
		{"summary_age", "summary", "resolution,bucket,series_id,segment_id"},
		{"summary_pending", "summary", "resolution,promoted_through,bucket,segment_id,series_id"},
	} {
		if t.s.cfg.Driver == DriverSQLite {
			switch idx.name {
			case "summary_series":
				idx.cols = "series_id"
			case "summary_age":
				idx.cols = "resolution,bucket"
			case "summary_pending":
				idx.cols = "resolution,promoted_through,bucket"
			}
		}
		q := fmt.Sprintf("CREATE INDEX %s ON %s (%s)", prefix+idx.name, t.table(idx.table), idx.cols)
		if t.s.cfg.Driver != DriverMySQL {
			q = strings.Replace(q, "CREATE INDEX", "CREATE INDEX IF NOT EXISTS", 1)
		}
		if _, err := t.s.db.ExecContext(ctx, q); err != nil {
			// MySQL has no CREATE INDEX IF NOT EXISTS. Verify the specific index
			// exists instead of swallowing arbitrary DDL errors.
			if t.s.cfg.Driver != DriverMySQL {
				return err
			}
			var n int
			if e := t.s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name=? AND index_name=?`, t.table(idx.table), prefix+idx.name).Scan(&n); e != nil || n == 0 {
				return err
			}
		}
	}
	if err := t.createLatestSchema(ctx); err != nil {
		return err
	}
	for _, task := range []string{"ingest", "summary", "hot_bytes", "live_bytes", "export_epoch"} {
		q := fmt.Sprintf("INSERT INTO %s (task,value) VALUES (?,0)", t.table("progress"))
		if t.s.cfg.Driver == DriverMySQL {
			q += " ON DUPLICATE KEY UPDATE task=task"
		} else {
			q += " ON CONFLICT(task) DO NOTHING"
		}
		if _, err := t.s.db.ExecContext(ctx, t.bind(q), task); err != nil {
			return err
		}
	}
	identitySQL := "INSERT INTO " + t.table("identity") + " (slot,id) VALUES (1,?)"
	if t.s.cfg.Driver == DriverMySQL {
		identitySQL += " ON DUPLICATE KEY UPDATE slot=slot"
	} else {
		identitySQL += " ON CONFLICT(slot) DO NOTHING"
	}
	if _, err = t.s.db.ExecContext(ctx, t.bind(identitySQL), telemetryID()); err != nil {
		return err
	}
	if err = t.s.db.QueryRowContext(ctx, "SELECT id FROM "+t.table("identity")+" WHERE slot=1").Scan(&t.instance); err != nil {
		return err
	}
	// Core schema v1/v2 was never an application format. Existing segments are
	// indexed lazily from their owned hot rows or verified immutable archives;
	// opening the store never performs a historical scan.
	_, err = t.s.db.ExecContext(ctx, "UPDATE "+progressTable+" SET value=7 WHERE task='schema_version' AND value IN (-1,1,2,3,4,5,6)")
	if err != nil {
		return err
	}
	if v, e := t.progress(ctx, t.s.db, "legacy_until"); e == nil {
		t.legacyUntil = v
	} else if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	if v, e := t.progress(ctx, t.s.db, "legacy_resolution"); e == nil {
		t.legacyResolution = time.Duration(v)
	} else if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	return t.createScalarSchema(ctx)
}

func (t *Telemetry) progress(ctx context.Context, q querier, task string) (int64, error) {
	var n int64
	err := q.QueryRowContext(ctx, t.bind("SELECT value FROM "+t.table("progress")+" WHERE task=?"), task).Scan(&n)
	return n, err
}
func (t *Telemetry) advance(ctx context.Context, tx *sql.Tx, task string, old, next int64) error {
	r, err := tx.ExecContext(ctx, t.bind("UPDATE "+t.table("progress")+" SET value=? WHERE task=? AND value=?"), next, task, old)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("telemetry concurrent progress update; retry")
	}
	return nil
}

func (t *Telemetry) begin(ctx context.Context) (*sql.Tx, error) {
	return t.s.db.BeginTx(ctx, t.s.dialect.compactTxOptions())
}

// Accept commits receipt, immutable hot rows, segment membership and outbox as
// one transaction. No in-memory queue is part of the acknowledgement contract.
func (t *Telemetry) Accept(ctx context.Context, b TelemetryBatch, now time.Time) (TelemetryReceipt, error) {
	return t.accept(ctx, b, now, false)
}

func (t *Telemetry) accept(ctx context.Context, b TelemetryBatch, now time.Time, importing bool) (TelemetryReceipt, error) {
	var result TelemetryReceipt
	if b.Source == "" || len(b.Source) > 255 || strings.ContainsRune(b.Source, 0) || b.Identity == "" || strings.ContainsRune(b.Identity, 0) || len(b.Identity) > 256 || (len(b.Points) == 0 && b.Traffic == nil) || len(b.Points) > 256 {
		return result, fmt.Errorf("%w: telemetry batch", ErrInvalidArgument)
	}
	b.Points = append([]Point(nil), b.Points...)
	oldest := now.UnixNano()
	resolvedKeys := make(map[string]string)
	type memberBounds struct{ first, last, points int64 }
	members := make(map[string]memberBounds)
	latest := make(map[string]int)
	for i, p := range b.Points {
		if err := p.Validate(); err != nil {
			return result, err
		}
		if p.EntityID != b.Source || strings.ContainsRune(p.MetricName, 0) || len(p.MetricName) > 255 || len(p.Tags) > 32 || len(p.Labels) > 32 {
			return result, fmt.Errorf("%w: telemetry series budget/identity", ErrInvalidArgument)
		}
		if !importing && (p.Timestamp.Before(now.Add(-t.cfg.MaxLateness)) || p.Timestamp.After(now.Add(5*time.Minute))) {
			return result, fmt.Errorf("%w: telemetry observed time outside acceptance window", ErrInvalidArgument)
		}
		b.Points[i] = p.normalized()
		oldest = min(oldest, p.Timestamp.UnixNano())
	}
	if b.Traffic != nil && (b.Traffic.TotalUp < 0 || b.Traffic.TotalDown < 0 || b.Traffic.Up < 0 || b.Traffic.Down < 0) {
		return result, fmt.Errorf("%w: negative traffic", ErrInvalidArgument)
	}
	if b.Traffic != nil && b.Traffic.ObservedAt != 0 {
		at := time.Unix(0, b.Traffic.ObservedAt)
		if at.Before(now.Add(-t.cfg.MaxLateness)) || at.After(now.Add(5*time.Minute)) {
			return result, ErrInvalidArgument
		}
		oldest = min(oldest, b.Traffic.ObservedAt)
	}
	payload, err := json.Marshal(b)
	if err != nil {
		return result, err
	}
	batchBytes := int64(len(payload)) + int64(len(b.Points))*256
	if batchBytes > t.cfg.SegmentBytes || int64(len(b.Points)) > t.cfg.SegmentPoints || len(payload) > 4<<20 {
		return result, fmt.Errorf("%w: telemetry payload budget", ErrInvalidArgument)
	}
	release, err := t.acquire(ctx)
	if err != nil {
		return result, err
	}
	defer release()
	tx, err := t.begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, t.bind("SELECT ingest_id,points FROM "+t.table("receipts")+" WHERE source=? AND identity=?"), b.Source, b.Identity).Scan(&result.IngestID, &result.Points)
	if importing && errors.Is(err, sql.ErrNoRows) {
		err = t.legacyImportReceipt(ctx, tx, b.Source, b.Identity, &result)
	}
	if err == nil {
		result.Duplicate = true
		return result, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	if err = t.CheckCapacity(ctx, false); err != nil {
		return result, err
	}
	if b.Traffic != nil {
		traffic := *b.Traffic
		b.Traffic = &traffic
		if err = t.deriveTraffic(ctx, tx, &b, oldest); err != nil {
			return result, err
		}
	}
	last, err := t.progress(ctx, tx, "ingest")
	if err != nil {
		return result, err
	}
	if last == math.MaxInt64 {
		return result, ErrTelemetryBackpressure
	}
	hotBytes, err := t.progress(ctx, tx, "hot_bytes")
	if err != nil {
		return result, err
	}
	if _, err = t.pruneLive(ctx, tx, now, 512); err != nil {
		return result, err
	}
	liveBytes, err := t.progress(ctx, tx, "live_bytes")
	if err != nil {
		return result, err
	}
	if hotBytes+liveBytes > t.cfg.MaxHotBytes-batchBytes {
		return result, ErrTelemetryBackpressure
	}
	entity, day := telemetryBatchPartition(b.Points)
	seg, err := t.activeSegment(ctx, tx, now, int64(len(b.Points)), batchBytes, entity, day)
	if err != nil {
		return result, err
	}
	result = TelemetryReceipt{IngestID: last + 1, Points: len(b.Points)}
	importFlag := 0
	if importing {
		importFlag = 1
	}
	if _, err = tx.ExecContext(ctx, t.bind("INSERT INTO "+t.table("receipts")+" (ingest_id,source,identity,observed,received,points,segment_id,importing) VALUES (?,?,?,?,?,?,?,?)"), result.IngestID, b.Source, b.Identity, oldest, now.UnixNano(), len(b.Points), seg.ID, importFlag); err != nil {
		return result, err
	}
	seen := make(map[string]bool)
	seriesRows, err := tx.QueryContext(ctx, t.bind("SELECT id FROM "+t.table("series")+" WHERE entity=? LIMIT 1025"), b.Source)
	if err != nil {
		return result, err
	}
	for seriesRows.Next() {
		var id string
		if err = seriesRows.Scan(&id); err != nil {
			break
		}
		seen[id] = true
	}
	seriesErr := seriesRows.Err()
	seriesRows.Close()
	if err != nil {
		return result, err
	}
	if seriesErr != nil {
		return result, seriesErr
	}
	if len(seen) > 1024 {
		return result, ErrTelemetryBackpressure
	}
	for i, p := range b.Points {
		tags, _ := json.Marshal(p.Tags)
		labels, _ := json.Marshal(p.Labels)
		if len(tags)+len(labels) > 16<<10 {
			return TelemetryReceipt{}, fmt.Errorf("%w: telemetry tags budget", ErrInvalidArgument)
		}
		key := sha256.Sum256([]byte(p.MetricName + "\x00" + p.EntityID + "\x00" + string(tags)))
		canonical := hex.EncodeToString(key[:])
		sid, ok := resolvedKeys[canonical]
		if !ok {
			sid, err = t.seriesKey(ctx, tx, canonical)
			if err != nil {
				return result, err
			}
			resolvedKeys[canonical] = sid
		}
		if !seen[sid] {
			if len(seen) >= 1024 {
				return result, fmt.Errorf("%w: maximum series per source", ErrTelemetryBackpressure)
			}
			q := "INSERT INTO " + t.table("series") + " (id,metric,entity,tags) VALUES (?,?,?,?)"
			if t.s.cfg.Driver == DriverMySQL {
				q += " ON DUPLICATE KEY UPDATE id=id"
			} else {
				q += " ON CONFLICT(id) DO NOTHING"
			}
			if _, err = tx.ExecContext(ctx, t.bind(q), sid, p.MetricName, p.EntityID, string(tags)); err != nil {
				return result, err
			}
			seen[sid] = true
		}
		if _, err = tx.ExecContext(ctx, t.bind("INSERT INTO "+t.table("hot")+" (ingest_id,ordinal,series_id,observed,received,value,labels,segment_id) VALUES (?,?,?,?,?,?,?,?)"), result.IngestID, i, sid, p.Timestamp.UnixNano(), now.UnixNano(), p.Value, string(labels), seg.ID); err != nil {
			return result, err
		}
		m, exists := members[sid]
		observed := p.Timestamp.UnixNano()
		if !exists {
			m.first, m.last = observed, observed
		}
		m.first, m.last, m.points = min(m.first, observed), max(m.last, observed), m.points+1
		members[sid] = m
		if telemetryLiveMetric(p.MetricName) {
			prior, ok := latest[sid]
			if !ok || !p.Timestamp.Before(b.Points[prior].Timestamp) {
				latest[sid] = i
			}
		}
	}
	for sid, m := range members {
		q := "INSERT INTO " + t.table("members") + " (segment_id,series_id,first_observed,last_observed,points) VALUES (?,?,?,?,?)"
		if t.s.cfg.Driver == DriverMySQL {
			q += " ON DUPLICATE KEY UPDATE first_observed=LEAST(first_observed,VALUES(first_observed)),last_observed=GREATEST(last_observed,VALUES(last_observed)),points=points+VALUES(points)"
		} else {
			q += " ON CONFLICT(segment_id,series_id) DO UPDATE SET first_observed=CASE WHEN excluded.first_observed<" + t.table("members") + ".first_observed THEN excluded.first_observed ELSE " + t.table("members") + ".first_observed END,last_observed=CASE WHEN excluded.last_observed>" + t.table("members") + ".last_observed THEN excluded.last_observed ELSE " + t.table("members") + ".last_observed END,points=" + t.table("members") + ".points+excluded.points"
		}
		if _, err = tx.ExecContext(ctx, t.bind(q), seg.ID, sid, m.first, m.last, m.points); err != nil {
			return result, err
		}
	}
	for sid, i := range latest {
		if err = t.putLatest(ctx, tx, sid, result.IngestID, i, b.Points[i]); err != nil {
			return result, err
		}
	}
	if b.Traffic != nil {
		v, _ := json.Marshal(b.Traffic)
		if _, err = tx.ExecContext(ctx, t.bind("INSERT INTO "+t.table("outbox")+" (ingest_id,source,received,payload,delivered) VALUES (?,?,?,?,0)"), result.IngestID, b.Source, now.UnixNano(), string(v)); err != nil {
			return result, err
		}
	}
	if _, err = tx.ExecContext(ctx, t.bind("UPDATE "+t.table("segments")+" SET last_id=?,points=points+?,bytes=bytes+? WHERE id=? AND state='open'"), result.IngestID, len(b.Points), batchBytes, seg.ID); err != nil {
		return result, err
	}
	if err = t.advance(ctx, tx, "ingest", last, result.IngestID); err != nil {
		return result, err
	}
	if err = t.advance(ctx, tx, "hot_bytes", hotBytes, hotBytes+batchBytes); err != nil {
		return result, err
	}
	if err = t.fault("ingest-before-commit"); err != nil {
		return TelemetryReceipt{}, err
	}
	if err = tx.Commit(); err != nil {
		return TelemetryReceipt{}, err
	}
	t.notify()
	return result, t.fault("ingest-after-commit")
}

// Reports from a machine share a file only with that machine and observation
// day. Generic mixed batches retain the compatibility partition; their v3
// frames still carry entity/time selection metadata.
func telemetryBatchPartition(points []Point) (string, int64) {
	if len(points) == 0 {
		return "", 0
	}
	entity := points[0].EntityID
	day := points[0].Timestamp.UTC().Truncate(24 * time.Hour).Unix()
	for _, p := range points[1:] {
		if p.EntityID != entity || p.Timestamp.UTC().Truncate(24*time.Hour).Unix() != day {
			return "", 0
		}
	}
	return entity, day
}

func (t *Telemetry) activeSegment(ctx context.Context, tx *sql.Tx, now time.Time, points, bytes int64, entity string, day int64) (telemetrySegment, error) {
	var seg telemetrySegment
	seg.ArchiveEntity, seg.ArchiveDay = entity, day
	err := tx.QueryRowContext(ctx, t.bind("SELECT id,points,bytes,created FROM "+t.table("segments")+" WHERE state='open' AND archive_entity=? AND archive_day=? ORDER BY created,id LIMIT 1"), entity, day).Scan(&seg.ID, &seg.Points, &seg.Bytes, &seg.Created)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return seg, err
	}
	if err == nil && (now.UnixNano()-seg.Created >= int64(t.cfg.SegmentDuration) || seg.Points+points > t.cfg.SegmentPoints || seg.Bytes+bytes > t.cfg.SegmentBytes) {
		if _, err = tx.ExecContext(ctx, t.bind("UPDATE "+t.table("segments")+" SET state='sealed' WHERE id=?"), seg.ID); err != nil {
			return seg, err
		}
		seg.ID = ""
	}
	if seg.ID == "" {
		seg.ID = telemetryID()
		last, err := t.progress(ctx, tx, "ingest")
		if err != nil {
			return seg, err
		}
		_, err = tx.ExecContext(ctx, t.bind("INSERT INTO "+t.table("segments")+" (id,state,first_id,last_id,points,bytes,created,deleted,checksum,file_bytes,staging_owner,summary_version,archive_entity,archive_day) VALUES (?,'open',?,?,0,0,?,0,'',0,'',3,?,?)"), seg.ID, last+1, last, now.UnixNano(), entity, day)
		if err != nil {
			return seg, err
		}
	}
	return seg, nil
}

func (t *Telemetry) SealDue(ctx context.Context, now time.Time) (int64, error) {
	release, err := t.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	tx, err := t.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// Day rollover follows the handoff clock, not the age of imported points.
	// Historical batches in one fixed import session must accumulate normally
	// instead of creating a new tiny extent for every migration page.
	r, err := tx.ExecContext(ctx, t.bind("UPDATE "+t.table("segments")+" SET state='sealed' WHERE state='open' AND (created<=? OR points>=? OR bytes>=? OR created<?)"), now.Add(-t.cfg.SegmentDuration).UnixNano(), t.cfg.SegmentPoints, t.cfg.SegmentBytes, now.UTC().Truncate(24*time.Hour).UnixNano())
	if err != nil {
		return 0, err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return 0, err
	}
	hot, err := t.progress(ctx, tx, "hot_bytes")
	if err != nil {
		return 0, err
	}
	live, err := t.progress(ctx, tx, "live_bytes")
	if err != nil {
		return 0, err
	}
	// Under global pressure flush the largest partition, not every machine's
	// tiny tail. The shared budget bounds SQL even with a full-day idle limit.
	if n == 0 && hot >= max(int64(1), (t.cfg.MaxHotBytes-live)*3/4) {
		var id string
		err = tx.QueryRowContext(ctx, "SELECT id FROM "+t.table("segments")+" WHERE state='open' ORDER BY bytes DESC,created,id LIMIT 1").Scan(&id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if err == nil {
			if _, err = tx.ExecContext(ctx, t.bind("UPDATE "+t.table("segments")+" SET state='sealed' WHERE id=? AND state='open'"), id); err != nil {
				return 0, err
			}
			n++
		}
	}
	return n, tx.Commit()
}
