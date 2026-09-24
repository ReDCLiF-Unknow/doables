package demo

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"doables/internal/store"
)

func open(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "demo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// A demo that starts empty shows nothing: the visitor should land in a list
// that already has people, work, a conversation and something finished.
func TestAVisitorGetsSomethingToPlayWith(t *testing.T) {
	s := open(t)
	me, _, _ := s.CreateUser("Visitor")

	first, err := Seed(s, me)
	if err != nil {
		t.Fatal(err)
	}
	lists, _ := s.Lists(me.ID)
	if len(lists) != 3 {
		t.Fatalf("the visitor has %d lists, want 3", len(lists))
	}
	trip, err := s.Access(first, me.ID)
	if err != nil {
		t.Fatalf("the list the visitor is sent to is not theirs: %v", err)
	}
	if trip.Name != "Weekend in Lisbon" || trip.OwnerID != me.ID || trip.Members != 3 {
		t.Errorf("the first list is %+v, want the visitor's own, shared with two others", trip)
	}

	tasks, _ := s.Tasks(first)
	var done, withComments, theirs int
	for _, task := range tasks {
		if task.Done {
			done++
		}
		if task.Comments > 0 {
			withComments++
		}
		if task.AssigneeID == me.ID {
			theirs++
		}
	}
	if len(tasks) != 6 || done != 1 || withComments != 1 || theirs != 2 {
		t.Errorf("the trip has %d tasks, %d done, %d with comments, %d the visitor's; want 6, 1, 1, 2",
			len(tasks), done, withComments, theirs)
	}
	if mine, _ := s.AssignedTasks(me.ID); len(mine) == 0 {
		t.Error("My tasks is empty, so it shows nothing in a demo")
	}
}

// Each visitor's sandbox is theirs alone, so nobody sees what anybody else
// wrote and there is nothing to moderate.
func TestSandboxesArePrivate(t *testing.T) {
	s := open(t)
	alice, _, _ := s.CreateUser("Alice")
	bob, _, _ := s.CreateUser("Bob")
	aliceTrip, _ := Seed(s, alice)
	bobTrip, _ := Seed(s, bob)

	if aliceTrip == bobTrip {
		t.Fatal("two visitors were given the same list")
	}
	if _, err := s.Access(aliceTrip, bob.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Bob can see Alice's sandbox: %v", err)
	}
	if lists, _ := s.Lists(bob.ID); len(lists) != 3 {
		t.Errorf("Bob sees %d lists, want only his own 3", len(lists))
	}
}

// Sweep clears out old sandboxes as soon as it starts, and stops when told to.
func TestSweepStopsWhenAsked(t *testing.T) {
	s := open(t)
	if err := Prepare(s); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { Sweep(ctx, s, Lifetime, time.Hour); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Sweep did not stop when its context was cancelled")
	}
}

// Demo mode deletes every account a day after it was made. Pointed at a real
// database by mistake, that would delete everyone, so it must refuse.
func TestTheDemoRefusesARealDatabase(t *testing.T) {
	fresh := open(t)
	if err := Prepare(fresh); err != nil {
		t.Fatalf("an empty database was refused: %v", err)
	}
	if demo, _ := fresh.IsDemo(); !demo {
		t.Error("preparing an empty database did not set it aside for the demo")
	}
	// Once set aside, it stays usable however many visitors it has.
	fresh.CreateUser("Visitor")
	if err := Prepare(fresh); err != nil {
		t.Errorf("the demo's own database was refused once it had visitors: %v", err)
	}

	real := open(t)
	real.CreateUser("Somebody who uses Doables for real")
	err := Prepare(real)
	if err == nil || !strings.Contains(err.Error(), "database of its own") {
		t.Fatalf("a database with real accounts was accepted for the demo: %v", err)
	}
	if demo, _ := real.IsDemo(); demo {
		t.Error("a refused database was still marked as the demo's")
	}
}

// Even if something calls Sweep on an ordinary database, it deletes nothing.
func TestSweepLeavesAnOrdinaryDatabaseAlone(t *testing.T) {
	sweepOnce := func(s *store.Store) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // one pass, then stop
		Sweep(ctx, s, 0, time.Hour)
	}

	real := open(t)
	real.CreateUser("Somebody real")
	time.Sleep(1100 * time.Millisecond) // older than a lifetime of zero, to the second
	sweepOnce(real)
	if n, _ := real.UserCount(); n != 1 {
		t.Fatalf("Sweep deleted %d account(s) from a database that was never the demo's", 1-n)
	}

	// The same pass on the demo's own database does delete, which shows it
	// was the refusal that saved the real one, not the timing.
	demoDB := open(t)
	Prepare(demoDB)
	demoDB.CreateUser("Visitor")
	time.Sleep(1100 * time.Millisecond)
	sweepOnce(demoDB)
	if n, _ := demoDB.UserCount(); n != 0 {
		t.Errorf("Sweep left %d expired visitor(s) in the demo's database", n)
	}
}
