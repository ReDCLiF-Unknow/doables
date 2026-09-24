package store

import (
	"path/filepath"
	"strings"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// failWrites makes every write of the given kind to a table fail, standing in
// for a disk filling up or a constraint firing halfway through an operation.
func failWrites(t *testing.T, s *Store, table, when string) {
	t.Helper()
	sql := `CREATE TRIGGER sabotage BEFORE ` + when + ` ON ` + table +
		` BEGIN SELECT RAISE(ABORT, 'sabotaged'); END`
	if _, err := s.db.Exec(sql); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// An owned list whose owner is not a member is invisible to everyone, its
// owner included. Creating one is two writes, so they have to succeed or fail
// together.
func TestCreatingAListIsAllOrNothing(t *testing.T) {
	s := openTest(t)
	u, _, _ := s.CreateUser("Alex")
	failWrites(t, s, "list_members", "INSERT")

	if _, err := s.CreateList("Weekend", u.ID); err == nil {
		t.Fatal("creating a list succeeded although its owner could not be added")
	}
	if n := count(t, s, `SELECT COUNT(*) FROM lists`); n != 0 {
		t.Errorf("a failed create left %d list(s) behind with no owner on them", n)
	}
}

func TestClaimingAListIsAllOrNothing(t *testing.T) {
	s := openTest(t)
	u, _, _ := s.CreateUser("Alex")
	l, _ := s.CreateList("From an old database", 0)
	failWrites(t, s, "list_members", "INSERT")

	if err := s.ClaimList(l.ID, u.ID); err == nil {
		t.Fatal("claiming succeeded although the claimant could not be added")
	}
	// Half a claim would make the list owned and memberless: gone for everyone.
	if n := count(t, s, `SELECT COUNT(*) FROM lists WHERE id = ? AND owner_id IS NULL`, l.ID); n != 1 {
		t.Error("a failed claim still took ownership, hiding the list from everyone")
	}
	if _, err := s.Access(l.ID, 0); err != nil {
		t.Errorf("after a failed claim the list is no longer public: %v", err)
	}
}

// Removing someone and freeing their tasks go together: otherwise a task could
// stay assigned to a person who can no longer see it.
func TestRemovingSomeoneIsAllOrNothing(t *testing.T) {
	s := openTest(t)
	alex, _, _ := s.CreateUser("Alex")
	sam, _, _ := s.CreateUser("Sam")
	l, _ := s.CreateList("Weekend", alex.ID)
	s.AddMember(l.ID, sam.ID)
	task, _ := s.AddTask(l.ID, alex.ID, "Book the ferry", "", "")
	if _, err := s.AssignTask(task.ID, sam.ID); err != nil {
		t.Fatal(err)
	}
	failWrites(t, s, "tasks", "UPDATE")

	if err := s.RemoveMember(l.ID, sam.ID); err == nil {
		t.Fatal("removing Sam succeeded although his task could not be freed")
	}
	if !s.isMember(l.ID, sam.ID) {
		t.Error("a failed removal took Sam off the list but left his task assigned to him")
	}
}

// Every page load asks which lists a person is on. The primary key is
// (list_id, user_id), so without its own index that question scans the table.
func TestFindingSomeonesListsUsesAnIndex(t *testing.T) {
	s := openTest(t)
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT list_id FROM list_members WHERE user_id = ?`, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	joined := strings.Join(plan, "; ")
	if !strings.Contains(joined, "list_members_user_id") {
		t.Errorf("looking up someone's lists does not use the index: %s", joined)
	}
}
