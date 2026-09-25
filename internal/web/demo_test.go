package web

import (
	"net/url"
	"strings"
	"testing"

	"doables/internal/demo"
	"doables/internal/store"
)

// In demo mode, picking a name lands you inside a filled-in list of your own,
// and every page says that nothing here lasts.
func TestTheDemoStartsSomewhereUseful(t *testing.T) {
	e := newEnv(t)
	app := e.srv.Config.Handler.(*Server)
	app.SetDemo(func(u store.User) (int64, error) { return demo.Seed(e.st, u) })

	_, welcome := e.page("", "/welcome")
	if !strings.Contains(welcome, "This is a demo") {
		t.Error("the welcome page does not say it is a demo")
	}

	resp := e.post("/welcome", url.Values{"name": {"Visitor"}}, nil)
	want(t, "picking a name", resp.StatusCode, 303)
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/lists/") {
		t.Fatalf("a new visitor was sent to %q, want straight into a list", loc)
	}
	c := sessionCookie(resp)
	if c == nil {
		t.Fatal("picking a name set no session")
	}

	_, page := e.page(c.Value, loc)
	for _, want := range []string{"Weekend in Lisbon", "This is a demo", "Get Doables", "2 new"} {
		if !strings.Contains(page, want) {
			t.Errorf("the visitor's first page does not show %q", want)
		}
	}
	// The key reminder means nothing in a sandbox that is erased tomorrow.
	if strings.Contains(page, "Save your sign-in key") {
		t.Error("the demo nags about saving a key for an account that lasts a day")
	}

	// Someone who was on their way somewhere, such as an invite, goes there.
	resp = e.post("/welcome", url.Values{"name": {"Invited"}, "next": {"/today"}}, nil)
	if got := resp.Header.Get("Location"); got != "/today" {
		t.Errorf("a visitor heading for /today was sent to %q", got)
	}
}

func TestAnOrdinaryServerIsNoDemo(t *testing.T) {
	e := newEnv(t)
	if _, welcome := e.page("", "/welcome"); strings.Contains(welcome, "This is a demo") {
		t.Error("a normal server calls itself a demo")
	}
	resp := e.post("/welcome", url.Values{"name": {"Alex"}}, nil)
	if got := resp.Header.Get("Location"); got != "/" {
		t.Errorf("a normal sign-up was sent to %q, want the empty dashboard", got)
	}
	_, page := e.page(sessionCookie(resp).Value, "/")
	if strings.Contains(page, "This is a demo") || strings.Contains(page, "Weekend in Lisbon") {
		t.Error("a normal account was given demo content")
	}
}
