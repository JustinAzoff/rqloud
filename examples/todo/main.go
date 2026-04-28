// Command todo is a demo rqloud application: a per-user todo list served over tsnet.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rqloud/rqloud"
)

var tmpl = template.Must(template.New("page").Parse(`<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>rqloud todo</title>
<style>
body { font-family: sans-serif; max-width: 600px; margin: 40px auto; padding: 0 20px; }
.completed { text-decoration: line-through; color: #888; }
form.inline { display: inline; }
ul { list-style: none; padding: 0; }
li { padding: 6px 0; display: flex; align-items: center; gap: 8px; }
input[type=text] { flex: 1; padding: 6px; font-size: 14px; }
button { padding: 6px 12px; cursor: pointer; }
.add-form { display: flex; gap: 8px; margin-bottom: 20px; }
a { color: #0066cc; }
</style>
</head>
<body>
<h2>todos for {{.User}}</h2>
<form method="POST" action="/add" class="add-form">
  <input type="text" name="title" placeholder="What needs to be done?" autofocus>
  <button type="submit">Add</button>
</form>
<p>
  {{if .ShowCompleted}}
    <a href="/?show=active">Hide completed</a>
  {{else}}
    <a href="/?show=all">Show completed</a>
  {{end}}
</p>
<ul>
{{range .Todos}}
  <li>
    <form method="POST" action="/toggle" class="inline">
      <input type="hidden" name="id" value="{{.ID}}">
      <button type="submit">{{if .Completed}}☑{{else}}☐{{end}}</button>
    </form>
    <span {{if .Completed}}class="completed"{{end}}>{{.Title}}</span>
  </li>
{{else}}
  <li>No todos yet.</li>
{{end}}
</ul>
</body>
</html>
`))

type Todo struct {
	ID        int64
	Title     string
	Completed bool
}

func main() {
	instance := flag.String("instance", "todo", "tsnet hostname for this instance")
	dataDir := flag.String("data-dir", "", "data directory (default: auto based on instance name)")
	verbose := flag.Bool("verbose", false, "enable verbose logging")
	flag.Parse()

	srv := &rqloud.Server{
		Hostname: *instance,
		Dir:      *dataDir,
		Verbose:  *verbose,
	}

	log.Println("starting rqloud...")
	if err := srv.Start(); err != nil {
		log.Fatalf("start: %v", err)
	}
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := srv.Wait(ctx); err != nil {
		log.Fatalf("wait: %v", err)
	}

	db, err := srv.DB()
	if err != nil {
		log.Fatalf("db: %v", err)
	}

	if err := initSchema(db); err != nil {
		log.Fatalf("schema: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		handleIndex(w, r, srv, db)
	})
	mux.HandleFunc("/add", func(w http.ResponseWriter, r *http.Request) {
		handleAdd(w, r, srv, db)
	})
	mux.HandleFunc("/toggle", func(w http.ResponseWriter, r *http.Request) {
		handleToggle(w, r, srv, db)
	})

	ln, err := srv.Listen("tcp", ":80")
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Println("todo app listening on tsnet :80")

	go http.Serve(ln, mux)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Println("shutting down")
}

func initSchema(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS todos (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		owner TEXT NOT NULL,
		title TEXT NOT NULL,
		completed BOOLEAN NOT NULL DEFAULT 0
	)`)
	return err
}

func getUser(srv *rqloud.Server, r *http.Request) string {
	who, err := srv.WhoIs(r)
	if err != nil {
		return "unknown"
	}
	return who.UserProfile.LoginName
}

func handleIndex(w http.ResponseWriter, r *http.Request, srv *rqloud.Server, db *sql.DB) {
	user := getUser(srv, r)
	showCompleted := r.URL.Query().Get("show") == "all"

	query := "SELECT id, title, completed FROM todos WHERE owner = ?"
	if !showCompleted {
		query += " AND completed = 0"
	}
	query += " ORDER BY id DESC"

	rows, err := db.Query(query, user)
	if err != nil {
		http.Error(w, fmt.Sprintf("query: %v", err), 500)
		return
	}
	defer rows.Close()

	var todos []Todo
	for rows.Next() {
		var t Todo
		if err := rows.Scan(&t.ID, &t.Title, &t.Completed); err != nil {
			http.Error(w, fmt.Sprintf("scan: %v", err), 500)
			return
		}
		todos = append(todos, t)
	}

	data := struct {
		User          string
		Todos         []Todo
		ShowCompleted bool
	}{user, todos, showCompleted}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tmpl.Execute(w, data)
}

func handleAdd(w http.ResponseWriter, r *http.Request, srv *rqloud.Server, db *sql.DB) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	user := getUser(srv, r)
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if _, err := db.Exec("INSERT INTO todos (owner, title) VALUES (?, ?)", user, title); err != nil {
		http.Error(w, fmt.Sprintf("insert: %v", err), 500)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func handleToggle(w http.ResponseWriter, r *http.Request, srv *rqloud.Server, db *sql.DB) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	user := getUser(srv, r)
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if _, err := db.Exec("UPDATE todos SET completed = NOT completed WHERE id = ? AND owner = ?", id, user); err != nil {
		http.Error(w, fmt.Sprintf("update: %v", err), 500)
		return
	}
	referer := r.Header.Get("Referer")
	if referer == "" {
		referer = "/"
	}
	http.Redirect(w, r, referer, http.StatusSeeOther)
}
