package database

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

func init() {
	RegisterDriver(Driver{
		Name:     "redis",
		Language: LanguageRedis,
		Analyze:  AnalyzeRedis,
		Open:     openRedis,
	})
}

// redisScanLimit bounds how many keys ListTables reports. Keyspaces routinely
// hold millions of keys, and the listing is for orientation, not export.
const redisScanLimit = 1000

// redisScanBatch is the COUNT hint passed to each SCAN call.
const redisScanBatch = 200

type redisBackend struct {
	client *redis.Client
}

func openRedis(ctx context.Context, def ConnectionDef) (Backend, error) {
	opts, err := redis.ParseURL(def.Source)
	if err != nil {
		// The parser error can echo the URL, password included.
		return nil, errors.New("opening redis: source must be a redis:// or rediss:// URL")
	}

	if maxOpen := def.Pool.withDefaults().MaxOpen; maxOpen > 0 {
		opts.PoolSize = maxOpen
	}

	b := &redisBackend{client: redis.NewClient(opts)}

	if err := b.Ping(ctx); err != nil {
		_ = b.client.Close()

		return nil, fmt.Errorf("opening redis, err: %w", err)
	}

	return b, nil
}

func (b *redisBackend) Execute(ctx context.Context, statement string, opts QueryOptions) (*Result, error) {
	args, err := ParseRedisCommand(strings.TrimSuffix(strings.TrimSpace(statement), ";"))
	if err != nil {
		return nil, err
	}

	kind := AnalyzeRedis(statement).Kind

	if opts.Timeout > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	start := time.Now()

	cmdArgs := make([]any, len(args))
	for i, a := range args {
		cmdArgs[i] = a
	}

	val, err := b.client.Do(ctx, cmdArgs...).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("executing command, err: %w", err)
	}

	out := redisResult(val, opts.maxRows(), opts.maxCellChars())
	out.Kind = kind

	if !kind.ReturnsRows() {
		if n, ok := val.(int64); ok {
			out.RowsAffected = &n
		}
	}

	out.setDuration(time.Since(start))

	return out, nil
}

// redisResult lays a reply out as a table. A map becomes field/value rows, a
// list becomes one row per element, and anything else is a single value.
func redisResult(val any, limit, cellLimit int) *Result {
	out := &Result{Rows: [][]any{}}

	add := func(row ...any) bool {
		if limit >= 0 && len(out.Rows) >= limit {
			out.Truncated = true

			return false
		}

		for i := range row {
			row[i] = limitCell(out, redisValue(row[i]), cellLimit)
		}

		out.Rows = append(out.Rows, row)

		return true
	}

	switch v := val.(type) {
	case map[any]any:
		out.Columns = []string{"field", "value"}

		keys := make([]string, 0, len(v))
		byKey := make(map[string]any, len(v))

		for k, item := range v {
			s := fmt.Sprint(k)
			keys = append(keys, s)
			byKey[s] = item
		}

		sort.Strings(keys)

		for _, k := range keys {
			if !add(k, byKey[k]) {
				break
			}
		}
	case []any:
		out.Columns = []string{"#", "value"}

		for i, item := range v {
			if !add(i+1, item) {
				break
			}
		}
	default:
		out.Columns = []string{"value"}
		add(v)
	}

	out.RowCount = len(out.Rows)

	return out
}

// redisValue renders nested replies (a SCAN cursor and its key list, an
// XRANGE entry) as text so every cell stays a scalar.
func redisValue(v any) any {
	switch t := v.(type) {
	case nil, string, int64, float64, bool, int:
		return t
	case []any:
		parts := make([]string, len(t))
		for i, item := range t {
			parts[i] = fmt.Sprint(redisValue(item))
		}

		return "[" + strings.Join(parts, ", ") + "]"
	default:
		return normalize(t)
	}
}

// ListTables scans keys. schema is an optional MATCH pattern such as "user:*";
// each key is reported with its Redis type.
func (b *redisBackend) ListTables(ctx context.Context, schema string) ([]Table, error) {
	match := schema
	if match == "" {
		match = "*"
	}

	var (
		keys   []string
		cursor uint64
	)

	for {
		batch, next, err := b.client.Scan(ctx, cursor, match, redisScanBatch).Result()
		if err != nil {
			return nil, fmt.Errorf("scanning keys, err: %w", err)
		}

		keys = append(keys, batch...)
		cursor = next

		if cursor == 0 || len(keys) >= redisScanLimit {
			break
		}
	}

	if len(keys) > redisScanLimit {
		keys = keys[:redisScanLimit]
	}

	sort.Strings(keys)

	pipe := b.client.Pipeline()
	types := make([]*redis.StatusCmd, len(keys))

	for i, k := range keys {
		types[i] = pipe.Type(ctx, k)
	}

	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("reading key types, err: %w", err)
	}

	out := make([]Table, 0, len(keys))

	for i, k := range keys {
		typ := types[i].Val()
		if typ == "none" {
			// Expired between SCAN and TYPE.
			continue
		}

		out = append(out, Table{Name: k, Type: typ})
	}

	return out, nil
}

// DescribeTable reports a key's type, TTL, size and encoding; for a hash, its
// fields are listed as columns since that is what a model will query.
func (b *redisBackend) DescribeTable(ctx context.Context, schema, key string) (*TableDetail, error) {
	if key == "" {
		return nil, errors.New("key is required")
	}

	if schema != "" {
		key = schema + key
	}

	typ, err := b.client.Type(ctx, key).Result()
	if err != nil {
		return nil, fmt.Errorf("describing key, err: %w", err)
	}

	if typ == "none" {
		return nil, fmt.Errorf("key %q not found", key)
	}

	detail := &TableDetail{Name: key, Type: typ}

	ttl := "none"
	if d, err := b.client.PTTL(ctx, key).Result(); err == nil && d > 0 {
		ttl = d.Round(time.Millisecond).String()
	}

	size := redisSize(ctx, b.client, key, typ)
	encoding, _ := b.client.ObjectEncoding(ctx, key).Result()

	detail.Columns = []Column{
		{Name: "type", Type: typ, Position: 1},
		{Name: "ttl", Type: ttl, Position: 2},
		{Name: "size", Type: size, Position: 3},
		{Name: "encoding", Type: encoding, Position: 4},
	}

	if typ == "hash" {
		fields, err := b.client.HKeys(ctx, key).Result()
		if err == nil {
			sort.Strings(fields)

			if len(fields) > redisScanLimit {
				fields = fields[:redisScanLimit]
			}

			for _, f := range fields {
				detail.Columns = append(detail.Columns, Column{
					Name: "field:" + f, Type: "string", Position: len(detail.Columns) + 1,
				})
			}
		}
	}

	return detail, nil
}

func redisSize(ctx context.Context, c *redis.Client, key, typ string) string {
	var cmd *redis.IntCmd

	switch typ {
	case "string":
		cmd = c.StrLen(ctx, key)
	case "list":
		cmd = c.LLen(ctx, key)
	case "set":
		cmd = c.SCard(ctx, key)
	case "zset":
		cmd = c.ZCard(ctx, key)
	case "hash":
		cmd = c.HLen(ctx, key)
	case "stream":
		cmd = c.XLen(ctx, key)
	default:
		return ""
	}

	n, err := cmd.Result()
	if err != nil {
		return ""
	}

	unit := " items"
	if typ == "string" {
		unit = " bytes"
	}

	return strconv.FormatInt(n, 10) + unit
}

func (b *redisBackend) Ping(ctx context.Context) error {
	return b.client.Ping(ctx).Err()
}

func (b *redisBackend) Close() error {
	return b.client.Close()
}
