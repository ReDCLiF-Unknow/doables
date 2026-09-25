package store

// A comment is new to someone until they have seen its task's conversation.
// Their own comments never are, nor are ones written before they joined the
// list: joining a list is not a reason to be told about its whole past.
//
// comment_reads keeps, for each person and task, the newest comment they
// have seen. Upgrading to the release that added this would otherwise make
// every comment ever written new to everyone at once, so the settings table
// remembers where counting started ("unread_since"): only comments after it
// can be new.

// Unread is what is new to someone in one task's conversation.
type Unread struct {
	ListID int64
	Count  int
	Latest Comment        // the most recent of them
	IDs    map[int64]bool // which comments are new, to pick them out
}

// Unread returns, by task, the comments new to userID across every list they
// are on.
func (s *Store) Unread(userID int64) (map[int64]Unread, error) {
	rows, err := s.db.Query(`
		SELECT c.id, c.task_id, COALESCE(c.user_id, 0), COALESCE(u.name, ''), c.body, c.created_at, t.list_id
		FROM comments c
		JOIN tasks t ON t.id = c.task_id AND t.deleted_at IS NULL
		JOIN lists l ON l.id = t.list_id AND l.deleted_at IS NULL
		JOIN list_members m ON m.list_id = t.list_id AND m.user_id = ?
		LEFT JOIN users u ON u.id = c.user_id
		LEFT JOIN comment_reads r ON r.user_id = m.user_id AND r.task_id = c.task_id
		WHERE (c.user_id IS NULL OR c.user_id != m.user_id)
		  AND c.created_at >= m.joined_at
		  AND c.id > COALESCE(r.seen_up_to, 0)
		  AND c.id > COALESCE((SELECT CAST(value AS INTEGER) FROM settings WHERE key = 'unread_since'), 0)
		ORDER BY c.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	unread := map[int64]Unread{}
	for rows.Next() {
		var c Comment
		var listID int64
		if err := rows.Scan(&c.ID, &c.TaskID, &c.AuthorID, &c.Author, &c.Body, &c.CreatedAt, &listID); err != nil {
			return nil, err
		}
		u := unread[c.TaskID]
		if u.IDs == nil {
			u.IDs = map[int64]bool{}
		}
		u.ListID, u.Latest = listID, c
		u.Count++
		u.IDs[c.ID] = true
		unread[c.TaskID] = u
	}
	return unread, rows.Err()
}

// UnreadByList adds up what is new, list by list.
func UnreadByList(unread map[int64]Unread) map[int64]int {
	byList := map[int64]int{}
	for _, u := range unread {
		byList[u.ListID] += u.Count
	}
	return byList
}

// MarkSeen records that userID has seen every comment on taskID so far. Their
// other open pages are told, so a badge cleared on the phone clears on the
// laptop too.
func (s *Store) MarkSeen(userID, taskID int64) error {
	if err := s.markSeen(userID, taskID); err != nil {
		return err
	}
	s.publish([]int64{userID}, false)
	return nil
}

func (s *Store) markSeen(userID, taskID int64) error {
	_, err := s.db.Exec(`
		INSERT INTO comment_reads (user_id, task_id, seen_up_to)
		VALUES (?, ?, (SELECT COALESCE(MAX(id), 0) FROM comments WHERE task_id = ?))
		ON CONFLICT (user_id, task_id) DO UPDATE SET seen_up_to = MAX(seen_up_to, excluded.seen_up_to)`,
		userID, taskID, taskID)
	return err
}

// startUnreadCounting makes the comments already in the database count as
// seen, the first time a database is opened by a release that tracks this.
func (s *Store) startUnreadCounting() error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO settings (key, value)
		SELECT 'unread_since', COALESCE(MAX(id), 0) FROM comments`)
	return err
}
