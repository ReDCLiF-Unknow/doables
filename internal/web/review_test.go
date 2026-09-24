package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"doables/internal/store"
)

// Tests for the findings of the September 2026 code review.

// post sends a form with the given extra headers, as a browser on some page
// would, and returns the response unread.
func (e *env) post(path string, form url.Values, headers map[string]string) *http.Response {
	e.t.Helper()
	req, _ := http.NewRequest("POST", e.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// members lists who is on a list, as seen by token.
func (e *env) members(token string, listID int64) []store.Member {
	e.t.Helper()
	var ms []store.Member
	e.call("GET", "/api/lists/"+itoa(listID)+"/members", token, "", &ms)
	return ms
}

func sessionCookie(resp *http.Response) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			return c
		}
	}
	return nil
}

// 1. The token is the whole account, so its cookie must be Secure whenever the
// browser is on HTTPS, including behind a proxy that terminated TLS.
func TestSessionCookieIsSecureBehindAnHTTPSProxy(t *testing.T) {
	e := newEnv(t)
	token := e.register("Alex")
	form := url.Values{"token": {token}}

	behindProxy := e.post("/welcome/token", form, map[string]string{"X-Forwarded-Proto": "https"})
	c := sessionCookie(behindProxy)
	if c == nil {
		t.Fatalf("signing in behind a proxy set no cookie (status %d)", behindProxy.StatusCode)
	}
	if !c.Secure {
		t.Error("behind an HTTPS proxy the session cookie is not Secure, so it could leak over plain HTTP")
	}

	// Plain HTTP with no proxy, as on a laptop at localhost: Secure would stop
	// the browser sending the cookie at all, so it must be off.
	direct := sessionCookie(e.post("/welcome/token", form, nil))
	if direct == nil || direct.Secure {
		t.Error("over plain HTTP the cookie should be set, and not Secure")
	}
}

// 2. Another site must not be able to sign you in as someone else. That would
// replace your token, which is the only way back into your account.
func TestAnotherSiteCannotSignYouIn(t *testing.T) {
	e := newEnv(t)
	attacker := e.register("Mallory")
	form := url.Values{"token": {attacker}}

	for name, headers := range map[string]map[string]string{
		"a modern browser on another site": {"Sec-Fetch-Site": "cross-site"},
		"an older browser on another site": {"Origin": "https://evil.example"},
	} {
		resp := e.post("/welcome/token", form, headers)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", name, resp.StatusCode)
		}
		if sessionCookie(resp) != nil {
			t.Errorf("%s: the response still set a session cookie", name)
		}
	}

	// The same protection covers everything that changes something.
	alex := e.register("Alex")
	l := e.newList(alex, "Weekend")
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/lists/"+itoa(l.ID)+"/tasks", strings.NewReader(`{"title":"x"}`))
	req.Header.Set("Authorization", "Bearer "+alex)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	want(t, "a cross-site write to the API", resp.StatusCode, http.StatusForbidden)

	// What must keep working: this site's own forms, the CLI (which sends no
	// browser headers at all), and opening an invite link that was sent from
	// somewhere else, which is a plain GET.
	want(t, "the site's own sign-in form",
		e.post("/welcome/token", url.Values{"token": {alex}}, map[string]string{"Sec-Fetch-Site": "same-origin"}).StatusCode, 303)
	want(t, "a client with no browser headers", e.call("POST", "/api/lists", alex, `{"name":"From the CLI"}`, nil), 201)
	req, _ = http.NewRequest("GET", e.srv.URL+"/join/"+l.InviteCode, nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err = noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	want(t, "opening an invite link from a chat app", resp.StatusCode, 200)
}

// 3. Joining through an invite link creates an identity, so it must count
// against the same limit as signing up.
func TestInviteLinksAreNoWayRoundTheSignupLimit(t *testing.T) {
	e := newEnv(t)
	alex := e.register("Alex") // one of the burst
	l := e.newList(alex, "Weekend")
	for i := 1; i < signupBurst; i++ {
		e.register("Someone")
	}

	resp := e.post("/join/"+l.InviteCode, url.Values{"name": {"One too many"}}, nil)
	want(t, "an anonymous join after the limit", resp.StatusCode, http.StatusTooManyRequests)
	if n := len(e.members(alex, l.ID)); n != 1 {
		t.Errorf("the refused join still added someone: %d members", n)
	}

	// Somebody who already has an identity is not creating one.
	bob := e.post("/join/"+l.InviteCode, url.Values{}, map[string]string{"Authorization": "Bearer " + alex})
	if bob.StatusCode == http.StatusTooManyRequests {
		t.Error("a signed-in person joining was caught by the signup limit")
	}
}

// 4. One token must not be able to hold open any number of live-update streams.
func TestLiveUpdateStreamsAreCappedPerPerson(t *testing.T) {
	e := newEnv(t)
	alex, sam := e.register("Alex"), e.register("Sam")

	open := func(token string) (int, context.CancelFunc) {
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/events", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
		}
		return resp.StatusCode, cancel
	}

	var cancels []context.CancelFunc
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()
	for i := 0; i < maxStreams; i++ {
		code, cancel := open(alex)
		cancels = append(cancels, cancel)
		if code != 200 {
			t.Fatalf("stream %d of %d was refused: %d", i+1, maxStreams, code)
		}
	}
	code, cancel := open(alex)
	cancel()
	want(t, "one stream more than the cap", code, http.StatusTooManyRequests)

	// It is per person: Sam is unaffected by Alex's tabs.
	code, cancel = open(sam)
	cancels = append(cancels, cancel)
	want(t, "someone else's stream", code, 200)

	// Closing a tab frees its place.
	cancels[0]()
	deadline := time.Now().Add(3 * time.Second)
	for {
		code, cancel = open(alex)
		cancels = append(cancels, cancel)
		if code == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("closing a stream never freed a place for a new one")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
