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
	"strings"
	"time"

	"github.com/rqlite/gorqlite"
	"github.com/rqlite/rqlite/v10/cluster"
	command "github.com/rqlite/rqlite/v10/command/proto"
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
	// Only used when rqloud creates its own tsnet (i.e. not NewWithTSNet).
	AuthKey string

	// AdvertiseTags is a list of ACL tags to advertise (e.g. "tag:todo").
	// Only used when rqloud creates its own tsnet (i.e. not NewWithTSNet).
	AdvertiseTags []string

	// BootstrapExpect is the number of nodes expected to form the initial
	// cluster. When set, nodes use the notify/bootstrap protocol to
	// coordinate simultaneous startup. When 0 (default), the first node
	// bootstraps solo and others join it.
	BootstrapExpect int

	// RaftHeartbeat controls the Raft heartbeat timeout. All other Raft
	// timeouts (election, lease, commit) are scaled proportionally from
	// Raft's default ratios. Higher values reduce idle traffic but
	// increase failover time. Defaults to 3s (Raft default is 1s).
	RaftHeartbeat time.Duration

	// Verbose enables verbose tsnet logging.
	Verbose bool

	ownsTS      bool
	ts          *tsnet.Server
	store       *store.Store
	httpService *httpd.Service
	clstrServ   *cluster.Service
	mux         *tcp.Mux
	muxLn       net.Listener

	driverName string
	db         *sql.DB
	grqConn    *gorqlite.Connection

	logger *log.Logger
}

// New creates a new rqloud Server that will create and manage its own
// tsnet node. Call Start() to begin.
func New() *Server {
	return &Server{}
}

// NewWithTSNet creates a new rqloud Server using an existing tsnet.Server.
// The caller is responsible for the tsnet lifecycle (starting it before
// calling Start, closing it after calling Close). Hostname is derived
// from the tsnet server.
func NewWithTSNet(ts *tsnet.Server) *Server {
	return &Server{
		ts:       ts,
		Hostname: ts.Hostname,
	}
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

	if s.ts == nil {
		// Create and start our own tsnet node.
		s.ownsTS = true
		s.ts = &tsnet.Server{
			Hostname:      s.Hostname,
			Dir:           filepath.Join(s.Dir, "tsnet"),
			AuthKey:       s.AuthKey,
			ControlURL:    os.Getenv("RQLOUD_CONTROL_URL"),
			AdvertiseTags: s.AdvertiseTags,
		}
		if s.Verbose {
			s.ts.Logf = s.logger.Printf
		}
		if err := s.ts.Start(); err != nil {
			return fmt.Errorf("tsnet start: %w", err)
		}
		s.logger.Println("tsnet started, waiting for tailnet...")
		if err := s.waitForTailnet(5 * time.Minute); err != nil {
			return fmt.Errorf("tailnet: %w", err)
		}
	} else {
		// Using caller-provided tsnet; derive hostname if not set.
		if s.Hostname == "" {
			s.Hostname = s.ts.Hostname
		}
		s.logger.Println("using external tsnet, waiting for tailnet...")
		if err := s.waitForTailnet(5 * time.Minute); err != nil {
			return fmt.Errorf("tailnet: %w", err)
		}
	}

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
		addr:   NewAddr(s.Hostname, defaultMuxPort),
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
	// Skip address resolution — tsnet handles name resolution internally.
	str.ResolveAddress = func(addr string) (string, error) {
		h, _, err := net.SplitHostPort(addr)
		if err != nil {
			h = addr
		}
		return h, nil
	}

	// Scale all Raft timeouts proportionally from the heartbeat.
	// Raft defaults: heartbeat=1s, election=1s, lease=500ms, commit=50ms.
	heartbeat := s.RaftHeartbeat
	if heartbeat == 0 {
		heartbeat = 3 * time.Second
	}
	scale := float64(heartbeat) / float64(time.Second) // ratio vs Raft's 1s default
	str.HeartbeatTimeout = heartbeat
	str.ElectionTimeout = heartbeat
	str.LeaderLeaseTimeout = time.Duration(float64(500*time.Millisecond) * scale)
	str.CommitTimeout = time.Duration(float64(50*time.Millisecond) * scale)
	if s.Verbose {
		str.RaftLogLevel = "DEBUG"
	}

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

	// Register a database/sql driver that uses tsnet's HTTP client.
	s.driverName = registerDriver(s.ts.HTTPClient())

	// Open the store.
	if err := str.Open(); err != nil {
		return fmt.Errorf("store open: %w", err)
	}

	// Determine cluster membership after opening (Raft state is now loaded).
	nodes, err := str.Nodes()
	if err != nil {
		return fmt.Errorf("get nodes: %w", err)
	}
	hasPeers := len(nodes) > 0

	if hasPeers {
		// Existing node with Raft state. Raft reconnects to known peers automatically.
		s.logger.Printf("existing Raft state with %d node(s), rejoining cluster", len(nodes))
	} else if clusterPrefix(s.Hostname) == "" {
		// Standalone instance (no hyphen in hostname), bootstrap solo immediately.
		s.logger.Println("standalone instance, bootstrapping new cluster")
		if err := str.Bootstrap(store.NewServer(nodeID, raftAddr, true)); err != nil {
			return fmt.Errorf("bootstrap: %w", err)
		}
	} else {
		// New node with a cluster prefix. Use the Bootstrapper to try joining
		// an existing cluster or forming one with simultaneously-starting nodes.
		if s.BootstrapExpect > 0 {
			str.BootstrapExpect = s.BootstrapExpect
		}
		provider := &tailnetAddressProvider{srv: s}
		bs := cluster.NewBootstrapper(provider, clstrClient)
		bootDone := func() bool {
			leader, _ := str.LeaderAddr()
			return leader != ""
		}
		if err := bs.Boot(context.Background(), nodeID, raftAddr, command.Suffrage_VOTER, bootDone, 120*time.Second); err != nil {
			return fmt.Errorf("bootstrap: %w", err)
		}
	}

	// Wait for the store to be fully ready (leader elected, all channels
	// registered, etc.) before returning so callers can use the database.
	s.logger.Println("waiting for store to be ready...")
	deadline := time.Now().Add(2 * time.Minute)
	for !str.Ready() {
		if time.Now().After(deadline) {
			return fmt.Errorf("store did not become ready within timeout")
		}
		time.Sleep(100 * time.Millisecond)
	}
	leader, _ := str.LeaderAddr()
	s.logger.Printf("store ready, leader: %s", leader)

	// Tell the user the node is ready for HTTP, giving some advice on how to connect.
	s.logger.Printf("connect using the command-line tool via 'rqlite -H %s -p %d'", s.Hostname, defaultHTTPPort)
	s.logger.Printf("visit the rqlite console for this node at http://%s/console/", net.JoinHostPort(s.Hostname, strconv.Itoa(defaultHTTPPort)))

	if s.Verbose {
		go s.periodicStatus()
	}

	return nil
}

func (s *Server) periodicStatus() {
	for {
		time.Sleep(15 * time.Second)
		// Tailnet status.
		if lc, err := s.ts.LocalClient(); err == nil {
			if st, err := lc.Status(context.Background()); err == nil {
				var peerNames []string
				for _, p := range st.Peer {
					status := "offline"
					if p.Online {
						status = "online"
					}
					peerNames = append(peerNames, fmt.Sprintf("%s(%s)", p.HostName, status))
				}
				s.logger.Printf("[status] tailnet peers: %v", peerNames)
			}
		}
		// Store status.
		if s.store != nil {
			leader, _ := s.store.LeaderAddr()
			nodes, _ := s.store.Nodes()
			var nodeIDs []string
			for _, n := range nodes {
				nodeIDs = append(nodeIDs, n.ID)
			}
			s.logger.Printf("[status] leader=%s nodes=%v ready=%v", leader, nodeIDs, s.store.Ready())
		}
	}
}

// tailnetAddressProvider implements cluster.AddressProvider by discovering
// rqloud peers on the tailnet that share our hostname prefix.
type tailnetAddressProvider struct {
	srv *Server
}

func (p *tailnetAddressProvider) Lookup() ([]string, error) {
	prefix := clusterPrefix(p.srv.Hostname)
	if prefix == "" {
		return nil, nil
	}

	lc, err := p.srv.ts.LocalClient()
	if err != nil {
		return nil, fmt.Errorf("get local client: %w", err)
	}
	st, err := lc.Status(context.Background())
	if err != nil {
		return nil, fmt.Errorf("get status: %w", err)
	}

	// Include ourselves so that the notify protocol counts this node too.
	// (BootstrapExpect requires N notifications including self.)
	peers := []string{net.JoinHostPort(p.srv.Hostname, strconv.Itoa(defaultMuxPort))}
	for _, peer := range st.Peer {
		if !peer.Online {
			continue
		}
		if strings.HasPrefix(peer.HostName, prefix) {
			peers = append(peers, net.JoinHostPort(peer.HostName, strconv.Itoa(defaultMuxPort)))
		}
	}
	if len(peers) > 1 {
		p.srv.logger.Printf("discovered peers: %v", peers)
	}
	return peers, nil
}

// clusterPrefix extracts the cluster name prefix from a hostname.
// "todo-1" → "todo-", "myapp-node-3" → "myapp-node-".
// Returns "" if there is no hyphen (standalone instance, no clustering).
func clusterPrefix(hostname string) string {
	i := strings.LastIndex(hostname, "-")
	if i < 0 {
		return ""
	}
	return hostname[:i+1]
}

func (s *Server) waitForTailnet(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	lc, err := s.ts.LocalClient()
	if err != nil {
		return fmt.Errorf("get local client: %w", err)
	}

	for {
		status, err := lc.Status(ctx)
		if err != nil {
			return fmt.Errorf("get status: %w", err)
		}
		if status.CurrentTailnet != nil {
			s.logger.Printf("connected to tailnet %s", status.CurrentTailnet.Name)
			return nil
		}
		s.logger.Println("waiting for tailnet...")
		select {
		case <-ctx.Done():
			return fmt.Errorf("tailscale did not become ready: %w", ctx.Err())
		case <-time.After(1 * time.Second):
		}
	}
}

// Close shuts down the server.
func (s *Server) Close() error {
	if s.db != nil {
		s.db.Close()
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
	if s.ownsTS && s.ts != nil {
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
// Uses a custom driver that routes all HTTP traffic through tsnet.
func (s *Server) DB() (*sql.DB, error) {
	if s.db != nil {
		return s.db, nil
	}
	url := fmt.Sprintf("http://%s:%d/", s.Hostname, defaultHTTPPort)
	db, err := sql.Open(s.driverName, url)
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

// TS returns ths
func (s *Server) TS() *tsnet.Server {
	return s.ts
}
