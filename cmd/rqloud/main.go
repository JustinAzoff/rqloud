// Command rqloud runs a standalone rqlite cluster over Tailscale.
//
// It starts an embedded rqlite node with all inter-node communication
// (Raft consensus, cluster coordination, HTTP API) running over a tsnet
// Tailscale network. Nodes discover each other automatically by hostname
// prefix — name instances like "db-1", "db-2", "db-3" and they will
// form a replicated cluster.
//
// The rqlite HTTP API is available on the tailnet at port 4001, so you
// can connect with the standard rqlite CLI:
//
//	rqlite -H db-1 -p 4001
//
// For local (non-tailnet) access, use -local-rqlite-bind to expose the
// rqlite HTTP API on a local address:
//
//	rqloud -instance db-1 -local-rqlite-bind 127.0.0.1:4001
//
// This starts a reverse proxy on localhost that forwards requests to the
// internal rqlite HTTP API over tsnet.
//
// Usage:
//
//	rqloud [flags]
//
// Flags:
//
//	-instance string        tsnet hostname for this node (required)
//	-data-dir string        data directory (default: ~/.config/rqloud/<instance>)
//	-bootstrap-expect int   number of nodes expected to form initial cluster
//	-auth-key string        Tailscale auth key (default: interactive login)
//	-raft-heartbeat duration Raft heartbeat interval (default: 3s)
//	-local-rqlite-bind string local address to expose rqlite HTTP API (default: off)
//	-verbose                enable verbose logging
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"syscall"

	"github.com/rqloud/rqloud"
)

func main() {
	instance := flag.String("instance", "", "tsnet hostname for this node (required)")
	dataDir := flag.String("data-dir", "", "data directory (default: auto)")
	bootstrapExpect := flag.Int("bootstrap-expect", 0, "number of nodes expected to form initial cluster")
	authKey := flag.String("auth-key", "", "Tailscale auth key (default: interactive login)")
	raftHeartbeat := flag.Duration("raft-heartbeat", 0, "Raft heartbeat interval (default: 3s)")
	localBind := flag.String("local-rqlite-bind", "", "local address to expose rqlite HTTP API (e.g. 127.0.0.1:4001)")
	verbose := flag.Bool("verbose", false, "enable verbose logging")
	flag.Parse()

	if *instance == "" {
		fmt.Fprintln(os.Stderr, "error: -instance is required")
		flag.Usage()
		os.Exit(1)
	}

	srv := &rqloud.Server{
		Hostname:        *instance,
		Dir:             *dataDir,
		AuthKey:         *authKey,
		BootstrapExpect: *bootstrapExpect,
		RaftHeartbeat:   *raftHeartbeat,
		Verbose:         *verbose,
	}

	if err := srv.Start(); err != nil {
		log.Fatalf("start: %v", err)
	}
	defer srv.Close()

	if *localBind != "" {
		target, _ := url.Parse(fmt.Sprintf("http://%s:4001/", *instance))
		proxy := httputil.NewSingleHostReverseProxy(target)
		proxy.Transport = srv.TS().HTTPClient().Transport

		ln, err := net.Listen("tcp", *localBind)
		if err != nil {
			log.Fatalf("local rqlite bind: %v", err)
		}
		log.Printf("rqlite HTTP API available at http://%s/", *localBind)
		go http.Serve(ln, proxy)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Println("shutting down")
}
