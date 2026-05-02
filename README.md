# rqloud

rqloud combines [Tailscale](https://tailscale.com/) (tsnet) networking with [rqlite](https://rqlite.io/) (distributed SQLite) into a self-contained replicated application platform.

Your application gets a `database/sql` or native [gorqlite](https://github.com/rqlite/gorqlite) interface backed by a Raft-replicated SQLite database, with all inter-node communication happening over your Tailscale network.

For a real-world example, see [JustinAzoff/golink](https://github.com/JustinAzoff/golink/tree/rqloud), a fork of [tailscale/golink](https://github.com/tailscale/golink) using rqloud instead of a local SQLite database.

## Features

- Embedded rqlite — no separate database process to manage
- Tailscale networking — all traffic (Raft consensus, cluster coordination, HTTP API) runs over tsnet, with caller identity via `WhoIs()`
- Automatic clustering — nodes discover each other by hostname prefix and auto-join
- Two database interfaces — `database/sql` for standard Go code, or native gorqlite for rqlite-specific features

## Standalone Binary

`cmd/rqloud` runs a bare rqlite cluster over Tailscale with no application code on top. Use it to deploy a replicated SQLite database accessible via the standard `rqlite` CLI or HTTP API.

```bash
CGO_ENABLED=1 go build -o rqloud ./cmd/rqloud/
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

Or open the rqlite web console at `http://mydb-1:4001/`.

To access rqlite from outside of the tailnet, use `-local-rqlite-bind`:

```bash
./rqloud -instance mydb-1 -local-rqlite-bind 127.0.0.1:4001 /tmp/rqloud-test/mydb-1
rqlite  # connects to localhost:4001
```

This is useful for local tooling, monitoring, or applications that don't run on the tailnet.

## Quick Start (Library)

```go
package main

import (
    "flag"
    "fmt"
    "log"
    "net/http"

    "github.com/JustinAzoff/rqloud"
)

func main() {
    flag.Usage = func() {
        fmt.Fprintf(flag.CommandLine.Output(), "Usage: hitcount [hostname]\n\n")
        fmt.Fprintf(flag.CommandLine.Output(), "Use \"hitcount\" for a single node, or \"hitcount-1\", \"hitcount-2\", etc. for a cluster.\n\n")
        flag.PrintDefaults()
    }
    flag.Parse()

    hostname := "hitcount"
    if flag.NArg() > 0 {
        hostname = flag.Arg(0)
    }

    srv := &rqloud.Server{
        Hostname: hostname,
    }
    if err := srv.Start(); err != nil {
        log.Fatal(err)
    }
    defer srv.Close()

    db, _ := srv.DB()
    db.Exec(`CREATE TABLE IF NOT EXISTS hits (count INTEGER)`)
    db.Exec(`INSERT INTO hits (count) SELECT 0 WHERE NOT EXISTS (SELECT 1 FROM hits)`)

    ln, _ := srv.Listen("tcp", ":80")
    log.Printf("listening on http://%s/", hostname)
    http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        db.Exec(`UPDATE hits SET count = count + 1`)
        var count int
        db.QueryRow(`SELECT count FROM hits`).Scan(&count)
        fmt.Fprintf(w, "hits: %d\n", count)
    }))
}
```

## Clustering

Nodes discover peers automatically by hostname prefix. Name your instances with a shared prefix and a unique suffix separated by a hyphen:

```
myapp-1
myapp-2
myapp-3
```

<!-- TODO: investigate tag-based peer discovery as an alternative to hostname prefix -->

The first node to start bootstraps a new single-node cluster. Subsequent nodes discover existing peers on the tailnet and join automatically.

A standalone instance (hostname with no hyphen, e.g. `myapp`) runs as a single non-clustered node.

## API

### Constructors

```go
// Create a server that manages its own tsnet node.
srv := rqloud.New()
srv.Hostname = "myapp-1"

// Or use an existing tsnet.Server.
srv := rqloud.NewWithTSNet(existingTS)
```

### `rqloud.Server`

```go
srv := &rqloud.Server{
    Hostname:      "myapp-1",         // tsnet hostname (required)
    Dir:           "/data/myapp-1",   // rqlite data directory (default: ~/.config/rqloud/<hostname>)
    TSDir:         "",                // tsnet config directory (default: ~/.config/tsnet-<hostname>)
    AuthKey:       "tskey-...",       // Tailscale auth key (default: interactive login)
    AdvertiseTags: []string{"tag:myapp"},
    Verbose:       false,
}
```

| Method | Returns | Description |
|--------|---------|-------------|
| `Start()` | `error` | Initialize tsnet, wait for tailnet, start rqlite store and HTTP API |
| `Close()` | `error` | Graceful shutdown |
| `Up(ctx)` | `error` | Wait for the tsnet node to connect to the tailnet |
| `Listen(net, addr)` | `net.Listener, error` | Listen on the tsnet interface (for your app's traffic) |
| `ListenService(name, mode)` | `*tsnet.ServiceListener, error` | Register a Tailscale Service listener |
| `DB()` | `*sql.DB, error` | Standard database/sql handle |
| `Gorqlite()` | `*gorqlite.Connection, error` | Native gorqlite connection (uses tsnet HTTP client) |
| `LocalListen(net, addr)` | `net.Listener, error` | Listen on a normal network interface |
| `WhoIs(r)` | `*apitype.WhoIsResponse, error` | Identify the Tailscale caller from an HTTP request |
| `TS()` | `*tsnet.Server` | Access the underlying tsnet server |

## Example: Todo App

See [`examples/todo/`](examples/todo/) for a complete per-user todo list application.

```bash
CGO_ENABLED=1 go build -o rqloud-todo ./examples/todo/
./rqloud-todo -instance todo-1
```

Start a second instance to form a cluster:

```bash
./rqloud-todo -instance todo-2
```

They'll discover each other on the tailnet and replicate automatically.

## Building

CGO is required (rqlite uses a fork of go-sqlite3):

```bash
CGO_ENABLED=1 go build ./...
```

## rqlite Fork

rqloud depends on a [fork of rqlite](https://github.com/JustinAzoff/rqlite) with two small patches:

- `http/service.go` — `Listener` field so `Start()` accepts an external `net.Listener` (for tsnet)
- `store/store.go` — `ResolveAddress` hook to override DNS resolution (tsnet handles its own name resolution)

These are tracked as separate branches (`custom-http-listener`, `custom-dns`) merged into the `justin-integration` branch, which `go.mod` references via a `replace` directive.
