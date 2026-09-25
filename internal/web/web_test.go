package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"doables/internal/store"
)

type env struct {
	t   *testing.T
	srv *httptest.Server
	st  *store.Store // for making the kind of data only an old database has
	app *Server      // for moving its clock on
}

func newEnv(t *testing.T) *env {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	app := New(s)
	srv := httptest.NewServer(app)
	t.Cleanup(func() { srv.Close(); s.Close() })
	return &env{t: t, srv: srv, st: s, app: app}
}

// legacyPublicList makes an ownerless list, the sort that exists in databases
// from before sharing. The app will not make one any more, but it still has to
// cope with the ones that are out there.
func (e *env) legacyPublicList(name string) store.List {
	e.t.Helper()
	l, err := e.st.CreateList(name, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	return l
}

// noRedirect lets tests inspect redirects instead of following them.
var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// call makes a request; token (if any) is sent as a bearer token. The
// response body is decoded into out when out is non-nil.
func (e *env) call(method, path, token string, body string, out any) int {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func (e *env) register(name string) string {
	e.t.Helper()
	var u struct {
		Token string `json:"token"`
	}
	if c := e.call("POST", "/api/users", "", `{"name":"`+name+`"}`, &u); c != 201 || u.Token == "" {
		e.t.Fatalf("register %s: status %d", name, c)
	}
	return u.Token
}

func (e *env) newList(token, name string) store.List {
	e.t.Helper()
	var l store.List
	if c := e.call("POST", "/api/lists", token, `{"name":"`+name+`"}`, &l); c != 201 {
		e.t.Fatalf("create list: status %d", c)
	}
	return l
}

func want(t *testing.T, what string, got, expect int) {
	t.Helper()
	if got != expect {
		t.Errorf("%s: got %d, want %d", what, got, expect)
	}
}

func TestPrivateListsAndInvites(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.register("Alice"), e.register("Bob")

	l := e.newList(alice, "Trip")
	if l.Public() || l.InviteCode == "" || l.Members != 1 {
		t.Fatalf("new list should be private with Alice as its only member: %+v", l)
	}
	id := "/api/lists/" + itoa(l.ID)
	e.call("POST", id+"/tasks", alice, `{"title":"Book flights"}`, nil)

	// Bob can't see it, and it looks exactly like a list that doesn't exist.
	want(t, "stranger reads tasks", e.call("GET", id+"/tasks", bob, "", nil), 404)
	want(t, "stranger adds task", e.call("POST", id+"/tasks", bob, `{"title":"x"}`, nil), 404)
	want(t, "stranger toggles task", e.call("PATCH", "/api/tasks/1", bob, `{"done":true}`, nil), 404)
	want(t, "stranger deletes task", e.call("DELETE", "/api/tasks/1", bob, "", nil), 404)
	want(t, "stranger deletes list", e.call("DELETE", id, bob, "", nil), 404)
	want(t, "anonymous reads tasks", e.call("GET", id+"/tasks", "", "", nil), 404)
	var bobLists []store.List
	e.call("GET", "/api/lists", bob, "", &bobLists)
	if len(bobLists) != 0 {
		t.Errorf("Bob should see no lists, got %+v", bobLists)
	}

	// A wrong invite code gets nowhere; the right one joins.
	want(t, "bad invite", e.call("POST", "/api/join/nope", bob, "", nil), 404)
	want(t, "join without a token", e.call("POST", "/api/join/"+l.InviteCode, "", "", nil), 401)
	want(t, "join", e.call("POST", "/api/join/"+l.InviteCode, bob, "", nil), 200)

	// Members collaborate, and tasks remember who did what.
	want(t, "member adds task", e.call("POST", id+"/tasks", bob, `{"title":"Pack bags"}`, nil), 201)
	want(t, "member finishes task", e.call("PATCH", "/api/tasks/1", bob, `{"done":true}`, nil), 200)
	var tasks []store.Task
	e.call("GET", id+"/tasks", alice, "", &tasks)
	byTitle := map[string]store.Task{}
	for _, tk := range tasks {
		byTitle[tk.Title] = tk
	}
	if got := byTitle["Book flights"]; !got.Done || got.AddedBy != "Alice" || got.DoneBy != "Bob" {
		t.Errorf("Book flights: %+v", got)
	}
	if got := byTitle["Pack bags"]; got.AddedBy != "Bob" || got.Done || got.DoneBy != "" {
		t.Errorf("Pack bags: %+v", got)
	}
	// Un-ticking clears who finished it.
	e.call("PATCH", "/api/tasks/1", alice, `{"done":false}`, &store.Task{})
	e.call("GET", id+"/tasks", alice, "", &tasks)
	for _, tk := range tasks {
		if tk.ID == 1 && (tk.Done || tk.DoneBy != "") {
			t.Errorf("un-ticked task still credited: %+v", tk)
		}
	}

	// Only the owner may delete the list.
	want(t, "member deletes list", e.call("DELETE", id, bob, "", nil), 403)
	var ms []store.Member
	e.call("GET", id+"/members", bob, "", &ms)
	if len(ms) != 2 || !ms[0].Owner || ms[0].Name != "Alice" || ms[1].Name != "Bob" {
		t.Errorf("members: %+v", ms)
	}
	want(t, "owner deletes list", e.call("DELETE", id, alice, "", nil), 204)
}

func TestRemovingMembersAndResettingInvites(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.register("Alice"), e.register("Bob")
	l := e.newList(alice, "Shared")
	e.call("POST", "/api/join/"+l.InviteCode, bob, "", nil)

	// Alice removes Bob through the web UI (form post with her token as bearer).
	post := func(token, path string, form url.Values) int {
		req, _ := http.NewRequest("POST", e.srv.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	base := "/lists/" + itoa(l.ID)
	bobID := findMember(e, alice, l.ID, "Bob")

	want(t, "member removes owner's guest", post(bob, base+"/members/"+itoa(bobID)+"/remove", nil), 403)
	want(t, "owner removes Bob", post(alice, base+"/members/"+itoa(bobID)+"/remove", nil), 303)
	want(t, "removed member is locked out", e.call("GET", "/api/lists/"+itoa(l.ID)+"/tasks", bob, "", nil), 404)

	// The old link keeps working until reset, then stops.
	old := l.InviteCode
	want(t, "reset by non-owner", post(bob, base+"/invite/reset", nil), 404)
	want(t, "reset by owner", post(alice, base+"/invite/reset", nil), 303)
	want(t, "old link after reset", e.call("POST", "/api/join/"+old, bob, "", nil), 404)
}

func TestPublicListsCanBeClaimed(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.register("Alice"), e.register("Bob")

	// A list from before sharing existed belongs to nobody, so everyone can use it.
	pub := e.legacyPublicList("Old list")
	if !pub.Public() {
		t.Fatalf("an ownerless list should be public: %+v", pub)
	}
	id := "/api/lists/" + itoa(pub.ID)
	want(t, "anonymous adds to public list", e.call("POST", id+"/tasks", "", `{"title":"a"}`, nil), 201)
	want(t, "bob reads public list", e.call("GET", id+"/tasks", bob, "", nil), 200)

	// Alice claims it through the UI; from then on Bob is locked out.
	req, _ := http.NewRequest("POST", e.srv.URL+"/lists/"+itoa(pub.ID)+"/claim", nil)
	req.Header.Set("Authorization", "Bearer "+alice)
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	want(t, "claim", resp.StatusCode, 303)
	want(t, "bob after claim", e.call("GET", id+"/tasks", bob, "", nil), 404)
	want(t, "alice after claim", e.call("GET", id+"/tasks", alice, "", nil), 200)
	want(t, "anonymous after claim", e.call("GET", id+"/tasks", "", "", nil), 404)
}

func TestWebSignInAndJoinFlow(t *testing.T) {
	e := newEnv(t)
	alice := e.register("Alice")
	l := e.newList(alice, "Party <planning>")

	get := func(path string, cookies ...*http.Cookie) *http.Response {
		req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
		for _, c := range cookies {
			req.AddCookie(c)
		}
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Someone without a name is sent to the welcome page, then back.
	resp := get("/lists/" + itoa(l.ID))
	want(t, "anonymous page", resp.StatusCode, 303)
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/welcome?next=") {
		t.Errorf("redirect to %q", loc)
	}

	// The invite link shows the list name (escaped) and asks for a name.
	resp = get("/join/" + l.InviteCode)
	want(t, "invite page", resp.StatusCode, 200)
	buf := new(strings.Builder)
	readAll(buf, resp)
	if !strings.Contains(buf.String(), "Party &lt;planning&gt;") || !strings.Contains(buf.String(), `name="name"`) {
		t.Errorf("invite page missing list name or name field")
	}

	// Submitting a name creates the person, sets the cookie and joins.
	post := func(path string, form url.Values) *http.Response {
		req, _ := http.NewRequest("POST", e.srv.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp = post("/join/"+l.InviteCode, url.Values{"name": {"Dana"}})
	want(t, "join post", resp.StatusCode, 303)
	var session *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			session = c
		}
	}
	if session == nil || !session.HttpOnly || session.Value == "" {
		t.Fatalf("expected an HttpOnly session cookie, got %+v", session)
	}

	resp = get("/lists/"+itoa(l.ID), session)
	want(t, "list page as Dana", resp.StatusCode, 200)
	buf.Reset()
	readAll(buf, resp)
	if !strings.Contains(buf.String(), "Dana") || !strings.Contains(buf.String(), "Alice") {
		t.Errorf("list page should show both members")
	}

	// A blank name is rejected; an open-redirect target is ignored.
	want(t, "blank name", post("/welcome", url.Values{"name": {"  "}}).StatusCode, 400)
	resp = post("/welcome", url.Values{"name": {"Eve"}, "next": {"//evil.example"}})
	want(t, "welcome post", resp.StatusCode, 303)
	if loc := resp.Header.Get("Location"); loc != "/" {
		t.Errorf("open redirect: Location = %q", loc)
	}

	// Signing in on another device with the token works; junk tokens don't.
	want(t, "bad token", post("/welcome/token", url.Values{"token": {"junk"}}).StatusCode, 400)
	resp = post("/welcome/token", url.Values{"token": {alice}, "next": {"/"}})
	want(t, "good token", resp.StatusCode, 303)
}

// ---- small helpers ----

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func readAll(b *strings.Builder, resp *http.Response) {
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	b.Write(data)
}

func findMember(e *env, token string, listID int64, name string) int64 {
	var ms []store.Member
	e.call("GET", "/api/lists/"+itoa(listID)+"/members", token, "", &ms)
	for _, m := range ms {
		if m.Name == name {
			return m.ID
		}
	}
	e.t.Fatalf("member %q not found", name)
	return 0
}
