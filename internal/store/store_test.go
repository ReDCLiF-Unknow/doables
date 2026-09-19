package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// A database created before sharing existed must upgrade in place: old lists
// become public, keep their tasks, and gain an invite code.
func TestOpenUpgradesPreSharingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE lists (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL)`,
		`CREATE TABLE tasks (id INTEGER PRIMARY KEY AUTOINCREMENT,
			list_id INTEGER NOT NULL REFERENCES lists(id) ON DELETE CASCADE,
			title TEXT NOT NULL, description TEXT NOT NULL DEFAULT '',
			done INTEGER NOT NULL DEFAULT 0, created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`INSERT INTO lists (name) VALUES ('Groceries'), ('Work')`,
		`INSERT INTO tasks (list_id, title, done) VALUES (1, 'Buy milk', 1), (1, 'Bread', 0)`,
	} {
		if _, err := old.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	old.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade failed: %v", err)
	}
	defer s.Close()

	lists, err := s.Lists(0) // anonymous: sees public lists only
	if err != nil {
		t.Fatal(err)
	}
	if len(lists) != 2 || !lists[0].Public() || lists[0].Total != 2 || lists[0].Open != 1 {
		t.Fatalf("unexpected lists after upgrade: %+v", lists)
	}
	// Invite codes exist internally (used once a list is claimed) and are unique.
	l1, _ := s.firstList(`WHERE l.id = 1`)
	l2, _ := s.firstList(`WHERE l.id = 2`)
	var c1, c2 string
	s.db.QueryRow(`SELECT invite_code FROM lists WHERE id = 1`).Scan(&c1)
	s.db.QueryRow(`SELECT invite_code FROM lists WHERE id = 2`).Scan(&c2)
	if c1 == "" || c2 == "" || c1 == c2 || l1.ID != 1 || l2.ID != 2 {
		t.Fatalf("bad invite codes: %q %q", c1, c2)
	}

	// Re-opening an already-upgraded database is a no-op.
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	s2.Close()
}
