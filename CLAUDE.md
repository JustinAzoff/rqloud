# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

See `README.md` for project overview, API documentation, architecture, and usage examples.

## Build Commands

```bash
# Build everything
CGO_ENABLED=1 CC=clang go build ./...

# Run vet
CGO_ENABLED=1 CC=clang go vet ./...

# Build with nix
nix build .#rqloud

# Run NixOS integration test
nix build .#checks.x86_64-linux.integration -L
```

CGO is required because rqlite uses a fork of go-sqlite3 that needs a C compiler.

## Key files

- `rqloud.go` — Server struct, Start/Close, constructors (New, NewWithTSNet), DB/Gorqlite/Listen/WhoIs/Up methods
- `layer.go` — `tsnetDialer` (writes mux header byte over tsnet) and `tsnetRaftLayer` (implements `store.Layer` interface)
- `driver.go` — registers a `database/sql` driver that routes through tsnet's HTTP client
- `cmd/rqloud/main.go` — Standalone rqloud binary
- `examples/` — hitcount, counter, todo demo apps
- `flake.nix` — Nix build (produces rqloud, rqloud-counter, rqloud-todo)
- `test.nix` — NixOS VM integration test for counter example

## Version control

This project uses jj (Jujutsu), not git directly. Use `jj` commands for commits, bookmarks, etc. Do not add co-author lines to commit messages — especially not to commits for changes that were made by the user, not by Claude.
