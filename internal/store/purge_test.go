package store

import (
	"testing"
	"time"
)

// The demo removes each visitor's sandbox a day after they arrived: their
// account, their pretend teammates, their lists and everything on them. And
// nothing must be left behind ownerless, which here would mean public.
func TestPurgingOldAccountsTakesTheirListsWithThem(t *testing.T) {
	s := openTest(t)

	sandbox := func(name string) (User, int64) {
		me, _, _ := s.CreateUser(name)
		mate, _, _ := s.CreateUser(name + "'s teammate")
		l, _ := s.CreateList(name+"'s list", me.ID)
		s.AddMember(l.ID, mate.ID)
		task, _ := s.AddTask(l.ID, me.ID, "Something", "", "")
		s.AssignTask(task.ID, mate.ID)
		s.AddComment(task.ID, mate.ID, "On it")
		return me, l.ID
	}
	_, oldList := sandbox("Yesterday")
	s.db.Exec(`UPDATE users SET created_at = datetime('now', '-25 hours')`)
	fresh, freshList := sandbox("Today")

	n, err := s.PurgeUsersOlderThan(24 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("purged %d accounts, want 2: yesterday's visitor and their teammate", n)
	}
	if _, err := s.Access(oldList, 0); err == nil {
		t.Error("the old visitor's list survived, and is now open to anyone")
	}
	if n := count(t, s, `SELECT COUNT(*) FROM lists WHERE owner_id IS NULL`); n != 0 {
		t.Errorf("%d list(s) were left without an owner, which makes them public", n)
	}
	for _, orphan := range []string{
		`SELECT COUNT(*) FROM tasks WHERE list_id NOT IN (SELECT id FROM lists)`,
		`SELECT COUNT(*) FROM comments WHERE task_id NOT IN (SELECT id FROM tasks)`,
		`SELECT COUNT(*) FROM list_members WHERE user_id NOT IN (SELECT id FROM users)`,
	} {
		if n := count(t, s, orphan); n != 0 {
			t.Errorf("%d row(s) left pointing at nothing: %s", n, orphan)
		}
	}

	// Today's visitor is untouched.
	if _, err := s.Access(freshList, fresh.ID); err != nil {
		t.Errorf("a fresh sandbox was removed too: %v", err)
	}
}
