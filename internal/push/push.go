// Package push sends notifications to people's browsers through their push
// services (Google's for Chrome, Mozilla's for Firefox, Apple's for Safari),
// the only way to reach a phone that does not have Doables open.
//
// What is sent is encrypted for the one browser it is for, so the push
// service carries it without being able to read it; it only learns that
// something was sent. Nothing is sent to anyone who has not turned
// notifications on.
package push

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"doables/internal/store"
)

// Message is one notification.
type Message struct {
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	URL   string `json:"url"`           // where tapping it goes, on this server
	Tag   string `json:"tag,omitempty"` // a newer message with the same tag replaces an older one
}

// Hosts are the push services a browser can ask us to send to. A
// subscription names the address to deliver to, and it comes from the
// browser, so without this list anyone signed in could make the server send
// requests to any address it can reach, its own network included.
var Hosts = []string{
	"fcm.googleapis.com",                // Chrome, Edge on Android, most Android browsers
	"updates.push.services.mozilla.com", // Firefox
	"push.apple.com",                    // Safari, and apps added to an iPhone's Home Screen
	"notify.windows.com",                // Edge on Windows
}

// Allowed reports whether endpoint is at one of the push services in hosts
// (a host, or any address under it), over HTTPS.
func Allowed(endpoint string, hosts []string) bool {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" && (host == h || strings.HasSuffix(host, "."+h)) {
			return true
		}
	}
	return false
}

// Sender delivers notifications in the background, so nobody's click waits
// for a push service.
type Sender struct {
	store   *store.Store
	private string
	public  string
	contact string // how a push service can reach whoever runs this server
	hosts   []string
	client  *http.Client
	queue   chan job
}

type job struct {
	users []int64
	msg   Message
}

// New prepares a sender, making this server's keys the first time.
// contact is an email address or https URL for push services to reach the
// server's operator; when empty, the https address the server was first used
// at stands in. extraHosts adds push services to Hosts.
func New(s *store.Store, contact string, extraHosts []string) (*Sender, error) {
	private, public, err := s.PushKeys(webpush.GenerateVAPIDKeys)
	if err != nil {
		return nil, err
	}
	return &Sender{
		store: s, private: private, public: public, contact: contact,
		hosts: append(append([]string{}, Hosts...), extraHosts...),
		client: &http.Client{
			Timeout: 15 * time.Second,
			// A push service answers; it does not send us elsewhere. Following
			// a redirect would undo the check on where requests may go.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		queue: make(chan job, 256),
	}, nil
}

// PublicKey is what a browser needs to subscribe to this server.
func (p *Sender) PublicKey() string { return p.public }

// Accepts reports whether a subscription's endpoint is one this server will
// send to.
func (p *Sender) Accepts(endpoint string) bool { return Allowed(endpoint, p.hosts) }

// Notify queues msg for every browser of each of users. It never blocks: if
// the queue is full, the message is dropped, as a notification that comes
// much later is worse than none.
func (p *Sender) Notify(users []int64, msg Message) {
	if len(users) == 0 {
		return
	}
	select {
	case p.queue <- job{users, msg}:
	default:
		log.Printf("push: too many waiting; dropped %q", msg.Title)
	}
}

// Run delivers queued messages until ctx is done.
func (p *Sender) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-p.queue:
			p.deliver(ctx, j)
		}
	}
}

func (p *Sender) deliver(ctx context.Context, j job) {
	subs, err := p.store.PushSubscriptions(j.users)
	if err != nil {
		log.Printf("push: %v", err)
		return
	}
	payload, _ := json.Marshal(j.msg)
	for _, sub := range subs {
		if err := p.send(ctx, sub, payload, j.msg.Tag); err != nil {
			log.Printf("push: to %s: %v", host(sub.Endpoint), err)
		}
	}
}

// errGone is a push service saying a browser no longer takes notifications.
var errGone = errors.New("the browser has unsubscribed")

func (p *Sender) send(ctx context.Context, sub store.PushSubscription, payload []byte, tag string) error {
	if !p.Accepts(sub.Endpoint) {
		return errors.New("not a push service this server sends to")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, err := webpush.SendNotificationWithContext(ctx, payload,
		&webpush.Subscription{Endpoint: sub.Endpoint, Keys: webpush.Keys{P256dh: sub.P256dh, Auth: sub.Auth}},
		&webpush.Options{
			HTTPClient:      p.client,
			Subscriber:      p.subscriber(),
			VAPIDPublicKey:  p.public,
			VAPIDPrivateKey: p.private,
			TTL:             24 * 60 * 60, // after a day offline, it is old news
			Urgency:         webpush.UrgencyNormal,
			Topic:           topic(tag),
		})
	if err != nil {
		return err
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		// Uninstalled, or permission taken back: stop sending there.
		if err := p.store.ForgetPushEndpoint(sub.Endpoint); err != nil {
			return err
		}
		return errGone
	case resp.StatusCode >= 300:
		return errors.New(resp.Status)
	}
	return nil
}

// subscriber is the contact put in every request.
func (p *Sender) subscriber() string {
	if p.contact != "" {
		return p.contact
	}
	if c, err := p.store.PushContact(); err == nil && c != "" {
		return c
	}
	// Not yet used over https (so, being tried out locally). Push services
	// want some contact; this says there is none.
	return "https://localhost"
}

// topic turns a tag into a Topic header, which lets a push service replace a
// message still waiting to be delivered with a newer one: at most 32
// characters from the base64url alphabet.
func topic(tag string) string {
	var b strings.Builder
	for _, r := range tag {
		if b.Len() == 32 {
			break
		}
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func host(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil {
		return u.Host
	}
	return "?"
}
