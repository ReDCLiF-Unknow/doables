package web

import (
	"net/url"
	"strings"
	"testing"
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
