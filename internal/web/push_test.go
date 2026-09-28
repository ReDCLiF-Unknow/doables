package web

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"doables/internal/push"
	"doables/internal/store"
)

// fakePush records what would have been sent, instead of sending it.
type fakePush struct {
	mu   sync.Mutex
	sent []sent
}

type sent struct {
	to  []int64
	msg push.Message
}

func (f *fakePush) PublicKey() string            { return "BPublicKeyForTests" }
func (f *fakePush) Accepts(endpoint string) bool { return push.Allowed(endpoint, push.Hosts) }
func (f *fakePush) Notify(to []int64, m push.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sent{append([]int64{}, to...), m})
}

// take returns what was sent since last asked.
func (f *fakePush) take() []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.sent
	f.sent = nil
	return out
}

// browserKeys are what a real browser would hand over when subscribing.
func browserKeys(t *testing.T) (p256dh, auth string) {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 16)
	rand.Read(secret)
	enc := base64.RawURLEncoding.EncodeToString
	return enc(k.PublicKey().Bytes()), enc(secret)
}

// subscribe turns notifications on for someone, as their browser would.
func (e *env) subscribe(t *testing.T, token, endpoint string) int {
	t.Helper()
	p, a := browserKeys(t)
	return e.call("POST", "/me/push", token, `{"endpoint":"`+endpoint+`","keys":{"p256dh":"`+p+`","auth":"`+a+`"}}`, nil)
}

func userIDOf(t *testing.T, e *env, token string) int64 {
	t.Helper()
	var u store.User
	e.call("GET", "/api/me", token, "", &u)
	return u.ID
}

func TestWhoIsNotified(t *testing.T) {
	e := newEnv(t)
	fp := &fakePush{}
	e.app.SetPush(fp)
	alex, sam, rae := e.register("Alex"), e.register("Sam"), e.register("Rae")
	l := e.newList(alex, "Weekend in Lisbon")
	for _, tok := range []string{sam, rae} {
		e.call("POST", "/api/join/"+l.InviteCode, tok, "", nil)
	}
	alexID, samID, raeID := userIDOf(t, e, alex), userIDOf(t, e, sam), userIDOf(t, e, rae)
	for i, tok := range []string{alex, sam, rae} {
		want(t, "turning notifications on", e.subscribe(t, tok, "https://fcm.googleapis.com/fcm/send/device"+itoa(int64(i))), 204)
	}
	listPage := "/lists/" + itoa(l.ID)
	e.call("POST", "/api/lists/"+itoa(l.ID)+"/tasks", alex, `{"title":"Book the ferry"}`, nil)
	fp.take()

	// A comment reaches everyone else on the list, from the page or the API,
	// with a link that opens the conversation.
	e.form(sam, "/tasks/1/comments", url.Values{"body": {"Friday, 7pm?"}, "next": {listPage}})
	e.call("POST", "/api/tasks/1/comments", rae, `{"body":"Works for me"}`, nil)
	got := fp.take()
	if len(got) != 2 {
		t.Fatalf("two comments sent %d notifications", len(got))
	}
	if !slices.Equal(got[0].to, []int64{alexID, raeID}) || !slices.Equal(got[1].to, []int64{alexID, samID}) {
		t.Errorf("comments went to %v and %v; want everyone but the writer", got[0].to, got[1].to)
	}
	want := push.Message{Title: "Sam on “Book the ferry”", Body: "Friday, 7pm?", URL: listPage + "#thread-1", Tag: "comment-1"}
	if got[0].msg != want {
		t.Errorf("the comment's notification is %+v, want %+v", got[0].msg, want)
	}

	// Someone who does not want them about comments is left out.
	e.form(rae, "/me/notifications", url.Values{"assigned": {"on"}})
	e.call("POST", "/api/tasks/1/comments", sam, `{"body":"Booked"}`, nil)
	if got := fp.take(); len(got) != 1 || !slices.Equal(got[0].to, []int64{alexID}) {
		t.Errorf("with Rae's comment notifications off, it went to %+v; want only Alex", got)
	}

	// Giving someone a task tells them, and only them; giving it to
	// yourself, or to whoever has it already, tells nobody.
	e.form(alex, "/tasks/1/assign", url.Values{"user": {itoa(samID)}, "next": {listPage}})
	got = fp.take()
	if len(got) != 1 || !slices.Equal(got[0].to, []int64{samID}) || got[0].msg.Title != "Alex gave you “Book the ferry”" || got[0].msg.Body != "In Weekend in Lisbon" {
		t.Errorf("assigning to Sam sent %+v", got)
	}
	e.form(alex, "/tasks/1/assign", url.Values{"user": {itoa(samID)}, "next": {listPage}})
	e.call("PATCH", "/api/tasks/1", alex, `{"assignee_id":`+itoa(alexID)+`}`, nil)
	if got := fp.take(); len(got) != 0 {
		t.Errorf("reassigning to the same person, then to yourself, sent %+v", got)
	}
	e.call("PATCH", "/api/tasks/1", alex, `{"assignee_id":`+itoa(raeID)+`}`, nil)
	if got := fp.take(); len(got) != 1 || !slices.Equal(got[0].to, []int64{raeID}) {
		t.Errorf("assigning to Rae through the API sent %+v", got)
	}

	// New tasks tell nobody unless they asked; then, everyone else who did.
	e.call("POST", "/api/lists/"+itoa(l.ID)+"/tasks", alex, `{"title":"Sunscreen"}`, nil)
	if got := fp.take(); len(got) != 0 {
		t.Errorf("a new task notified people by default: %+v", got)
	}
	e.form(rae, "/me/notifications", url.Values{"added": {"on"}})
	e.form(alex, listPage+"/tasks", url.Values{"title": {"Milk"}, "next": {listPage}})
	got = fp.take()
	if len(got) != 1 || !slices.Equal(got[0].to, []int64{raeID}) || got[0].msg.Title != "Alex added “Milk”" || got[0].msg.Tag != "added-"+itoa(l.ID) {
		t.Errorf("a new task, with Rae asking, sent %+v", got)
	}

	// Turning a device off stops it.
	e.call("POST", "/me/push/delete", rae, `{"endpoint":"https://fcm.googleapis.com/fcm/send/device2"}`, nil)
	e.call("POST", "/api/tasks/1/comments", sam, `{"body":"One more thing"}`, nil)
	if got := fp.take(); len(got) != 1 || slices.Contains(got[0].to, raeID) {
		t.Errorf("Rae turned her only device off and still got %+v", got)
	}
}

func TestTurningNotificationsOn(t *testing.T) {
	e := newEnv(t)
	alex := e.register("Alex")

	// Without a sender, there is nothing to turn on, and the page offers nothing.
	if _, page := e.page(alex, "/"); strings.Contains(page, `id="push-panel"`) {
		t.Error("the profile offers notifications on a server where they are off")
	}
	want(t, "turning on where they are off", e.subscribe(t, alex, "https://fcm.googleapis.com/fcm/send/x"), 404)

	fp := &fakePush{}
	e.app.SetPush(fp)
	_, page := e.page(alex, "/")
	for _, s := range []string{`id="push-panel" data-key="BPublicKeyForTests"`, `name="comments" value="on" checked`, `name="added" value="on">`} {
		if !strings.Contains(page, s) {
			t.Errorf("the profile does not contain %s", s)
		}
	}

	// Only real push services, and only keys a browser would give.
	p, a := browserKeys(t)
	for endpoint, status := range map[string]int{
		"http://fcm.googleapis.com/fcm/send/x":                 400,
		"https://169.254.169.254/latest/meta-data":             400,
		"https://localhost:8080/api/lists":                     400,
		"https://web.push.apple.com/QGx":                       204,
		"https://updates.push.services.mozilla.com/wpush/v2/x": 204,
	} {
		body := `{"endpoint":"` + endpoint + `","keys":{"p256dh":"` + p + `","auth":"` + a + `"}}`
		want(t, "subscribing "+endpoint, e.call("POST", "/me/push", alex, body, nil), status)
	}
	for name, body := range map[string]string{
		"a short key":   `{"endpoint":"https://fcm.googleapis.com/fcm/send/y","keys":{"p256dh":"AAAA","auth":"` + a + `"}}`,
		"no secret":     `{"endpoint":"https://fcm.googleapis.com/fcm/send/y","keys":{"p256dh":"` + p + `","auth":""}}`,
		"not base64url": `{"endpoint":"https://fcm.googleapis.com/fcm/send/y","keys":{"p256dh":"!!!","auth":"` + a + `"}}`,
	} {
		want(t, "subscribing with "+name, e.call("POST", "/me/push", alex, body, nil), 400)
	}
	subs, _ := e.st.PushSubscriptions([]int64{userIDOf(t, e, alex)})
	if len(subs) != 2 {
		t.Errorf("Alex has %d devices on record, want the 2 accepted", len(subs))
	}

	// The settings save, unticked boxes included.
	e.form(alex, "/me/notifications", url.Values{"added": {"on"}})
	if prefs, _ := e.st.NotifyPrefs(userIDOf(t, e, alex)); prefs != (store.NotifyPrefs{Added: true}) {
		t.Errorf("the settings are %+v, want only new tasks", prefs)
	}

	// A test goes to the one who asked.
	want(t, "sending a test", e.call("POST", "/me/push/test", alex, "", nil), 204)
	if got := fp.take(); len(got) != 1 || !slices.Equal(got[0].to, []int64{userIDOf(t, e, alex)}) {
		t.Errorf("the test notification went to %+v", got)
	}

	// The service worker is served from the root, where it can look after
	// every page, as a script.
	resp, body := e.fetch("GET", "/sw.js", "", nil)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/javascript") || !strings.Contains(string(body), "showNotification") {
		t.Errorf("/sw.js: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}
