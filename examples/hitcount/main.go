package main

import (
	"fmt"
	"log"
	"net/http"

	"github.com/JustinAzoff/rqloud"
)

func main() {
	// Use "hitcount" for a single node, or "hitcount-1", "hitcount-2", etc. for a cluster.
	srv := &rqloud.Server{
		Hostname: "hitcount",
	}
	if err := srv.Start(); err != nil {
		log.Fatal(err)
	}
	defer srv.Close()

	db, _ := srv.DB()
	db.Exec(`CREATE TABLE IF NOT EXISTS hits (count INTEGER)`)
	db.Exec(`INSERT INTO hits (count) SELECT 0 WHERE NOT EXISTS (SELECT 1 FROM hits)`)

	ln, _ := srv.Listen("tcp", ":80")
	http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		db.Exec(`UPDATE hits SET count = count + 1`)
		var count int
		db.QueryRow(`SELECT count FROM hits`).Scan(&count)
		fmt.Fprintf(w, "hits: %d\n", count)
	}))
}
