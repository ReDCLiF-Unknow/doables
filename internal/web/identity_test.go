package web

import (
	"net/url"
	"strings"
	"testing"

	"doables/internal/store"
)

// There are no passwords here: whoever holds the token is the account, and
// losing it cannot be undone. So the app says so until the person says they
// have it somewhere.
func TestTheKeyReminderStaysUntilAcknowledged(t *testing.T) {
	e := newEnv(t)
	alice := e.register("Alice")

	_, page := e.page(alice, "/")
	if !strings.Contains(page, "Save your sign-in key") {
		t.Fatal("a new account is not told to save its key")
	}

	resp := e.form(alice, "/me/token/saved", url.Values{"next": {"/"}})
	if resp.StatusCode != 303 {
		t.Fatalf("acknowledging: status %d, want 303", resp.StatusCode)
	}
	if _, page = e.page(alice, "/"); strings.Contains(page, "Save your sign-in key") {
		t.Error("the reminder is still there after being acknowledged")
	}

	// It stays gone, including on other pages and after signing in elsewhere.
	for _, path := range []string{"/today", "/mine"} {
		if _, p := e.page(alice, path); strings.Contains(p, "Save your sign-in key") {
			t.Errorf("the reminder came back on %s", path)
		}
	}

	// Somebody else's acknowledgement is not mine.
	bob := e.register("Bob")
	if _, p := e.page(bob, "/"); !strings.Contains(p, "Save your sign-in key") {
		t.Error("a second person was not asked to save their key")
	}

	// It cannot be dismissed by someone who is not signed in.
	resp = e.form("", "/me/token/saved", url.Values{})
	if resp.StatusCode == 303 && resp.Header.Get("Location") == "/" {
		t.Error("an anonymous request dismissed somebody's reminder")
	}
}

// A list with no owner is readable and editable by anyone who can reach the
// server. Databases from before sharing have such lists and must keep working,
// but nobody should be able to make one by forgetting a token.
func TestListsCannotBeMadeWithoutAnOwner(t *testing.T) {
	e := newEnv(t)

	var err struct {
		Error string `json:"error"`
	}
	want(t, "anonymous creates a list", e.call("POST", "/api/lists", "", `{"name":"Shed"}`, &err), 401)
	if !strings.Contains(err.Error, "register") {
		t.Errorf("the refusal does not say what to do instead: %q", err.Error)
	}

	// With an identity it works, and the list belongs to them.
	alice := e.register("Alice")
	var l store.List
	want(t, "Alice creates a list", e.call("POST", "/api/lists", alice, `{"name":"Shed"}`, &l), 201)
	if l.Public() {
		t.Errorf("a list made with a token should not be public: %+v", l)
	}

	// The ones already out there still work: readable, editable, claimable.
	old := e.legacyPublicList("From an old database")
	id := "/api/lists/" + itoa(old.ID)
	want(t, "anyone reads it", e.call("GET", id+"/tasks", "", "", nil), 200)
	want(t, "anyone adds to it", e.call("POST", id+"/tasks", "", `{"title":"still works"}`, nil), 201)
	want(t, "claiming it", e.form(alice, "/lists/"+itoa(old.ID)+"/claim", url.Values{}).StatusCode, 303)
	want(t, "and then it is private", e.call("GET", id+"/tasks", "", "", nil), 404)
}
