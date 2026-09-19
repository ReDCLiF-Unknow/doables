package main

import (
	"flag"
	"log"
	"net/http"

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

	log.Printf("listening on http://%s (db: %s)", *addr, *dbPath)
	log.Fatal(http.ListenAndServe(*addr, web.New(s)))
}
