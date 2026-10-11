package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"cps/internal/model"
	_ "github.com/mattn/go-sqlite3"
)

func TestCreateUpdateAndListInstance(t *testing.T) {
	ctx := context.Background()
	store, err := NewStore(filepath.Join(t.TempDir(), "cps.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	inst := &model.Instance{ID: "i-test123", Status: model.StateCreating, SSHHost: "127.0.0.1", SSHPort: 2222, SSHUser: "alpine", VCPUs: 1, MemoryMB: 512, DiskSizeGB: 10, ImageName: "alpine-cloud", CreatedAt: now, UpdatedAt: now}
	if err := store.CreateInstance(ctx, inst, "accepted test instance"); err != nil {
		t.Fatal(err)
	}
	inst.PID = 1234
	inst.Status = model.StateRunning
	inst.UpdatedAt = time.Now().UTC()
	if err := store.UpdateInstance(ctx, inst, "INFO", "lifecycle", "STATE_TRANSITION", "CREATING -> RUNNING"); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetInstance(ctx, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.StateRunning || got.PID != 1234 || got.SSHPort != 2222 {
		t.Fatalf("unexpected instance: %#v", got)
	}
	rows, err := store.ListInstances(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 instance, got %d", len(rows))
	}
	events, err := store.ListEvents(ctx, inst.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 audit events, got %d", len(events))
	}
}

func TestMigrationPreservesLegacyInventory(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`CREATE TABLE instances (id TEXT PRIMARY KEY,pid INTEGER,status TEXT,ssh_port INTEGER,vcpus INTEGER,memory_mb INTEGER,disk_size_gb INTEGER,image_name TEXT,created_at DATETIME DEFAULT CURRENT_TIMESTAMP)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`INSERT INTO instances(id,pid,status,ssh_port,vcpus,memory_mb,disk_size_gb,image_name,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, "i-legacy", 4321, "running", 2222, 2, 1024, 10, "alpine-cloud", time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	inst, err := store.GetInstance(ctx, "i-legacy")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Status != model.StateRunning {
		t.Fatalf("legacy status was not normalized: %s", inst.Status)
	}
	if inst.PID != 4321 || inst.SSHPort != 2222 || inst.ImageName != "alpine-cloud" {
		t.Fatalf("legacy fields were lost: %#v", inst)
	}
}
