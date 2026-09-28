package store

import (
	"database/sql"
	"errors"
)

// Push notifications: which browsers want them, for whom, about what.
//
// A browser that agrees to notifications hands over a subscription: an
// address at its maker's push service, and two keys that let us encrypt what
// we send so that the push service cannot read it. One person can have
// several (a phone, a laptop), and a browser belongs to whoever last turned
// notifications on in it.

// PushSubscription is one browser's standing permission to be notified.
type PushSubscription struct {
	UserID   int64
	Endpoint string
	P256dh   string // the browser's public key, base64url
	Auth     string // a shared secret, base64url
}

// NotifyPrefs is what a person wants to be told about.
type NotifyPrefs struct {
	Comments bool `json:"comments"` // someone comments on a task in one of their lists
	Assigned bool `json:"assigned"` // someone gives them a task
	Added    bool `json:"added"`    // someone adds a task to one of their shared lists
}

// maxDevices is how many browsers one person can have notified. An old
// phone's subscription lingers until its push service says it has gone, so
// the oldest make way.
const maxDevices = 10

// SavePushSubscription stores a browser's subscription for userID, taking it
// over from anyone who had it before: it is the same browser, now used by
// someone else.
func (s *Store) SavePushSubscription(sub PushSubscription) error {
	if sub.Endpoint == "" || sub.P256dh == "" || sub.Auth == "" {
		return ErrInvalid
	}
	return s.tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`
			INSERT INTO push_subscriptions (user_id, endpoint, p256dh, auth) VALUES (?, ?, ?, ?)
			ON CONFLICT (endpoint) DO UPDATE SET user_id = excluded.user_id, p256dh = excluded.p256dh,
				auth = excluded.auth, created_at = CURRENT_TIMESTAMP`,
			sub.UserID, sub.Endpoint, sub.P256dh, sub.Auth); err != nil {
			return err
		}
		_, err := tx.Exec(`
			DELETE FROM push_subscriptions WHERE user_id = ? AND id NOT IN (
				SELECT id FROM push_subscriptions WHERE user_id = ? ORDER BY created_at DESC, id DESC LIMIT ?)`,
			sub.UserID, sub.UserID, maxDevices)
		return err
	})
}

// RemovePushSubscription forgets a browser, when its owner turns
// notifications off in it.
func (s *Store) RemovePushSubscription(userID int64, endpoint string) error {
	_, err := s.db.Exec(`DELETE FROM push_subscriptions WHERE user_id = ? AND endpoint = ?`, userID, endpoint)
	return err
}

// ForgetPushEndpoint forgets a browser whoever it belonged to, when its push
// service says it no longer exists.
func (s *Store) ForgetPushEndpoint(endpoint string) error {
	_, err := s.db.Exec(`DELETE FROM push_subscriptions WHERE endpoint = ?`, endpoint)
	return err
}

// PushSubscriptions returns every browser of the given people.
func (s *Store) PushSubscriptions(userIDs []int64) ([]PushSubscription, error) {
	var subs []PushSubscription
	for _, id := range userIDs {
		rows, err := s.db.Query(`SELECT user_id, endpoint, p256dh, auth FROM push_subscriptions WHERE user_id = ? ORDER BY id`, id)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var sub PushSubscription
			if err := rows.Scan(&sub.UserID, &sub.Endpoint, &sub.P256dh, &sub.Auth); err != nil {
				rows.Close()
				return nil, err
			}
			subs = append(subs, sub)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return subs, nil
}

// NotifyPrefs returns what userID wants to be told about.
func (s *Store) NotifyPrefs(userID int64) (NotifyPrefs, error) {
	var p NotifyPrefs
	err := s.db.QueryRow(`SELECT notify_comments, notify_assigned, notify_added FROM users WHERE id = ?`, userID).
		Scan(&p.Comments, &p.Assigned, &p.Added)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// SetNotifyPrefs changes what userID wants to be told about.
func (s *Store) SetNotifyPrefs(userID int64, p NotifyPrefs) error {
	return s.affected(s.db.Exec(`UPDATE users SET notify_comments = ?, notify_assigned = ?, notify_added = ? WHERE id = ?`,
		p.Comments, p.Assigned, p.Added, userID))
}

// Notice is a kind of thing people can be told about.
type Notice int

const (
	NoticeComment Notice = iota
	NoticeAssigned
	NoticeAdded
)

// prefColumn is the users column that says whether someone wants a notice.
// It comes from this fixed table, never from a request.
var prefColumn = map[Notice]string{
	NoticeComment:  "notify_comments",
	NoticeAssigned: "notify_assigned",
	NoticeAdded:    "notify_added",
}

// ToNotify returns who should hear about something actorID did in listID: the
// list's other members who want this kind of notice and have a browser to be
// told in. Only returns candidates; the caller narrows it further (an
// assignment concerns just the assignee).
func (s *Store) ToNotify(kind Notice, listID, actorID int64) ([]int64, error) {
	col, ok := prefColumn[kind]
	if !ok {
		return nil, ErrInvalid
	}
	rows, err := s.db.Query(`
		SELECT DISTINCT m.user_id FROM list_members m
		JOIN users u ON u.id = m.user_id
		JOIN push_subscriptions p ON p.user_id = m.user_id
		WHERE m.list_id = ? AND m.user_id != ? AND u.`+col+` = 1
		ORDER BY m.user_id`, listID, actorID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// PushKeys returns this server's own key pair for signing what it sends to
// push services, making one the first time it is asked. The keys are kept,
// because every browser's subscription is tied to them: new keys would
// silently cut off everyone.
func (s *Store) PushKeys(generate func() (private, public string, err error)) (private, public string, err error) {
	err = s.db.QueryRow(`SELECT (SELECT value FROM settings WHERE key = 'vapid_private'),
		(SELECT value FROM settings WHERE key = 'vapid_public')`).Scan(&private, &public)
	if err == nil && private != "" && public != "" {
		return private, public, nil
	}
	priv, pub, err := generate()
	if err != nil {
		return "", "", err
	}
	err = s.tx(func(tx *sql.Tx) error {
		// If two ask at once, the first to store its pair wins, and both use it.
		for k, v := range map[string]string{"vapid_private": priv, "vapid_public": pub} {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO settings (key, value) VALUES (?, ?)`, k, v); err != nil {
				return err
			}
		}
		return tx.QueryRow(`SELECT (SELECT value FROM settings WHERE key = 'vapid_private'),
			(SELECT value FROM settings WHERE key = 'vapid_public')`).Scan(&private, &public)
	})
	return private, public, err
}

// PushContact is how a push service can reach whoever runs this server, if
// it has to: the https address it was first used at, or "" if it has not
// been used over https yet.
func (s *Store) PushContact() (string, error) {
	var v sql.NullString
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = 'push_contact'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v.String, err
}

// NotePushContact records contact as the push contact, unless there is one.
func (s *Store) NotePushContact(contact string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO settings (key, value) VALUES ('push_contact', ?)`, contact)
	return err
}
