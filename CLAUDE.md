# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

rqloud is a Go library that combines Tailscale (tsnet) networking with rqlite (distributed SQLite) into a self-contained replicated application platform. Applications get a `database/sql` or native gorqlite interface backed by a replicated database, with all inter-node communication over Tailscale.

## Workspace Structure

This repo lives in a go workspace (`~/projects/rqloud-workspace/`):
- `rqloud/` — this library + examples
- `rqlite/` — local rqlite with a small patch (Listener field on http.Service)

The `go.work` file at the workspace root ties them together.

## Build Commands

```bash
# Build (CGO required for rqlite's go-sqlite3 fork)
CGO_ENABLED=1 CC=clang go build ./...

# Build the todo example
CGO_ENABLED=1 CC=clang go build ./examples/todo/

# Run vet
CGO_ENABLED=1 CC=clang go vet ./...

# Tidy deps
go mod tidy
```

CGO is required because rqlite uses a fork of go-sqlite3 (`github.com/rqlite/go-sqlite3`) that needs a C compiler. Use `CC=clang` since gcc may not be available.

## Architecture

### Core: `rqloud.Server`

The `Server` struct in `rqloud.go` wires together:
1. **tsnet.Server** — embedded Tailscale node
2. **rqlite store.Store** — Raft-backed SQLite (embedded, not subprocess)
3. **rqlite http.Service** — rqlite HTTP API, served on tsnet and localhost

Networking flow:
- One tsnet listener on port 4002 feeds a `tcp.Mux` that demuxes Raft (header byte 1) and cluster (header byte 2) traffic
- rqlite HTTP API listens on tsnet port 4001 (for remote access) and `127.0.0.1:0` (for local database/sql driver)
- Application traffic uses its own tsnet listener (e.g., `:80`)

### Key files

- `rqloud.go` — Server struct, Start/Close, DB/Gorqlite/Listen/WhoIs methods
- `layer.go` — `tsnetDialer` (writes mux header byte over tsnet) and `tsnetRaftLayer` (implements `store.Layer` interface)
- `examples/todo/main.go` — Demo per-user todo app

### Database access patterns

- `srv.DB()` — returns `*sql.DB` via gorqlite/stdlib, connects through localhost listener
- `srv.Gorqlite()` — returns native `*gorqlite.Connection`, connects through tsnet HTTP client (`OpenWithClient`)
- `srv.WhoIs(r)` — identifies Tailscale caller from HTTP request

### rqlite patch

The only change to rqlite is adding a `Listener net.Listener` field to `http.Service` in `http/service.go`. When set, `Start()` uses it instead of calling `net.Listen`. This lets us pass tsnet listeners.

## Dependencies requiring replace directives

- `github.com/armon/go-metrics` → `github.com/hashicorp/go-metrics` (module rename)
- `github.com/mattn/go-sqlite3` → `github.com/rqlite/go-sqlite3` (rqlite's fork, in rqlite's go.mod)
