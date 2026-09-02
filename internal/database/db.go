package database

import (
	"context"
	"fmt"
	"slices"
	"time"

	// Drivers register themselves with database/sql on import; the names
	// they register are listed in Drivers below.
	_ "github.com/alexbrainman/odbc"
	_ "github.com/denisenkom/go-mssqldb"
	_ "github.com/godror/godror"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/mattn/go-sqlite3"

	"github.com/jmoiron/sqlx"
)

// PoolConfig tunes the database/sql pool behind one connection.
//
// Zero means "use the default"; a negative value means unlimited (for
// MaxIdle, which cannot be unlimited, it means "as many as MaxOpen"). Keeping
// the pool small by default is deliberate: dbq is usually one of many clients
// of a database, and an agent can issue queries far faster than a person.
type PoolConfig struct {
	// MaxOpen caps open connections, idle or in use.
	MaxOpen int
	// MaxIdle caps idle connections kept for reuse.
	MaxIdle int
	// MaxLifetime retires a connection after this long, so the pool follows
	// DNS changes and failovers. Negative disables the limit.
	MaxLifetime time.Duration
	// MaxIdleTime closes a connection idle for this long. Negative disables it.
	MaxIdleTime time.Duration
}

// DefaultPool is used for every field left at zero.
var DefaultPool = PoolConfig{
	MaxOpen:     3,
	MaxIdle:     3,
	MaxLifetime: 15 * time.Minute,
}

// Merged returns p with every zero field taken from fallback, which is how a
// connection's own settings sit on top of the global ones.
func (p PoolConfig) Merged(fallback PoolConfig) PoolConfig {
	if p.MaxOpen == 0 {
		p.MaxOpen = fallback.MaxOpen
	}

	if p.MaxIdle == 0 {
		p.MaxIdle = fallback.MaxIdle
	}

	if p.MaxLifetime == 0 {
		p.MaxLifetime = fallback.MaxLifetime
	}

	if p.MaxIdleTime == 0 {
		p.MaxIdleTime = fallback.MaxIdleTime
	}

	return p
}

// withDefaults resolves zero fields to DefaultPool and translates the negative
// "unlimited" convention into the zero database/sql expects.
func (p PoolConfig) withDefaults() PoolConfig {
	out := p.Merged(DefaultPool)

	// database/sql has no unlimited-idle mode: zero means "keep none". The
	// most that can ever be idle is MaxOpen, so that is what negative means;
	// with MaxOpen itself unlimited, fall back to the default.
	if out.MaxIdle < 0 {
		out.MaxIdle = out.MaxOpen
		if out.MaxIdle <= 0 {
			out.MaxIdle = DefaultPool.MaxIdle
		}
	}

	return PoolConfig{
		MaxOpen:     max(out.MaxOpen, 0),
		MaxIdle:     out.MaxIdle,
		MaxLifetime: max(out.MaxLifetime, 0),
		MaxIdleTime: max(out.MaxIdleTime, 0),
	}
}

// Drivers are the database/sql driver names registered by the blank imports above.
var Drivers = []string{"pgx", "odbc", "godror", "sqlite3", "sqlserver"}

// ValidateDriver reports whether dbType is a driver dbq was built with.
//
// database/sql would also reject an unknown driver, but only after the caller
// has handed over a connection string; checking up front keeps credentials out
// of driver-level error paths and gives a usable message.
func ValidateDriver(dbType string) error {
	if slices.Contains(Drivers, dbType) {
		return nil
	}

	return fmt.Errorf("unsupported driver %q, supported: %v", dbType, Drivers)
}

// ConnectDB opens a pool and verifies it with a ping.
func ConnectDB(ctx context.Context, dbType, dbSource string, pool PoolConfig) (*sqlx.DB, error) {
	if err := ValidateDriver(dbType); err != nil {
		return nil, err
	}

	db, err := sqlx.ConnectContext(ctx, dbType, dbSource)
	if err != nil {
		return nil, fmt.Errorf("opening database, err: %w", err)
	}

	pool = pool.withDefaults()

	db.SetMaxOpenConns(pool.MaxOpen)
	db.SetMaxIdleConns(pool.MaxIdle)
	db.SetConnMaxLifetime(pool.MaxLifetime)
	db.SetConnMaxIdleTime(pool.MaxIdleTime)

	return db, nil
}
