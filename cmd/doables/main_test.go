package main

import (
	"bytes"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"doables/internal/store"
	"doables/internal/web"
)

// The CLI is a documented way to use Doables, so it is tested the way someone
// uses it: run a command against a real server and read what it printed.

type cli struct {
	t     *testing.T
	url   string
	token string
}

func newCLI(t *testing.T) *cli {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "cli.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(web.New(s))
	t.Cleanup(func() { srv.Close(); s.Close() })
	return &cli{t: t, url: srv.URL}
}

// run executes a command and returns what it wrote. It fails the test if the
// command did.
func (c *cli) run(args ...string) string {
	c.t.Helper()
	out, err := c.try(args...)
	if err != nil {
		c.t.Fatalf("doables %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// try is run for commands that are expected to fail.
func (c *cli) try(args ...string) (string, error) {
	c.t.Helper()
	full := append([]string{"-s", c.url}, args...)
	if c.token != "" {
		full = append([]string{"-t", c.token}, full...)
	}
	root := newRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(full)
	err := root.Execute()
	return out.String(), err
}

// signIn registers somebody and keeps their token for later commands.
func (c *cli) signIn(name string) string {
	c.t.Helper()
	out := c.run("register", name)
	m := regexp.MustCompile(`\b([0-9a-f]{64})\b`).FindStringSubmatch(out)
	if m == nil {
		c.t.Fatalf("register printed no token:\n%s", out)
	}
	c.token = m[1]
	return m[1]
}

func contains(t *testing.T, what, out, want string) {
	t.Helper()
	if !strings.Contains(out, want) {
		t.Errorf("%s: expected %q in:\n%s", what, want, out)
	}
}

func TestCLIGoesRoundTheHouses(t *testing.T) {
	c := newCLI(t)
	c.signIn("Alex")
	contains(t, "whoami", c.run("whoami"), "Alex (id 1)")

	contains(t, "new-list", c.run("new-list", "Weekend"), "Created list 1: Weekend")
	contains(t, "lists", c.run("lists"), "Weekend")

	contains(t, "add", c.run("add", "1", "Book the ferry", "-d", "Before sunset", "--due", "2026-10-01"), "Added task 1")
	out := c.run("tasks", "1")
	contains(t, "tasks", out, "Book the ferry")
	contains(t, "tasks shows the due date", out, "2026-10-01")
	contains(t, "tasks shows the description", out, "Before sunset")
	contains(t, "an open task", out, "[ ]")

	contains(t, "done", c.run("done", "1"), "marked done")
	contains(t, "a finished task", c.run("tasks", "1"), "[x]")
	contains(t, "undone", c.run("undone", "1"), "marked not done")

	// Only the flags given are changed, and --due "" clears the date.
	contains(t, "edit", c.run("edit", "1", "--title", "Book the ferry to Cacilhas"), "Updated task 1")
	out = c.run("tasks", "1")
	contains(t, "the new title", out, "Book the ferry to Cacilhas")
	contains(t, "the description survived", out, "Before sunset")
	c.run("edit", "1", "--due", "")
	if strings.Contains(c.run("tasks", "1"), "2026-10-01") {
		t.Error(`--due "" did not clear the due date`)
	}
	if out, err := c.try("edit", "1"); err == nil {
		t.Errorf("edit with no flags was accepted:\n%s", out)
	}

	contains(t, "rename-list", c.run("rename-list", "1", "Weekend away"), "Renamed list 1 to Weekend away")

	// Deleting is undoable, and says so.
	contains(t, "rm", c.run("rm", "1"), "Deleted task 1")
	contains(t, "rm-list", c.run("rm-list", "1"), "restore-list 1")
	if out := c.run("lists"); strings.Contains(out, "Weekend away") {
		t.Errorf("a deleted list is still listed:\n%s", out)
	}
	contains(t, "restore-list", c.run("restore-list", "1"), "Restored list 1: Weekend away")
	contains(t, "the list is back", c.run("lists"), "Weekend away")
}

func TestCLISharesAndAssigns(t *testing.T) {
	c := newCLI(t)
	alex := c.signIn("Alex")
	c.run("new-list", "Weekend")
	c.run("add", "1", "Book the ferry")

	link := strings.TrimSpace(c.run("invite", "1"))
	if !strings.Contains(link, "/join/") {
		t.Fatalf("invite printed %q", link)
	}

	// Somebody else joins with the link.
	sam := c.signIn("Sam")
	contains(t, "join", c.run("join", link), "Joined list 1: Weekend")
	contains(t, "members", c.run("members", "1"), "Sam")

	// Alex hands the task to Sam, who finds it under "mine".
	c.token = alex
	contains(t, "assign", c.run("assign", "1", "2"), "is now Sam's")
	contains(t, "the task says who it is for", c.run("tasks", "1"), "Sam")
	if out := c.run("mine"); !strings.Contains(out, "Nothing is assigned to you") {
		t.Errorf("Alex was given somebody else's task:\n%s", out)
	}
	c.token = sam
	out := c.run("mine")
	contains(t, "mine", out, "Book the ferry")
	contains(t, "mine names the list", out, "Weekend")

	// "me" and "none" are the shorthands people will actually type.
	contains(t, "assign me", c.run("assign", "1", "me"), "is now Sam's")
	contains(t, "assign none", c.run("assign", "1", "none"), "is now anyone's")
	if out, err := c.try("assign", "1", "nobody-by-that-name"); err == nil {
		t.Errorf("a nonsense person was accepted:\n%s", out)
	}
}

func TestCLIComplainsClearly(t *testing.T) {
	c := newCLI(t)

	// Without a token you are anonymous: you see public lists and nothing else.
	if out, err := c.try("tasks", "1"); err == nil {
		t.Errorf("a stranger read a list that does not exist for them:\n%s", out)
	} else if !strings.Contains(err.Error(), "404") {
		t.Errorf("a missing list gave %q, want a 404", err)
	}

	// A leading "-" is a flag to any command-line parser, so "tasks -1" gets
	// cobra's own complaint rather than ours; these are the ones we own.
	for _, args := range [][]string{
		{"tasks", "banana"},
		{"tasks", "1x"},
		{"add", "0", "x"},
		{"rm", "0"},
	} {
		if out, err := c.try(args...); err == nil {
			t.Errorf("doables %s was accepted:\n%s", strings.Join(args, " "), out)
		} else if !strings.Contains(err.Error(), "invalid id") {
			t.Errorf("doables %s said %q, want something about an invalid id", strings.Join(args, " "), err)
		}
	}

	// A list needs an owner, and the message says how to get one.
	c.token = ""
	if out, err := c.try("new-list", "Ownerless"); err == nil {
		t.Errorf("a list was created with no token:\n%s", out)
	} else if !strings.Contains(err.Error(), "register") {
		t.Errorf("creating a list without a token said %q, which does not say what to do", err)
	}

	// An invite for a public list cannot exist, and says why.
	c.signIn("Alex")
	if out, err := c.try("invite", "999"); err == nil {
		t.Errorf("invite for a list that is not there was accepted:\n%s", out)
	} else if !strings.Contains(err.Error(), "not found") {
		t.Errorf("invite for a missing list said %q", err)
	}
}

func TestCLITalksAboutTasks(t *testing.T) {
	c := newCLI(t)
	alex := c.signIn("Alex")
	c.run("new-list", "Weekend")
	c.run("add", "1", "Book the ferry")
	link := strings.TrimSpace(c.run("invite", "1"))
	sam := c.signIn("Sam")
	c.run("join", link)

	contains(t, "no comments yet", c.run("comments", "1"), "No comments yet.")
	contains(t, "comment", c.run("comment", "1", "Friday, 7pm?"), "Commented on task 1")
	c.token = alex
	c.run("comment", "1", "Works for me")

	out := c.run("comments", "1")
	contains(t, "the thread", out, "Sam")
	contains(t, "the thread", out, "Friday, 7pm?")
	contains(t, "the thread", out, "Works for me")
	if strings.Index(out, "Friday, 7pm?") > strings.Index(out, "Works for me") {
		t.Errorf("comments are out of order:\n%s", out)
	}

	// Alex cannot delete what Sam said, and is told why.
	if out, err := c.try("rm-comment", "1"); err == nil {
		t.Errorf("Alex deleted Sam's comment:\n%s", out)
	} else if !strings.Contains(err.Error(), "403") {
		t.Errorf("deleting someone else's comment said %q, want a 403", err)
	}
	c.token = sam
	contains(t, "rm-comment", c.run("rm-comment", "1"), "Deleted comment 1")
	if strings.Contains(c.run("comments", "1"), "Friday, 7pm?") {
		t.Error("a deleted comment is still listed")
	}
}
