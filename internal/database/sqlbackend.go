package database

import (
	"context"

	"github.com/jmoiron/sqlx"
)

// RegisterSQLDriver registers a database/sql driver, already registered with
// database/sql under the same name, as a dbq connection type.
func RegisterSQLDriver(name string) {
	RegisterDriver(Driver{
		Name:     name,
		Language: LanguageSQL,
		Analyze:  Analyze,
		Open: func(ctx context.Context, def ConnectionDef) (Backend, error) {
			db, err := ConnectDB(ctx, def.Type, def.Source, def.Pool)
			if err != nil {
				return nil, err
			}

			return &SQLBackend{DB: db, Catalog: def.CatalogType()}, nil
		},
	})
}

// SQLBackend serves every database/sql driver.
type SQLBackend struct {
	DB *sqlx.DB
	// Catalog selects the catalog queries: the driver name, or the dialect
	// configured for a generic driver such as ODBC.
	Catalog string
}

// Execute implements Backend.
func (b *SQLBackend) Execute(ctx context.Context, statement string, opts QueryOptions) (*Result, error) {
	return Execute(ctx, b.DB, statement, opts)
}

// ListTables implements Backend.
func (b *SQLBackend) ListTables(ctx context.Context, schema string) ([]Table, error) {
	return ListTables(ctx, b.DB, b.Catalog, schema)
}

// DescribeTable implements Backend.
func (b *SQLBackend) DescribeTable(ctx context.Context, schema, table string) (*TableDetail, error) {
	return DescribeTable(ctx, b.DB, b.Catalog, schema, table)
}

// Ping implements Backend.
func (b *SQLBackend) Ping(ctx context.Context) error {
	return b.DB.PingContext(ctx)
}

// Close implements Backend.
func (b *SQLBackend) Close() error {
	return b.DB.Close()
}
