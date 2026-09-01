package database

import (
	"context"
	"fmt"
	"slices"
	"time"

	_ "github.com/alexbrainman/odbc"
	_ "github.com/denisenkom/go-mssqldb"
	_ "github.com/godror/godror"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/mattn/go-sqlite3"

	"github.com/jmoiron/sqlx"
)

var (
	ConnMaxLifetime = 15 * time.Minute
	MaxIdleConns    = 3
	MaxOpenConns    = 3
)

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
func ConnectDB(ctx context.Context, dbType, dbSource string) (*sqlx.DB, error) {
	if err := ValidateDriver(dbType); err != nil {
		return nil, err
	}

	db, err := sqlx.ConnectContext(ctx, dbType, dbSource)
	if err != nil {
		return nil, fmt.Errorf("opening database, err: %w", err)
	}

	db.SetConnMaxLifetime(ConnMaxLifetime)
	db.SetMaxIdleConns(MaxIdleConns)
	db.SetMaxOpenConns(MaxOpenConns)

	return db, nil
}
