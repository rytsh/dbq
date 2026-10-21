package database

import (
	"reflect"
	"testing"
)

func TestParseRedisCommand(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"GET user:1", []string{"GET", "user:1"}},
		{`SET k "hello world" EX 60`, []string{"SET", "k", "hello world", "EX", "60"}},
		{`SET k "a\"b\n\x41"`, []string{"SET", "k", "a\"b\nA"}},
		{`SET k 'it\s raw'`, []string{"SET", "k", `it\s raw`}},
		{`SET k ""`, []string{"SET", "k", ""}},
		{"  HGETALL   h  ", []string{"HGETALL", "h"}},
	}

	for _, tt := range tests {
		got, err := ParseRedisCommand(tt.in)
		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("ParseRedisCommand(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}

	for _, bad := range []string{"", "   ", `GET "open`, `GET 'open`} {
		if _, err := ParseRedisCommand(bad); err == nil {
			t.Errorf("ParseRedisCommand(%q) = nil error", bad)
		}
	}
}

func TestAnalyzeRedis(t *testing.T) {
	tests := []struct {
		cmd       string
		kind      Kind
		unbounded bool
		refused   bool
	}{
		{"GET user:1", KindRead, false, false},
		{"get user:1", KindRead, false, false},
		{"HGETALL user:1", KindRead, false, false},
		{"SCAN 0 MATCH user:* COUNT 100", KindRead, false, false},
		{"CONFIG GET maxmemory", KindRead, false, false},
		{"MEMORY USAGE k", KindRead, false, false},
		{"SORT list BY w_*", KindRead, false, false},
		{"SET k v", KindWrite, false, false},
		{"DEL k1 k2", KindWrite, false, false},
		{"SORT list STORE dst", KindWrite, false, false},
		{"FLUSHALL", KindWrite, true, false},
		{"flushdb async", KindWrite, true, false},
		{"KEYS *", KindSchema, false, false},
		{"CONFIG SET maxmemory 1", KindSchema, false, false},
		{"MEMORY PURGE", KindSchema, false, false},
		{"EVAL \"return 1\" 0", KindSchema, false, false},
		{"EVAL_RO \"return 1\" 0", KindSchema, false, false},
		{"FT.CREATE idx ON HASH SCHEMA a TEXT", KindSchema, false, false},
		{"SELECT 1", KindSession, false, true},
		{"MULTI", KindSession, false, true},
		{"BLPOP q 0", KindSession, false, true},
		{"SUBSCRIBE ch", KindSession, false, true},
		{"SHUTDOWN", KindUnknown, true, false},
		{"FROBNICATE k", KindUnknown, true, false},
		{`GET "unterminated`, KindUnknown, false, true},
	}

	for _, tt := range tests {
		t.Run(tt.cmd, func(t *testing.T) {
			got := AnalyzeRedis(tt.cmd)
			if got.Kind != tt.kind || got.Unbounded != tt.unbounded || got.Refused != tt.refused {
				t.Errorf("AnalyzeRedis(%q) = %+v, want kind=%s unbounded=%v refused=%v",
					tt.cmd, got, tt.kind, tt.unbounded, tt.refused)
			}
		})
	}
}

func TestRedisResultShapes(t *testing.T) {
	scalar := redisResult("v", 10, 100)
	if len(scalar.Columns) != 1 || scalar.RowCount != 1 || scalar.Rows[0][0] != "v" {
		t.Errorf("scalar = %+v", scalar)
	}

	list := redisResult([]any{"a", "b", "c"}, 2, 100)
	if list.RowCount != 2 || !list.Truncated || list.Rows[1][1] != "b" {
		t.Errorf("list = %+v", list)
	}

	m := redisResult(map[any]any{"b": "2", "a": "1"}, 10, 100)
	if m.RowCount != 2 || m.Rows[0][0] != "a" || m.Rows[0][1] != "1" {
		t.Errorf("map = %+v", m)
	}

	nested := redisResult([]any{"0", []any{"k1", "k2"}}, 10, 100)
	if nested.Rows[1][1] != "[k1, k2]" {
		t.Errorf("nested = %+v", nested)
	}

	if empty := redisResult(nil, 10, 100); empty.RowCount != 1 || empty.Rows[0][0] != nil {
		t.Errorf("nil = %+v", empty)
	}
}
