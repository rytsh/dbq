//go:build duckdb

package database

import (
	// DuckDB is linked in through cgo, so it is only part of builds made with
	// the duckdb tag. It registers itself as "duckdb".
	_ "github.com/duckdb/duckdb-go/v2"
)

func init() {
	RegisterSQLDriver("duckdb")
}
