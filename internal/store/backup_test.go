package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A backup taken while the server is running holds everything written so
// far, including what is still only in the write-ahead log.
func TestBackupWhileRunning(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "live.db")
	s, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	alex, _, _ := s.CreateUser("Alex")
	sam, _, _ := s.CreateUser("Sam")
	l, _ := s.CreateList("Weekly shop", alex.ID)
	s.AddMember(l.ID, sam.ID)
	task, _ := s.AddTask(l.ID, alex.ID, "Milk", "", "")
	s.AddComment(task.ID, sam.ID, "Oat or cow?")
	// The newest change, which with the server still running has not been
	// written back into the database file itself.
	last, _ := s.AddTask(l.ID, sam.ID, "Bread, written just now", "", "")
	if fi, err := os.Stat(src + "-wal"); err != nil || fi.Size() == 0 {
		t.Fatalf("expected recent changes to be in the write-ahead log: %v", err)
	}

	dst := filepath.Join(dir, "backup.db")
	sum, err := Backup(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if sum.People != 2 || sum.Lists != 1 || sum.Tasks != 2 || sum.Comments != 1 || sum.Bytes == 0 {
		t.Errorf("the backup reports %s", sum)
	}

	// The copy is a working database on its own, with the newest change in it.
	b, err := Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if got, err := b.Task(last.ID); err != nil || got.Title != "Bread, written just now" {
		t.Errorf("the backup lacks the latest change: %+v, %v", got, err)
	}
	if _, err := b.UserByToken("nothing"); err == nil {
		t.Error("the backup let an unknown token in")
	}

	// The live database carries on as if nothing happened.
	if _, err := s.AddTask(l.ID, alex.ID, "Eggs", "", ""); err != nil {
		t.Errorf("the live database broke after a backup: %v", err)
	}
}

func TestBackupNeverOverwritesOrInvents(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "live.db")
	s, _ := Open(src)
	s.CreateUser("Alex")
	defer s.Close()

	// An existing file is never replaced: it may be yesterday's good backup.
	dst := filepath.Join(dir, "yesterday.db")
	os.WriteFile(dst, []byte("yesterday's backup"), 0o600)
	if _, err := Backup(src, dst); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("backing up over an existing file: %v", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "yesterday's backup" {
		t.Error("the existing file was changed")
	}

	// A mistyped database path is an error, not a backup of a new empty one.
	missing := filepath.Join(dir, "typo.db")
	if _, err := Backup(missing, filepath.Join(dir, "out.db")); err == nil {
		t.Error("backing up a database that does not exist succeeded")
	}
	if _, err := os.Stat(missing); err == nil {
		t.Error("backing up a missing database created it")
	}
	if _, err := os.Stat(filepath.Join(dir, "out.db")); err == nil {
		t.Error("a failed backup left a file behind")
	}
}
