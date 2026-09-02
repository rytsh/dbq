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

## Install

```sh
curl -fSL https://github.com/rytsh/dbq/releases/latest/download/dbq_Linux_x86_64.tar.gz \
  | tar -xz --overwrite -C ~/bin/ dbq
```

Or pull the container image, `ghcr.io/rytsh/dbq`.

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

Statements are terminated with `;`. Use `-n` to strip the `;` before it reaches
the driver, and `--ping` to just verify the connection and exit.

## Configuration

`dbq` reads `dbq.yaml` (or `.toml`/`.json`) from the current directory, or the
file named by `CONFIG_FILE` / `--config`. Every value can also be set from the
environment with the `DBQ_` prefix, e.g. `DBQ_SERVER_PORT=9090`.

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

  bas:
    disabled: true # retained in config but not exposed or checked
    type: odbc
    dialect: ingres
    source: "DSN=bas"
    description: "Ingres BAS database"
    permission: read-only

server:
  host: ""
  port: "8080"
  connection_check_timeout: 10s

mcp:
  max_rows: 200
  endpoints:
    - path: /mcp
      permission: full
      allow: [local, prod]
    - path: /mcp/reporting
      permission: read-only
      allow: [prod]
```

Put secrets in the environment rather than the file:

```sh
export DBQ_CONNECTIONS_PROD_SOURCE='postgres://readonly:...@prod.internal:5432/app'
```

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
cannot narrow anything (`WHERE TRUE`, `WHERE 1=1`, `WHERE id = id`, `WHERE name
LIKE '%'`, `WHERE id = 1 OR id <> 1`, a predicate naming no column at all), when
it spans joined tables, when it contains a subquery, or when it is an
`INSERT ... SELECT`, an upsert, or a `MERGE`.

Some statements are refused at **every** level:

- **Batches.** `SELECT 1; DROP TABLE users` is rejected. Classifying only the
  first verb would let the rest through, and whether it executes would then
  depend on driver settings — not something a security boundary may rest on.
- **Connection-state statements** (`BEGIN`, `COMMIT`, `USE`, `SET`, `LOCK`).
  dbq runs each statement on a pooled connection, so the change would leak into
  an unrelated caller's next query.

### How the classifier reads SQL

It is a lexer, not a parser, but it takes the adversarial cases seriously. It
handles `--` and `#` comments, nested and MySQL executable (`/*!50000 ... */`)
block comments, backslash and doubled-quote string escapes, `"…"` / `` `…` `` /
`[…]` quoted identifiers, and PostgreSQL `$$…$$` dollar quoting — so a verb
hidden inside data cannot be read as code.

Because the same bytes lex differently per engine, every statement is read under
**both** a MySQL-style and a standard-SQL dialect and the more dangerous reading
wins. `SELECT 'a\'; DROP TABLE users; --'` is one string literal to MySQL and
two statements to PostgreSQL, so dbq treats it as a batch and refuses it.

It also catches statements that look like reads but are not: `SELECT ... FOR
UPDATE` (takes row locks), `SELECT ... INTO OUTFILE` (writes to the server's
filesystem), `SELECT ... INTO newtable`, `EXPLAIN ANALYZE <write>` (executes),
and writable CTEs such as
`WITH x AS (DELETE FROM t RETURNING *) SELECT * FROM x`.

Anything it cannot classify requires `full`. It fails closed.

## Server

```sh
dbq server
```

### HTTP probes

| Method | Path                                |
| ------ | ----------------------------------- |
| GET    | `/healthz` — pings every connection |
| GET    | `/livez`                            |

Database discovery and queries are exposed only through MCP.

## MCP server

`dbq` mounts the configured MCP paths. Each path has a permission ceiling and
an optional connection allowlist. With no explicit endpoint configuration,
`/mcp` is mounted with a `full` ceiling.

`dbq` does not authenticate MCP traffic itself. Put it behind your own auth;
separate paths such as `/mcp` and `/mcp/reporting` can receive different
policies upstream.

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

The row cap alone is not enough: one `TEXT` column can hold megabytes, so
`SELECT * FROM documents` would blow the window even at a small row limit. The
timeout is what stops an agent-issued cartesian join from pinning a connection
until the client gives up.

## Docker

```sh
# database client shell
docker run -it --rm ghcr.io/rytsh/dbq:latest

# server
docker run --rm -p 8080:8080 -v $PWD/dbq.yaml:/dbq.yaml \
  -e CONFIG_FILE=/dbq.yaml ghcr.io/rytsh/dbq:latest dbq server
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
```
