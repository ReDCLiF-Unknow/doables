package web

import (
	"net/url"
	"strings"
	"testing"

	"doables/internal/store"
)

// Tests for giving a task to someone. Shared helpers (env, call, register,
// form, page, ...) live in web_test.go and features_test.go.

// me returns who a token belongs to.
func (e *env) me(token string) store.User {
	e.t.Helper()
	var u store.User
	if c := e.call("GET", "/api/me", token, "", &u); c != 200 {
		e.t.Fatalf("whoami: status %d", c)
	}
	return u
}

// assigned returns the open tasks assigned to the holder of token.
func (e *env) assigned(token string) []store.Task {
	e.t.Helper()
	var ts []store.Task
	if c := e.call("GET", "/api/mine", token, "", &ts); c != 200 {
		e.t.Fatalf("my tasks: status %d", c)
	}
	return ts
}

// oneTask returns a single task by id, read as the given person.
func (e *env) oneTask(token string, listID, taskID int64) store.Task {
	e.t.Helper()
	for _, t := range e.tasks(token, listID) {
		if t.ID == taskID {
			return t
		}
	}
	e.t.Fatalf("task %d not found in list %d", taskID, listID)
	return store.Task{}
}

func TestAssigningTasks(t *testing.T) {
	e := newEnv(t)
	alice, bob, carol := e.register("Alice"), e.register("Bob"), e.register("Carol")
	bobID := e.me(bob).ID

	l := e.newList(alice, "Trip")
	e.call("POST", "/api/join/"+l.InviteCode, bob, "", nil)
	e.call("POST", "/api/lists/"+itoa(l.ID)+"/tasks", alice, `{"title":"Book flights"}`, nil)
	e.call("POST", "/api/lists/"+itoa(l.ID)+"/tasks", alice, `{"title":"Pack bags"}`, nil)

	// Alice makes the first task Bob's job.
	var got store.Task
	want(t, "assign to a member", e.call("PATCH", "/api/tasks/1", alice, `{"assignee_id":`+itoa(bobID)+`}`, &got), 200)
	if got.AssigneeID != bobID || got.Assignee != "Bob" {
		t.Errorf("after assigning: got assignee %d/%q, want %d/Bob", got.AssigneeID, got.Assignee, bobID)
	}

	// Carol is not on the list, so nothing can be given to her, and the task
	// is left as it was rather than half-changed.
	carolID := e.me(carol).ID
	want(t, "assign to a stranger", e.call("PATCH", "/api/tasks/1", alice, `{"assignee_id":`+itoa(carolID)+`}`, nil), 400)
	if still := e.oneTask(alice, l.ID, 1); still.AssigneeID != bobID {
		t.Errorf("a refused assignment changed the task: %+v", still)
	}
	want(t, "assign to somebody who does not exist", e.call("PATCH", "/api/tasks/1", alice, `{"assignee_id":999}`, nil), 400)

	// Bob sees it in his own list; Alice has nothing of her own.
	mine := e.assigned(bob)
	if len(mine) != 1 || mine[0].ID != 1 || mine[0].ListName != "Trip" {
		t.Fatalf("Bob's tasks: %+v", mine)
	}
	if other := e.assigned(alice); len(other) != 0 {
		t.Errorf("Alice should have nothing assigned, got %+v", other)
	}

	// Carol cannot see the list, so she cannot assign anything in it either,
	// and it looks to her like it does not exist.
	want(t, "stranger assigns", e.call("PATCH", "/api/tasks/1", carol, `{"assignee_id":`+itoa(carolID)+`}`, nil), 404)

	// Finishing it takes it off Bob's plate without forgetting whose it was.
	e.call("PATCH", "/api/tasks/1", bob, `{"done":true}`, nil)
	if mine := e.assigned(bob); len(mine) != 0 {
		t.Errorf("a finished task is still on Bob's list: %+v", mine)
	}
	if done := e.oneTask(alice, l.ID, 1); done.AssigneeID != bobID {
		t.Errorf("finishing a task forgot who it was for: %+v", done)
	}

	// Handing it back to nobody.
	e.call("PATCH", "/api/tasks/1", alice, `{"done":false}`, nil)
	var cleared store.Task
	want(t, "unassign", e.call("PATCH", "/api/tasks/1", alice, `{"assignee_id":0}`, &cleared), 200)
	if cleared.AssigneeID != 0 || cleared.Assignee != "" {
		t.Errorf("after unassigning: %+v", cleared)
	}
	if mine := e.assigned(bob); len(mine) != 0 {
		t.Errorf("Bob still has an unassigned task: %+v", mine)
	}
}

// A patch with no recognised field is still rejected.
func TestPatchStillNeedsSomethingToDo(t *testing.T) {
	e := newEnv(t)
	alice := e.register("Alice")
	l := e.newList(alice, "Trip")
	e.call("POST", "/api/lists/"+itoa(l.ID)+"/tasks", alice, `{"title":"Book flights"}`, nil)
	want(t, "empty patch", e.call("PATCH", "/api/tasks/1", alice, `{}`, nil), 400)
}

// Nothing can be assigned in a public list: it has no members, so there is
// nobody for a task to belong to.
func TestPublicListsHaveNobodyToAssignTo(t *testing.T) {
	e := newEnv(t)
	var l store.List
	e.call("POST", "/api/lists", "", `{"name":"Shed"}`, &l)
	if !l.Public() {
		t.Fatalf("a list made without a token should be public: %+v", l)
	}
	alice := e.register("Alice")
	aliceID := e.me(alice).ID
	e.call("POST", "/api/lists/"+itoa(l.ID)+"/tasks", alice, `{"title":"Paint it"}`, nil)
	want(t, "assign in a public list", e.call("PATCH", "/api/tasks/1", alice, `{"assignee_id":`+itoa(aliceID)+`}`, nil), 400)

	// Claiming it makes Alice a member, and then it works.
	e.form(alice, "/lists/"+itoa(l.ID)+"/claim", url.Values{})
	want(t, "assign after claiming", e.call("PATCH", "/api/tasks/1", alice, `{"assignee_id":`+itoa(aliceID)+`}`, nil), 200)
}

// Someone who is removed from a list cannot see it any more, so whatever was
// theirs goes back to being anyone's.
func TestRemovingSomeoneFreesTheirTasks(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.register("Alice"), e.register("Bob")
	bobID := e.me(bob).ID
	l := e.newList(alice, "Trip")
	e.call("POST", "/api/join/"+l.InviteCode, bob, "", nil)
	e.call("POST", "/api/lists/"+itoa(l.ID)+"/tasks", alice, `{"title":"Book flights"}`, nil)
	e.call("PATCH", "/api/tasks/1", alice, `{"assignee_id":`+itoa(bobID)+`}`, nil)

	e.form(alice, "/lists/"+itoa(l.ID)+"/members/"+itoa(bobID)+"/remove", url.Values{})

	if freed := e.oneTask(alice, l.ID, 1); freed.AssigneeID != 0 || freed.Assignee != "" {
		t.Errorf("task still belongs to someone who was removed: %+v", freed)
	}
	if mine := e.assigned(bob); len(mine) != 0 {
		t.Errorf("Bob still has tasks from a list he was removed from: %+v", mine)
	}
}

// The same thing through the web UI: the menu on a task posts a person's id,
// the row then says whose job it is, and it turns up under "My tasks".
func TestAssigningFromThePage(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.register("Alice"), e.register("Bob")
	bobID := e.me(bob).ID
	l := e.newList(alice, "Trip")
	e.call("POST", "/api/join/"+l.InviteCode, bob, "", nil)
	e.call("POST", "/api/lists/"+itoa(l.ID)+"/tasks", alice, `{"title":"Book flights"}`, nil)
	path := "/lists/" + itoa(l.ID)

	// The menu is on the page, with the members in it.
	_, html := e.page(alice, path)
	if !strings.Contains(html, "Whose job is this?") {
		t.Error("the list page has no assign menu")
	}
	if !strings.Contains(html, `name="user" value="`+itoa(bobID)+`"`) {
		t.Error("the assign menu does not offer Bob")
	}

	resp := e.form(alice, "/tasks/1/assign", url.Values{"user": {itoa(bobID)}, "next": {path}})
	want(t, "assign from the page", resp.StatusCode, 303)
	if loc := resp.Header.Get("Location"); loc != path {
		t.Errorf("assigning sent us to %q, want %q", loc, path)
	}

	if _, html = e.page(alice, path); !strings.Contains(html, "Bob's job") {
		t.Error("the task row does not say it is Bob's job")
	}

	// Bob sees the same task as his own, with a count in the sidebar.
	_, bobsPage := e.page(bob, "/mine")
	if !strings.Contains(bobsPage, "Book flights") {
		t.Error(`"My tasks" does not show Bob's task`)
	}
	if !strings.Contains(bobsPage, "Yours") {
		t.Error("Bob's own task is not marked as his")
	}
	if !strings.Contains(bobsPage, `data-live="mine-badge"><span class="badge bg-blue-lt text-blue">1</span>`) {
		t.Error("the sidebar badge does not count Bob's task")
	}
	if _, alicesPage := e.page(alice, "/mine"); !strings.Contains(alicesPage, "Nothing is yours yet") {
		t.Error("Alice should have nothing assigned")
	}

	// Handing it back to nobody, the way the menu's last item does.
	e.form(alice, "/tasks/1/assign", url.Values{"user": {""}, "next": {path}})
	if free := e.oneTask(alice, l.ID, 1); free.AssigneeID != 0 {
		t.Errorf("task is still assigned: %+v", free)
	}

	// A stranger gets the same 404 as for a list that does not exist.
	carol := e.register("Carol")
	resp = e.form(carol, "/tasks/1/assign", url.Values{"user": {itoa(bobID)}})
	want(t, "stranger assigns from a page", resp.StatusCode, 404)
}

// The web form caps a title at 200 characters and a description at 500. The
// API and the CLI have to agree, or they become a way round the limit.
func TestOverlongTasksAreRejected(t *testing.T) {
	e := newEnv(t)
	alice := e.register("Alice")
	l := e.newList(alice, "Trip")
	tasks := "/api/lists/" + itoa(l.ID) + "/tasks"

	long := strings.Repeat("x", 201)
	want(t, "overlong title", e.call("POST", tasks, alice, `{"title":"`+long+`"}`, nil), 400)
	want(t, "overlong description", e.call("POST", tasks, alice,
		`{"title":"Fine","description":"`+strings.Repeat("y", 501)+`"}`, nil), 400)

	// The boundary itself is allowed, and counted in characters rather than
	// bytes, so an accented title is not cut short.
	want(t, "title at the limit", e.call("POST", tasks, alice,
		`{"title":"`+strings.Repeat("x", 200)+`"}`, nil), 201)
	want(t, "accented title at the limit", e.call("POST", tasks, alice,
		`{"title":"`+strings.Repeat("é", 200)+`"}`, nil), 201)

	// Editing is held to the same limit.
	want(t, "edit to an overlong title", e.call("PATCH", "/api/tasks/1", alice, `{"title":"`+long+`"}`, nil), 400)
	want(t, "edit to a sane title", e.call("PATCH", "/api/tasks/1", alice, `{"title":"Shorter"}`, nil), 200)
}
