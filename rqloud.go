// Package rqloud provides a self-contained replicated application platform
// combining Tailscale (tsnet) networking with rqlite distributed SQLite.
package rqloud

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/rqlite/gorqlite"
	_ "github.com/rqlite/gorqlite/stdlib"
	"github.com/rqlite/rqlite/v10/cluster"
	httpd "github.com/rqlite/rqlite/v10/http"
	"github.com/rqlite/rqlite/v10/proxy"
	"github.com/rqlite/rqlite/v10/store"
	"github.com/rqlite/rqlite/v10/tcp"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tsnet"
)

const (
	defaultMuxPort  = 4002 // Internode mux (Raft + cluster)
	defaultHTTPPort = 4001 // rqlite HTTP API (tsnet)
)

// Server is the main rqloud server. It manages a tsnet node, an embedded
// rqlite store, and provides database access over the tailnet.
type Server struct {
	// Hostname is the tsnet hostname for this node.
	Hostname string

	// Dir is the base directory for all state (tsnet + rqlite data).
	// Defaults to a directory based on Hostname in os.UserConfigDir().
	Dir string

	// AuthKey is the Tailscale auth key. If empty, interactive login is used.
	AuthKey string

	// AdvertiseTags is a list of ACL tags to advertise (e.g. "tag:todo").
	AdvertiseTags []string

	// Verbose enables verbose tsnet logging.
	Verbose bool

	ts          *tsnet.Server
	store       *store.Store
	httpService *httpd.Service
	clstrServ   *cluster.Service
	mux         *tcp.Mux
	muxLn       net.Listener

	// localHTTPLn is a localhost-only listener for the rqlite HTTP API,
	// used by the database/sql driver which can't use a custom HTTP client.
	localHTTPLn  net.Listener
	localHTTPSrv *httpd.Service

	db      *sql.DB
	grqConn *gorqlite.Connection

	logger *log.Logger
}

// Start initializes and starts the tsnet node, rqlite store, and HTTP API.
func (s *Server) Start() error {
	s.logger = log.New(os.Stderr, fmt.Sprintf("[rqloud:%s] ", s.Hostname), log.LstdFlags)

	if s.Dir == "" {
		configDir, err := os.UserConfigDir()
		if err != nil {
			return fmt.Errorf("get config dir: %w", err)
		}
		s.Dir = filepath.Join(configDir, "rqloud", s.Hostname)
	}
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}

	// Start tsnet.
	s.ts = &tsnet.Server{
		Hostname:      s.Hostname,
		Dir:           filepath.Join(s.Dir, "tsnet"),
		AuthKey:       s.AuthKey,
		AdvertiseTags: s.AdvertiseTags,
	}
	if s.Verbose {
		s.ts.Logf = s.logger.Printf
	}
	if err := s.ts.Start(); err != nil {
		return fmt.Errorf("tsnet start: %w", err)
	}
	s.logger.Println("tsnet started")

	// Listen on the mux port for internode traffic (Raft + cluster).
	muxLn, err := s.ts.Listen("tcp", fmt.Sprintf(":%d", defaultMuxPort))
	if err != nil {
		return fmt.Errorf("listen mux port: %w", err)
	}
	s.muxLn = muxLn

	mux, err := tcp.NewMux(muxLn, nil)
	if err != nil {
		return fmt.Errorf("create mux: %w", err)
	}
	s.mux = mux
	go mux.Serve()

	// Create Raft layer: mux sub-listener + tsnet dialer with Raft header.
	raftLn := mux.Listen(cluster.MuxRaftHeader)
	raftLayer := &tsnetRaftLayer{
		ln:     raftLn,
		dialer: &tsnetDialer{srv: s.ts, header: cluster.MuxRaftHeader},
	}

	// Create the rqlite store.
	nodeID := s.Hostname
	raftAddr := net.JoinHostPort(s.Hostname, strconv.Itoa(defaultMuxPort))
	httpAddr := net.JoinHostPort(s.Hostname, strconv.Itoa(defaultHTTPPort))

	str := store.New(&store.Config{
		DBConf: store.NewDBConfig(),
		Dir:    filepath.Join(s.Dir, "rqlite"),
		ID:     nodeID,
	}, raftLayer)
	s.store = str

	// Create cluster service for internode communication.
	clstrLn := mux.Listen(cluster.MuxClusterHeader)
	clstrServ := cluster.New(clstrLn, str, str, nil)
	clstrServ.SetAPIAddr(httpAddr)
	if err := clstrServ.Open(); err != nil {
		return fmt.Errorf("cluster service open: %w", err)
	}
	s.clstrServ = clstrServ

	// Create cluster client with tsnet dialer.
	clstrClient := cluster.NewClient(
		&tsnetDialer{srv: s.ts, header: cluster.MuxClusterHeader},
		30*time.Second,
	)
	if err := clstrClient.SetLocal(raftAddr, clstrServ); err != nil {
		return fmt.Errorf("set cluster client local: %w", err)
	}

	// Create proxy and HTTP service on tsnet.
	pxy := proxy.New(str, clstrClient)
	pxy.SetAPIAddr(httpAddr)

	httpLn, err := s.ts.Listen("tcp", fmt.Sprintf(":%d", defaultHTTPPort))
	if err != nil {
		return fmt.Errorf("listen http port: %w", err)
	}

	httpServ := httpd.New("", str, clstrClient, pxy, nil)
	httpServ.Listener = httpLn
	if err := httpServ.Start(); err != nil {
		return fmt.Errorf("http service start: %w", err)
	}
	s.httpService = httpServ
	s.logger.Printf("rqlite HTTP API on tsnet %s:%d", s.Hostname, defaultHTTPPort)

	// Also start a localhost-only HTTP API for the database/sql driver,
	// which can't use a custom HTTP client. Bind to :0 for a random port.
	localLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen local http: %w", err)
	}
	s.localHTTPLn = localLn

	localHTTPServ := httpd.New("", str, clstrClient, pxy, nil)
	localHTTPServ.Listener = localLn
	if err := localHTTPServ.Start(); err != nil {
		return fmt.Errorf("local http service start: %w", err)
	}
	s.localHTTPSrv = localHTTPServ
	s.logger.Printf("rqlite local HTTP API on %s", localLn.Addr())

	isNew := store.IsNewNode(str.Path())

	// Open the store and bootstrap if new.
	if err := str.Open(); err != nil {
		return fmt.Errorf("store open: %w", err)
	}
	if isNew {
		s.logger.Println("bootstrapping single new node")
		if err := str.Bootstrap(store.NewServer(nodeID, raftAddr, true)); err != nil {
			return fmt.Errorf("bootstrap: %w", err)
		}
	}
	s.logger.Println("rqlite store ready")

	// Tell the user the node is ready for HTTP, giving some advice on how to connect.
	s.logger.Printf("connect using the command-line tool via 'rqlite -H %s -p %d'", s.Hostname, defaultHTTPPort)
	s.logger.Printf("visit the rqlite console for this node at http://%s/console/", net.JoinHostPort(s.Hostname, strconv.Itoa(defaultHTTPPort)))

	return nil
}

func (s *Server) Wait(maxWait time.Duration) error {
	deadline := time.Now().Add(maxWait)

	lc, err := s.ts.LocalClient()
	if err != nil {
		log.Fatal(err)
	}

	for time.Now().Before(deadline) {
		status, err := lc.Status(context.TODO())
		if err != nil {
			return fmt.Errorf("Status: %w", err)
		}
		s.logger.Printf("CurrentTailnet: %v", status.CurrentTailnet)
		if status.CurrentTailnet != nil {
			return nil
		}
		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("tailscale did not become ready")
}

// Close shuts down the server.
func (s *Server) Close() error {
	if s.db != nil {
		s.db.Close()
	}
	if s.localHTTPSrv != nil {
		s.localHTTPSrv.Close()
	}
	if s.httpService != nil {
		s.httpService.Close()
	}
	if s.clstrServ != nil {
		s.clstrServ.Close()
	}
	if s.muxLn != nil {
		s.muxLn.Close()
	}
	if s.store != nil {
		s.store.Close(true)
	}
	if s.ts != nil {
		s.ts.Close()
	}
	return nil
}

// Listen returns a net.Listener on the tsnet interface.
func (s *Server) Listen(network, addr string) (net.Listener, error) {
	return s.ts.Listen(network, addr)
}

// LocalListen returns a net.Listener on a normal network interface.
func (s *Server) LocalListen(network, addr string) (net.Listener, error) {
	return net.Listen(network, addr)
}

// DB returns a database/sql handle connected to the local rqlite node.
// Uses a localhost-only HTTP listener since gorqlite/stdlib doesn't support
// custom HTTP clients.
func (s *Server) DB() (*sql.DB, error) {
	if s.db != nil {
		return s.db, nil
	}
	addr := s.localHTTPLn.Addr().String()
	db, err := sql.Open("rqlite", fmt.Sprintf("http://%s/", addr))
	if err != nil {
		return nil, fmt.Errorf("open rqlite db: %w", err)
	}
	s.db = db
	return db, nil
}

// Gorqlite returns a native gorqlite connection to the local rqlite node.
// Uses tsnet's HTTP client so all traffic stays on the tailnet.
func (s *Server) Gorqlite() (*gorqlite.Connection, error) {
	if s.grqConn != nil {
		return s.grqConn, nil
	}
	url := fmt.Sprintf("http://%s:%d/", s.Hostname, defaultHTTPPort)
	conn, err := gorqlite.OpenWithClient(url, s.ts.HTTPClient())
	if err != nil {
		return nil, fmt.Errorf("open gorqlite: %w", err)
	}
	s.grqConn = conn
	return s.grqConn, nil
}

// WhoIs returns the Tailscale identity of the caller for the given HTTP request.
func (s *Server) WhoIs(r *http.Request) (*apitype.WhoIsResponse, error) {
	lc, err := s.ts.LocalClient()
	if err != nil {
		return nil, fmt.Errorf("get local client: %w", err)
	}
	return lc.WhoIs(r.Context(), r.RemoteAddr)
}
