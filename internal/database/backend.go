package database

import (
	"context"
	"fmt"
	"strings"
)

// Query languages a backend can speak. They decide how a statement is
// classified and tell a model which syntax to write.
const (
	LanguageSQL     = "sql"
	LanguageCQL     = "cql"
	LanguageMongoDB = "mongodb"
	LanguageRedis   = "redis"
)

// Backend is an open connection, whatever kind of database is behind it.
//
// SQL databases share one implementation over database/sql. Databases that do
// not speak SQL bring their own, together with their own statement analyzer
// (see Driver), because the permission gate is only as good as its
// understanding of the language it is guarding.
type Backend interface {
	// Execute runs a statement the caller has already authorized.
	Execute(ctx context.Context, statement string, opts QueryOptions) (*Result, error)
	// ListTables lists tables, collections or keys. schema narrows the
	// listing; what it means depends on the backend.
	ListTables(ctx context.Context, schema string) ([]Table, error)
	// DescribeTable describes one table, collection or key.
	DescribeTable(ctx context.Context, schema, table string) (*TableDetail, error)
	// Ping verifies the connection.
	Ping(ctx context.Context) error
	// Close releases every resource the backend holds.
	Close() error
}

// Driver is a connection type dbq can open.
type Driver struct {
	// Name is the connection type used in configuration.
	Name string
	// Language is the query language statements must be written in.
	Language string
	// Analyze classifies a statement without a connection, so a statement
	// is authorized before anything is sent to the database. It must parse
	// the statement exactly the way Open's backend will execute it.
	Analyze func(statement string) Analysis
	// Open connects. No statement is run until Execute.
	Open func(ctx context.Context, def ConnectionDef) (Backend, error)
}

var registry = map[string]Driver{}

// Drivers are the connection types this build supports, in registration order.
var Drivers []string

// RegisterDriver makes a connection type available. It is meant to be called
// from init functions and panics on a duplicate or incomplete registration,
// which is a programming error.
func RegisterDriver(d Driver) {
	if _, dup := registry[d.Name]; dup {
		panic(fmt.Sprintf("database: driver %q registered twice", d.Name))
	}

	if d.Name == "" || d.Language == "" || d.Analyze == nil || d.Open == nil {
		panic(fmt.Sprintf("database: driver %q registered incomplete", d.Name))
	}

	registry[d.Name] = d
	Drivers = append(Drivers, d.Name)
}

// ValidateDriver reports whether dbType is a driver dbq was built with.
//
// The driver would also reject an unknown type, but only after the caller has
// handed over a connection string; checking up front keeps credentials out of
// driver-level error paths and gives a usable message.
func ValidateDriver(dbType string) error {
	if _, ok := registry[dbType]; ok {
		return nil
	}

	return fmt.Errorf("unsupported driver %q, supported: %v", dbType, Drivers)
}

// LanguageOf returns the query language of a connection type, or "" when the
// type is unknown.
func LanguageOf(dbType string) string {
	return registry[dbType].Language
}

// AnalyzeFor classifies a statement in the language of a connection type. An
// unknown type yields a refusal rather than a guess.
func AnalyzeFor(dbType, statement string) Analysis {
	d, ok := registry[dbType]
	if !ok {
		return Analysis{Kind: KindUnknown, Refused: true, Reason: fmt.Sprintf("unsupported driver %q", dbType)}
	}

	return d.Analyze(statement)
}

func openBackend(ctx context.Context, def ConnectionDef) (Backend, error) {
	d, ok := registry[def.Type]
	if !ok {
		return nil, ValidateDriver(def.Type)
	}

	return d.Open(ctx, def)
}

// SplitQualified splits "schema.table" when schema is empty, which is the
// shape people actually type.
func SplitQualified(schema, table string) (string, string) {
	if schema == "" {
		if s, t, ok := strings.Cut(table, "."); ok {
			return s, t
		}
	}

	return schema, table
}
