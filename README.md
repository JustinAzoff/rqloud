# rqloud

rqloud combines [Tailscale](https://tailscale.com/) (tsnet) networking with [rqlite](https://rqlite.io/) (distributed SQLite) into a self-contained replicated application platform.

Your application gets a `database/sql` or native [gorqlite](https://github.com/rqlite/gorqlite) interface backed by a Raft-replicated SQLite database, with all inter-node communication happening over your Tailscale network.

## Features

- **Embedded rqlite** — no separate database process to manage
- **Tailscale networking** — all traffic (Raft consensus, cluster coordination, HTTP API) runs over tsnet
- **Automatic clustering** — nodes discover each other by hostname prefix and auto-join
- **User identity** — `WhoIs()` identifies callers by their Tailscale identity
- **Two database interfaces** — `database/sql` for standard Go code, or native gorqlite for rqlite-specific features

## Quick Start

```go
package main

import (
    "log"
    "github.com/rqloud/rqloud"
)

func main() {
    srv := &rqloud.Server{
        Hostname: "myapp-1",
    }
    if err := srv.Start(); err != nil {
        log.Fatal(err)
    }
    defer srv.Close()

    db, err := srv.DB()
    if err != nil {
        log.Fatal(err)
    }

    db.Exec(`CREATE TABLE IF NOT EXISTS kv (key TEXT PRIMARY KEY, value TEXT)`)
    db.Exec(`INSERT INTO kv (key, value) VALUES (?, ?)`, "hello", "world")
}
```

## Clustering

Nodes discover peers automatically by hostname prefix. Name your instances with a shared prefix and a unique suffix separated by a hyphen:

```
myapp-1
myapp-2
myapp-3
```

The first node to start bootstraps a new single-node cluster. Subsequent nodes discover existing peers on the tailnet and join automatically.

A standalone instance (hostname with no hyphen, e.g. `myapp`) runs as a single non-clustered node.

## API

### `rqloud.Server`

```go
srv := &rqloud.Server{
    Hostname:      "myapp-1",        // tsnet hostname (required)
    Dir:           "/data/myapp-1",  // state directory (default: ~/.config/rqloud/<hostname>)
    AuthKey:       "tskey-...",      // Tailscale auth key (default: interactive login)
    AdvertiseTags: []string{"tag:myapp"},
    Verbose:       false,
}
```

**Methods:**

| Method | Returns | Description |
|--------|---------|-------------|
| `Start()` | `error` | Initialize tsnet, wait for tailnet, start rqlite store and HTTP API |
| `Close()` | `error` | Graceful shutdown |
| `DB()` | `*sql.DB, error` | Standard database/sql handle |
| `Gorqlite()` | `*gorqlite.Connection, error` | Native gorqlite connection (uses tsnet HTTP client) |
| `Listen(net, addr)` | `net.Listener, error` | Listen on the tsnet interface (for your app's traffic) |
| `LocalListen(net, addr)` | `net.Listener, error` | Listen on a normal network interface |
| `WhoIs(r)` | `*apitype.WhoIsResponse, error` | Identify the Tailscale caller from an HTTP request |

## Architecture

```
                    tsnet
                 ┌──────────────────────────────┐
  App traffic    │  :80    your HTTP handlers    │
                 │                               │
  rqlite HTTP    │  :4001  rqlite HTTP API       │
                 │         (database/sql + CLI)   │
                 │                               │
  Raft + cluster │  :4002  tcp.Mux               │
                 │          ├─ byte 1: Raft       │
                 │          └─ byte 2: cluster    │
                 └──────────────────────────────┘
```

- **Port 4002** carries multiplexed Raft and cluster traffic over tsnet, using rqlite's `tcp.Mux` protocol
- **Port 4001** serves the rqlite HTTP API on tsnet (accessible via `rqlite -H <hostname> -p 4001`). Both `DB()` and `Gorqlite()` connect through tsnet to this port using a custom `database/sql` driver
- Your application listens on whatever port you choose (e.g. `:80`)

All traffic — application, database, Raft consensus — stays on the tailnet.

## Standalone Binary

`cmd/rqloud` is a standalone binary that runs a bare rqlite cluster over Tailscale with no application code on top. Use it to deploy a replicated SQLite database accessible via the standard `rqlite` CLI or HTTP API.

```bash
CGO_ENABLED=1 CC=clang go build -o rqloud ./cmd/rqloud/
```

Start a single node:

```bash
./rqloud -instance mydb /tmp/rqloud-test/mydb
```

Start a 3-node cluster:

```bash
./rqloud -instance mydb-1 -bootstrap-expect 3 /tmp/rqloud-test/mydb-1
./rqloud -instance mydb-2 -bootstrap-expect 3 /tmp/rqloud-test/mydb-2
./rqloud -instance mydb-3 -bootstrap-expect 3 /tmp/rqloud-test/mydb-3
```

The three nodes can all run on the same machine or on three different machines — since all communication happens over the tailnet, it doesn't matter where they are.

Connect via the `rqlite` CLI over the tailnet:

```bash
rqlite -H mydb-1 -p 4001
```

To access rqlite from localhost without the tailnet, use `-local-rqlite-bind`:

```bash
./rqloud -instance mydb-1 -local-rqlite-bind 127.0.0.1:4001 /tmp/rqloud-test/mydb-1
rqlite  # connects to localhost:4001
```

This is useful for local tooling, monitoring, or applications that don't run on the tailnet.

## Example: Todo App

See [`examples/todo/`](examples/todo/) for a complete per-user todo list application.

```bash
CGO_ENABLED=1 CC=clang go build -o todo ./examples/todo/
./todo -instance todo-1
```

Start a second instance to form a cluster:

```bash
./todo -instance todo-2
```

They'll discover each other on the tailnet and replicate automatically.

## Building

CGO is required (rqlite uses a fork of go-sqlite3):

```bash
CGO_ENABLED=1 CC=clang go build ./...
```

## rqlite Patch

rqloud requires a small patch to rqlite: a `Listener net.Listener` field on `http.Service` so that `Start()` uses an external listener instead of calling `net.Listen`. This lets us pass in tsnet listeners. Use a [Go workspace](https://go.dev/ref/mod#workspaces) to develop with a local rqlite copy:

```
rqloud-workspace/
├── go.work
├── rqloud/
└── rqlite/    # with Listener field patch
```
