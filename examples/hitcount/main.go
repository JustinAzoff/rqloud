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
