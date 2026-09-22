package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"doables/internal/store"
	"doables/internal/web"
)

func main() {
	addr := flag.String("addr", "localhost:8080", "address to listen on")
	dbPath := flag.String("db", "doables.db", "path to the SQLite database file")
	flag.Parse()

	s, err := store.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()

	// Ctrl-C, or the TERM a container runtime sends, cancels this context.
	// Every request's context descends from it (see BaseContext), so cancelling
	// it also ends the /events streams, which are meant to last forever and
	// would otherwise hold the shutdown open until it gave up on them.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:        *addr,
		Handler:     web.New(s),
		BaseContext: func(net.Listener) context.Context { return ctx },
		// Enough that nobody can hold a connection open by dribbling out a
		// request, without cutting anyone off mid-request.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout on purpose: it covers the whole response, and a
		// server-sent event stream is a response that never ends.
	}

	go func() {
		<-ctx.Done()
		log.Print("shutting down")
		quit, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(quit); err != nil {
			log.Printf("some connections were cut short: %v", err)
		}
	}()

	log.Printf("listening on http://%s (db: %s)", *addr, *dbPath)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	log.Print("stopped")
}
