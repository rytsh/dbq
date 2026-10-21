package database

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// Redis statements are one command per call in redis-cli syntax:
//
//	GET user:1
//	HGETALL "user:1"
//	SET greeting "hello world" EX 60
//
// Classification is a table lookup on the command name, mirroring the
// readonly / write flags of COMMAND INFO. It is static on purpose: a command
// is authorized before anything is sent, and a server cannot widen what dbq
// lets through. Anything not listed needs full access.

// redisRead only read. Commands that walk the whole keyspace (KEYS) are
// absent: they block the server, so they need full access.
var redisRead = setOf(
	// strings
	"get", "mget", "getrange", "strlen", "substr", "lcs",
	// keys
	"exists", "type", "ttl", "pttl", "expiretime", "pexpiretime", "scan",
	"randomkey", "dump", "object", "touch", "dbsize",
	// hashes
	"hget", "hmget", "hgetall", "hkeys", "hvals", "hlen", "hexists", "hstrlen",
	"hscan", "hrandfield", "httl", "hpttl", "hexpiretime", "hpexpiretime",
	// lists
	"lrange", "lindex", "llen", "lpos",
	// sets
	"smembers", "sismember", "smismember", "scard", "srandmember", "sscan",
	"sinter", "sintercard", "sunion", "sdiff",
	// sorted sets
	"zrange", "zrangebyscore", "zrangebylex", "zrevrange", "zrevrangebyscore",
	"zrevrangebylex", "zrank", "zrevrank", "zscore", "zmscore", "zcard", "zcount",
	"zlexcount", "zscan", "zrandmember", "zinter", "zunion", "zdiff", "zintercard",
	// streams
	"xrange", "xrevrange", "xlen", "xread", "xinfo", "xpending",
	// bitmaps, hyperloglog, geo
	"getbit", "bitcount", "bitpos", "bitfield_ro", "pfcount",
	"geopos", "geodist", "geohash", "georadius_ro", "georadiusbymember_ro", "geosearch",
	// json, search and time series modules
	"json.get", "json.mget", "json.type", "json.strlen", "json.arrlen", "json.objkeys",
	"json.objlen", "json.resp", "json.arrindex", "ft.search", "ft.aggregate", "ft.info",
	"ft._list", "ft.explain", "ft.profile", "ft.tagvals", "ts.get", "ts.mget", "ts.range",
	"ts.revrange", "ts.mrange", "ts.mrevrange", "ts.info", "ts.queryindex",
	// server introspection
	"ping", "echo", "time", "info", "lastsave", "role", "lolwut",
)

// redisWriteKey writes the keys it names, and only those: bounded.
var redisWriteKey = setOf(
	"set", "setnx", "setex", "psetex", "getset", "getdel", "getex", "mset", "msetnx",
	"append", "setrange", "incr", "incrby", "incrbyfloat", "decr", "decrby",
	"del", "unlink", "expire", "pexpire", "expireat", "pexpireat", "persist",
	"rename", "renamenx", "copy", "restore",
	"hset", "hsetnx", "hmset", "hdel", "hincrby", "hincrbyfloat", "hexpire",
	"hpexpire", "hexpireat", "hpexpireat", "hpersist", "hgetdel", "hgetex", "hsetex",
	"lpush", "rpush", "lpushx", "rpushx", "lpop", "rpop", "lset", "linsert", "lrem",
	"ltrim", "lmove", "rpoplpush", "lmpop",
	"sadd", "srem", "spop", "smove", "sinterstore", "sunionstore", "sdiffstore",
	"zadd", "zrem", "zincrby", "zpopmin", "zpopmax", "zremrangebyrank",
	"zremrangebyscore", "zremrangebylex", "zrangestore", "zinterstore",
	"zunionstore", "zdiffstore", "zmpop",
	"xadd", "xdel", "xtrim", "xack", "xclaim", "xautoclaim", "xsetid",
	"setbit", "bitfield", "bitop", "pfadd", "pfmerge", "geoadd", "geosearchstore",
	"json.set", "json.del", "json.forget", "json.merge", "json.mset", "json.numincrby",
	"json.strappend", "json.arrappend", "json.arrinsert", "json.arrpop",
	"json.arrtrim", "json.toggle", "json.clear",
	"ts.add", "ts.madd", "ts.incrby", "ts.decrby", "ts.del",
)

// redisSession changes connection state; on a pooled client the change would
// leak into unrelated commands, or a blocking call would pin a connection.
var redisSession = setOf(
	"select", "auth", "hello", "client", "reset", "quit",
	"multi", "exec", "discard", "watch", "unwatch",
	"subscribe", "psubscribe", "ssubscribe", "unsubscribe", "punsubscribe",
	"sunsubscribe", "monitor", "readonly", "readwrite",
	"blpop", "brpop", "blmove", "brpoplpush", "blmpop", "bzpopmin", "bzpopmax",
	"bzmpop", "wait", "waitaof",
)

func setOf(names ...string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}

	return out
}

// ParseRedisCommand splits a redis-cli style line into arguments. Double
// quotes support the redis-cli escapes (\n, \t, \", \\, \xHH); single quotes
// are literal.
func ParseRedisCommand(line string) ([]string, error) {
	line = strings.TrimSpace(line)

	var (
		args []string
		cur  strings.Builder
		in   bool
	)

	runes := []rune(line)

	for i := 0; i < len(runes); i++ {
		c := runes[i]

		switch {
		case unicode.IsSpace(c):
			if in {
				args = append(args, cur.String())
				cur.Reset()
				in = false
			}

		case c == '"':
			end, s, err := redisDoubleQuoted(runes, i)
			if err != nil {
				return nil, err
			}

			cur.WriteString(s)
			in = true
			i = end

		case c == '\'':
			end := i + 1
			for end < len(runes) && runes[end] != '\'' {
				end++
			}

			if end >= len(runes) {
				return nil, errors.New("unterminated single quote")
			}

			cur.WriteString(string(runes[i+1 : end]))
			in = true
			i = end

		default:
			cur.WriteRune(c)
			in = true
		}
	}

	if in {
		args = append(args, cur.String())
	}

	if len(args) == 0 {
		return nil, errors.New("empty command")
	}

	return args, nil
}

func redisDoubleQuoted(runes []rune, start int) (int, string, error) {
	var b strings.Builder

	for i := start + 1; i < len(runes); i++ {
		c := runes[i]

		if c == '"' {
			return i, b.String(), nil
		}

		if c != '\\' || i+1 >= len(runes) {
			b.WriteRune(c)

			continue
		}

		i++

		switch runes[i] {
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'x':
			if i+2 < len(runes) {
				var v byte
				if _, err := fmt.Sscanf(string(runes[i+1:i+3]), "%02x", &v); err == nil {
					b.WriteByte(v)

					i += 2

					continue
				}
			}

			b.WriteRune('x')
		default:
			b.WriteRune(runes[i])
		}
	}

	return 0, "", errors.New("unterminated double quote")
}

// AnalyzeRedis classifies one Redis command.
func AnalyzeRedis(statement string) Analysis {
	args, err := ParseRedisCommand(strings.TrimSuffix(strings.TrimSpace(statement), ";"))
	if err != nil {
		return Analysis{Kind: KindUnknown, Refused: true, Reason: err.Error(), Advice: "Send one Redis command, e.g. GET user:1"}
	}

	name := strings.ToLower(args[0])

	switch {
	case redisContainers[name]:
		// Two-word commands are classified by their subcommand.
		return analyzeRedisContainer(name, args)
	case redisRead[name]:
		return Analysis{Kind: KindRead}
	case redisWriteKey[name]:
		return Analysis{Kind: KindWrite}
	case redisSession[name]:
		return Analysis{
			Kind: KindSession, Refused: true,
			Reason: strings.ToUpper(name) + " changes or holds connection state, which would leak into unrelated commands on a pooled client",
			Advice: "Run it with redis-cli, which owns its own connection",
		}
	case name == "sort" || name == "sort_ro":
		return analyzeRedisSort(name, args)
	}

	if a, ok := redisSpecial[name]; ok {
		return a
	}

	if redisModuleDDL[name] {
		return Analysis{Kind: KindSchema}
	}

	return Analysis{
		Kind: KindUnknown, Unbounded: true, Reason: "command " + strings.ToUpper(name) + " is not known to dbq",
		Advice: "Unknown commands need full access; ask the user to raise this connection's permission level if it is genuinely needed",
	}
}

// redisContainers take a subcommand that decides between read and admin.
var redisContainers = setOf(
	"memory", "config", "acl", "cluster", "function", "script", "slowlog",
	"latency", "module", "debug", "command", "pubsub",
)

// redisModuleDDL create or drop search indexes and time series.
var redisModuleDDL = setOf(
	"ft.create", "ft.dropindex", "ft.alter", "ft.aliasadd", "ft.aliasdel", "ft.aliasupdate",
	"ts.create", "ts.alter", "ts.createrule", "ts.deleterule",
)

var redisScriptAnalysis = func(name string) Analysis {
	return Analysis{Kind: KindSchema, Reason: strings.ToUpper(name) + " runs a server-side script whose effects dbq cannot see"}
}

// redisSpecial are commands with a fixed analysis that does not fit the
// read / bounded-write / session tables.
var redisSpecial = map[string]Analysis{
	"keys": {
		Kind: KindSchema, Reason: "KEYS walks the whole keyspace and blocks the server",
		Advice: "Use SCAN 0 MATCH <pattern> COUNT 100 instead",
	},
	"flushdb": {
		Kind: KindWrite, Unbounded: true, Reason: "FLUSHDB deletes every key",
		Advice: "Delete specific keys with DEL or UNLINK, or ask the user to raise this connection's permission level",
	},
	"flushall": {
		Kind: KindWrite, Unbounded: true, Reason: "FLUSHALL deletes every key",
		Advice: "Delete specific keys with DEL or UNLINK, or ask the user to raise this connection's permission level",
	},
	"eval":       redisScriptAnalysis("eval"),
	"evalsha":    redisScriptAnalysis("evalsha"),
	"eval_ro":    redisScriptAnalysis("eval_ro"),
	"evalsha_ro": redisScriptAnalysis("evalsha_ro"),
	"fcall":      redisScriptAnalysis("fcall"),
	"fcall_ro":   redisScriptAnalysis("fcall_ro"),
	"publish":    {Kind: KindWrite, Reason: "PUBLISH delivers a message to subscribers"},
	"spublish":   {Kind: KindWrite, Reason: "SPUBLISH delivers a message to subscribers"},
}

// analyzeRedisContainer handles CONFIG GET vs CONFIG SET and friends.
func analyzeRedisContainer(name string, args []string) Analysis {
	sub := ""
	if len(args) > 1 {
		sub = strings.ToLower(args[1])
	}

	readSubs := map[string]map[string]bool{
		"config":   setOf("get"),
		"memory":   setOf("usage", "stats", "doctor", "malloc-stats", "help"),
		"acl":      setOf("whoami", "cat", "list", "users", "getuser", "log", "genpass", "dryrun"),
		"cluster":  setOf("info", "nodes", "slots", "shards", "myid", "keyslot", "countkeysinslot", "getkeysinslot"),
		"function": setOf("list", "stats", "dump"),
		"script":   setOf("exists"),
		"slowlog":  setOf("get", "len"),
		"latency":  setOf("latest", "history", "graph", "doctor", "histogram"),
		"module":   setOf("list"),
		"command":  setOf("info", "count", "docs", "list", "getkeys", "getkeysandflags"),
		"pubsub":   setOf("channels", "numsub", "numpat", "shardchannels", "shardnumsub"),
	}

	if readSubs[name][sub] {
		return Analysis{Kind: KindRead}
	}

	return Analysis{
		Kind:   KindSchema,
		Reason: strings.ToUpper(name+" "+sub) + " changes server configuration or state",
	}
}

// analyzeRedisSort: SORT ... STORE writes a key, and BY / GET patterns read
// arbitrary other keys, which on a read-only grant is still a read.
func analyzeRedisSort(name string, args []string) Analysis {
	if name == "sort" {
		for _, a := range args[1:] {
			if strings.EqualFold(a, "store") {
				return Analysis{Kind: KindWrite}
			}
		}
	}

	return Analysis{Kind: KindRead}
}
