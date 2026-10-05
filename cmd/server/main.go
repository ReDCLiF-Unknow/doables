package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"doables/internal/demo"
	"doables/internal/push"
	"doables/internal/store"
	"doables/internal/web"
)

// pushDefault reads DOABLES_PUSH: notifications can be turned on by anyone
// using the server unless it says off.
func pushDefault(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "off", "0", "false", "no":
		return false
	}
	return true
}

func splitList(v string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' }) {
		out = append(out, f)
	}
	return out
}

// runBackup copies the database at dbPath to dst, or to stdout when dst is
// "-", and says what is in the copy on stderr, which keeps stdout for the
// file itself:
//
//	docker exec doables doables-server -db /data/doables.db -backup - > doables-backup.db
func runBackup(dbPath, dst string, stdout, stderr io.Writer) error {
	if dst != "-" {
		sum, err := store.Backup(dbPath, dst)
		if err != nil {
			return err
		}
		fmt.Fprintf(stderr, "backed up %s to %s\n", sum, dst)
		return nil
	}
	dir, err := os.MkdirTemp("", "doables-backup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	tmp := filepath.Join(dir, "backup.db")
	sum, err := store.Backup(dbPath, tmp)
	if err != nil {
		return err
	}
	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(stdout, f); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "backed up %s\n", sum)
	return nil
}

func main() {
	addr := flag.String("addr", "localhost:8080", "address to listen on")
	dbPath := flag.String("db", "doables.db", "path to the SQLite database file")
	demoMode := flag.Bool("demo", os.Getenv("DOABLES_DEMO") != "",
		"run as a public demo: every visitor gets example lists of their own, erased a day later (env DOABLES_DEMO)")
	notifications := flag.Bool("push", pushDefault(os.Getenv("DOABLES_PUSH")),
		"let people turn on notifications, sent through their browser's push service (env DOABLES_PUSH=off to disable)")
	backup := flag.String("backup", "",
		"write a copy of the database to this file and exit, or - for standard output; safe while the server is running")
	flag.Parse()

	if *backup != "" {
		if err := runBackup(*dbPath, *backup, os.Stdout, os.Stderr); err != nil {
			log.Fatalf("backup: %v", err)
		}
		return
	}

	s, err := store.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close() // only after serve has returned, so no request is still using it

	// Ctrl-C, or the TERM a container runtime sends.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app := web.New(s)
	if *demoMode {
		if err := demo.Prepare(s); err != nil {
			s.Close()
			log.Fatalf("refusing to start in demo mode: %v", err) // non-zero, so a supervisor notices
		}
		app.SetDemo(func(u store.User) (int64, error) { return demo.Seed(s, u) })
		go demo.Sweep(ctx, s, demo.Lifetime, 10*time.Minute)
		log.Printf("demo mode: every visitor gets example lists, erased %.0f hours after they start", demo.Lifetime.Hours())
	}
	// In a demo the other people are pretend, so there is nobody to notify.
	if *notifications && !*demoMode {
		p, err := push.New(s, os.Getenv("DOABLES_PUSH_CONTACT"), splitList(os.Getenv("DOABLES_PUSH_HOSTS")))
		if err != nil {
			s.Close()
			log.Fatalf("notifications: %v", err)
		}
		go p.Run(ctx)
		app.SetPush(p)
	}
	srv := &http.Server{
		Handler: app,
		// Enough that nobody can hold a connection open by dribbling out a
		// request, without cutting anyone off mid-request.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout on purpose: it covers the whole response, and a
		// server-sent event stream is a response that never ends.
	}
	// Those streams never finish by themselves, so shutting down ends them
	// explicitly; every other request is left to finish.
	srv.RegisterOnShutdown(app.CloseStreams)

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("listening on http://%s (db: %s)", *addr, *dbPath)
	if err := serve(ctx, srv, ln); err != nil {
		log.Print(err)
		return // through the deferred Close, which log.Fatal would skip
	}
	log.Print("stopped")
}

// serve runs srv on ln until ctx is cancelled, then shuts it down and returns
// only once the requests already in progress have finished, or ten seconds
// have passed. Returning any sooner would let main close the database under
// requests that are still using it.
func serve(ctx context.Context, srv *http.Server, ln net.Listener) error {
	drained := make(chan error, 1)
	go func() {
		<-ctx.Done()
		log.Print("shutting down")
		quit, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		drained <- srv.Shutdown(quit)
	}()

	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	// Serve returns as soon as Shutdown closes the listener. The requests that
	// were running are only done when Shutdown itself returns.
	if err := <-drained; err != nil {
		log.Printf("some requests were cut short: %v", err)
	}
	return nil
}
