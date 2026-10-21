package database

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/connstring"
)

func init() {
	RegisterDriver(Driver{
		Name:     "mongodb",
		Language: LanguageMongoDB,
		Analyze:  AnalyzeMongo,
		Open:     openMongo,
	})
}

// mongoSampleSize is how many documents DescribeTable reads to infer the
// fields of a collection, which has no fixed schema to look up.
const mongoSampleSize = 100

// mongoCursorCommands return a cursor rather than a single result document.
var mongoCursorCommands = map[string]bool{
	"find": true, "aggregate": true, "listcollections": true, "listindexes": true,
	"listsearchindexes": true,
}

// mongoAdminCommands run against the admin database whatever the connection's
// database is.
var mongoAdminCommands = map[string]bool{
	"listdatabases": true, "ping": true, "buildinfo": true, "hello": true,
	"ismaster": true, "serverstatus": true, "hostinfo": true, "getparameter": true,
	"connectionstatus": true,
}

type mongoBackend struct {
	client   *mongo.Client
	database string
}

func openMongo(ctx context.Context, def ConnectionDef) (Backend, error) {
	cs, err := connstring.ParseAndValidate(def.Source)
	if err != nil {
		// The driver error echoes the URI, password included.
		return nil, errors.New("opening mongodb: invalid connection string")
	}

	opts := options.Client().ApplyURI(def.Source)
	if maxOpen := def.Pool.withDefaults().MaxOpen; maxOpen > 0 {
		opts.SetMaxPoolSize(uint64(maxOpen))
	}

	client, err := mongo.Connect(opts)
	if err != nil {
		return nil, fmt.Errorf("opening mongodb, err: %w", err)
	}

	b := &mongoBackend{client: client, database: cs.Database}

	if err := b.Ping(ctx); err != nil {
		_ = client.Disconnect(context.Background())

		return nil, fmt.Errorf("opening mongodb, err: %w", err)
	}

	return b, nil
}

func (b *mongoBackend) db(name string) (*mongo.Database, error) {
	if name == "" {
		name = b.database
	}

	if name == "" {
		return nil, errors.New("no database: pass a schema or put the database in the connection string")
	}

	return b.client.Database(name), nil
}

// Execute runs a command document. Cursor commands (find, aggregate, ...)
// return one row per document, with one column per top-level field; any
// other command returns its reply as a single row.
func (b *mongoBackend) Execute(ctx context.Context, statement string, opts QueryOptions) (*Result, error) {
	doc, name, err := ParseMongoCommand(statement)
	if err != nil {
		return nil, err
	}

	kind := AnalyzeMongo(statement).Kind

	if opts.Timeout > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	var db *mongo.Database
	if mongoAdminCommands[name] {
		db = b.client.Database("admin")
	} else if db, err = b.db(""); err != nil {
		return nil, err
	}

	start := time.Now()
	limit, cellLimit := opts.maxRows(), opts.maxCellChars()

	var docs []bson.Raw

	truncated := false

	if mongoCursorCommands[name] {
		docs, truncated, err = runMongoCursor(ctx, db, doc, name, limit)
	} else {
		var raw bson.Raw

		raw, err = db.RunCommand(ctx, doc).Raw()
		docs = []bson.Raw{raw}
	}

	if err != nil {
		return nil, fmt.Errorf("executing command, err: %w", err)
	}

	out := documentsToResult(docs, cellLimit)
	out.Kind = kind
	out.Truncated = truncated

	if !kind.ReturnsRows() {
		if n, ok := mongoAffected(docs); ok {
			out.RowsAffected = &n
		}
	}

	out.setDuration(time.Since(start))

	return out, nil
}

// runMongoCursor runs a cursor command and reads at most limit documents,
// plus one to detect truncation. The batch size is set on the command so the
// server never ships a large first batch that would be thrown away.
func runMongoCursor(ctx context.Context, db *mongo.Database, doc bson.D, name string, limit int) ([]bson.Raw, bool, error) {
	if limit >= 0 && name != "listcollections" && name != "listindexes" {
		doc = withMongoBatchSize(doc, name, limit+1)
	}

	cursor, err := db.RunCommandCursor(ctx, doc)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = cursor.Close(context.Background()) }()

	var docs []bson.Raw

	for cursor.Next(ctx) {
		if limit >= 0 && len(docs) >= limit {
			return docs, true, nil
		}

		docs = append(docs, slices.Clone(cursor.Current))
	}

	return docs, false, cursor.Err()
}

// withMongoBatchSize sets the first-batch size: batchSize for find, and
// cursor.batchSize for aggregate.
func withMongoBatchSize(doc bson.D, name string, n int) bson.D {
	doc = slices.Clone(doc)
	size := int32(min(n, math.MaxInt32)) //nolint:gosec // n is a positive row limit, clamped to int32

	if name == "find" {
		for _, e := range doc {
			if strings.EqualFold(e.Key, "batchSize") {
				return doc
			}
		}

		return append(doc, bson.E{Key: "batchSize", Value: size})
	}

	for i, e := range doc {
		if strings.EqualFold(e.Key, "cursor") {
			if c, ok := e.Value.(bson.D); ok && mongoField(c, "batchSize") == nil {
				doc[i].Value = append(slices.Clone(c), bson.E{Key: "batchSize", Value: size})
			}

			return doc
		}
	}

	return append(doc, bson.E{Key: "cursor", Value: bson.D{{Key: "batchSize", Value: size}}})
}

// mongoAffected reads n / nModified from a write reply.
func mongoAffected(docs []bson.Raw) (int64, bool) {
	if len(docs) != 1 {
		return 0, false
	}

	for _, key := range []string{"nModified", "n"} {
		if v, err := docs[0].LookupErr(key); err == nil {
			if n, ok := v.AsInt64OK(); ok {
				return n, true
			}
		}
	}

	return 0, false
}

// documentsToResult lays documents out as a table: the union of top-level
// fields, in first-seen order with _id first, and one row per document. Nested
// documents and arrays are rendered as relaxed Extended JSON.
func documentsToResult(docs []bson.Raw, cellLimit int) *Result {
	out := &Result{Rows: [][]any{}}
	index := map[string]int{}

	addColumn := func(name string) {
		if _, ok := index[name]; !ok {
			index[name] = len(out.Columns)
			out.Columns = append(out.Columns, name)
		}
	}

	for _, doc := range docs {
		if _, err := doc.LookupErr("_id"); err == nil {
			addColumn("_id")

			break
		}
	}

	parsed := make([][]bson.RawElement, len(docs))

	for i, doc := range docs {
		elems, err := doc.Elements()
		if err != nil {
			continue
		}

		parsed[i] = elems

		for _, e := range elems {
			addColumn(e.Key())
		}
	}

	for _, elems := range parsed {
		row := make([]any, len(out.Columns))

		for _, e := range elems {
			row[index[e.Key()]] = limitCell(out, mongoValue(e.Value()), cellLimit)
		}

		out.Rows = append(out.Rows, row)
	}

	out.RowCount = len(out.Rows)

	return out
}

// mongoValue converts a BSON value into a JSON-friendly Go value.
func mongoValue(v bson.RawValue) any {
	switch v.Type {
	case bson.TypeNull, bson.TypeUndefined:
		return nil
	case bson.TypeString:
		return v.StringValue()
	case bson.TypeBoolean:
		return v.Boolean()
	case bson.TypeInt32:
		return v.Int32()
	case bson.TypeInt64:
		return v.Int64()
	case bson.TypeDouble:
		return v.Double()
	case bson.TypeObjectID:
		return v.ObjectID().Hex()
	case bson.TypeDateTime:
		return v.Time().UTC().Format(time.RFC3339Nano)
	case bson.TypeDecimal128:
		return v.Decimal128().String()
	default:
		var x any
		if err := v.Unmarshal(&x); err == nil {
			if b, err := bson.MarshalExtJSON(bson.D{{Key: "v", Value: x}}, false, false); err == nil {
				// Strip the {"v": ...} wrapper MarshalExtJSON needs.
				s := string(b)

				return strings.TrimSuffix(strings.TrimPrefix(s, `{"v":`), "}")
			}
		}

		return v.String()
	}
}

func (b *mongoBackend) ListTables(ctx context.Context, schema string) ([]Table, error) {
	db, err := b.db(schema)
	if err != nil {
		return nil, err
	}

	specs, err := db.ListCollectionSpecifications(ctx, bson.D{})
	if err != nil {
		return nil, fmt.Errorf("listing collections, err: %w", err)
	}

	out := make([]Table, 0, len(specs))

	for _, s := range specs {
		if strings.HasPrefix(s.Name, "system.") {
			continue
		}

		t := Table{Schema: db.Name(), Name: s.Name, Type: "BASE TABLE"}
		if s.Type == "view" {
			t.Type = "VIEW"
		}

		out = append(out, t)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	return out, nil
}

// DescribeTable infers a collection's fields from a sample of its documents.
// A field is reported nullable when some sampled document lacks it or holds
// null, and its type lists every BSON type seen.
func (b *mongoBackend) DescribeTable(ctx context.Context, schema, table string) (*TableDetail, error) {
	if table == "" {
		return nil, errors.New("collection name is required")
	}

	schema, table = SplitQualified(schema, table)

	db, err := b.db(schema)
	if err != nil {
		return nil, err
	}

	cursor, err := db.Collection(table).Aggregate(ctx, bson.A{bson.D{{Key: "$sample", Value: bson.D{{Key: "size", Value: mongoSampleSize}}}}})
	if err != nil {
		return nil, fmt.Errorf("describing collection, err: %w", err)
	}
	defer func() { _ = cursor.Close(context.Background()) }()

	type field struct {
		types map[string]bool
		seen  int
		null  bool
		order int
	}

	fields := map[string]*field{}
	docs := 0

	for cursor.Next(ctx) {
		docs++

		elems, err := cursor.Current.Elements()
		if err != nil {
			continue
		}

		for _, e := range elems {
			f, ok := fields[e.Key()]
			if !ok {
				f = &field{types: map[string]bool{}, order: len(fields)}
				fields[e.Key()] = f
			}

			f.seen++

			if e.Value().Type == bson.TypeNull {
				f.null = true

				continue
			}

			f.types[mongoTypeAlias(e.Value().Type)] = true
		}
	}

	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("describing collection, err: %w", err)
	}

	if docs == 0 {
		return nil, fmt.Errorf("collection %q not found or empty", qualify(db.Name(), table))
	}

	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}

	sort.Slice(names, func(i, j int) bool {
		if names[i] == "_id" || names[j] == "_id" {
			return names[i] == "_id"
		}

		return fields[names[i]].order < fields[names[j]].order
	})

	detail := &TableDetail{Schema: db.Name(), Name: table, Columns: make([]Column, len(names))}

	for i, name := range names {
		f := fields[name]
		types := make([]string, 0, len(f.types))

		for t := range f.types {
			types = append(types, t)
		}

		sort.Strings(types)

		detail.Columns[i] = Column{
			Name:       name,
			Type:       strings.Join(types, "|"),
			Nullable:   f.null || f.seen < docs,
			Position:   i + 1,
			PrimaryKey: name == "_id",
		}
	}

	return detail, nil
}

// mongoTypeAlias returns the $type alias of a BSON type, the name a query
// would use to match it.
func mongoTypeAlias(t bson.Type) string {
	switch t {
	case bson.TypeDouble:
		return "double"
	case bson.TypeString:
		return "string"
	case bson.TypeEmbeddedDocument:
		return "object"
	case bson.TypeArray:
		return "array"
	case bson.TypeBinary:
		return "binData"
	case bson.TypeObjectID:
		return "objectId"
	case bson.TypeBoolean:
		return "bool"
	case bson.TypeDateTime:
		return "date"
	case bson.TypeRegex:
		return "regex"
	case bson.TypeJavaScript, bson.TypeCodeWithScope:
		return "javascript"
	case bson.TypeInt32:
		return "int"
	case bson.TypeTimestamp:
		return "timestamp"
	case bson.TypeInt64:
		return "long"
	case bson.TypeDecimal128:
		return "decimal"
	case bson.TypeMinKey:
		return "minKey"
	case bson.TypeMaxKey:
		return "maxKey"
	default:
		return t.String()
	}
}

func (b *mongoBackend) Ping(ctx context.Context) error {
	return b.client.Ping(ctx, readpref.Primary())
}

func (b *mongoBackend) Close() error {
	return b.client.Disconnect(context.Background())
}
