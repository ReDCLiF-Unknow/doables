package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
)

// BackupSummary is what a backup holds, so whoever made it can see at a
// glance that it is the database they meant.
type BackupSummary struct {
	People, Lists, Tasks, Comments int
	Bytes                          int64
}

func (b BackupSummary) String() string {
	return fmt.Sprintf("%d people, %d lists, %d tasks, %d comments (%d KB)",
		b.People, b.Lists, b.Tasks, b.Comments, (b.Bytes+1023)/1024)
}

// Backup writes a consistent copy of the database at src to dst, while a
// server may be using it. Copying the file itself is not safe: recent changes
// can still be in its write-ahead log, and a copy taken mid-write can be torn.
// SQLite's VACUUM INTO reads the database as of one moment, log included, and
// writes a compact, self-contained file.
//
// The database is opened read-only, so a backup can never change it, and an
// existing dst is never overwritten. The copy is checked before it counts.
func Backup(src, dst string) (BackupSummary, error) {
	var sum BackupSummary
	if _, err := os.Stat(src); err != nil {
		return sum, fmt.Errorf("no database at %s: %w", src, err)
	}
	if _, err := os.Stat(dst); err == nil {
		return sum, fmt.Errorf("%s already exists; give the backup a new name", dst)
	} else if !errors.Is(err, os.ErrNotExist) {
		return sum, err
	}

	db, err := sql.Open("sqlite", "file:"+src+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		return sum, err
	}
	defer db.Close()
	if _, err := db.Exec(`VACUUM INTO ?`, dst); err != nil {
		os.Remove(dst)
		return sum, fmt.Errorf("copying %s: %w", src, err)
	}

	if sum, err = checkBackup(dst); err != nil {
		os.Remove(dst)
		return sum, fmt.Errorf("the copy did not check out, so it was removed: %w", err)
	}
	return sum, nil
}

// checkBackup opens a finished backup on its own and reports what is in it.
func checkBackup(path string) (BackupSummary, error) {
	var sum BackupSummary
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return sum, err
	}
	defer db.Close()
	var ok string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&ok); err != nil {
		return sum, err
	}
	if ok != "ok" {
		return sum, errors.New(ok)
	}
	err = db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM users),
		(SELECT COUNT(*) FROM lists WHERE deleted_at IS NULL),
		(SELECT COUNT(*) FROM tasks WHERE deleted_at IS NULL),
		(SELECT COUNT(*) FROM comments)`).Scan(&sum.People, &sum.Lists, &sum.Tasks, &sum.Comments)
	if err != nil {
		return sum, err
	}
	if fi, err := os.Stat(path); err == nil {
		sum.Bytes = fi.Size()
	}
	return sum, nil
}
