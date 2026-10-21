package database

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	gocql "github.com/apache/cassandra-gocql-driver/v2"
)

func init() {
	RegisterDriver(Driver{
		Name:     "cassandra",
		Language: LanguageCQL,
		Analyze:  AnalyzeCQL,
		Open:     openCassandra,
	})
}

// cassandraBackend serves Cassandra and ScyllaDB over the native protocol.
type cassandraBackend struct {
	session  *gocql.Session
	keyspace string
}

// parseCassandraSource reads a cassandra:// URL:
//
//	cassandra://user:pass@host1:9042,host2/keyspace?consistency=local_quorum&dc=eu1&tls=true
func parseCassandraSource(source string) (*gocql.ClusterConfig, error) {
	rest, ok := strings.CutPrefix(source, "cassandra://")
	if !ok {
		return nil, errors.New("source must be a cassandra:// URL")
	}

	// net/url rejects the comma-separated host list, so the authority is
	// split off by hand and only the remainder is parsed as a URL.
	authority, tail, _ := strings.Cut(rest, "/")
	if i := strings.IndexAny(authority, "?"); i >= 0 {
		authority, tail = authority[:i], tail+authority[i:]
	}

	var user *url.Userinfo

	if at := strings.LastIndex(authority, "@"); at >= 0 {
		u, err := url.Parse("x://" + authority[:at] + "@h")
		if err != nil {
			return nil, errors.New("invalid credentials in source")
		}

		user, authority = u.User, authority[at+1:]
	}

	if authority == "" {
		return nil, errors.New("source has no hosts")
	}

	u, err := url.Parse("x:///" + tail)
	if err != nil {
		return nil, errors.New("invalid keyspace or options in source")
	}

	cluster := gocql.NewCluster(strings.Split(authority, ",")...)
	cluster.Keyspace = strings.Trim(u.Path, "/")
	cluster.Consistency = gocql.LocalQuorum

	if user != nil {
		password, _ := user.Password()
		cluster.Authenticator = gocql.PasswordAuthenticator{Username: user.Username(), Password: password}
	}

	if err := applyCassandraOptions(cluster, u.Query()); err != nil {
		return nil, err
	}

	return cluster, nil
}

func applyCassandraOptions(cluster *gocql.ClusterConfig, q url.Values) error {
	for key, values := range q {
		value := values[len(values)-1]

		switch key {
		case "consistency":
			c, err := gocql.ParseConsistencyWrapper(strings.ToUpper(value))
			if err != nil {
				return fmt.Errorf("invalid consistency %q", value)
			}

			cluster.Consistency = c
		case "dc":
			cluster.PoolConfig.HostSelectionPolicy = gocql.TokenAwareHostPolicy(gocql.DCAwareRoundRobinPolicy(value))
		case "timeout", "connect_timeout":
			d, err := time.ParseDuration(value)
			if err != nil {
				return fmt.Errorf("invalid %s %q", key, value)
			}

			if key == "timeout" {
				cluster.Timeout = d
			} else {
				cluster.ConnectTimeout = d
			}
		case "tls":
			on, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("invalid tls %q", value)
			}

			if on {
				cluster.SslOpts = &gocql.SslOptions{EnableHostVerification: true}
			}
		case "proto_version":
			n, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("invalid proto_version %q", value)
			}

			cluster.ProtoVersion = n
		default:
			return fmt.Errorf("unknown source option %q", key)
		}
	}

	return nil
}

func openCassandra(_ context.Context, def ConnectionDef) (Backend, error) {
	cluster, err := parseCassandraSource(def.Source)
	if err != nil {
		return nil, fmt.Errorf("opening cassandra: %w", err)
	}

	if maxOpen := def.Pool.withDefaults().MaxOpen; maxOpen > 0 {
		cluster.NumConns = maxOpen
	}

	session, err := cluster.CreateSession()
	if err != nil {
		return nil, fmt.Errorf("opening cassandra, err: %w", err)
	}

	return &cassandraBackend{session: session, keyspace: cluster.Keyspace}, nil
}

func (b *cassandraBackend) Execute(ctx context.Context, statement string, opts QueryOptions) (*Result, error) {
	kind := AnalyzeCQL(statement).Kind

	if opts.Timeout > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	start := time.Now()
	query := b.session.Query(strings.TrimSuffix(strings.TrimSpace(statement), ";"))

	if !kind.ReturnsRows() {
		if err := query.ExecContext(ctx); err != nil {
			return nil, fmt.Errorf("executing statement, err: %w", err)
		}

		out := &Result{Kind: kind}
		out.setDuration(time.Since(start))

		return out, nil
	}

	limit, cellLimit := opts.maxRows(), opts.maxCellChars()

	// A page one row larger than the limit is enough to know whether the
	// result was truncated, without pulling further pages.
	if limit > 0 {
		query = query.PageSize(limit + 1)
	}

	iter := query.IterContext(ctx)
	out := &Result{Kind: kind, Rows: [][]any{}}

	// MapScan and RowData turn NULL into the zero value (an empty string, a
	// nil UUID), which would show a model data that is not there. Scanning
	// into a pointer to each column's type keeps NULL distinct.
	// A tuple column is scanned as one value per element, which is also how
	// the driver names them: col[0], col[1], ...
	var dest []any

	for _, col := range iter.Columns() {
		if tuple, ok := col.TypeInfo.(gocql.TupleTypeInfo); ok {
			for i, elem := range tuple.Elems {
				out.Columns = append(out.Columns, gocql.TupleColumnName(col.Name, i))
				dest = append(dest, nullableDest(elem))
			}

			continue
		}

		out.Columns = append(out.Columns, col.Name)
		dest = append(dest, nullableDest(col.TypeInfo))
	}

	for iter.Scan(dest...) {
		if limit >= 0 && len(out.Rows) >= limit {
			// One more row exists beyond the limit.
			out.Truncated = true

			break
		}

		values := make([]any, len(dest))
		for i, d := range dest {
			ptr := reflect.ValueOf(d).Elem()
			if ptr.IsNil() {
				continue
			}

			values[i] = limitCell(out, normalize(ptr.Elem().Interface()), cellLimit)
			ptr.Set(reflect.Zero(ptr.Type()))
		}

		out.Rows = append(out.Rows, values)
	}

	if err := iter.Close(); err != nil {
		return nil, fmt.Errorf("querying database, err: %w", err)
	}

	out.RowCount = len(out.Rows)
	out.Kind = kind
	out.setDuration(time.Since(start))

	return out, nil
}

// nullableDest returns a **T scan target for a column of type info, so that
// NULL leaves the inner pointer nil instead of producing a zero value.
func nullableDest(info gocql.TypeInfo) any {
	return reflect.New(reflect.PointerTo(reflect.TypeOf(info.Zero()))).Interface()
}

// limitCell shortens a text value to the cell limit, counting it on out.
func limitCell(out *Result, value any, cellLimit int) any {
	text, ok := value.(string)
	if !ok {
		return value
	}

	truncated, cut := truncateCell(text, cellLimit)
	if cut {
		out.CellsTruncated++
	}

	return truncated
}

func (b *cassandraBackend) keyspaceOr(schema string) (string, error) {
	if schema != "" {
		return schema, nil
	}

	if b.keyspace == "" {
		return "", errors.New("no keyspace: pass a schema or put the keyspace in the source URL")
	}

	return b.keyspace, nil
}

var cassandraSystemKeyspaces = map[string]bool{
	"system": true, "system_auth": true, "system_schema": true, "system_distributed": true,
	"system_traces": true, "system_views": true, "system_virtual_schema": true,
	"system_distributed_everywhere": true, "system_replicated_keys": true,
}

func (b *cassandraBackend) ListTables(ctx context.Context, schema string) ([]Table, error) {
	q := `SELECT keyspace_name, table_name FROM system_schema.tables`
	args := []any{}

	if schema == "" && b.keyspace != "" {
		schema = b.keyspace
	}

	if schema != "" {
		q += ` WHERE keyspace_name = ?`
		args = append(args, schema)
	}

	out := []Table{}

	iter := b.session.Query(q, args...).IterContext(ctx)

	var keyspace, name string
	for iter.Scan(&keyspace, &name) {
		if schema == "" && cassandraSystemKeyspaces[keyspace] {
			continue
		}

		out = append(out, Table{Schema: keyspace, Name: name, Type: "BASE TABLE"})
	}

	if err := iter.Close(); err != nil {
		return nil, fmt.Errorf("listing tables, err: %w", err)
	}

	views := b.session.Query(strings.Replace(q, "table_name FROM system_schema.tables", "view_name FROM system_schema.views", 1), args...).
		IterContext(ctx)
	for views.Scan(&keyspace, &name) {
		if schema == "" && cassandraSystemKeyspaces[keyspace] {
			continue
		}

		out = append(out, Table{Schema: keyspace, Name: name, Type: "VIEW"})
	}

	// Materialized views are optional on a cluster; failing to list them
	// should not hide the tables.
	_ = views.Close()

	sort.Slice(out, func(i, j int) bool {
		if out[i].Schema != out[j].Schema {
			return out[i].Schema < out[j].Schema
		}

		return out[i].Name < out[j].Name
	})

	return out, nil
}

func (b *cassandraBackend) DescribeTable(ctx context.Context, schema, table string) (*TableDetail, error) {
	if table == "" {
		return nil, errors.New("table name is required")
	}

	schema, table = SplitQualified(schema, table)

	keyspace, err := b.keyspaceOr(schema)
	if err != nil {
		return nil, err
	}

	iter := b.session.Query(`SELECT column_name, type, kind, position FROM system_schema.columns
		WHERE keyspace_name = ? AND table_name = ?`, keyspace, table).IterContext(ctx)

	type column struct {
		Column

		kind     string
		position int
	}

	var (
		cols                 []column
		name, typ, kindOfCol string
		position             int
	)

	for iter.Scan(&name, &typ, &kindOfCol, &position) {
		cols = append(cols, column{
			Column: Column{
				Name: name,
				Type: typ,
				// Only key columns are guaranteed present; every other
				// column of a row may be missing.
				Nullable:   kindOfCol == "regular" || kindOfCol == "static",
				PrimaryKey: kindOfCol == "partition_key" || kindOfCol == "clustering",
			},
			kind:     kindOfCol,
			position: position,
		})
	}

	if err := iter.Close(); err != nil {
		return nil, fmt.Errorf("describing table, err: %w", err)
	}

	if len(cols) == 0 {
		return nil, fmt.Errorf("table %q not found", qualify(keyspace, table))
	}

	// Primary key first, in key order, then the rest by name, which is how
	// cqlsh's DESCRIBE lays a table out.
	rank := map[string]int{"partition_key": 0, "clustering": 1, "static": 2, "regular": 3}
	sort.SliceStable(cols, func(i, j int) bool {
		if rank[cols[i].kind] != rank[cols[j].kind] {
			return rank[cols[i].kind] < rank[cols[j].kind]
		}

		if cols[i].position != cols[j].position {
			return cols[i].position < cols[j].position
		}

		return cols[i].Name < cols[j].Name
	})

	detail := &TableDetail{Schema: keyspace, Name: table, Columns: make([]Column, len(cols))}
	for i, c := range cols {
		c.Position = i + 1
		detail.Columns[i] = c.Column
	}

	return detail, nil
}

func (b *cassandraBackend) Ping(ctx context.Context) error {
	return b.session.Query(`SELECT release_version FROM system.local`).ExecContext(ctx)
}

func (b *cassandraBackend) Close() error {
	b.session.Close()

	return nil
}
