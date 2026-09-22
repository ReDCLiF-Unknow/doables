package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"doables/internal/store"
)

// Tests for editing, due dates, the Today view, undoable deletes and live
// updates. Shared helpers (env, call, register, ...) live in web_test.go.

// form posts a urlencoded form as a person and returns the response.
func (e *env) form(token, path string, form url.Values) *http.Response {
	e.t.Helper()
	req, _ := http.NewRequest("POST", e.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// page fetches an HTML page as a person and returns its status and body.
func (e *env) page(token, path string) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := noRedirect.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	var b strings.Builder
	readAll(&b, resp)
	return resp.StatusCode, b.String()
}

func (e *env) tasks(token string, listID int64) []store.Task {
	e.t.Helper()
	var ts []store.Task
	e.call("GET", "/api/lists/"+itoa(listID)+"/tasks", token, "", &ts)
	return ts
}

func TestEditingTasksAndRenamingLists(t *testing.T) {
	e := newEnv(t)
	alice, bob, carol := e.register("Alice"), e.register("Bob"), e.register("Carol")
	l := e.newList(alice, "Trip")
	e.call("POST", "/api/join/"+l.InviteCode, bob, "", nil)
	id := "/api/lists/" + itoa(l.ID)

	var task store.Task
	want(t, "add with due date", e.call("POST", id+"/tasks", alice, `{"title":"Book","description":"flights","due_date":"2031-05-04"}`, &task), 201)
	if task.DueDate != "2031-05-04" {
		t.Fatalf("due date not stored: %+v", task)
	}
	want(t, "bad due date", e.call("POST", id+"/tasks", alice, `{"title":"x","due_date":"05/04/2031"}`, nil), 400)
	want(t, "impossible date", e.call("POST", id+"/tasks", alice, `{"title":"x","due_date":"2031-02-30"}`, nil), 400)

	// A member edits title, description and due date; other fields survive.
	tp := "/api/tasks/" + itoa(task.ID)
	want(t, "member edits", e.call("PATCH", tp, bob, `{"title":"Book hotel","due_date":"2031-06-01"}`, &task), 200)
	if task.Title != "Book hotel" || task.Description != "flights" || task.DueDate != "2031-06-01" {
		t.Errorf("after edit: %+v", task)
	}
	// Decode into fresh values: an empty due_date is omitted from the JSON, so
	// decoding into `task` would keep its old value and hide the result.
	var cleared, both store.Task
	want(t, "clear due date", e.call("PATCH", tp, alice, `{"due_date":""}`, &cleared), 200)
	if cleared.DueDate != "" || cleared.Title != "Book hotel" {
		t.Errorf("due date should be cleared and the rest kept: %+v", cleared)
	}
	want(t, "edit and tick together", e.call("PATCH", tp, bob, `{"description":"both","done":true}`, &both), 200)
	if both.Description != "both" || !both.Done || both.DoneBy != "Bob" {
		t.Errorf("combined patch: %+v", both)
	}
	want(t, "empty title", e.call("PATCH", tp, alice, `{"title":"   "}`, nil), 400)
	want(t, "empty patch", e.call("PATCH", tp, alice, `{}`, nil), 400)
	want(t, "stranger edits", e.call("PATCH", tp, carol, `{"title":"hax"}`, nil), 404)

	// Renaming: any member may, strangers may not, blanks are refused.
	var renamed store.List
	want(t, "member renames", e.call("PATCH", id, bob, `{"name":"Lisbon trip"}`, &renamed), 200)
	if renamed.Name != "Lisbon trip" {
		t.Errorf("rename: %+v", renamed)
	}
	want(t, "stranger renames", e.call("PATCH", id, carol, `{"name":"mine now"}`, nil), 404)
	want(t, "blank rename", e.call("PATCH", id, alice, `{"name":" "}`, nil), 400)

	// The web form edits in place and returns to the page it came from.
	resp := e.form(alice, "/tasks/"+itoa(task.ID)+"/edit", url.Values{"title": {"Book B&B"}, "description": {""}, "due": {"2031-07-07"}, "next": {"/today"}})
	want(t, "edit form", resp.StatusCode, 303)
	if loc := resp.Header.Get("Location"); loc != "/today" {
		t.Errorf("edit form should return to next, got %q", loc)
	}
	if got := e.tasks(alice, l.ID)[0]; got.Title != "Book B&B" || got.DueDate != "2031-07-07" || got.Description != "" {
		t.Errorf("after form edit: %+v", got)
	}
	resp = e.form(alice, "/tasks/"+itoa(task.ID)+"/edit", url.Values{"title": {"Again"}, "next": {"//evil.example"}})
	if loc := resp.Header.Get("Location"); loc != "/" {
		t.Errorf("next must stay on this site, got %q", loc)
	}

	// Open tasks list soonest-due first, undated last, finished ones after.
	e.call("POST", id+"/tasks", alice, `{"title":"undated"}`, nil)
	e.call("POST", id+"/tasks", alice, `{"title":"soon","due_date":"2030-01-01"}`, nil)
	e.call("PATCH", tp, alice, `{"done":false}`, nil)
	var order []string
	for _, tk := range e.tasks(alice, l.ID) {
		order = append(order, tk.Title)
	}
	if got := strings.Join(order, ","); got != "soon,Again,undated" {
		t.Errorf("task order = %s", got)
	}
}

func TestTodayView(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.register("Alice"), e.register("Bob")
	mine, theirs := e.newList(alice, "Mine"), e.newList(bob, "Bobs")

	day := func(offset int) string { return time.Now().AddDate(0, 0, offset).Format("2006-01-02") }
	add := func(token string, l store.List, title string, due string) store.Task {
		var tk store.Task
		e.call("POST", "/api/lists/"+itoa(l.ID)+"/tasks", token, `{"title":"`+title+`","due_date":"`+due+`"}`, &tk)
		return tk
	}
	add(alice, mine, "was-due-last-week", day(-7))
	add(alice, mine, "due-today", day(0))
	add(alice, mine, "due-in-3-days", day(3))
	add(alice, mine, "due-in-2-months", day(60))
	add(alice, mine, "no-date", "")
	fin := add(alice, mine, "already-done", day(-2))
	e.call("PATCH", "/api/tasks/"+itoa(fin.ID), alice, `{"done":true}`, nil)
	add(bob, theirs, "bobs-secret-overdue", day(-3))

	status, body := e.page(alice, "/today")
	want(t, "today page", status, 200)
	for _, title := range []string{"was-due-last-week", "due-today", "due-in-3-days"} {
		if !strings.Contains(body, title) {
			t.Errorf("Today should list %q", title)
		}
	}
	for _, title := range []string{"due-in-2-months", "no-date", "already-done", "bobs-secret-overdue"} {
		if strings.Contains(body, title) {
			t.Errorf("Today must not list %q", title)
		}
	}
	for _, heading := range []string{"Overdue<", "Today<span", "Next 7 days<"} {
		if !strings.Contains(body, heading) {
			t.Errorf("Today should have a %q section", heading)
		}
	}
	if !strings.Contains(body, "Overdue · ") {
		t.Errorf("overdue tasks should say how late they are")
	}

	// The sidebar badge counts overdue + due today (2 for Alice), red because some are overdue.
	_, home := e.page(alice, "/")
	if !strings.Contains(home, `bg-red-lt text-red">2<`) {
		t.Errorf("sidebar Today badge should show 2 in red")
	}
	// Bob has one overdue task of his own and sees only that.
	_, bobToday := e.page(bob, "/today")
	if !strings.Contains(bobToday, "bobs-secret-overdue") || strings.Contains(bobToday, "due-today") {
		t.Errorf("Bob's Today view should contain only his own tasks")
	}
	// Someone with nothing due sees the empty state.
	carol := e.register("Carol")
	_, empty := e.page(carol, "/today")
	if !strings.Contains(empty, "Nothing due") {
		t.Errorf("empty Today view should say so")
	}
}

func TestDeletedTasksCanBeRestored(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.register("Alice"), e.register("Bob")
	l := e.newList(alice, "Chores")
	var task store.Task
	e.call("POST", "/api/lists/"+itoa(l.ID)+"/tasks", alice, `{"title":"Dishes"}`, &task)
	e.call("POST", "/api/lists/"+itoa(l.ID)+"/tasks", alice, `{"title":"Laundry"}`, nil)

	del := e.form(alice, "/tasks/"+itoa(task.ID)+"/delete", url.Values{"next": {"/lists/" + itoa(l.ID)}})
	want(t, "delete", del.StatusCode, 303)
	if got := e.tasks(alice, l.ID); len(got) != 1 || got[0].Title != "Laundry" {
		t.Fatalf("deleted task should vanish: %+v", got)
	}
	var lists []store.List
	e.call("GET", "/api/lists", alice, "", &lists)
	if lists[0].Total != 1 || lists[0].Open != 1 {
		t.Errorf("counts should ignore deleted tasks: %+v", lists[0])
	}
	// A deleted task can't be ticked, edited or deleted again.
	want(t, "tick deleted", e.call("PATCH", "/api/tasks/"+itoa(task.ID), alice, `{"done":true}`, nil), 404)
	want(t, "delete twice", e.call("DELETE", "/api/tasks/"+itoa(task.ID), alice, "", nil), 404)

	want(t, "stranger restores", e.form(bob, "/tasks/"+itoa(task.ID)+"/restore", nil).StatusCode, 404)
	want(t, "owner restores", e.form(alice, "/tasks/"+itoa(task.ID)+"/restore", url.Values{"next": {"/lists/" + itoa(l.ID)}}).StatusCode, 303)
	if got := e.tasks(alice, l.ID); len(got) != 2 {
		t.Errorf("restored task should be back: %+v", got)
	}
	want(t, "restore a live task", e.form(alice, "/tasks/"+itoa(task.ID)+"/restore", nil).StatusCode, 404)
}

// events opens /events as a person and returns a channel that receives once
// per "changed" signal. Call the returned func to disconnect.
func (e *env) events(token string) (<-chan struct{}, context.CancelFunc) {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		e.t.Fatal(err)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		cancel()
		e.t.Fatalf("Content-Type = %q", ct)
	}
	got := make(chan struct{}, 16)
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if sc.Text() == "event: changed" {
				got <- struct{}{}
			}
		}
	}()
	return got, cancel
}

func expectSignal(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Errorf("%s: no update arrived", what)
	}
}

// expectQuiet fails if a signal arrives shortly; a pending signal is drained
// first with drain=true so leftovers from earlier steps don't count.
func expectQuiet(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
		t.Errorf("%s: got an update it should not have seen", what)
	case <-time.After(400 * time.Millisecond):
	}
}

func TestLiveUpdatesReachOnlyThePeopleWhoCanSeeTheList(t *testing.T) {
	e := newEnv(t)
	alice, bob, carol := e.register("Alice"), e.register("Bob"), e.register("Carol")
	l := e.newList(alice, "Shared")
	e.call("POST", "/api/join/"+l.InviteCode, bob, "", nil)
	id := "/api/lists/" + itoa(l.ID)

	bobEvents, stopBob := e.events(bob)
	defer stopBob()
	carolEvents, stopCarol := e.events(carol)
	defer stopCarol()
	expectQuiet(t, "just connecting", bobEvents)

	var task store.Task
	e.call("POST", id+"/tasks", alice, `{"title":"Buy tickets"}`, &task)
	expectSignal(t, "Bob sees a new task", bobEvents)
	expectQuiet(t, "Carol (not a member) sees a new task", carolEvents)

	e.call("PATCH", "/api/tasks/"+itoa(task.ID), alice, `{"done":true}`, nil)
	expectSignal(t, "Bob sees a task ticked", bobEvents)
	e.call("PATCH", "/api/tasks/"+itoa(task.ID), alice, `{"title":"Buy train tickets"}`, nil)
	expectSignal(t, "Bob sees an edit", bobEvents)
	e.call("PATCH", id, alice, `{"name":"Trip"}`, nil)
	expectSignal(t, "Bob sees a rename", bobEvents)
	e.form(alice, "/tasks/"+itoa(task.ID)+"/delete", nil)
	expectSignal(t, "Bob sees a delete", bobEvents)
	expectQuiet(t, "Carol sees anything at all", carolEvents)

	// Removing Bob tells Bob too, so his page can react to losing access.
	bobID := findMember(e, alice, l.ID, "Bob")
	e.form(alice, "/lists/"+itoa(l.ID)+"/members/"+itoa(bobID)+"/remove", nil)
	expectSignal(t, "Bob learns he was removed", bobEvents)
	e.call("POST", id+"/tasks", alice, `{"title":"private now"}`, nil)
	expectQuiet(t, "removed Bob sees later changes", bobEvents)

	// Public lists are visible to everyone, so changes reach everyone.
	pub := e.legacyPublicList("Public")
	expectSignal(t, "Carol sees a public list appear", carolEvents)
	e.call("POST", "/api/lists/"+itoa(pub.ID)+"/tasks", "", `{"title":"hello"}`, nil)
	expectSignal(t, "Carol sees a public task", carolEvents)
}

func TestProfileDialogBackend(t *testing.T) {
	e := newEnv(t)
	alice := e.register("Alice")
	other := e.register("Other")
	l := e.newList(alice, "Home")

	// The old /me page is gone; its URL opens the dialog on the home page.
	req, _ := http.NewRequest("GET", e.srv.URL+"/me", nil)
	req.Header.Set("Authorization", "Bearer "+alice)
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	want(t, "GET /me", resp.StatusCode, 303)
	if loc := resp.Header.Get("Location"); loc != "/#profile" {
		t.Errorf("GET /me redirects to %q", loc)
	}

	// Pages must not contain the token; it is only served on request, uncached.
	_, page := e.page(alice, "/")
	if strings.Contains(page, alice) {
		t.Errorf("the sign-in token must not be written into pages")
	}
	if !strings.Contains(page, `id="profile-modal"`) {
		t.Errorf("pages should include the profile dialog")
	}
	req, _ = http.NewRequest("GET", e.srv.URL+"/me/token", nil)
	req.Header.Set("Authorization", "Bearer "+alice)
	resp, err = noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ Token string }
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got.Token != alice || resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("/me/token: token match=%v, Cache-Control=%q", got.Token == alice, resp.Header.Get("Cache-Control"))
	}
	anon, _ := http.NewRequest("GET", e.srv.URL+"/me/token", nil)
	resp, _ = noRedirect.Do(anon)
	resp.Body.Close()
	want(t, "anonymous /me/token", resp.StatusCode, 303)

	// Renaming updates every place the name shows, and returns to the page it came from.
	e.call("POST", "/api/join/"+l.InviteCode, other, "", nil)
	e.call("POST", "/api/lists/"+itoa(l.ID)+"/tasks", alice, `{"title":"Water plants"}`, nil)
	r := e.form(alice, "/me", url.Values{"name": {"Alicia"}, "next": {"/lists/" + itoa(l.ID)}})
	want(t, "rename self", r.StatusCode, 303)
	if loc := r.Header.Get("Location"); loc != "/lists/"+itoa(l.ID) {
		t.Errorf("rename returns to %q", loc)
	}
	if got := e.tasks(other, l.ID)[0].AddedBy; got != "Alicia" {
		t.Errorf("tasks should show the new name, got %q", got)
	}
	e.form(alice, "/me", url.Values{"name": {"   "}})
	var me store.User
	e.call("GET", "/api/me", alice, "", &me)
	if me.Name != "Alicia" {
		t.Errorf("a blank name must leave the old one, got %q", me.Name)
	}
}

func TestDueLabels(t *testing.T) {
	const today = "2026-09-20"
	for _, c := range []struct {
		due, want string
		done      bool
	}{
		{due: "2026-09-20", want: "Today"},
		{due: "2026-09-21", want: "Tomorrow"},
		{due: "2026-09-19", want: "Yesterday"},
		{due: "2026-09-17", want: "Overdue · Sep 17"},
		{due: "2026-09-27", want: "Sep 27"},
		{due: "2027-01-04", want: "Jan 4, 2027"},
		// A finished task is never late, so it keeps a plain date.
		{due: "2026-09-17", done: true, want: "Sep 17"},
		{due: "2026-09-19", done: true, want: "Sep 19"},
		{due: "2026-09-20", done: true, want: "Today"},
		{due: "not-a-date", want: "not-a-date"},
	} {
		if got := dueLabel(c.due, today, c.done); got != c.want {
			t.Errorf("dueLabel(%q, done=%v) = %q, want %q", c.due, c.done, got, c.want)
		}
	}

	// An overdue task's badge is red, a finished one's is muted.
	if got := dueClass("2026-09-17", today, false); !strings.Contains(got, "red") {
		t.Errorf("overdue badge = %q, want red", got)
	}
	if got := dueClass("2026-09-17", today, true); !strings.Contains(got, "secondary") {
		t.Errorf("finished badge = %q, want muted", got)
	}
}

func TestAppIsInstallable(t *testing.T) {
	e := newEnv(t)
	get := func(path string) (*http.Response, []byte) {
		resp, err := http.Get(e.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var b strings.Builder
		readAll(&b, resp)
		return resp, []byte(b.String())
	}

	// The manifest and icons are public (browsers fetch them without cookies).
	resp, body := get("/manifest.webmanifest")
	want(t, "manifest", resp.StatusCode, 200)
	if ct := resp.Header.Get("Content-Type"); ct != "application/manifest+json" {
		t.Errorf("manifest Content-Type = %q", ct)
	}
	var tagged struct {
		Name     string `json:"name"`
		StartURL string `json:"start_url"`
		Display  string `json:"display"`
		Icons    []struct {
			Src   string `json:"src"`
			Sizes string `json:"sizes"`
		} `json:"icons"`
	}
	if err := json.Unmarshal(body, &tagged); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	if tagged.Name != "Doables" || tagged.StartURL != "/" || tagged.Display != "standalone" || len(tagged.Icons) < 2 {
		t.Errorf("manifest: %+v", tagged)
	}
	for _, ic := range tagged.Icons {
		r, img := get(ic.Src)
		want(t, "icon "+ic.Src, r.StatusCode, 200)
		if ct := r.Header.Get("Content-Type"); ct != "image/png" || len(img) < 100 || string(img[1:4]) != "PNG" {
			t.Errorf("%s: Content-Type %q, %d bytes", ic.Src, ct, len(img))
		}
	}
	r, _ := get("/static/apple-touch-icon.png")
	want(t, "apple touch icon", r.StatusCode, 200)
	r, _ = get("/static/nope.png")
	want(t, "missing static file", r.StatusCode, 404)

	// Pages link to it and can use the full screen on notched phones.
	alice := e.register("Alice")
	_, page := e.page(alice, "/")
	for _, needle := range []string{`rel="manifest"`, `name="theme-color"`, `rel="apple-touch-icon"`, "viewport-fit=cover", `class="mobile-tabs`} {
		if !strings.Contains(page, needle) {
			t.Errorf("page is missing %q", needle)
		}
	}
}

func TestEventsRequireSignIn(t *testing.T) {
	e := newEnv(t)
	req, _ := http.NewRequest("GET", e.srv.URL+"/events", nil)
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	want(t, "anonymous /events", resp.StatusCode, 303)
}
