//go:build duckdb

package database

import (
	"context"
	"testing"
)

func TestDuckDBIntrospection(t *testing.T) {
	ctx := context.Background()

	db, err := ConnectDB(ctx, "duckdb", "", PoolConfig{MaxOpen: 1})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.ExecContext(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name VARCHAR NOT NULL);
		CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INTEGER REFERENCES users(id))`); err != nil {
		t.Fatalf("create: %v", err)
	}

	tables, err := ListTables(ctx, db, "duckdb", "")
	if err != nil || len(tables) != 2 {
		t.Fatalf("ListTables = %v, %v; want 2 tables", tables, err)
	}

	detail, err := DescribeTable(ctx, db, "duckdb", "", "users")
	if err != nil {
		t.Fatalf("DescribeTable: %v", err)
	}
	if len(detail.Columns) != 2 || !detail.Columns[0].PrimaryKey || detail.Columns[1].Nullable {
		t.Errorf("columns = %+v", detail.Columns)
	}

	orders, err := DescribeTable(ctx, db, "duckdb", "", "orders")
	if err != nil {
		t.Fatalf("DescribeTable orders: %v", err)
	}
	if len(orders.ForeignKeys) != 1 || orders.ForeignKeys[0].ReferencedTable != "users" {
		t.Errorf("foreign keys = %+v", orders.ForeignKeys)
	}
}
