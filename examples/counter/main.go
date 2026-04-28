// Command counter is a minimal rqloud example: a replicated counter with
// increment/decrement buttons, useful for integration testing.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/rqloud/rqloud"
)

const page = `<!DOCTYPE html>
<html>
<head><meta charset="utf-8"><title>counter</title>
<style>
body { font-family: sans-serif; text-align: center; margin-top: 80px; }
h1 { font-size: 96px; margin: 20px; }
button { font-size: 32px; padding: 10px 30px; margin: 5px; cursor: pointer; }
</style>
</head>
<body>
<h1>%d</h1>
<form method="POST" style="display:inline"><button name="action" value="dec">-</button></form>
<form method="POST" style="display:inline"><button name="action" value="inc">+</button></form>
</body>
</html>
`

func main() {
	instance := flag.String("instance", "counter", "tsnet hostname for this instance")
	dataDir := flag.String("data-dir", "", "data directory (default: auto)")
	verbose := flag.Bool("verbose", false, "enable verbose logging")
	flag.Parse()

	srv := &rqloud.Server{
		Hostname: *instance,
		Dir:      *dataDir,
		Verbose:  *verbose,
	}

	if err := srv.Start(); err != nil {
		log.Fatalf("start: %v", err)
	}
	defer srv.Close()

	db, err := srv.DB()
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	if err := initSchema(db); err != nil {
		log.Fatalf("schema: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			handlePost(w, r, db)
			return
		}
		handleGet(w, r, db)
	})
	mux.HandleFunc("/value", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "%d", getCounter(db))
	})

	// Listen on tsnet for tailnet access.
	tsLn, err := srv.Listen("tcp", ":80")
	if err != nil {
		log.Fatalf("tsnet listen: %v", err)
	}
	log.Println("counter listening on tsnet :80")
	go http.Serve(tsLn, mux)

	// Listen on localhost:8080 for local/test access.
	localLn, err := net.Listen("tcp", ":8080")
	if err != nil {
		log.Fatalf("local listen: %v", err)
	}
	log.Println("counter listening on :8080")
	go http.Serve(localLn, mux)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Println("shutting down")
}

func initSchema(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS counter (value INTEGER NOT NULL)`)
	if err != nil {
		return err
	}
	// Insert the initial row if it doesn't exist.
	_, err = db.Exec(`INSERT INTO counter (value) SELECT 0 WHERE NOT EXISTS (SELECT 1 FROM counter)`)
	return err
}

func getCounter(db *sql.DB) int {
	var v int
	db.QueryRow("SELECT value FROM counter").Scan(&v)
	return v
}

func handleGet(w http.ResponseWriter, r *http.Request, db *sql.DB) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, page, getCounter(db))
}

func handlePost(w http.ResponseWriter, r *http.Request, db *sql.DB) {
	switch r.FormValue("action") {
	case "inc":
		db.Exec("UPDATE counter SET value = value + 1")
	case "dec":
		db.Exec("UPDATE counter SET value = value - 1")
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
