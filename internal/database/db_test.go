package database

import (
	"context"
	"testing"
	"time"
)

func TestConnectDBAppliesPool(t *testing.T) {
	db, err := ConnectDB(context.Background(), "sqlite3", ":memory:", PoolConfig{MaxOpen: 7})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	if got := db.Stats().MaxOpenConnections; got != 7 {
		t.Errorf("MaxOpenConnections = %d, want 7", got)
	}
}

// TestPoolConfigDefaults: zero inherits the default, negative means unlimited.
func TestPoolConfigDefaults(t *testing.T) {
	got := PoolConfig{MaxOpen: -1}.withDefaults()

	if got.MaxOpen != 0 {
		t.Errorf("MaxOpen = %d, want 0 (unlimited for database/sql)", got.MaxOpen)
	}

	if got.MaxIdle != DefaultPool.MaxIdle || got.MaxLifetime != DefaultPool.MaxLifetime {
		t.Errorf("defaults not applied: %+v", got)
	}

	// database/sql has no "unlimited idle"; negative follows max_open, which is
	// the most that could ever be idle anyway.
	if got := (PoolConfig{MaxOpen: 9, MaxIdle: -1}).withDefaults(); got.MaxIdle != 9 {
		t.Errorf("MaxIdle = %d, want 9 (follows MaxOpen)", got.MaxIdle)
	}

	if got := (PoolConfig{MaxOpen: -1, MaxIdle: -1}).withDefaults(); got.MaxIdle != DefaultPool.MaxIdle {
		t.Errorf("MaxIdle = %d, want the default when MaxOpen is unlimited", got.MaxIdle)
	}

	if DefaultPool.MaxLifetime != 15*time.Minute {
		t.Errorf("DefaultPool.MaxLifetime = %s, want 15m", DefaultPool.MaxLifetime)
	}
}
