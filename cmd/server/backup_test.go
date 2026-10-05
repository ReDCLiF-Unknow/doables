package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"doables/internal/store"
)

// "-backup -" writes the database to standard output, which is how a
// container is backed up in one command, and says what is in it on standard
// error, so that the file and the message do not get mixed up.
func TestBackupToStandardOutput(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "doables.db")
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() // still running while it is backed up
	u, _, _ := s.CreateUser("Alex")
	s.CreateList("Weekly shop", u.ID)

	var out, msg bytes.Buffer
	if err := runBackup(db, "-", &out, &msg); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(out.Bytes(), []byte("SQLite format 3\x00")) {
		t.Fatalf("standard output is not a database (%d bytes)", out.Len())
	}
	if !strings.Contains(msg.String(), "1 people, 1 lists") {
		t.Errorf("the message says %q", msg.String())
	}

	// What came out is a database Doables can open.
	restored := filepath.Join(dir, "restored.db")
	os.WriteFile(restored, out.Bytes(), 0o600)
	r, err := store.Open(restored)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if lists, _ := r.Lists(u.ID); len(lists) != 1 || lists[0].Name != "Weekly shop" {
		t.Errorf("the restored database has %+v", lists)
	}

	// To a named file, the message says where it went.
	msg.Reset()
	if err := runBackup(db, filepath.Join(dir, "named.db"), &out, &msg); err != nil || !strings.Contains(msg.String(), "named.db") {
		t.Errorf("a named backup: %v, %q", err, msg.String())
	}
}
