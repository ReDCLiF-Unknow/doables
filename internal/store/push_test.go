package store

import (
	"fmt"
	"slices"
	"testing"
)

func TestPushSubscriptions(t *testing.T) {
	s := openTest(t)
	alex, _, _ := s.CreateUser("Alex")
	sam, _, _ := s.CreateUser("Sam")
	sub := func(u User, n int) PushSubscription {
		return PushSubscription{UserID: u.ID, Endpoint: fmt.Sprintf("https://fcm.googleapis.com/fcm/send/%d", n), P256dh: "key", Auth: "secret"}
	}

	s.SavePushSubscription(sub(alex, 1))
	s.SavePushSubscription(sub(alex, 2))
	if subs, _ := s.PushSubscriptions([]int64{alex.ID}); len(subs) != 2 {
		t.Fatalf("Alex has %d devices, want 2", len(subs))
	}
	// The same browser, now used by Sam, is Sam's.
	s.SavePushSubscription(sub(sam, 2))
	if a, _ := s.PushSubscriptions([]int64{alex.ID}); len(a) != 1 {
		t.Errorf("after Sam took over a browser, Alex still has %d", len(a))
	}
	if b, _ := s.PushSubscriptions([]int64{sam.ID}); len(b) != 1 || b[0].Endpoint != sub(sam, 2).Endpoint {
		t.Errorf("Sam's devices are %+v", b)
	}
	// Only its owner can turn a browser off; its push service can always.
	s.RemovePushSubscription(alex.ID, sub(sam, 2).Endpoint)
	if b, _ := s.PushSubscriptions([]int64{sam.ID}); len(b) != 1 {
		t.Error("Alex turned off Sam's browser")
	}
	s.ForgetPushEndpoint(sub(sam, 2).Endpoint)
	if b, _ := s.PushSubscriptions([]int64{sam.ID}); len(b) != 0 {
		t.Error("a browser its push service said was gone is still kept")
	}
	// Nobody collects devices without end.
	for n := 10; n < 25; n++ {
		s.SavePushSubscription(sub(alex, n))
	}
	if a, _ := s.PushSubscriptions([]int64{alex.ID}); len(a) != maxDevices {
		t.Errorf("Alex has %d devices on record, want at most %d", len(a), maxDevices)
	}
	if err := s.SavePushSubscription(PushSubscription{UserID: alex.ID, Endpoint: "https://x"}); err == nil {
		t.Error("a subscription without keys was kept")
	}
}

func TestWhoToNotify(t *testing.T) {
	s := openTest(t)
	alex, _, _ := s.CreateUser("Alex")
	sam, _, _ := s.CreateUser("Sam")
	rae, _, _ := s.CreateUser("Rae")
	outsider, _, _ := s.CreateUser("Outsider")
	l, _ := s.CreateList("Trip", alex.ID)
	s.AddMember(l.ID, sam.ID)
	s.AddMember(l.ID, rae.ID)
	for i, u := range []User{alex, sam, outsider} {
		s.SavePushSubscription(PushSubscription{UserID: u.ID, Endpoint: fmt.Sprintf("https://fcm.googleapis.com/fcm/send/%d", i), P256dh: "k", Auth: "a"})
	}
	// Two browsers for Sam, still one Sam.
	s.SavePushSubscription(PushSubscription{UserID: sam.ID, Endpoint: "https://fcm.googleapis.com/fcm/send/sam2", P256dh: "k", Auth: "a"})

	got, _ := s.ToNotify(NoticeComment, l.ID, alex.ID)
	// Not Alex (who did it), not Rae (no browser), not the outsider (not on the list).
	if !slices.Equal(got, []int64{sam.ID}) {
		t.Errorf("a comment by Alex would notify %v, want only Sam", got)
	}
	if got, _ := s.ToNotify(NoticeAdded, l.ID, alex.ID); len(got) != 0 {
		t.Errorf("new tasks notify %v by default, want nobody", got)
	}
	s.SetNotifyPrefs(sam.ID, NotifyPrefs{Added: true})
	if got, _ := s.ToNotify(NoticeAdded, l.ID, alex.ID); !slices.Equal(got, []int64{sam.ID}) {
		t.Errorf("after Sam asked for new tasks: %v", got)
	}
	if got, _ := s.ToNotify(NoticeComment, l.ID, alex.ID); len(got) != 0 {
		t.Errorf("Sam turned comment notifications off, yet %v", got)
	}
	if p, _ := s.NotifyPrefs(rae.ID); p != (NotifyPrefs{Comments: true, Assigned: true}) {
		t.Errorf("a new person's settings are %+v, want comments and tasks given to them", p)
	}
}

func TestPushKeysAreMadeOnce(t *testing.T) {
	s := openTest(t)
	made := 0
	gen := func() (string, string, error) {
		made++
		return fmt.Sprint("private", made), fmt.Sprint("public", made), nil
	}
	priv, pub, _ := s.PushKeys(gen)
	priv2, pub2, _ := s.PushKeys(gen)
	if made != 1 || priv != priv2 || pub != pub2 || pub != "public1" {
		t.Errorf("keys made %d times: %s/%s then %s/%s", made, priv, pub, priv2, pub2)
	}
	if c, _ := s.PushContact(); c != "" {
		t.Errorf("a new server has the contact %q", c)
	}
	s.NotePushContact("https://doables.example.org")
	s.NotePushContact("https://other.example.org")
	if c, _ := s.PushContact(); c != "https://doables.example.org" {
		t.Errorf("the contact is %q, want the first address", c)
	}
}
