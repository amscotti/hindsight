package main

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"hindsight/db"
	"hindsight/handlers"
	"hindsight/realtime"
)

//go:embed static
var staticFiles embed.FS

// dbPath resolves the SQLite file location. Only this path is ever
// read from or written to disk at runtime; everything else served by
// the binary comes from embedded bytes. DB_PATH should be absolute in
// production; the SQLite file holds secret hashes, so its directory is
// created with owner-only permissions.
func dbPath() string {
	if p := os.Getenv("DB_PATH"); p != "" {
		return p
	}
	return "./data/hindsight.db"
}

// openBoards opens the database (running embedded migrations), loads
// the HMAC server secret, and wires the board routes. The caller owns
// the returned handle and must Close it when the handler is done.
func openBoards() (*handlers.Boards, *sql.DB, error) {
	path := dbPath()
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, nil, fmt.Errorf("create db dir: %w", err)
		}
	}
	sqldb, secret, err := db.Open(path)
	if err != nil {
		return nil, nil, err
	}
	broker := realtime.NewBroker()
	return handlers.NewBoards(db.NewStore(sqldb), secret, brokerPublisher{broker: broker}, broker), sqldb, nil
}

// brokerPublisher adapts the realtime broker to the handlers.Publisher
// seam. The broker assigns the per-board id and never fails, so Publish
// always reports success. It lives here because only package main may
// depend on the concrete broker type.
type brokerPublisher struct {
	broker *realtime.Broker
}

// Publish implements handlers.Publisher.
func (p brokerPublisher) Publish(boardID, name, html string) error {
	p.broker.Publish(boardID, name, html)
	return nil
}

// newHandler wires the HTTP routes. Static assets come from the embedded
// filesystem, so the binary serves without any sibling files on disk.
// The caller owns the returned DB handle and must Close it on shutdown.
func newHandler() (http.Handler, *sql.DB, error) {
	assets, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return nil, nil, fmt.Errorf("embedded static dir: %w", err)
	}
	boards, sqldb, err := openBoards()
	if err != nil {
		return nil, nil, err
	}

	// Restarts drop the in-memory timer fires, so re-arm future timers
	// from their persisted instants before serving.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := boards.RestoreArmedTimers(ctx); err != nil {
		_ = sqldb.Close()
		return nil, nil, fmt.Errorf("restore timers: %w", err)
	}

	// The mutation limiter bounds how fast one client may issue any
	// state-changing request. Exhausted clients get
	// 429 plus the flash fragment. Every wiring builds its own
	// limiter so handler tests stay isolated from each other.
	mutations := handlers.NewMutationLimiter()

	mux := http.NewServeMux()
	mux.Handle("GET /static/", noCache(noDirListing(http.StripPrefix("/static/", http.FileServerFS(assets)))))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		boards.Dashboard(w, r)
	})
	mux.HandleFunc("GET /new", boards.New)
	mux.HandleFunc("POST /boards", mutations.Limit(boards.Create))
	mux.HandleFunc("GET /b/{bid}", boards.Show)
	mux.HandleFunc("POST /b/{bid}/join", mutations.Limit(boards.Join))
	mux.HandleFunc("POST /boards/{bid}/archive", mutations.Limit(boards.Archive))
	mux.HandleFunc("POST /boards/{bid}/unarchive", mutations.Limit(boards.Unarchive))
	mux.HandleFunc("POST /boards/{bid}/duplicate", mutations.Limit(boards.Duplicate))
	mux.HandleFunc("POST /boards/{bid}/regenerate", mutations.Limit(boards.Regenerate))
	mux.HandleFunc("GET /b/{bid}/events", boards.Events)
	mux.HandleFunc("POST /b/{bid}/cards", mutations.Limit(boards.CreateCard))
	mux.HandleFunc("PUT /cards/{cid}", mutations.Limit(boards.UpdateCard))
	mux.HandleFunc("DELETE /cards/{cid}", mutations.Limit(boards.DeleteCard))
	mux.HandleFunc("POST /cards/{cid}/vote", mutations.Limit(boards.Vote))
	mux.HandleFunc("POST /cards/{cid}/unvote", mutations.Limit(boards.Unvote))
	mux.HandleFunc("POST /b/{bid}/phase", mutations.Limit(boards.SetPhase))
	mux.HandleFunc("POST /b/{bid}/reveal", mutations.Limit(boards.Reveal))
	mux.HandleFunc("POST /b/{bid}/lock", mutations.Limit(boards.Lock))
	mux.HandleFunc("POST /b/{bid}/timer", mutations.Limit(boards.Timer))
	mux.HandleFunc("POST /b/{bid}/focus", mutations.Limit(boards.Focus))
	mux.HandleFunc("POST /b/{bid}/focus/next", mutations.Limit(boards.FocusNext))
	mux.HandleFunc("POST /b/{bid}/focus/prev", mutations.Limit(boards.FocusPrev))
	mux.HandleFunc("POST /b/{bid}/sort", mutations.Limit(boards.Sort))
	mux.HandleFunc("POST /cards/{cid}/comments", mutations.Limit(boards.CreateComment))
	mux.HandleFunc("POST /b/{bid}/kudos", mutations.Limit(boards.CreateKudo))
	mux.HandleFunc("DELETE /kudos/{kid}", mutations.Limit(boards.DeleteKudo))
	mux.HandleFunc("POST /b/{bid}/actions", mutations.Limit(boards.CreateAction))
	mux.HandleFunc("PUT /actions/{aid}", mutations.Limit(boards.UpdateAction))
	mux.HandleFunc("DELETE /actions/{aid}", mutations.Limit(boards.DeleteAction))
	mux.HandleFunc("GET /b/{bid}/export.md", boards.Export)
	return mux, sqldb, nil
}

// noDirListing rejects directory paths so the embedded file server
// never renders auto-generated listings.
func noDirListing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// noCache stops browsers from keeping a stale app.css after a rebuild.
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func port() string {
	if p := os.Getenv("PORT"); p != "" {
		return p
	}
	return "8080"
}

func main() {
	addr := ":" + port()
	handler, sqldb, err := newHandler()
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		log.Printf("hindsight listening on http://127.0.0.1%s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal(err)
	}
	if err := sqldb.Close(); err != nil {
		log.Fatal(err)
	}
}
