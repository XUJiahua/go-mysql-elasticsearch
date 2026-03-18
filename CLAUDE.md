# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

go-mysql-elasticsearch syncs MySQL data into Elasticsearch. It does an initial full dump via `mysqldump`, then continuously streams changes via MySQL binlog (row format required).

## Build & Test Commands

```bash
make build          # Compiles to bin/go-mysql-elasticsearch
make test           # Runs all tests with race detector (go test -timeout 1m --race ./...)
make clean          # Removes build artifacts
make image          # Builds and pushes Docker image
```

Run a single test:
```bash
go test -timeout 1m --race -run TestName ./river/
```

Run the binary:
```bash
./bin/go-mysql-elasticsearch -config=./etc/river.toml
```

## Architecture

**Event-driven pipeline:** MySQL binlog events → rule-based transformation → batched Elasticsearch bulk operations → position persistence.

### Key Packages

- **`cmd/go-mysql-elasticsearch/`** — Entry point. Parses CLI flags, loads TOML config, creates a `River`, handles OS signals for graceful shutdown.
- **`river/`** — Core logic:
  - `river.go` — `River` struct orchestrates everything: creates the MySQL canal, manages rules, runs the sync loop.
  - `sync.go` — Event handler (`eventHandler`) processes INSERT/UPDATE/DELETE binlog events, converts rows to `BulkRequest`s, batches them via a buffered channel (4096), flushes on bulk_size or timer.
  - `config.go` — TOML config struct (`Config`).
  - `rule.go` — `Rule` defines MySQL table → ES index mapping with field renaming, type modifiers (`list`, `date`, `text`), and field filtering.
  - `master.go` — Persists binlog position to `data_dir/master.info` (TOML format, debounced to max once/sec).
  - `metrics.go` — Prometheus counters for inserts/updates/deletes per index, canal state, and replication delay.
- **`elastic/`** — Custom HTTP-based Elasticsearch client supporting bulk ops, HTTPS with self-signed certs, and basic auth.

### Data Flow for Row Events

1. `eventHandler.OnRow()` receives binlog event
2. Looks up `Rule` by schema:table
3. Extracts document ID from primary key(s) (multi-column PKs joined with `:`)
4. Applies field mapping and type conversions (ENUM→string, SET→string, BIT→int, JSON→object, DATETIME→RFC3339)
5. Creates `BulkRequest` and sends to `syncCh` channel
6. `syncLoop()` batches requests, flushes to ES when bulk_size (default 128) reached or flush interval (default 200ms) expires
7. Saves binlog position after successful flush

### Configuration

TOML-based config (`etc/river.toml`). Key settings:
- `[[source]]` blocks define which schemas/tables to sync (supports regex wildcards like `t_[0-9]{4}`)
- `[[rule]]` blocks map MySQL tables to ES indexes with field mappings, type modifiers, parent-child support, and ingest pipelines
- CLI flags and env vars (`MYSQL_PASSWORD`, `ES_PASSWORD`) can override config values

## Testing

Tests use the `pingcap/check` framework (gocheck-style). Integration tests require running MySQL and Elasticsearch instances. Test addresses configurable via flags:
```bash
go test ./river/ -my_addr=127.0.0.1:3306 -es_addr=127.0.0.1:9200
```

## Constraints

- MySQL must use **row-based binlog** with **full row image**
- All synced tables must have a primary key
- Table schema cannot be altered at runtime while syncing
- Elasticsearch mappings should be created manually before syncing
