# Query

Connect to database and execute a query.

> WARNING: This is raw query execution and testing tool. It is not checking query safety or performance. Use it with caution.

## Usage

Execute `query` command with `--source` and `--type` flags.

```
Usage:
  query [flags]

Examples:
query --source 'postgres://user:urlencodedpassword@localhost:5432/postgres?application_name=query&search_path=myschema' --type pgx

Flags:
      --echo             echo test input
  -h, --help             help for query
  -n, --no-delimeter ;   not include delimeter ;
      --ping             ping database and exit
      --source string    db data source
      --type string      db data source type, supported types: [pgx, odbc, godror, sqlite3, sqlserver]
  -v, --version          version for query
```

With docker

```sh
docker run -it --rm --name query ghcr.io/rytsh/query:latest
```
