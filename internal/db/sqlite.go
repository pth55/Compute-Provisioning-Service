package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cps/internal/model"
	_ "github.com/mattn/go-sqlite3"
)

type Store struct{ db *sql.DB }

func NewStore(dbPath string) (*Store, error) {
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	dsn := (&url.URL{Scheme: "file", Path: abs}).String() + "?_busy_timeout=5000&_journal_mode=WAL&_foreign_keys=on&_synchronous=NORMAL&_loc=auto"
	database, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open SQLite database: %w", err)
	}
	// One writer connection is intentionally used for this single-host service. WAL
	// still permits readers while a write transaction is active.
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	database.SetConnMaxLifetime(0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("ping SQLite database: %w", err)
	}
	if err := os.Chmod(abs, 0600); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("restrict SQLite database permissions: %w", err)
	}
	s := &Store{db: database}
	if err := s.migrate(ctx); err != nil {
		_ = database.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	// CREATE TABLE IF NOT EXISTS plus additive column migration keeps the simple
	// SQLite database from earlier CPS iterations instead of deleting user data.
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS instances (
		id TEXT PRIMARY KEY,
		pid INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'CREATING',
		ssh_host TEXT NOT NULL DEFAULT '127.0.0.1',
		ssh_port INTEGER NOT NULL DEFAULT 0,
		ssh_user TEXT NOT NULL DEFAULT 'alpine',
		vcpus INTEGER NOT NULL DEFAULT 1,
		memory_mb INTEGER NOT NULL DEFAULT 512,
		disk_size_gb INTEGER NOT NULL DEFAULT 10,
		image_name TEXT NOT NULL DEFAULT '',
		root_disk TEXT NOT NULL DEFAULT '',
		seed_iso TEXT NOT NULL DEFAULT '',
		instance_dir TEXT NOT NULL DEFAULT '',
		qmp_socket TEXT NOT NULL DEFAULT '',
		pid_file TEXT NOT NULL DEFAULT '',
		log_file TEXT NOT NULL DEFAULT '',
		serial_log_file TEXT NOT NULL DEFAULT '',
		debug_log_file TEXT NOT NULL DEFAULT '',
		hibernate_snapshot TEXT NOT NULL DEFAULT '',
		node_id TEXT NOT NULL DEFAULT 'local',
		node_name TEXT NOT NULL DEFAULT 'localhost',
		region TEXT NOT NULL DEFAULT 'local',
		zone TEXT NOT NULL DEFAULT 'local-1a',
		last_error TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL DEFAULT '',
		started_at TEXT NOT NULL DEFAULT '',
		stopped_at TEXT NOT NULL DEFAULT '',
		decommissioned_at TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		return fmt.Errorf("create instances table: %w", err)
	}

	columns := []struct{ name, definition string }{
		{"pid", "INTEGER NOT NULL DEFAULT 0"},
		{"status", "TEXT NOT NULL DEFAULT 'CREATING'"},
		{"ssh_host", "TEXT NOT NULL DEFAULT '127.0.0.1'"},
		{"ssh_port", "INTEGER NOT NULL DEFAULT 0"},
		{"ssh_user", "TEXT NOT NULL DEFAULT 'alpine'"},
		{"vcpus", "INTEGER NOT NULL DEFAULT 1"},
		{"memory_mb", "INTEGER NOT NULL DEFAULT 512"},
		{"disk_size_gb", "INTEGER NOT NULL DEFAULT 10"},
		{"image_name", "TEXT NOT NULL DEFAULT ''"},
		{"root_disk", "TEXT NOT NULL DEFAULT ''"},
		{"seed_iso", "TEXT NOT NULL DEFAULT ''"},
		{"instance_dir", "TEXT NOT NULL DEFAULT ''"},
		{"qmp_socket", "TEXT NOT NULL DEFAULT ''"},
		{"pid_file", "TEXT NOT NULL DEFAULT ''"},
		{"log_file", "TEXT NOT NULL DEFAULT ''"},
		{"serial_log_file", "TEXT NOT NULL DEFAULT ''"},
		{"debug_log_file", "TEXT NOT NULL DEFAULT ''"},
		{"hibernate_snapshot", "TEXT NOT NULL DEFAULT ''"},
		{"node_id", "TEXT NOT NULL DEFAULT 'local'"},
		{"node_name", "TEXT NOT NULL DEFAULT 'localhost'"},
		{"region", "TEXT NOT NULL DEFAULT 'local'"},
		{"zone", "TEXT NOT NULL DEFAULT 'local-1a'"},
		{"last_error", "TEXT NOT NULL DEFAULT ''"},
		{"created_at", "TEXT NOT NULL DEFAULT ''"},
		{"updated_at", "TEXT NOT NULL DEFAULT ''"},
		{"started_at", "TEXT NOT NULL DEFAULT ''"},
		{"stopped_at", "TEXT NOT NULL DEFAULT ''"},
		{"decommissioned_at", "TEXT NOT NULL DEFAULT ''"},
	}
	for _, column := range columns {
		if err := s.ensureColumn(ctx, column.name, column.definition); err != nil {
			return err
		}
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		instance_id TEXT NOT NULL DEFAULT '',
		level TEXT NOT NULL,
		component TEXT NOT NULL,
		event TEXT NOT NULL,
		message TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create events table: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_instances_status ON instances(status)`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_events_instance_id ON events(instance_id, id DESC)`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_events_created_at ON events(created_at DESC)`); err != nil {
		return err
	}
	// Normalize state strings from the original prototype, which used lowercase
	// states and deleted records on termination.
	if _, err := s.db.ExecContext(ctx, `UPDATE instances SET status = CASE lower(status)
		WHEN 'running' THEN 'RUNNING' WHEN 'stopped' THEN 'STOPPED' WHEN 'paused' THEN 'PAUSED'
		WHEN 'creating' THEN 'CREATING' WHEN 'starting' THEN 'STARTING' WHEN 'stopping' THEN 'STOPPING'
		WHEN 'hibernated' THEN 'HIBERNATED' WHEN 'hibernating' THEN 'HIBERNATING'
		WHEN 'terminated' THEN 'DECOMMISSIONED' WHEN 'decommissioned' THEN 'DECOMMISSIONED'
		WHEN 'failed' THEN 'FAILED' ELSE upper(status) END`); err != nil {
		return fmt.Errorf("normalize instance states: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE instances SET ssh_host='127.0.0.1' WHERE ssh_host=''`); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.ExecContext(ctx, `UPDATE instances SET created_at=? WHERE created_at='' OR created_at IS NULL`, now); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE instances SET updated_at=created_at WHERE updated_at='' OR updated_at IS NULL`); err != nil {
		return err
	}
	return nil
}

func (s *Store) ensureColumn(ctx context.Context, name, definition string) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(instances)`)
	if err != nil {
		return fmt.Errorf("inspect instances schema: %w", err)
	}
	found := false
	for rows.Next() {
		var cid int
		var columnName, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &columnName, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return err
		}
		if strings.EqualFold(columnName, name) {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	if found {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE instances ADD COLUMN `+name+` `+definition); err != nil {
		return fmt.Errorf("add instances.%s: %w", name, err)
	}
	return nil
}

const instanceColumns = `id,pid,status,ssh_host,ssh_port,ssh_user,vcpus,memory_mb,disk_size_gb,image_name,root_disk,seed_iso,instance_dir,qmp_socket,pid_file,log_file,serial_log_file,debug_log_file,hibernate_snapshot,node_id,node_name,region,zone,last_error,created_at,updated_at,started_at,stopped_at,decommissioned_at`

func (s *Store) CreateInstance(ctx context.Context, inst *model.Instance, message string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	query := `INSERT INTO instances (` + instanceColumns + `) VALUES (` + placeholders(29) + `)`
	if _, err := tx.ExecContext(ctx, query, instanceArgs(inst)...); err != nil {
		return fmt.Errorf("insert instance %s: %w", inst.ID, err)
	}
	if err := insertEvent(ctx, tx, model.Event{InstanceID: inst.ID, Level: "INFO", Component: "lifecycle", Event: "INSTANCE_CREATED", Message: message, CreatedAt: time.Now().UTC()}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) UpdateInstance(ctx context.Context, inst *model.Instance, level, component, eventName, message string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	args := instanceArgs(inst)
	setCols := strings.Split(instanceColumns, ",")
	sets := make([]string, 0, len(setCols)-1)
	for _, col := range setCols {
		if col != "id" {
			sets = append(sets, col+"=?")
		}
	}
	// instanceArgs is in the same field order as instanceColumns; skip the ID for UPDATE.
	updateArgs := append(append([]any{}, args[1:]...), inst.ID)
	if _, err := tx.ExecContext(ctx, `UPDATE instances SET `+strings.Join(sets, ",")+` WHERE id=?`, updateArgs...); err != nil {
		return fmt.Errorf("update instance %s: %w", inst.ID, err)
	}
	if err := insertEvent(ctx, tx, model.Event{InstanceID: inst.ID, Level: level, Component: component, Event: eventName, Message: message, CreatedAt: time.Now().UTC()}); err != nil {
		return err
	}
	return tx.Commit()
}

func instanceArgs(i *model.Instance) []any {
	return []any{i.ID, i.PID, string(i.Status), i.SSHHost, i.SSHPort, i.SSHUser, i.VCPUs, i.MemoryMB, i.DiskSizeGB, i.ImageName,
		i.RootDisk, i.SeedISO, i.InstanceDir, i.QMPsocket, i.PIDFile, i.LogFile, i.SerialLogFile, i.DebugLogFile, i.HibernateSnapshot,
		i.NodeID, i.NodeName, i.Region, i.Zone, i.LastError, formatTime(i.CreatedAt), formatTime(i.UpdatedAt), formatTime(i.StartedAt), formatTime(i.StoppedAt), formatTime(i.DecommissionedAt)}
}

func placeholders(n int) string {
	p := make([]string, n)
	for i := range p {
		p[i] = "?"
	}
	return strings.Join(p, ",")
}

func insertEvent(ctx context.Context, exec interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, e model.Event) error {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	_, err := exec.ExecContext(ctx, `INSERT INTO events(instance_id,level,component,event,message,created_at) VALUES(?,?,?,?,?,?)`, e.InstanceID, e.Level, e.Component, e.Event, e.Message, formatTime(e.CreatedAt))
	if err != nil {
		return fmt.Errorf("insert audit event: %w", err)
	}
	return nil
}

func (s *Store) RecordEvent(ctx context.Context, e model.Event) error {
	return insertEvent(ctx, s.db, e)
}

func (s *Store) GetInstance(ctx context.Context, id string) (*model.Instance, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+instanceColumns+` FROM instances WHERE id=?`, id)
	inst, err := scanInstance(row)
	if err != nil {
		return nil, err
	}
	return &inst, nil
}

func (s *Store) ListInstances(ctx context.Context) ([]model.Instance, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+instanceColumns+` FROM instances ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]model.Instance, 0)
	for rows.Next() {
		inst, err := scanInstance(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, inst)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Store) ListEvents(ctx context.Context, instanceID string, limit int) ([]model.Event, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	query := `SELECT id,instance_id,level,component,event,message,created_at FROM events`
	args := []any{}
	if instanceID != "" {
		query += ` WHERE instance_id=?`
		args = append(args, instanceID)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.Event, 0)
	for rows.Next() {
		var e model.Event
		var created string
		if err := rows.Scan(&e.ID, &e.InstanceID, &e.Level, &e.Component, &e.Event, &e.Message, &created); err != nil {
			return nil, err
		}
		e.CreatedAt = parseTime(created)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

type scanner interface{ Scan(dest ...any) error }

func scanInstance(row scanner) (model.Instance, error) {
	var i model.Instance
	var status, created, updated, started, stopped, decommissioned string
	err := row.Scan(&i.ID, &i.PID, &status, &i.SSHHost, &i.SSHPort, &i.SSHUser, &i.VCPUs, &i.MemoryMB, &i.DiskSizeGB, &i.ImageName,
		&i.RootDisk, &i.SeedISO, &i.InstanceDir, &i.QMPsocket, &i.PIDFile, &i.LogFile, &i.SerialLogFile, &i.DebugLogFile, &i.HibernateSnapshot,
		&i.NodeID, &i.NodeName, &i.Region, &i.Zone, &i.LastError, &created, &updated, &started, &stopped, &decommissioned)
	if err != nil {
		return i, err
	}
	i.Status = model.State(status)
	i.CreatedAt = parseTime(created)
	i.UpdatedAt = parseTime(updated)
	i.StartedAt = parseTime(started)
	i.StoppedAt = parseTime(stopped)
	i.DecommissionedAt = parseTime(decommissioned)
	return i, nil
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
