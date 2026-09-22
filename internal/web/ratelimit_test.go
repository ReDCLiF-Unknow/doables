package web

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestLimiterRefillsOverTime(t *testing.T) {
	clock := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	l := newLimiter(3, time.Minute)
	l.now = func() time.Time { return clock }

	for i := 1; i <= 3; i++ {
		if !l.allow("10.0.0.1") {
			t.Fatalf("attempt %d of the burst was refused", i)
		}
	}
	if l.allow("10.0.0.1") {
		t.Error("a fourth attempt got through the burst")
	}
	// Somebody else is unaffected.
	if !l.allow("10.0.0.2") {
		t.Error("a different address was caught by someone else's limit")
	}

	// One token comes back per minute, and no more than the burst ever.
	clock = clock.Add(90 * time.Second)
	if !l.allow("10.0.0.1") {
		t.Error("nothing had refilled after a minute and a half")
	}
	if l.allow("10.0.0.1") {
		t.Error("more than one token came back in 90 seconds")
	}
	clock = clock.Add(time.Hour)
	for i := 1; i <= 3; i++ {
		if !l.allow("10.0.0.1") {
			t.Fatalf("after an hour, attempt %d was refused", i)
		}
	}
	if l.allow("10.0.0.1") {
		t.Error("an idle hour refilled past the burst")
	}
}

func TestSweepForgetsIdleAddresses(t *testing.T) {
	clock := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	l := newLimiter(2, time.Minute)
	l.now = func() time.Time { return clock }
	for i := 0; i < 1200; i++ {
		l.allow(strings.Repeat("a", i%50) + string(rune(i)))
	}
	before := len(l.buckets)
	clock = clock.Add(time.Hour)
	l.allow("someone-new")
	if len(l.buckets) >= before {
		t.Errorf("idle addresses were not forgotten: %d before, %d after", before, len(l.buckets))
	}
}

// Creating identities is the only thing a stranger can do here, so it is the
// only thing that has to be capped.
func TestSignupsAreCapped(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < signupBurst; i++ {
		if c := e.call("POST", "/api/users", "", `{"name":"Someone"}`, nil); c != 201 {
			t.Fatalf("signup %d: status %d, want 201", i+1, c)
		}
	}
	want(t, "one signup too many", e.call("POST", "/api/users", "", `{"name":"Spammer"}`, nil), 429)

	// The web form says so on the page rather than dumping plain text, and
	// keeps the invite the person was heading for.
	resp := e.form("", "/welcome", url.Values{"name": {"Spammer"}, "next": {"/lists/1"}})
	want(t, "the form too", resp.StatusCode, 429)
	if got := resp.Header.Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After is %q, want 60", got)
	}

	// Somebody who already has an identity is unaffected: the cap is on
	// becoming a person, not on being one.
	e2 := newEnv(t)
	alice := e2.register("Alice")
	for i := 0; i < signupBurst+5; i++ {
		if c := e2.call("POST", "/api/lists", alice, `{"name":"List"}`, nil); c != 201 {
			t.Fatalf("list %d: status %d", i+1, c)
		}
	}
}
