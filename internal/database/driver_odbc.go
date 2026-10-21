//go:build odbc

package database

import (
	// The ODBC driver needs cgo and unixODBC, so it is only part of builds
	// made with the odbc tag. It registers itself as "odbc".
	_ "github.com/alexbrainman/odbc"
)

func init() {
	RegisterSQLDriver("odbc")
}
