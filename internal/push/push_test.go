package push

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"doables/internal/store"
)

// browser is one browser's side of a subscription: the keys it would keep
// to itself, so the test can read what it is sent.
type browser struct {
	key  *ecdh.PrivateKey
	auth []byte
}

func newBrowser(t *testing.T) browser {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	rand.Read(auth)
	return browser{key, auth}
}

func (b browser) subscription(userID int64, endpoint string) store.PushSubscription {
	enc := base64.RawURLEncoding.EncodeToString
	return store.PushSubscription{UserID: userID, Endpoint: endpoint,
		P256dh: enc(b.key.PublicKey().Bytes()), Auth: enc(b.auth)}
}

// open decrypts a message as the browser does (RFC 8291, aes128gcm).
func (b browser) open(t *testing.T, body []byte) []byte {
	t.Helper()
	if len(body) < 21 {
		t.Fatalf("a %d-byte message is too short to be one", len(body))
	}
	salt, idLen := body[:16], int(body[20])
	serverPub, ciphertext := body[21:21+idLen], body[21+idLen:]
	if rs := binary.BigEndian.Uint32(body[16:20]); rs < 18 {
		t.Fatalf("record size %d", rs)
	}
	pub, err := ecdh.P256().NewPublicKey(serverPub)
	if err != nil {
		t.Fatalf("the sender's key: %v", err)
	}
	shared, err := b.key.ECDH(pub)
	if err != nil {
		t.Fatal(err)
	}
	info := append(append([]byte("WebPush: info\x00"), b.key.PublicKey().Bytes()...), serverPub...)
	ikm, _ := hkdf.Key(sha256.New, shared, b.auth, string(info), 32)
	cek, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		t.Fatalf("the message does not decrypt with the browser's keys: %v", err)
	}
	// The last record ends with 0x02 and then padding.
	end := len(plain) - 1
	for end >= 0 && plain[end] == 0 {
		end--
	}
	if end < 0 || plain[end] != 2 {
		t.Fatal("no end-of-message marker")
	}
	return plain[:end]
}

func open(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "push.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// pushService stands in for a browser maker's push service.
type pushService struct {
	*httptest.Server
	status int
	got    chan *http.Request
	bodies chan []byte
}

func newPushService(t *testing.T) *pushService {
	ps := &pushService{status: http.StatusCreated, got: make(chan *http.Request, 10), bodies: make(chan []byte, 10)}
	ps.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ps.got <- r
		ps.bodies <- b
		w.WriteHeader(ps.status)
	}))
	t.Cleanup(ps.Close)
	return ps
}

// sender makes a Sender that trusts the test push service.
func sender(t *testing.T, s *store.Store, ps *pushService) *Sender {
	t.Helper()
	u, _ := url.Parse(ps.URL)
	p, err := New(s, "mailto:ops@example.org", []string{u.Hostname()})
	if err != nil {
		t.Fatal(err)
	}
	p.client.Transport = ps.Client().Transport
	return p
}

func TestAMessageReachesTheBrowserReadable(t *testing.T) {
	s := open(t)
	u, _, _ := s.CreateUser("Alex")
	ps := newPushService(t)
	p := sender(t, s, ps)
	b := newBrowser(t)
	if err := s.SavePushSubscription(b.subscription(u.ID, ps.URL+"/send/abc")); err != nil {
		t.Fatal(err)
	}

	msg := Message{Title: "Sam commented on “Book the ferry”", Body: "Friday, 7pm?", URL: "/lists/1#thread-2", Tag: "comments-2"}
	p.Notify([]int64{u.ID}, msg)
	p.deliver(context.Background(), <-p.queue)

	r := <-ps.got
	for header, want := range map[string]string{"Content-Encoding": "aes128gcm", "TTL": "86400", "Urgency": "normal", "Topic": "comments-2"} {
		if got := r.Header.Get(header); got != want {
			t.Errorf("%s: %q, want %q", header, got, want)
		}
	}
	if a := r.Header.Get("Authorization"); !strings.HasPrefix(a, "vapid t=") || !strings.Contains(a, "k="+p.PublicKey()) {
		t.Errorf("the request is not signed with this server's key: %q", a)
	}
	var got Message
	if err := json.Unmarshal(b.open(t, <-ps.bodies), &got); err != nil {
		t.Fatal(err)
	}
	if got != msg {
		t.Errorf("the browser read %+v, want %+v", got, msg)
	}
}

// A browser that has gone (uninstalled, or permission withdrawn) is
// forgotten the first time its push service says so.
func TestGoneBrowsersAreForgotten(t *testing.T) {
	s := open(t)
	u, _, _ := s.CreateUser("Alex")
	ps := newPushService(t)
	ps.status = http.StatusGone
	p := sender(t, s, ps)
	s.SavePushSubscription(newBrowser(t).subscription(u.ID, ps.URL+"/send/old-phone"))

	p.deliver(context.Background(), job{[]int64{u.ID}, Message{Title: "hello", URL: "/"}})
	<-ps.got
	if subs, _ := s.PushSubscriptions([]int64{u.ID}); len(subs) != 0 {
		t.Errorf("a browser its push service says is gone is still kept: %+v", subs)
	}
}

// The server's keys are made once and kept: every subscription depends on them.
func TestKeysAreKept(t *testing.T) {
	s := open(t)
	a, err := New(s, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := New(s, "", nil)
	if a.PublicKey() == "" || a.PublicKey() != b.PublicKey() {
		t.Errorf("the public key changed from %q to %q", a.PublicKey(), b.PublicKey())
	}
}

func TestOnlyPushServicesAreSentTo(t *testing.T) {
	for endpoint, want := range map[string]bool{
		"https://fcm.googleapis.com/fcm/send/abc":                true,
		"https://updates.push.services.mozilla.com/wpush/v2/abc": true,
		"https://web.push.apple.com/QGp0Z":                       true,
		"https://wns2-par02p.notify.windows.com/w/?token=abc":    true,
		"http://fcm.googleapis.com/fcm/send/abc":                 false, // not https
		"https://fcm.googleapis.com.evil.example/x":              false,
		"https://evilfcm.googleapis.com.example/x":               false,
		"https://notfcm.googleapis.com@169.254.169.254/latest":   false,
		"https://169.254.169.254/latest/meta-data":               false,
		"https://localhost:8080/api/lists":                       false,
		"https://user:pass@fcm.googleapis.com/fcm/send/abc":      false,
		"not a url at all":                                       false,
	} {
		if got := Allowed(endpoint, Hosts); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", endpoint, got, want)
		}
	}
	// A server can be told about another push service.
	if !Allowed("https://push.example.org/x", append(Hosts, "push.example.org")) {
		t.Error("an extra push service was not accepted")
	}
}

func TestTopics(t *testing.T) {
	for tag, want := range map[string]string{
		"comments-12":                   "comments-12",
		"added list 3 / ü":              "addedlist3",
		strings.Repeat("abcdefghij", 5): strings.Repeat("abcdefghij", 3) + "ab",
	} {
		if got := topic(tag); got != want {
			t.Errorf("topic(%q) = %q, want %q", tag, got, want)
		}
	}
}

// The sender checks addresses again before sending, not only when a browser
// subscribes: an address kept from before (say, a push service the operator
// has since stopped allowing) is never sent to.
func TestTheSenderChecksAgain(t *testing.T) {
	s := open(t)
	u, _, _ := s.CreateUser("Alex")
	ps := newPushService(t)
	p := sender(t, s, ps)
	// Every address leads to the test push service, certificate unchecked, so
	// the only thing that can keep the request from arriving is the check.
	p.client.Transport = &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, ps.Listener.Addr().String())
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	s.SavePushSubscription(newBrowser(t).subscription(u.ID, "https://no-longer-allowed.example/send/x"))

	p.deliver(context.Background(), job{[]int64{u.ID}, Message{Title: "hello", URL: "/"}})
	select {
	case r := <-ps.got:
		t.Errorf("sent to an address it does not allow: %s", r.Host)
	default:
	}
}
