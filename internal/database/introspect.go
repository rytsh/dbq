package database

import (
	"context"
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"
)

// Table is a table or view discovered by introspection.
type Table struct {
	Schema string `json:"schema,omitempty"`
	Name   string `json:"name"`
	Type   string `json:"type,omitempty"`
}

// Column describes one column of a table.
type Column struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Nullable   bool   `json:"nullable"`
	Default    string `json:"default,omitempty"`
	Position   int    `json:"position"`
	PrimaryKey bool   `json:"primary_key,omitempty"`
}

// ForeignKey is a reference from one column to another table's column.
type ForeignKey struct {
	Column          string `json:"column"`
	ReferencedTable string `json:"referenced_table"`
	// ReferencedColumn may be empty when the catalog does not expose it.
	ReferencedColumn string `json:"referenced_column,omitempty"`
}

// TableDetail is the full description of a single table.
type TableDetail struct {
	Schema string `json:"schema,omitempty"`
	Name   string `json:"name"`
	// Type is BASE TABLE or VIEW. It matters: a model will otherwise happily
	// generate an INSERT against a view.
	Type        string       `json:"type,omitempty"`
	Columns     []Column     `json:"columns"`
	ForeignKeys []ForeignKey `json:"foreign_keys,omitempty"`
}

// ListForeignKeys returns the outgoing foreign keys of a table.
//
// Without these a model writing a multi-table JOIN is guessing at join keys,
// which is the single most common way generated SQL goes wrong. Failure is not
// fatal to callers: a restricted account may not see the constraint catalog.
func ListForeignKeys(ctx context.Context, db *sqlx.DB, driver, schema, table string) ([]ForeignKey, error) {
	query, args := foreignKeyQuery(driver, schema, table)
	if query == "" {
		return nil, nil
	}

	rows, err := db.QueryxContext(ctx, db.Rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("listing foreign keys, err: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []ForeignKey{}

	for rows.Next() {
		var fk ForeignKey
		if err := rows.Scan(&fk.Column, &fk.ReferencedTable, &fk.ReferencedColumn); err != nil {
			return nil, fmt.Errorf("scanning foreign key, err: %w", err)
		}

		out = append(out, fk)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating foreign keys, err: %w", err)
	}

	return out, nil
}

func foreignKeyQuery(driver, schema, table string) (string, []any) {
	switch driver {
	case "sqlite3":
		return `SELECT "from", "table", COALESCE("to", '') FROM pragma_foreign_key_list(?)`, []any{table}

	case "godror":
		q := `SELECT cc.column_name, rc.table_name, rc.column_name
			FROM all_constraints c
			JOIN all_cons_columns cc ON c.owner = cc.owner AND c.constraint_name = cc.constraint_name
			JOIN all_cons_columns rc ON c.r_owner = rc.owner AND c.r_constraint_name = rc.constraint_name
				AND cc.position = rc.position
			WHERE c.constraint_type = 'R' AND c.table_name = UPPER(?)`

		args := []any{table}
		if schema != "" {
			q += ` AND c.owner = UPPER(?)`

			args = append(args, schema)
		}

		return q, args

	case "ingres":
		// Ingres predates information_schema and exposes constraints through
		// vendor catalogs. Foreign-key discovery can be added independently.
		return "", nil

	default:
		// referential_constraints is standard and present on PostgreSQL and
		// SQL Server; most ODBC targets expose it too.
		q := `SELECT kcu.column_name, ccu.table_name, ccu.column_name
			FROM information_schema.table_constraints tc
			JOIN information_schema.key_column_usage kcu
				ON tc.constraint_name = kcu.constraint_name AND tc.table_schema = kcu.table_schema
			JOIN information_schema.referential_constraints rc
				ON tc.constraint_name = rc.constraint_name AND tc.constraint_schema = rc.constraint_schema
			JOIN information_schema.key_column_usage ccu
				ON rc.unique_constraint_name = ccu.constraint_name
				AND rc.unique_constraint_schema = ccu.constraint_schema
				AND ccu.ordinal_position = kcu.position_in_unique_constraint
			WHERE tc.constraint_type = 'FOREIGN KEY' AND tc.table_name = ?`

		args := []any{table}
		if schema != "" {
			q += ` AND tc.table_schema = ?`

			args = append(args, schema)
		}

		return q, args
	}
}

// ListTables returns the tables and views visible to the connection.
//
// schema is optional; when empty every non-system schema is returned. The
// queries are driver-specific because catalog layout is not portable:
// information_schema exists on PostgreSQL and SQL Server, SQLite has
// sqlite_master, and Oracle has the ALL_* views.
func ListTables(ctx context.Context, db *sqlx.DB, driver, schema string) ([]Table, error) {
	query, args := tableQuery(driver, schema)

	rows, err := db.QueryxContext(ctx, db.Rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("listing tables, err: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Table{}

	for rows.Next() {
		var t Table
		if err := rows.Scan(&t.Schema, &t.Name, &t.Type); err != nil {
			return nil, fmt.Errorf("scanning table row, err: %w", err)
		}

		out = append(out, t)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating tables, err: %w", err)
	}

	return out, nil
}

func tableQuery(driver, schema string) (string, []any) {
	switch driver {
	case "sqlite3":
		// SQLite has no schemas; the column is selected as a constant so every
		// driver returns the same three-column shape.
		return `SELECT '' AS table_schema, name AS table_name,
			CASE type WHEN 'view' THEN 'VIEW' ELSE 'BASE TABLE' END AS table_type
			FROM sqlite_master
			WHERE type IN ('table','view') AND name NOT LIKE 'sqlite_%'
			ORDER BY name`, nil

	case "godror":
		// Oracle identifiers are stored upper-cased.
		if schema != "" {
			return `SELECT owner, table_name, 'BASE TABLE' FROM all_tables WHERE owner = UPPER(?)
				UNION ALL
				SELECT owner, view_name, 'VIEW' FROM all_views WHERE owner = UPPER(?)
				ORDER BY 1, 2`, []any{schema, schema}
		}

		return `SELECT owner, table_name, 'BASE TABLE' FROM all_tables
			WHERE owner NOT IN ('SYS','SYSTEM','XDB','OUTLN','DBSNMP','APPQOSSYS','CTXSYS','MDSYS','ORDSYS','WMSYS','LBACSYS','OLAPSYS','AUDSYS','GSMADMIN_INTERNAL','DVSYS','ORDDATA')
			UNION ALL
			SELECT owner, view_name, 'VIEW' FROM all_views
			WHERE owner NOT IN ('SYS','SYSTEM','XDB','OUTLN','DBSNMP','APPQOSSYS','CTXSYS','MDSYS','ORDSYS','WMSYS','LBACSYS','OLAPSYS','AUDSYS','GSMADMIN_INTERNAL','DVSYS','ORDDATA')
			ORDER BY 1, 2`, nil

	case "ingres":
		if schema != "" {
			return `SELECT TRIM(table_owner), TRIM(table_name),
				CASE table_type WHEN 'V' THEN 'VIEW' ELSE 'BASE TABLE' END
				FROM iitables
				WHERE system_use = 'U' AND table_type IN ('T', 'V') AND table_owner = ?
				ORDER BY table_owner, table_name`, []any{schema}
		}

		return `SELECT TRIM(table_owner), TRIM(table_name),
			CASE table_type WHEN 'V' THEN 'VIEW' ELSE 'BASE TABLE' END
			FROM iitables
			WHERE system_use = 'U' AND table_type IN ('T', 'V')
			ORDER BY table_owner, table_name`, nil

	default:
		// pgx, sqlserver and most ODBC targets expose information_schema.
		if schema != "" {
			return `SELECT table_schema, table_name, table_type
				FROM information_schema.tables
				WHERE table_schema = ?
				ORDER BY table_schema, table_name`, []any{schema}
		}

		return `SELECT table_schema, table_name, table_type
			FROM information_schema.tables
			WHERE table_schema NOT IN ('pg_catalog','information_schema','sys','INFORMATION_SCHEMA')
			ORDER BY table_schema, table_name`, nil
	}
}

// DescribeTable returns the column layout of a single table.
func DescribeTable(ctx context.Context, db *sqlx.DB, driver, schema, table string) (*TableDetail, error) {
	if table == "" {
		return nil, fmt.Errorf("table name is required")
	}

	// "schema.table" is the shape people actually type; accept it.
	if schema == "" {
		if s, t, ok := strings.Cut(table, "."); ok {
			schema, table = s, t
		}
	}

	columns, err := describeColumns(ctx, db, driver, schema, table)
	if err != nil {
		return nil, err
	}

	if len(columns) == 0 {
		return nil, fmt.Errorf("table %q not found", qualify(schema, table))
	}

	detail := &TableDetail{Schema: schema, Name: table, Columns: columns}

	// A restricted account may be denied the constraint catalog; that should
	// degrade the description, not fail the call.
	if fks, err := ListForeignKeys(ctx, db, driver, schema, table); err == nil {
		detail.ForeignKeys = fks
	}

	return detail, nil
}

func qualify(schema, table string) string {
	if schema == "" {
		return table
	}

	return schema + "." + table
}

func describeColumns(ctx context.Context, db *sqlx.DB, driver, schema, table string) ([]Column, error) {
	switch driver {
	case "sqlite3":
		return sqliteColumns(ctx, db, table)
	case "godror":
		return oracleColumns(ctx, db, schema, table)
	case "ingres":
		return ingresColumns(ctx, db, schema, table)
	default:
		return infoSchemaColumns(ctx, db, schema, table)
	}
}

func ingresColumns(ctx context.Context, db *sqlx.DB, schema, table string) ([]Column, error) {
	q := `SELECT TRIM(column_name), TRIM(column_datatype), column_nulls,
		'', column_sequence, 0
		FROM iicolumns WHERE table_name = ?`
	args := []any{table}

	if schema != "" {
		q += ` AND table_owner = ?`
		args = append(args, schema)
	}

	q += ` ORDER BY column_sequence`

	return scanColumns(ctx, db, q, args, "Y")
}

func sqliteColumns(ctx context.Context, db *sqlx.DB, table string) ([]Column, error) {
	// pragma_table_info is the table-valued form of PRAGMA table_info, which
	// unlike the PRAGMA statement accepts a bind parameter.
	const q = `SELECT cid, name, type, "notnull", COALESCE(dflt_value, ''), pk
		FROM pragma_table_info(?) ORDER BY cid`

	rows, err := db.QueryxContext(ctx, q, table)
	if err != nil {
		return nil, fmt.Errorf("describing table, err: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Column{}

	for rows.Next() {
		var (
			col      Column
			cid      int
			notNull  int
			pkIndex  int
			typeName string
		)

		if err := rows.Scan(&cid, &col.Name, &typeName, &notNull, &col.Default, &pkIndex); err != nil {
			return nil, fmt.Errorf("scanning column, err: %w", err)
		}

		col.Type = typeName
		col.Position = cid + 1
		col.Nullable = notNull == 0
		col.PrimaryKey = pkIndex > 0

		out = append(out, col)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating columns, err: %w", err)
	}

	return out, nil
}

func oracleColumns(ctx context.Context, db *sqlx.DB, schema, table string) ([]Column, error) {
	q := `SELECT c.column_name, c.data_type, c.nullable, COALESCE(TO_CHAR(c.data_default), ''), c.column_id,
			CASE WHEN pk.column_name IS NULL THEN 0 ELSE 1 END
		FROM all_tab_columns c
		LEFT JOIN (
			SELECT cc.owner, cc.table_name, cc.column_name
			FROM all_constraints ct
			JOIN all_cons_columns cc
				ON ct.owner = cc.owner AND ct.constraint_name = cc.constraint_name
			WHERE ct.constraint_type = 'P'
		) pk ON pk.owner = c.owner AND pk.table_name = c.table_name AND pk.column_name = c.column_name
		WHERE c.table_name = UPPER(?)`

	args := []any{table}

	if schema != "" {
		q += ` AND c.owner = UPPER(?)`

		args = append(args, schema)
	}

	q += ` ORDER BY c.column_id`

	return scanColumns(ctx, db, q, args, "Y")
}

func infoSchemaColumns(ctx context.Context, db *sqlx.DB, schema, table string) ([]Column, error) {
	q := `SELECT c.column_name, c.data_type, c.is_nullable, COALESCE(CAST(c.column_default AS VARCHAR(4000)), ''),
			c.ordinal_position,
			CASE WHEN k.column_name IS NULL THEN 0 ELSE 1 END
		FROM information_schema.columns c
		LEFT JOIN (
			SELECT kcu.table_schema, kcu.table_name, kcu.column_name
			FROM information_schema.table_constraints tc
			JOIN information_schema.key_column_usage kcu
				ON tc.constraint_name = kcu.constraint_name
				AND tc.table_schema = kcu.table_schema
				AND tc.table_name = kcu.table_name
			WHERE tc.constraint_type = 'PRIMARY KEY'
		) k ON k.table_schema = c.table_schema AND k.table_name = c.table_name AND k.column_name = c.column_name
		WHERE c.table_name = ?`

	args := []any{table}

	if schema != "" {
		q += ` AND c.table_schema = ?`

		args = append(args, schema)
	}

	q += ` ORDER BY c.ordinal_position`

	return scanColumns(ctx, db, q, args, "YES")
}

// scanColumns runs a six-column introspection query. nullableTrue is the value
// the catalog uses to mean "nullable" ("YES" for information_schema, "Y" for Oracle).
func scanColumns(ctx context.Context, db *sqlx.DB, q string, args []any, nullableTrue string) ([]Column, error) {
	rows, err := db.QueryxContext(ctx, db.Rebind(q), args...)
	if err != nil {
		return nil, fmt.Errorf("describing table, err: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Column{}

	for rows.Next() {
		var (
			col      Column
			nullable string
			pk       int
		)

		if err := rows.Scan(&col.Name, &col.Type, &nullable, &col.Default, &col.Position, &pk); err != nil {
			return nil, fmt.Errorf("scanning column, err: %w", err)
		}

		col.Nullable = strings.EqualFold(nullable, nullableTrue)
		col.PrimaryKey = pk > 0

		out = append(out, col)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating columns, err: %w", err)
	}

	return out, nil
}
