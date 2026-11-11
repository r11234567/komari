// Package historyexport owns durable, account-scoped background export jobs.
// Expensive work runs only in the separately constrained export-worker process.
package historyexport

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/komari-monitor/komari/pkg/metric"
	_ "github.com/mattn/go-sqlite3"
)

const TTL = 48 * time.Hour
const FormatVersion = 2

var ErrQueueFull = errors.New("background export queue is full (8 globally, 2 per account)")
var ErrNotFound = errors.New("export not found")

type Source struct {
	DSN       string                 `json:"dsn"`
	Prefix    string                 `json:"prefix"`
	Telemetry metric.TelemetryConfig `json:"telemetry"`
	Instance  string                 `json:"instance"`
	Epoch     int64                  `json:"epoch"`
}

type Request struct {
	Owner       string             `json:"owner"`
	UUID        string             `json:"uuid"`
	NodeName    string             `json:"node_name"`
	Category    string             `json:"category"`
	Mode        string             `json:"mode"`
	Start       time.Time          `json:"start"`
	End         time.Time          `json:"end"`
	Metrics     []string           `json:"metrics"`
	Aggregation metric.Aggregation `json:"aggregation"`
	Interval    time.Duration      `json:"interval"`
	Source      Source             `json:"-"`
	CSVVersion  int                `json:"csv_version,omitempty"`
	PingNames   map[string]string  `json:"ping_names,omitempty"`
}

type Job struct {
	Request
	ID                  string                  `json:"id"`
	FormatVersion       int                     `json:"format_version"`
	Status              string                  `json:"status"`
	Phase               string                  `json:"phase"`
	PhaseStartedAt      time.Time               `json:"phase_started_at"`
	PhaseActiveSeconds  float64                 `json:"phase_active_seconds"`
	WaitingSince        time.Time               `json:"waiting_since,omitempty"`
	WaitSeconds         float64                 `json:"wait_seconds"`
	CurrentWaitSeconds  float64                 `json:"current_wait_seconds"`
	Throughput          Throughput              `json:"throughput"`
	PhaseETA            PhaseETA                `json:"phase_eta"`
	CurrentInput        *InputProgress          `json:"current_input,omitempty"`
	Verification        *CheckpointVerification `json:"checkpoint_verification,omitempty"`
	Resources           *ResourceUsage          `json:"resources,omitempty"`
	observation         progressState
	ProgressKind        string    `json:"progress_kind"`
	CreatedAt           time.Time `json:"created_at"`
	StartedAt           time.Time `json:"started_at,omitempty"`
	ReadyAt             time.Time `json:"ready_at,omitempty"`
	ExpiresAt           time.Time `json:"expires_at,omitempty"`
	Heartbeat           time.Time `json:"last_heartbeat_at"`
	LastProgress        time.Time `json:"last_progress_at"`
	WaitReason          string    `json:"wait_reason,omitempty"`
	Error               string    `json:"error,omitempty"`
	PhaseCompleted      int64     `json:"phase_completed"`
	PhaseTotal          int64     `json:"phase_total"`
	DecodedRecords      int64     `json:"decoded_records"`
	WrittenRows         int64     `json:"written_rows"`
	OutputBytes         int64     `json:"output_bytes"`
	CompletedInputBytes int64     `json:"completed_input_bytes"`
	ScannedInputBytes   int64     `json:"source_scanned_bytes"`
	Checkpoint          int       `json:"confirmed_checkpoint"`
	SHA256              string    `json:"sha256,omitempty"`
	HashState           []byte    `json:"-"`
	Committed           Commit    `json:"-"`
	Filename            string    `json:"filename,omitempty"`
	CPUCoreLimit        float64   `json:"cpu_core_limit"`
	CPUShareLimit       float64   `json:"cpu_share_limit"`
	Generation          string    `json:"snapshot_id,omitempty"`
	Retries             int       `json:"retries"`
	AutomaticRetries    int       `json:"automatic_retries"`
	RetryAt             time.Time `json:"retry_at,omitempty"`
}

// DiskJob is private. Neither connection strings nor resumable hash state are
// returned by the API. SQLite transactions serialize control and creation.
type diskJob struct {
	Job         Job
	Source      Source
	HashState   []byte
	Committed   Commit
	Observation progressState
}
type Commit struct {
	Offset, Rows, Decoded int64
	Unit                  int
	Hash                  []byte
	SampleOutput          bool
	Slot                  int64
}
type Queue struct {
	db       *sql.DB
	Dir      string
	identity os.FileInfo
}

func Open(dir string) (*Queue, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("export queue must be a private real directory")
	}
	if err = os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(filepath.Join(dir, "queue.db"))+"?_busy_timeout=3000&_journal_mode=WAL&_synchronous=FULL&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	q := &Queue{db: db, Dir: dir}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS jobs(id TEXT PRIMARY KEY,owner TEXT NOT NULL,dedupe TEXT NOT NULL,created INTEGER NOT NULL,status TEXT NOT NULL,control TEXT NOT NULL DEFAULT '',body BLOB NOT NULL);
CREATE INDEX IF NOT EXISTS jobs_owner ON jobs(owner,created);
CREATE INDEX IF NOT EXISTS jobs_status ON jobs(status,created);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	q.identity, err = os.Stat(filepath.Join(dir, "queue.db"))
	if err != nil {
		db.Close()
		return nil, err
	}
	return q, nil
}
func (q *Queue) Close() error { return q.db.Close() }
func ValidID(id string) bool {
	if len(id) != 24 {
		return false
	}
	_, e := hex.DecodeString(id)
	return e == nil
}
func (q *Queue) Path(id, suffix string) string {
	if !ValidID(id) {
		panic("invalid generated export identity")
	}
	return filepath.Join(q.Dir, id+suffix)
}
func decodeJob(b []byte) (*Job, error) {
	var d diskJob
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	d.Job.Source = d.Source
	d.Job.HashState = d.HashState
	d.Job.Committed = d.Committed
	d.Job.observation = d.Observation
	return &d.Job, nil
}
func encodeJob(j *Job) ([]byte, error) {
	stored := *j
	stored.Resources = nil // live group observations belong to worker.json
	stored.CurrentWaitSeconds = 0
	return json.Marshal(diskJob{Job: stored, Source: j.Source, HashState: j.HashState, Committed: j.Committed, Observation: j.observation})
}
func (j *Job) Validate() error {
	if j.Owner == "" || j.UUID == "" || !j.Start.Before(j.End) || j.End.After(time.Now().Add(time.Minute)) || len(j.Metrics) == 0 || len(j.Metrics) > 64 {
		return errors.New("invalid export scope, range or metrics")
	}
	if j.Mode != "raw" && j.Mode != "summary" && j.Mode != "sample30s" {
		return errors.New("mode must be raw, summary or sample30s")
	}
	if j.CSVVersion != 0 && j.CSVVersion != 1 && j.CSVVersion != 2 {
		return errors.New("unsupported CSV version")
	}
	if j.CSVVersion == 1 && j.Mode != "sample30s" {
		return errors.New("CSV version 1 supports sample30s only")
	}
	if len(j.PingNames) > 1024 {
		return errors.New("ping metadata exceeds column budget")
	}
	if j.Mode == "summary" {
		return (metric.AggregateQuery{Query: metric.Query{MetricName: j.Metrics[0], EntityID: j.UUID, Start: j.Start, End: j.End}, Aggregation: j.Aggregation, Interval: j.Interval}).Validate()
	}
	return nil
}
func (q *Queue) Create(ctx context.Context, r Request) (*Job, error) {
	release, err := q.guard(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	j := &Job{Request: r, FormatVersion: FormatVersion, Status: "queued", Phase: "queued", ProgressKind: "indeterminate", CreatedAt: time.Now().UTC(), CPUShareLimit: .1}
	if a := q.Availability(); a.Available {
		j.CPUCoreLimit = a.CPUCoreLimit
	}
	if r.CSVVersion == 1 {
		j.FormatVersion = 1
	}
	if err := j.Validate(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(r.Source.DSN) || r.Source.Telemetry.ArchiveDirectory == "" {
		return nil, errors.New("strict background export requires a local SQLite source")
	}
	keyBytes, _ := json.Marshal(struct {
		R      Request
		Source Source
	}{r, r.Source})
	key := sha256.Sum256(keyBytes)
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var prior []byte
	err = tx.QueryRowContext(ctx, "SELECT body FROM jobs WHERE owner=? AND dedupe=? AND status IN ('queued','running','paused','publishing','recovering','retry_wait','done') ORDER BY created DESC LIMIT 1", r.Owner, hex.EncodeToString(key[:])).Scan(&prior)
	if err == nil {
		old, e := decodeJob(prior)
		if e != nil {
			return nil, e
		}
		if old.Status != "done" || time.Now().Before(old.ExpiresAt) {
			return old, nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var total, owned int
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(SUM(CASE WHEN owner=? THEN 1 ELSE 0 END),0) FROM jobs WHERE status IN ('queued','running','paused','publishing','recovering','retry_wait')", r.Owner).Scan(&total, &owned); err != nil {
		return nil, err
	}
	if total >= 8 || owned >= 2 {
		return nil, ErrQueueFull
	}
	var b [12]byte
	if _, err = rand.Read(b[:]); err != nil {
		return nil, err
	}
	j.ID = hex.EncodeToString(b[:])
	j.observe(j.CreatedAt)
	body, err := encodeJob(j)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO jobs(id,owner,dedupe,created,status,body) VALUES(?,?,?,?,?,?)", j.ID, r.Owner, hex.EncodeToString(key[:]), j.CreatedAt.UnixNano(), j.Status, body)
	if err != nil {
		return nil, err
	}
	return j, tx.Commit()
}
func (q *Queue) Save(ctx context.Context, j *Job) error {
	release, err := q.guard(ctx)
	if err != nil {
		return err
	}
	defer release()
	if !ValidID(j.ID) {
		return ErrNotFound
	}
	j.observe(time.Now())
	b, err := encodeJob(j)
	if err != nil {
		return err
	}
	_, err = q.db.ExecContext(ctx, "UPDATE jobs SET status=?,body=? WHERE id=?", j.Status, b, j.ID)
	return err
}
func (q *Queue) Get(ctx context.Context, owner, id string) (*Job, error) {
	if err := q.checkGeneration(); err != nil {
		return nil, err
	}
	var b []byte
	var control string
	err := q.db.QueryRowContext(ctx, "SELECT body,control FROM jobs WHERE id=? AND owner=?", id, owner).Scan(&b, &control)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	j, e := decodeJob(b)
	if e != nil {
		return nil, e
	}
	q.decorate(j, control)
	return j, nil
}
func (q *Queue) decorate(j *Job, control string) {
	if waitingStatus(j.Status) && !j.WaitingSince.IsZero() {
		j.CurrentWaitSeconds = max(0, time.Since(j.WaitingSince).Seconds())
	}
	if j.Status == "done" && !time.Now().Before(j.ExpiresAt) {
		j.Status = "expired"
	}
	if control == "cancel" && j.Status != "cancelled" && j.Status != "done" {
		j.Status = "cancel_requested"
	}
	if control == "pause" && j.Status == "running" {
		j.WaitReason = "user_pause_requested"
	}
	processing := activeStatus(j.Status) || j.Status == "recovering"
	if processing {
		a := q.Availability()
		if a.Available && a.ActiveJob == j.ID {
			j.Heartbeat = a.At
			j.Resources = a.Resources
			j.CPUCoreLimit = a.CPUCoreLimit
		}
	}
	if !activeStatus(j.Status) || time.Since(j.Heartbeat) > 15*time.Second {
		j.Throughput = Throughput{}
		j.PhaseETA = PhaseETA{State: "unknown", Scope: "phase", Reason: j.Status}
	} else {
		j.estimateETA(time.Now())
		if time.Since(j.Throughput.At) > 15*time.Second {
			j.Throughput = Throughput{}
		}
	}
	if !processing || time.Since(j.Heartbeat) > 15*time.Second {
		j.Resources = nil
	}
}
func (q *Queue) List(ctx context.Context, owner string) ([]*Job, error) {
	if err := q.checkGeneration(); err != nil {
		return nil, err
	}
	rows, err := q.db.QueryContext(ctx, "SELECT body,control FROM jobs WHERE owner=? ORDER BY CASE WHEN status IN ('queued','running','paused','publishing','recovering','retry_wait') THEN 0 ELSE 1 END,created DESC LIMIT 64", owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Job, 0)
	for rows.Next() {
		var b []byte
		var control string
		if err = rows.Scan(&b, &control); err != nil {
			return nil, err
		}
		j, e := decodeJob(b)
		if e != nil {
			return nil, e
		}
		q.decorate(j, control)
		out = append(out, j)
	}
	return out, rows.Err()
}
func (q *Queue) Control(ctx context.Context, owner, id, action string) error {
	release, err := q.guard(ctx)
	if err != nil {
		return err
	}
	defer release()
	if action != "pause" && action != "resume" && action != "cancel" && action != "retry" {
		return errors.New("invalid export action")
	}
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var b []byte
	if err = tx.QueryRowContext(ctx, "SELECT body FROM jobs WHERE id=? AND owner=?", id, owner).Scan(&b); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	j, err := decodeJob(b)
	if err != nil {
		return err
	}
	if j.Status == "done" || j.Status == "expired" || j.Status == "publishing" {
		return errors.New("export is already published")
	}
	if j.Status == "cancelled" || (j.Status == "failed" && action != "retry" && action != "cancel") {
		return errors.New("export is terminal; create a new task or retry a failed task")
	}
	if action == "retry" {
		if j.Status != "failed" {
			return errors.New("only failed tasks can be retried")
		}
		var total, owned int
		if err = tx.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(SUM(CASE WHEN owner=? THEN 1 ELSE 0 END),0) FROM jobs WHERE status IN ('queued','running','paused','publishing','recovering','retry_wait')", owner).Scan(&total, &owned); err != nil {
			return err
		}
		if total >= 8 || owned >= 2 {
			return ErrQueueFull
		}
		j.Status = "queued"
		if j.Committed.Offset > 0 {
			j.Status = "recovering"
		}
		j.Error = ""
		j.AutomaticRetries = 0
		j.RetryAt = time.Time{}
		j.Retries++
		action = "resume"
	}
	if action == "pause" && (j.Status == "queued" || j.Status == "retry_wait") {
		j.Status = "paused"
		j.WaitReason = "pause"
	}
	if action == "resume" && j.Status == "paused" {
		j.Status = "queued"
		if j.Committed.Offset > 0 {
			j.Status = "recovering"
		}
		j.WaitReason = ""
	}
	if action == "cancel" && j.Status == "failed" {
		j.Status = "queued"
	}
	j.observe(time.Now())
	b, err = encodeJob(j)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE jobs SET control=?,status=?,body=? WHERE id=?", action, j.Status, b, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (q *Queue) Desired(ctx context.Context, id string) (string, error) {
	release, err := q.guard(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	var s string
	err = q.db.QueryRowContext(ctx, "SELECT control FROM jobs WHERE id=?", id).Scan(&s)
	return s, err
}

// FencePublication serializes the final cancellation decision with control
// requests. Once this state commits, cancellation is explicitly too late.
func (q *Queue) FencePublication(ctx context.Context, j *Job) error {
	release, err := q.guard(ctx)
	if err != nil {
		return err
	}
	defer release()
	tx, e := q.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var desired string
	if e = tx.QueryRowContext(ctx, "SELECT control FROM jobs WHERE id=?", j.ID).Scan(&desired); e != nil {
		return e
	}
	if desired == "pause" || desired == "cancel" {
		return context.Canceled
	}
	j.Status = "publishing"
	j.observe(time.Now())
	b, e := encodeJob(j)
	if e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, "UPDATE jobs SET status=?,body=? WHERE id=?", j.Status, b, j.ID); e != nil {
		return e
	}
	return tx.Commit()
}

// Next alternates accounts at each durable checkpoint. A crashed worker's
// running task is eligible immediately after the exclusive process lock moves.
func (q *Queue) Next(ctx context.Context, lastOwner string) (*Job, error) {
	release, err := q.guard(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rows, err := q.db.QueryContext(ctx, "SELECT body,control FROM jobs WHERE status IN ('queued','running','paused','publishing','recovering','retry_wait') AND control<>'pause' ORDER BY CASE WHEN control='cancel' THEN 0 ELSE 1 END,CASE WHEN owner=? THEN 1 ELSE 0 END,created LIMIT 8", lastOwner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var b []byte
		var control string
		if err = rows.Scan(&b, &control); err != nil {
			return nil, err
		}
		j, e := decodeJob(b)
		if e != nil {
			return nil, e
		}
		if control != "cancel" && j.Status == "retry_wait" && time.Now().Before(j.RetryAt) {
			continue
		}
		return j, nil
	}
	return nil, rows.Err()
}

// Called only after the exclusive process lock is acquired. At most eight
// active tasks exist, and only interrupted work needs checkpoint validation.
func (q *Queue) Recover(ctx context.Context) error {
	release, err := q.guard(ctx)
	if err != nil {
		return err
	}
	defer release()
	rows, err := q.db.QueryContext(ctx, "SELECT body FROM jobs WHERE status IN ('running','publishing','recovering') LIMIT 8")
	if err != nil {
		return err
	}
	var jobs []*Job
	for rows.Next() {
		var b []byte
		if err = rows.Scan(&b); err != nil {
			break
		}
		var j *Job
		j, err = decodeJob(b)
		if err != nil {
			break
		}
		jobs = append(jobs, j)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, j := range jobs {
		j.Status, j.ProgressKind, j.WaitReason = "recovering", "indeterminate", "checkpoint_validation"
		j.recovered(time.Now())
		if err = q.Save(ctx, j); err != nil {
			return err
		}
	}
	return nil
}
func (q *Queue) Prune(ctx context.Context, now time.Time) error {
	release, err := q.guard(ctx)
	if err != nil {
		return err
	}
	defer release()
	cursor := ""
	for {
		rows, err := q.db.QueryContext(ctx, "SELECT id,body FROM jobs WHERE id>? AND status IN ('done','expired','failed','cancelled') ORDER BY id LIMIT 64", cursor)
		if err != nil {
			return err
		}
		var expired []*Job
		scanned := 0
		for rows.Next() {
			var id string
			var b []byte
			if err = rows.Scan(&id, &b); err != nil {
				break
			}
			cursor = id
			scanned++
			j, e := decodeJob(b)
			if e != nil {
				err = e
				break
			}
			expiry := j.ExpiresAt
			if j.Status != "done" && j.Status != "expired" {
				expiry = j.CreatedAt.Add(7 * 24 * time.Hour)
			}
			if !expiry.IsZero() && !now.Before(expiry) {
				expired = append(expired, j)
			}
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for _, j := range expired {
			id := j.ID
			if !ValidID(id) {
				return errors.New("invalid stored job identity")
			}
			for _, suffix := range []string{".csv", ".csv.part", ".snapshot", ".spool.db", ".spool.db-wal", ".spool.db-shm"} {
				p := q.Path(id, suffix)
				if suffix == ".snapshot" {
					if err = os.RemoveAll(p); err != nil {
						return err
					}
				} else if err = os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
			// Keep terminal metadata for seven days after the file expires, so
			// returning users can distinguish expiry from a lost task.
			if (j.Status == "done" || j.Status == "expired") && now.Before(j.ExpiresAt.Add(7*24*time.Hour)) {
				j.Status, j.Phase, j.ProgressKind = "expired", "expired", "indeterminate"
				j.HashState, j.Committed = nil, Commit{}
				if err = q.Save(ctx, j); err != nil {
					return err
				}
			} else {
				if _, err = q.db.ExecContext(ctx, "DELETE FROM jobs WHERE id=?", id); err != nil {
					return err
				}
			}
		}
		if scanned < 64 {
			return nil
		}
	}
}
func (j *Job) MakeFilename() string {
	return fmt.Sprintf("%s_%s_%s_%s_v%d.csv", strings.NewReplacer("/", "_", "\\", "_", "\r", "_", "\n", "_").Replace(j.UUID), j.Start.UTC().Format("20060102-1504"), j.Category, j.Mode, j.FormatVersion)
}
