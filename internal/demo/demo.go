// Package demo runs Doables as a public demo. Every visitor is given a private,
// filled-in copy of some example lists to play with, shared with two pretend
// teammates, and all of it is removed a day later.
//
// A sandbox per visitor, rather than one shared demo, means nobody sees what
// anybody else typed, so there is nothing to moderate.
package demo

import (
	"context"
	"fmt"
	"log"
	"time"

	"doables/internal/store"
)

// Lifetime is how long a visitor's sandbox lasts, counted from when they
// arrived rather than from a fixed hour, so nobody who turns up at 23:55 loses
// their work five minutes later.
const Lifetime = 24 * time.Hour

// Prepare sets a database aside for the demo, and refuses one that already
// holds real accounts. Demo mode deletes every account a day after it was
// made, so pointed at a real database by mistake it would quietly delete
// everyone within minutes. An empty database is claimed for the demo; after
// that only a database that was claimed can be used.
func Prepare(s *store.Store) error {
	demo, err := s.IsDemo()
	if err != nil || demo {
		return err
	}
	n, err := s.UserCount()
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("this database already has %d account(s), and demo mode deletes every account "+
			"a day after it was made; give the demo a database of its own", n)
	}
	return s.MarkDemo()
}

// Seed fills a new visitor's sandbox and returns the list to show them first.
// The visitor owns everything; Sam and Rae are pretend teammates whose tokens
// are thrown away, so nobody can ever sign in as them.
func Seed(s *store.Store, visitor store.User) (int64, error) {
	b := builder{s: s, me: visitor.ID}
	day := func(n int) string { return time.Now().AddDate(0, 0, n).Format("2006-01-02") }

	sam := b.user("Sam")
	rae := b.user("Rae")

	trip := b.list("Weekend in Lisbon", sam, rae)
	b.task(trip, b.me, "Sort out the airport transfer #transport", "Seven of us, two bags each", day(-1), b.me)
	ferry := b.task(trip, sam, "Book the ferry to Cacilhas #transport", "Friday evening, before sunset", day(0), sam)
	b.comment(ferry, rae, "Is the 7pm one still running in September?")
	b.comment(ferry, sam, "Checked: last one is 21:30")
	b.task(trip, b.me, "Find somewhere for dinner on Friday #food", "Somewhere near the water", day(1), rae)
	b.task(trip, rae, "Renew the travel card #transport", "", day(4), rae)
	b.task(trip, b.me, "Pack the camera #packing", "And the spare battery", "", b.me)
	adapters := b.task(trip, sam, "Buy travel adapters #packing", "", "", 0)
	b.done(adapters, sam)

	flat := b.list("Flat move")
	b.task(flat, b.me, "Call the letting agent", "Ask about the deposit", day(0), b.me)
	b.task(flat, b.me, "Return the router", "", day(-1), 0)
	b.task(flat, b.me, "Book a van", "", day(3), b.me)

	reading := b.list("Reading")
	b.task(reading, b.me, "Finish the Go book", "Chapter 12 onwards", day(3), b.me)

	return trip, b.err
}

// builder keeps the first error and skips everything after it, so Seed reads
// as the list of things it makes rather than as error handling.
type builder struct {
	s   *store.Store
	me  int64
	err error
}

func (b *builder) user(name string) int64 {
	if b.err != nil {
		return 0
	}
	u, _, err := b.s.CreateUser(name)
	b.err = err
	return u.ID
}

func (b *builder) list(name string, members ...int64) int64 {
	if b.err != nil {
		return 0
	}
	l, err := b.s.CreateList(name, b.me)
	if b.err = err; err != nil {
		return 0
	}
	for _, m := range members {
		if b.err = b.s.AddMember(l.ID, m); b.err != nil {
			return 0
		}
	}
	return l.ID
}

func (b *builder) task(list, author int64, title, description, due string, assignee int64) int64 {
	if b.err != nil {
		return 0
	}
	t, err := b.s.AddTask(list, author, title, description, due)
	if b.err = err; err != nil {
		return 0
	}
	if assignee != 0 {
		_, b.err = b.s.AssignTask(t.ID, assignee)
	}
	return t.ID
}

func (b *builder) comment(task, author int64, body string) {
	if b.err == nil {
		_, b.err = b.s.AddComment(task, author, body)
	}
}

func (b *builder) done(task, by int64) {
	if b.err == nil {
		_, b.err = b.s.SetDone(task, true, by)
	}
}

// Sweep removes sandboxes older than lifetime straight away and then every
// interval, until ctx is cancelled. It does nothing at all to a database that
// was never set aside for a demo.
func Sweep(ctx context.Context, s *store.Store, lifetime, every time.Duration) {
	// Deleting accounts is only ever right in a database set aside for it.
	if demo, err := s.IsDemo(); err != nil || !demo {
		log.Printf("demo: not clearing anything out: this database was never set aside for a demo (%v)", err)
		return
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if n, err := s.PurgeUsersOlderThan(lifetime); err != nil {
			log.Printf("demo: clearing out old sandboxes: %v", err)
		} else if n > 0 {
			log.Printf("demo: cleared out %d visitors' sandboxes", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
