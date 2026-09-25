package store

import (
	"path/filepath"
	"testing"
)

func TestWhatCountsAsNew(t *testing.T) {
	s := openTest(t)
	alex, _, _ := s.CreateUser("Alex")
	sam, _, _ := s.CreateUser("Sam")
	l, _ := s.CreateList("Trip", alex.ID)
	s.AddMember(l.ID, sam.ID)
	ferry, _ := s.AddTask(l.ID, alex.ID, "Book the ferry", "", "")
	hotel, _ := s.AddTask(l.ID, alex.ID, "Book the hotel", "", "")

	count := func(who User, task int64) int {
		t.Helper()
		u, err := s.Unread(who.ID)
		if err != nil {
			t.Fatal(err)
		}
		return u[task].Count
	}

	s.AddComment(ferry.ID, sam.ID, "Which day?")
	s.AddComment(ferry.ID, sam.ID, "Friday works for me")
	if n := count(alex, ferry.ID); n != 2 {
		t.Errorf("Alex has %d new comments from Sam, want 2", n)
	}
	if n := count(sam, ferry.ID); n != 0 {
		t.Errorf("Sam's own comments are new to him: %d", n)
	}
	u, _ := s.Unread(alex.ID)
	if got := u[ferry.ID]; got.Latest.Body != "Friday works for me" || got.Latest.Author != "Sam" || got.ListID != l.ID || len(got.IDs) != 2 {
		t.Errorf("Alex's news about the ferry is %+v", got)
	}
	if got := UnreadByList(u); got[l.ID] != 2 {
		t.Errorf("the trip has %d new comments for Alex, want 2", got[l.ID])
	}

	// Seeing the thread clears it; the next comment is new again.
	if err := s.MarkSeen(alex.ID, ferry.ID); err != nil {
		t.Fatal(err)
	}
	if n := count(alex, ferry.ID); n != 0 {
		t.Errorf("after seeing the thread, %d are still new", n)
	}
	s.AddComment(ferry.ID, sam.ID, "Booked!")
	if n := count(alex, ferry.ID); n != 1 {
		t.Errorf("a comment after Alex looked is not new: %d", n)
	}

	// Answering counts as having read what you answer.
	s.AddComment(hotel.ID, sam.ID, "Near the water?")
	s.AddComment(hotel.ID, alex.ID, "Yes please")
	if n := count(alex, hotel.ID); n != 0 {
		t.Errorf("Alex answered, yet %d comments are new to him", n)
	}

	// Someone who joins later is not told about everything said before.
	rae, _, _ := s.CreateUser("Rae")
	s.AddMember(l.ID, rae.ID)
	// Everything here happens within a second; make her arrival clearly later.
	if _, err := s.db.Exec(`UPDATE list_members SET joined_at = datetime('now', '+1 minute') WHERE user_id = ?`, rae.ID); err != nil {
		t.Fatal(err)
	}
	if n := count(rae, ferry.ID); n != 0 {
		t.Errorf("Rae joined after the ferry talk, yet %d comments are new to her", n)
	}

	// A deleted task's conversation is not news.
	s.DeleteTask(ferry.ID)
	if n := count(alex, ferry.ID); n != 0 {
		t.Errorf("a deleted task still has %d new comments", n)
	}
}

// Opening a database from before this existed must not make every comment
// ever written new to everyone at once.
func TestUpgradingDoesNotMakeOldCommentsNew(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	alex, _, _ := s.CreateUser("Alex")
	sam, _, _ := s.CreateUser("Sam")
	l, _ := s.CreateList("Trip", alex.ID)
	s.AddMember(l.ID, sam.ID)
	task, _ := s.AddTask(l.ID, alex.ID, "Book the ferry", "", "")
	s.AddComment(task.ID, sam.ID, "Said long ago")
	// Make it the database of a release before unread comments: no marker.
	s.db.Exec(`DELETE FROM settings WHERE key = 'unread_since'`)
	s.db.Exec(`DELETE FROM comment_reads`)
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if u, _ := s.Unread(alex.ID); len(u) != 0 {
		t.Errorf("after upgrading, old comments are new: %+v", u)
	}
	s.AddComment(task.ID, sam.ID, "Said after the upgrade")
	if u, _ := s.Unread(alex.ID); u[task.ID].Count != 1 {
		t.Errorf("a comment made after upgrading is not new: %+v", u)
	}
}
