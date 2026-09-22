package web

import (
	"net/url"
	"strings"
	"testing"

	"doables/internal/store"
)

// Deleting a task could always be undone; deleting a list, which is far worse,
// could not. Now both can.
func TestDeletingAListCanBeUndone(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.register("Alice"), e.register("Bob")
	l := e.newList(alice, "Trip")
	e.call("POST", "/api/join/"+l.InviteCode, bob, "", nil)
	e.call("POST", "/api/lists/"+itoa(l.ID)+"/tasks", alice, `{"title":"Book flights","due_date":"2026-09-22"}`, nil)
	e.call("PATCH", "/api/tasks/1", alice, `{"assignee_id":`+itoa(e.me(bob).ID)+`}`, nil)

	resp := e.form(alice, "/lists/"+itoa(l.ID)+"/delete", url.Values{})
	want(t, "delete the list", resp.StatusCode, 303)
	if loc := resp.Header.Get("Location"); loc != "/?undo="+itoa(l.ID) {
		t.Errorf("after deleting we went to %q, which carries no offer to undo", loc)
	}

	// While it is in the trash it is gone as far as anyone can tell, including
	// from the views that gather tasks from every list.
	var lists []store.List
	e.call("GET", "/api/lists", alice, "", &lists)
	if len(lists) != 0 {
		t.Errorf("a deleted list is still listed: %+v", lists)
	}
	want(t, "reading a deleted list", e.call("GET", "/api/lists/"+itoa(l.ID)+"/tasks", alice, "", nil), 404)
	want(t, "adding to a deleted list", e.call("POST", "/api/lists/"+itoa(l.ID)+"/tasks", alice, `{"title":"x"}`, nil), 404)
	if mine := e.assigned(bob); len(mine) != 0 {
		t.Errorf("a deleted list's tasks are still on Bob's list: %+v", mine)
	}
	if _, page := e.page(alice, "/today"); strings.Contains(page, "Book flights") {
		t.Error("a deleted list's task is still in Today")
	}
	// Nor can anyone join it from an invite that is still lying around.
	carol := e.register("Carol")
	want(t, "joining a deleted list", e.call("POST", "/api/join/"+l.InviteCode, carol, "", nil), 404)

	// Undo brings back the list, its tasks, its members and the assignment.
	resp = e.form(alice, "/lists/"+itoa(l.ID)+"/restore", url.Values{})
	want(t, "undo", resp.StatusCode, 303)
	if loc := resp.Header.Get("Location"); loc != "/lists/"+itoa(l.ID) {
		t.Errorf("undo sent us to %q, want the list", loc)
	}
	e.call("GET", "/api/lists", alice, "", &lists)
	if len(lists) != 1 || lists[0].Name != "Trip" || lists[0].Members != 2 {
		t.Fatalf("the list did not come back properly: %+v", lists)
	}
	back := e.oneTask(alice, l.ID, 1)
	if back.Title != "Book flights" || back.Assignee != "Bob" {
		t.Errorf("the tasks did not come back properly: %+v", back)
	}
	if mine := e.assigned(bob); len(mine) != 1 {
		t.Errorf("Bob's assignment did not come back: %+v", mine)
	}
}

// Undo is for the person who deleted it, not for anyone who can guess a number.
func TestOnlyTheOwnerCanUndoADelete(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.register("Alice"), e.register("Bob")
	l := e.newList(alice, "Trip")
	e.call("POST", "/api/join/"+l.InviteCode, bob, "", nil)
	e.form(alice, "/lists/"+itoa(l.ID)+"/delete", url.Values{})

	// A member is not the owner, and a stranger is nobody at all.
	want(t, "member undoes", e.form(bob, "/lists/"+itoa(l.ID)+"/restore", url.Values{}).StatusCode, 404)
	carol := e.register("Carol")
	want(t, "stranger undoes", e.form(carol, "/lists/"+itoa(l.ID)+"/restore", url.Values{}).StatusCode, 404)
	want(t, "nobody undoes", e.form("", "/lists/"+itoa(l.ID)+"/restore", url.Values{}).StatusCode, 303) // sent to sign in

	// And a list that was never deleted cannot be "restored" either.
	other := e.newList(alice, "Still here")
	want(t, "undo a live list", e.form(alice, "/lists/"+itoa(other.ID)+"/restore", url.Values{}).StatusCode, 404)

	// The owner still can.
	want(t, "owner undoes", e.form(alice, "/lists/"+itoa(l.ID)+"/restore", url.Values{}).StatusCode, 303)
}
