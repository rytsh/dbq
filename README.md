# dbq

[![License](https://img.shields.io/github/license/rytsh/dbq?color=red&style=flat-square)](https://raw.githubusercontent.com/rytsh/dbq/main/LICENSE)
[![Coverage](https://img.shields.io/sonar/coverage/rytsh_dbq?logo=sonarcloud&server=https%3A%2F%2Fsonarcloud.io&style=flat-square)](https://sonarcloud.io/summary/overall?id=rytsh_dbq)
[![GitHub Workflow Status](https://img.shields.io/github/actions/workflow/status/rytsh/dbq/test.yml?branch=main&logo=github&style=flat-square&label=ci)](https://github.com/rytsh/dbq/actions)
[![Go PKG](https://raw.githubusercontent.com/rakunlabs/.github/main/assets/badges/gopkg.svg)](https://pkg.go.dev/github.com/rytsh/dbq)

Run SQL against databases from the terminal or from an AI agent.

`dbq` is three things over one core:

- an interactive REPL that executes raw SQL and prints a table;
- an HTTP server with health and liveness probes;
- an **MCP server** so agents like Claude Code, Cursor or Windsurf can inspect
  schemas and query your databases.

Supported drivers: `pgx` (PostgreSQL), `sqlite3`, `sqlserver`, `godror` (Oracle),
`odbc`.

## Run with Docker

Create `~/.config/dbq/dbq.yaml`, then run dbq as a background service that
starts again automatically after a reboot or failure:

```sh
# change port number and config file path as needed
docker run -d --name dbq --restart unless-stopped -p 8080:8080 -v "$HOME/.config/dbq/dbq.yaml:/etc/dbq.yaml:ro" ghcr.io/rytsh/dbq:latest
```

The HTTP server, health probes, and MCP endpoint are then available on port
`8080` (for example, `http://localhost:8080/mcp`).

## Install

Check the [releases page](https://github.com/rytsh/dbq/releases/latest)

```sh
curl -fSL https://github.com/rytsh/dbq/releases/latest/download/dbq_Linux_x86_64.tar.gz | tar -xz --overwrite -C ~/bin/ dbq
```

## Quick start

Ad-hoc, no config file:

```sh
dbq --source 'postgres://user:urlencodedpassword@localhost:5432/postgres?application_name=dbq' --type pgx
```

```
> select 1 as a, 'x' as b;
+---+---+
| a | b |
+---+---+
| 1 | x |
+---+---+
```

Statements are terminated with `;` and may span lines; a `;` inside a string
literal or a comment does not end a statement. Use `-n` to strip the `;` before
it reaches the driver, and `--ping` to just verify the connection and exit.

Scripts and one-liners work the same way. Statements can come from `-e`, from a
file with `-f`, or from a pipe, and the output format is selectable:

```sh
dbq -c prod -e 'select count(*) as n from orders' -o json
dbq -c prod -f report.sql -o csv > report.csv
cat migrate.sql | dbq -c local
```

| `-o`    | Output                                                   |
| ------- | -------------------------------------------------------- |
| `table` | ASCII table, the default                                 |
| `json`  | one array of row objects per statement; a repeated column name such as a second `id` becomes `id_2` |
| `csv`   | header line plus one line per row                        |

When statements come from anything other than a terminal, the first failing
statement stops the run and `dbq` exits non-zero. Truncation notices for
`json` and `csv` go to stderr so stdout stays parseable.

## Configuration

`dbq` reads `dbq.yaml` (or `.toml`/`.json`) from the first matching location:

1. the current directory;
2. the OS user config directory's `dbq/` subdirectory (`$XDG_CONFIG_HOME/dbq/`
   or `~/.config/dbq/` on Linux, `~/Library/Application Support/dbq/` on macOS,
   `%AppData%\\dbq\\` on Windows);
3. `/etc/` (for example `/etc/dbq.yaml`).

The file named by `CONFIG_FILE` / `--config` bypasses this search. Every value
can also be set from the environment with the `DBQ_` prefix, e.g.
`DBQ_SERVER_PORT=9090`.

```yaml
log_level: info

connections:
  local:
    type: pgx
    source: "postgres://user:pass@localhost:5432/postgres"
    description: "local dev database"
    permission: full

  prod:
    type: pgx
    source: "postgres://readonly:pass@prod.internal:5432/app"
    description: "production, reporting replica"
    permission: read-only
    pool:
      max_open: 1 # overrides the global pool block, field by field

  bas:
    disabled: true # retained in config but not exposed or checked
    type: odbc
    dialect: ingres
    source: "DSN=bas"
    description: "Ingres BAS database"
    permission: read-only

pool:
  max_open: 3
  max_idle: 3
  max_lifetime: 15m

server:
  host: "127.0.0.1" # default; use 0.0.0.0 only with authentication/network policy
  port: "8080"
  connection_check_timeout: 10s

mcp:
  max_rows: 200
  export_enabled: true # bulk export is opt-in
  max_export_rows: 100000
  max_export_bytes: 104857600 # 100 MiB
  max_total_export_bytes: 524288000 # 500 MiB across live exports
  max_export_files: 100
  max_concurrent_exports: 2
  export_ttl: 15m
  # public_base_url: "https://dbq.example.com" # absolute download links behind a proxy
  # allowed_origins: ["https://mcp-client.example.com"] # exact browser origins
  endpoints:
    - path: /mcp
      permission: full
      allow: [local, prod]
      export: true
    - path: /mcp/reporting
      permission: read-only
      allow: [prod]
```

Put secrets in the environment rather than the file:

```sh
export DBQ_CONNECTIONS_PROD_SOURCE='postgres://readonly:...@prod.internal:5432/app'
```

The `pool` block sets `max_open`, `max_idle`, `max_lifetime` and
`max_idle_time` for every connection; a connection's own `pool` block overrides
any of them. Zero inherits and a negative value means unlimited (`max_idle`
cannot be unlimited, so a negative value there follows `max_open`). The defaults
are deliberately small: an agent issues queries faster than a person, and dbq
is rarely the only client of a database.

Check what was loaded:

```sh
dbq connections
dbq --connection prod --ping
```

## Permissions

Every connection carries a permission level, and every statement is classified
before it reaches the driver.

| Level        | Allows                                                          |
| ------------ | --------------------------------------------------------------- |
| `read-only`  | `SELECT`, `SHOW`, `EXPLAIN`, `WITH ... SELECT`, …               |
| `safe-write` | the above plus writes whose reach is **bounded**                 |
| `full`       | everything else: DDL, `GRANT`, and writes of unbounded reach      |

Permission depends on blast radius, not just on the verb. `DELETE FROM users
WHERE id = 1` is safe-write; `DELETE FROM users` and `DELETE FROM users WHERE
1=1` are `full`, because "safe-write" should not mean "may empty any table".

A write is treated as unbounded when it has no `WHERE`, when the predicate
cannot narrow anything (`WHERE TRUE`, `WHERE 1=1`, `WHERE id = id`, `WHERE NOT
id <> id`, `WHERE id BETWEEN id AND id`, `WHERE name LIKE '%'`, `WHERE id = 1 OR
id <> 1`, `NOT (id = 1 AND 1 = 2)`, a predicate naming no column at all), when it spans joined tables,
when it contains a subquery, or when it is an `INSERT ... SELECT` in any of its
spellings (`INSERT INTO t (SELECT ...)`, `INSERT INTO t TABLE src`), an upsert,
or a `MERGE`.

Some statements are refused at **every** level:

- **Batches.** `SELECT 1; DROP TABLE users` is rejected. Classifying only the
  first verb would let the rest through, and whether it executes would then
  depend on driver settings — not something a security boundary may rest on.
- **Connection-state statements** (`BEGIN`, `COMMIT`, `USE`, `SET`, `LOCK`),
  and SQLite `PRAGMA` assignments such as `PRAGMA journal_mode = WAL`. dbq runs
  each statement on a pooled connection, so the change would leak into an
  unrelated caller's next query. `PRAGMA table_info(t)`, `PRAGMA journal_mode`
  and the other query-only pragmas remain reads; maintenance pragmas such as
  `PRAGMA wal_checkpoint` or `PRAGMA optimize` need `full`.

### How the classifier reads SQL

It is a lexer, not a parser, but it takes the adversarial cases seriously. It
handles `--` and `#` comments, nested and MySQL executable (`/*!50000 ... */`)
block comments, backslash and doubled-quote string escapes, `"…"` / `` `…` `` /
`[…]` quoted identifiers, and PostgreSQL `$$…$$` dollar quoting — so a verb
hidden inside data cannot be read as code.

Because the same bytes lex differently per engine, every statement is read under
**both** a MySQL-style and a standard-SQL dialect and the more dangerous reading
wins. `SELECT 'a\'; DROP TABLE users; --'` is one string literal to MySQL and
two statements to PostgreSQL, so dbq treats it as a batch and refuses it. The
reverse holds too: MySQL only treats `--` as a comment when whitespace follows,
so `SELECT 1--1; DROP TABLE users` is arithmetic and a second statement there,
and dbq refuses it as a batch.

It also catches statements that look like reads but are not: `SELECT ... FOR
UPDATE` (takes row locks), `SELECT ... INTO OUTFILE` (writes to the server's
filesystem), `SELECT ... INTO newtable`, `EXPLAIN ANALYZE <write>` (executes),
and writable CTEs such as
`WITH x AS (DELETE FROM t RETURNING *) SELECT * FROM x`.

Anything it cannot classify requires `full`. It fails closed.

What it cannot see is what a function does. `SELECT pg_read_file(...)`,
`SELECT lo_import(...)` or `SELECT pg_advisory_lock(1)` are reads to a lexer,
and the last one leaves a lock on a pooled connection. The classifier is a
guard against an agent's mistakes, not a substitute for database privileges:
give each connection a database role that can only do what its dbq permission
level promises. See [Database-side read-only](#database-side-read-only).

### Database-side read-only

> [!WARNING]
> `permission: read-only` is enforced by dbq, not by the database. The
> database only refuses what the connection's user is not allowed to do. For
> any database that matters, connect a `read-only` connection with a user that
> can only read, or point it at a read replica.

Some statements pass the classifier as reads but still change something:

| Database   | Example                                                                                  |
| ---------- | ---------------------------------------------------------------------------------------- |
| PostgreSQL | `SELECT nextval('seq')`, `SELECT set_config(...)`, `SELECT pg_terminate_backend(pid)`, a user-defined function that writes |
| Oracle     | `SELECT f() FROM dual` where `f` writes and commits in an autonomous transaction          |
| SQL Server | a scalar or CLR function with side effects called from a `SELECT`                       |

Only database privileges stop these. Use the strongest option available, from
most to least effective:

1. **A read replica.** It refuses writes no matter who connects.
2. **A user that can only read.** Examples are below.
3. **A read-only setting in the DSN.** Use this as a second layer, not instead
   of 2.

Match the user to the dbq level. A `read-only` connection gets `SELECT` only.
A `safe-write` connection gets `SELECT`, `INSERT`, `UPDATE` and `DELETE` on the
tables it needs, and no DDL. Only `full` should have DDL or ownership rights.
Never connect dbq as a superuser, `db_owner`, `DBA` or the schema owner.

**PostgreSQL**

```sql
CREATE ROLE dbq_ro LOGIN PASSWORD '...';
GRANT CONNECT ON DATABASE app TO dbq_ro;
GRANT USAGE ON SCHEMA public TO dbq_ro;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO dbq_ro;
-- Tables that app_owner (the role that creates your tables) adds later are readable too.
ALTER DEFAULT PRIVILEGES FOR ROLE app_owner IN SCHEMA public
  GRANT SELECT ON TABLES TO dbq_ro;
-- Every transaction this user starts is read-only by default.
ALTER ROLE dbq_ro SET default_transaction_read_only = on;
-- PostgreSQL 14 and earlier let every user create tables in public.
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
```

On PostgreSQL 14+, `GRANT pg_read_all_data TO dbq_ro` gives read access to
every schema at once. Do not grant `pg_read_server_files`,
`pg_execute_server_program` or `pg_signal_backend`.

**SQL Server**

```sql
CREATE LOGIN dbq_ro WITH PASSWORD = '...';
USE app;
CREATE USER dbq_ro FOR LOGIN dbq_ro;
ALTER ROLE db_datareader ADD MEMBER dbq_ro;
-- Optional: also block stored procedures and functions.
DENY EXECUTE TO dbq_ro;
```

**Oracle**

```sql
CREATE USER dbq_ro IDENTIFIED BY "...";
GRANT CREATE SESSION TO dbq_ro;
GRANT SELECT ON app.users TO dbq_ro;  -- per table, or:
GRANT READ ANY TABLE TO dbq_ro;        -- 12c+; unlike SELECT ANY TABLE it cannot lock rows
```

**ODBC targets (e.g. Ingres):** create a user that has only `SELECT` on the
tables dbq should see.

**Read-only settings in the DSN:**

| Driver      | DSN                                                     | Notes |
| ----------- | ------------------------------------------------------- | ----- |
| `pgx`       | `postgres://dbq_ro@host/app?default_transaction_read_only=on` | Can be turned off with `SELECT set_config(...)`, so use it together with a read-only user. |
| `sqlite3`   | `file:./app.db?mode=ro`                                 | SQLite has no users. `mode=ro` opens the file read-only. Also make the file read-only for the dbq process's OS user. |
| `sqlserver` | none                                                    | `ApplicationIntent=ReadOnly` only routes to an Always On secondary. It does not block writes. |
| `godror`    | none                                                    | Use a read-only user. |

A `read-only` connection whose user can write still works, but its safety then
depends entirely on the classifier.

## Server

```sh
dbq server
```

### HTTP probes

| Method | Path                                                                 |
| ------ | -------------------------------------------------------------------- |
| GET    | `/healthz` — pings every connection concurrently, each bounded by `server.connection_check_timeout`; 503 with per-connection detail when one fails |
| GET    | `/livez`                                                             |

Database discovery and queries are exposed only through MCP.

## MCP server

`dbq server` mounts the configured MCP paths. Each path has a permission ceiling and
an optional connection allowlist. With no explicit endpoint configuration,
`/mcp` is mounted with a `read-only` ceiling. Write access must be configured
explicitly.

`dbq` does not authenticate MCP traffic itself. Put it behind your own auth;
separate paths such as `/mcp` and `/mcp/reporting` can receive different
policies upstream. When export is enabled, protect `/exports` with the same
upstream policy. Export IDs are short-lived capabilities, not a replacement for
transport authentication. dbq listens on `127.0.0.1` by default. Set
`server.host: 0.0.0.0` only when remote access is protected by upstream
authentication and network policy. The published Docker image makes this
override automatically through `DBQ_SERVER_HOST=0.0.0.0`.

The ceiling only ever *restricts*. A connection configured as `read-only` stays
read-only even on a `full` endpoint; the effective permission is the lower of
the endpoint and connection permissions. This allows one endpoint to expose a
read-only production database and a full-access local database safely.

Enable more at runtime:

```sh
dbq server
dbq server --mcp=false            # no MCP at all
```

The endpoints are **stateless** by default. dbq carries nothing between tool
calls, so sessions would only add a failure mode: a request that lands on
another replica, or arrives after a restart, is rejected with `session not
found`. Set `mcp.stateless: false` only if a client requires the session
handshake.

### Client config

For a local client, stdio avoids opening a network port and keeps one dbq
process alive for the MCP connection:

```json
{
  "mcpServers": {
    "dbq": {
      "type": "stdio",
      "command": "dbq",
      "args": ["mcp"]
    }
  }
}
```

The stdio command uses the `/mcp` endpoint's permission ceiling and connection
allowlist by default. Select another configured policy with
`dbq mcp --endpoint /mcp/reporting`. Bulk exports are HTTP-only and are not
advertised over stdio because there is no download route.

If the host does not have dbq's native database libraries, run the stdio server
through Docker instead. Keep stdin attached with `-i` and do not allocate a TTY:

```sh
docker run --rm -i \
  -v "$HOME/.config/dbq/dbq.yaml:/etc/dbq.yaml:ro" \
  ghcr.io/rytsh/dbq:latest mcp
```

An MCP client can launch that container directly. Use an absolute host path in
the volume argument because clients do not necessarily expand `$HOME`:

```json
{
  "mcpServers": {
    "dbq": {
      "type": "stdio",
      "command": "docker",
      "args": [
        "run", "--rm", "-i",
        "-v", "/absolute/path/to/dbq.yaml:/etc/dbq.yaml:ro",
        "ghcr.io/rytsh/dbq:latest", "mcp"
      ]
    }
  }
}
```

From a container, `localhost` refers to the container itself. On Docker Desktop,
use `host.docker.internal` in a database DSN to reach a database on the host. On
Linux, either use the host's reachable address or add `--network host`.

For a remote or long-running service, use Streamable HTTP:

```json
{
  "mcpServers": {
    "dbq": {
      "type": "http",
      "url": "http://localhost:8080/mcp"
    }
  }
}
```

### Tools

| Tool                    | Description                                                            |
| ----------------------- | ---------------------------------------------------------------------- |
| `dbq_list_connections`  | List reachable databases with their driver type and permission level   |
| `dbq_list_tables`       | List tables and views, optionally within one schema                    |
| `dbq_describe_table`    | Columns of one table: type, nullability, default, primary key          |
| `dbq_schema_context`    | Compact one-line-per-table schema summary, cheap in tokens             |
| `dbq_query`             | Run one read-only statement; writes are always refused here            |
| `dbq_export`            | Export a SELECT to a downloadable batched-INSERT `.sql` artifact        |
| `dbq_execute`           | Run one modifying statement, subject to the permission level           |

`dbq_execute` is **not advertised** on an endpoint where it could never succeed
— a read-only endpoint, or one whose every visible connection is read-only. A
tool that always fails only invites the model to try it and burn turns on the
refusal.

`dbq_schema_context` is the cheapest way for a model to learn a schema. It emits
one line per table plus foreign keys, which is what lets it write correct JOINs
instead of guessing at key names:

```
users(id INTEGER pk, name TEXT not null, email TEXT)
orders(id INTEGER pk, user_id INTEGER not null)
  fk: orders.user_id -> users.id
active_users(id INTEGER, name TEXT) [VIEW, not writable]
```

`dbq_export` is for moving or saving the rows visible to a `SELECT`. It streams
the result into batched `INSERT` statements on the server and returns only a
small manifest (download URL, row count, size, SHA-256 and expiry) through MCP.
The exported values are never placed in the model context. Download URLs contain
a random capability ID, expire after `mcp.export_ttl`, and are served with
`Content-Disposition: attachment` and `Cache-Control: private, no-store`.

Set `mcp.public_base_url` when callers need an absolute URL through a reverse
proxy; otherwise the tool returns a root-relative `/exports/...` URL. Artifacts
use a private temporary directory by default, or `mcp.export_dir` when set. One
export is bounded by `mcp.max_export_rows`, `mcp.max_export_bytes`, and the normal
`mcp.query_timeout`. The target table and target dialect are explicit, so a
filtered or projected query can be imported under a different table name or SQL
dialect.

Bulk export is disabled unless `mcp.export_enabled` is true. With explicit MCP
endpoints, only endpoints carrying `export: true` advertise the tool. Live
artifacts are also bounded collectively by `mcp.max_total_export_bytes`,
`mcp.max_export_files`, and `mcp.max_concurrent_exports`. Expired files are
removed periodically, generated capability paths are excluded from dbq access
logs, and process-owned temporary exports are deleted during graceful shutdown.

Ask the agent things like:

- "List my dbq connections"
- "Show me the schema of the prod database"
- "How many orders were created in the last seven days?"

Responses are bounded on three axes, because any one of them alone can exhaust a
model's context:

| Setting                 | Default | Bounds                                     |
| ----------------------- | ------- | ------------------------------------------ |
| `mcp.max_rows`          | 200     | rows per call → `truncated`                |
| `mcp.max_cell_chars`    | 500     | characters per value → `cells_truncated`   |
| `mcp.max_schema_tables` | 40      | tables per `dbq_schema_context` call       |
| `mcp.query_timeout`     | 30s     | wall-clock per statement                   |
| `mcp.max_export_rows`   | 100000  | rows per downloadable SQL export           |
| `mcp.max_export_bytes`  | 100 MiB | bytes per downloadable SQL export          |
| `mcp.export_ttl`        | 15m     | lifetime of an export download link        |
| `mcp.max_total_export_bytes` | 500 MiB | bytes across all live exports          |
| `mcp.max_export_files`  | 100     | live downloadable artifacts                |
| `mcp.max_concurrent_exports` | 2   | simultaneous export jobs                   |

The row cap alone is not enough: one `TEXT` column can hold megabytes, so
`SELECT * FROM documents` would blow the window even at a small row limit. The
timeout is what stops an agent-issued cartesian join from pinning a connection
until the client gives up. A tool call may pass `max_rows` or `max_chars` to
ask for less; it can never raise either above the configured cap.

## Docker

```sh
# database client
docker run -it --rm ghcr.io/rytsh/dbq:latest \
  --source 'postgres://user:pass@host:5432/postgres' --type pgx

# background server; see "Run with Docker" above for details
docker run -d --name dbq --restart unless-stopped -p 8080:8080 \
  -v "$HOME/.config/dbq/dbq.yaml:/etc/dbq.yaml:ro" \
  ghcr.io/rytsh/dbq:latest
```

## Building

The `odbc`, `godror` and `sqlite3` drivers are cgo, so `CGO_ENABLED=1` and the
unixODBC headers are required:

```sh
# macOS
brew install unixodbc
export CGO_CFLAGS="-I/opt/homebrew/include" CGO_LDFLAGS="-L/opt/homebrew/lib"

# Debian/Ubuntu
sudo apt-get install -y unixodbc-dev

make build
make test
make lint   # golangci-lint, configured in .golangci.yml; CI fails on findings
```
