package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid input")
)

const (
	maxNameLen     = 40
	maxListNameLen = 80
	// The same limits the web form enforces, so the API and the CLI cannot
	// smuggle in something the UI would never let you type.
	maxTitleLen = 200
	maxDescLen  = 500
	dateLayout  = "2006-01-02"
	// trashRetention is how long a deleted task stays restorable.
	trashRetention = "-1 day"
)

type User struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// TokenSaved is whether this person has said they have their token
	// somewhere safe. Losing it means losing the account, so until they
	// say so, every page reminds them. Kept out of the API: it is about
	// nagging someone in a browser, not about the data.
	TokenSaved bool `json:"-"`
}

type Member struct {
	User
	Owner    bool      `json:"owner"`
	JoinedAt time.Time `json:"joined_at"`
}

// List is a task list. A list with no owner (OwnerID == 0) is "public": it
// predates sharing, and anyone may see and edit it until somebody claims it.
type List struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	OwnerID    int64  `json:"owner_id,omitempty"`
	InviteCode string `json:"invite_code,omitempty"`
	Members    int    `json:"members"`
	Open       int    `json:"open"`
	Total      int    `json:"total"`
}

// Public reports whether the list has no owner.
func (l List) Public() bool { return l.OwnerID == 0 }

// Done is the number of finished tasks in the list.
func (l List) Done() int { return l.Total - l.Open }

// Percent is the completion percentage (0 for an empty list).
func (l List) Percent() int {
	if l.Total == 0 {
		return 0
	}
	return l.Done() * 100 / l.Total
}

type Task struct {
	ID          int64     `json:"id"`
	ListID      int64     `json:"list_id"`
	ListName    string    `json:"list_name,omitempty"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Done        bool      `json:"done"`
	CreatedAt   time.Time `json:"created_at"`
	DueDate     string    `json:"due_date,omitempty"` // YYYY-MM-DD, empty when there is none
	AddedBy     string    `json:"added_by,omitempty"`
	DoneBy      string    `json:"done_by,omitempty"`
	// AssigneeID is who the task is for, 0 when it is for nobody in
	// particular. Assignee is that person's name.
	AssigneeID int64  `json:"assignee_id,omitempty"`
	Assignee   string `json:"assignee,omitempty"`
}

// TaskUpdate is a partial edit: nil fields are left alone. A DueDate pointing
// at "" clears the due date.
type TaskUpdate struct {
	Title       *string
	Description *string
	DueDate     *string
}

type Store struct {
	db     *sql.DB
	notify func(userIDs []int64, all bool)
}

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	name        TEXT NOT NULL,
	token_hash  TEXT NOT NULL UNIQUE,
	created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	token_saved INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS lists (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	name        TEXT NOT NULL,
	owner_id    INTEGER REFERENCES users(id) ON DELETE SET NULL,
	invite_code TEXT,
	deleted_at  TIMESTAMP
);
CREATE TABLE IF NOT EXISTS list_members (
	list_id   INTEGER NOT NULL REFERENCES lists(id) ON DELETE CASCADE,
	user_id   INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	joined_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (list_id, user_id)
);
CREATE TABLE IF NOT EXISTS tasks (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	list_id     INTEGER NOT NULL REFERENCES lists(id) ON DELETE CASCADE,
	title       TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	done        INTEGER NOT NULL DEFAULT 0,
	created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	added_by    INTEGER REFERENCES users(id) ON DELETE SET NULL,
	done_by     INTEGER REFERENCES users(id) ON DELETE SET NULL,
	assigned_to INTEGER REFERENCES users(id) ON DELETE SET NULL,
	due_date    TEXT,
	deleted_at  TIMESTAMP
);
CREATE INDEX IF NOT EXISTS tasks_list_id ON tasks(list_id);
`

// Open opens (creating or upgrading if needed) the SQLite database at path.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite allows a single writer; one connection keeps things simple and
	// makes ":memory:" databases behave. Never issue a query while iterating
	// another query's rows: it would wait forever for the only connection.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// SetNotifier registers a callback fired after every change. It receives the
// users who can see the changed list, or all=true when the list is public.
func (s *Store) SetNotifier(fn func(userIDs []int64, all bool)) { s.notify = fn }

// migrate upgrades databases created by earlier versions.
func (s *Store) migrate() error {
	for _, c := range []struct{ table, column, def string }{
		{"users", "token_saved", "INTEGER NOT NULL DEFAULT 0"},
		{"lists", "owner_id", "INTEGER REFERENCES users(id) ON DELETE SET NULL"},
		{"lists", "invite_code", "TEXT"},
		{"lists", "deleted_at", "TIMESTAMP"},
		{"tasks", "added_by", "INTEGER REFERENCES users(id) ON DELETE SET NULL"},
		{"tasks", "done_by", "INTEGER REFERENCES users(id) ON DELETE SET NULL"},
		{"tasks", "assigned_to", "INTEGER REFERENCES users(id) ON DELETE SET NULL"},
		{"tasks", "due_date", "TEXT"},
		{"tasks", "deleted_at", "TIMESTAMP"},
	} {
		has, err := s.hasColumn(c.table, c.column)
		if err != nil {
			return err
		}
		if !has {
			if _, err := s.db.Exec("ALTER TABLE " + c.table + " ADD COLUMN " + c.column + " " + c.def); err != nil {
				return err
			}
		}
	}

	// Indexes on columns added above: they cannot live in the schema, which
	// runs before those columns exist.
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS tasks_assigned_to ON tasks(assigned_to)`); err != nil {
		return err
	}

	// Every list gets an invite code, including ones that predate sharing.
	rows, err := s.db.Query(`SELECT id FROM lists WHERE invite_code IS NULL`)
	if err != nil {
		return err
	}
	var missing []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		missing = append(missing, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range missing {
		if _, err := s.db.Exec(`UPDATE lists SET invite_code = ? WHERE id = ?`, newInviteCode(), id); err != nil {
			return err
		}
	}
	if _, err = s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS lists_invite_code ON lists(invite_code)`); err != nil {
		return err
	}
	return s.purgeTrash()
}

func (s *Store) hasColumn(table, column string) (bool, error) {
	rows, err := s.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid       int
			name, typ string
			notnull   int
			dflt      sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// ---- change notifications --------------------------------------------

// recipients reports who can see a list: its owner and members, or everyone
// for a public list.
func (s *Store) recipients(listID int64) (ids []int64, all bool) {
	var owner sql.NullInt64
	if err := s.db.QueryRow(`SELECT owner_id FROM lists WHERE id = ?`, listID).Scan(&owner); err != nil || !owner.Valid {
		return nil, true // unknown or public: tell everyone, they re-check access
	}
	rows, err := s.db.Query(`SELECT user_id FROM list_members WHERE list_id = ?`, listID)
	if err != nil {
		return nil, true
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	return ids, false
}

func (s *Store) publish(ids []int64, all bool) {
	if s.notify != nil {
		s.notify(ids, all)
	}
}

// changed tells everyone who can see listID (plus extra users, e.g. someone
// who was just removed) that something in it changed.
func (s *Store) changed(listID int64, extra ...int64) {
	if s.notify == nil {
		return
	}
	ids, all := s.recipients(listID)
	s.publish(append(ids, extra...), all)
}

// ---- users ------------------------------------------------------------

func newToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func newInviteCode() string {
	b := make([]byte, 12)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func cleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxNameLen {
		return "", ErrInvalid
	}
	return name, nil
}

// nullID maps the zero ID to SQL NULL.
func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// cleanDue validates a YYYY-MM-DD date; the empty string means "no due date".
func cleanDue(d string) (any, error) {
	d = strings.TrimSpace(d)
	if d == "" {
		return nil, nil
	}
	if _, err := time.Parse(dateLayout, d); err != nil {
		return nil, ErrInvalid
	}
	return d, nil
}

// CreateUser adds a person and returns their secret token. Only a hash of the
// token is stored, so it cannot be recovered later from the database.
func (s *Store) CreateUser(name string) (User, string, error) {
	name, err := cleanName(name)
	if err != nil {
		return User{}, "", err
	}
	token := newToken()
	res, err := s.db.Exec(`INSERT INTO users (name, token_hash) VALUES (?, ?)`, name, hashToken(token))
	if err != nil {
		return User{}, "", err
	}
	id, _ := res.LastInsertId()
	return User{ID: id, Name: name}, token, nil
}

func (s *Store) UserByToken(token string) (User, error) {
	var u User
	if token == "" {
		return u, ErrNotFound
	}
	err := s.db.QueryRow(`SELECT id, name, token_saved FROM users WHERE token_hash = ?`,
		hashToken(token)).Scan(&u.ID, &u.Name, &u.TokenSaved)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

// MarkTokenSaved records that someone has their token somewhere safe, which
// stops the app reminding them about it.
func (s *Store) MarkTokenSaved(id int64) error {
	_, err := s.db.Exec(`UPDATE users SET token_saved = 1 WHERE id = ?`, id)
	return err
}

func (s *Store) RenameUser(id int64, name string) error {
	name, err := cleanName(name)
	if err != nil {
		return err
	}
	return s.affected(s.db.Exec(`UPDATE users SET name = ? WHERE id = ?`, name, id))
}

// ---- lists ------------------------------------------------------------

const listSelect = `
	SELECT l.id, l.name, COALESCE(l.owner_id, 0), l.invite_code,
	       (SELECT COUNT(*) FROM list_members m WHERE m.list_id = l.id),
	       COALESCE(SUM(CASE WHEN t.done = 0 THEN 1 ELSE 0 END), 0),
	       COUNT(t.id)
	FROM lists l LEFT JOIN tasks t ON t.list_id = l.id AND t.deleted_at IS NULL `

func (s *Store) queryLists(where string, args ...any) ([]List, error) {
	rows, err := s.db.Query(listSelect+where+` GROUP BY l.id ORDER BY l.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	lists := []List{}
	for rows.Next() {
		var (
			l    List
			code sql.NullString
		)
		if err := rows.Scan(&l.ID, &l.Name, &l.OwnerID, &code, &l.Members, &l.Open, &l.Total); err != nil {
			return nil, err
		}
		if !l.Public() {
			l.InviteCode = code.String
		}
		lists = append(lists, l)
	}
	return lists, rows.Err()
}

func (s *Store) firstList(where string, args ...any) (List, error) {
	lists, err := s.queryLists(where, args...)
	if err != nil {
		return List{}, err
	}
	if len(lists) == 0 {
		return List{}, ErrNotFound
	}
	return lists[0], nil
}

// visibleLists is a SQL condition on the lists table (alias l) matching the
// lists a user can see; it takes one argument, the user ID. A list in the
// trash is not one of them, which also hides its tasks from Today and My
// tasks without those queries having to think about it.
const visibleLists = `(l.deleted_at IS NULL
	AND (l.owner_id IS NULL OR l.id IN (SELECT list_id FROM list_members WHERE user_id = ?)))`

// Lists returns the lists userID can see: the ones they belong to, plus all
// public ones. Pass 0 for an anonymous caller, who sees only public lists.
func (s *Store) Lists(userID int64) ([]List, error) {
	return s.queryLists(`WHERE `+visibleLists, userID)
}

// Access returns the list if userID may see it, and ErrNotFound otherwise, so
// that lists you cannot see look exactly like lists that do not exist.
func (s *Store) Access(listID, userID int64) (List, error) {
	l, err := s.firstList(`WHERE l.id = ? AND l.deleted_at IS NULL`, listID)
	if err != nil || l.Public() {
		return l, err
	}
	var one int
	err = s.db.QueryRow(`SELECT 1 FROM list_members WHERE list_id = ? AND user_id = ?`, listID, userID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return List{}, ErrNotFound
	}
	return l, err
}

func (s *Store) ListByInvite(code string) (List, error) {
	if code == "" {
		return List{}, ErrNotFound
	}
	return s.firstList(`WHERE l.invite_code = ? AND l.deleted_at IS NULL`, code)
}

// CreateList makes a new list owned by ownerID. With ownerID 0 the list is
// public, which is how anonymous API/CLI callers create lists.
func (s *Store) CreateList(name string, ownerID int64) (List, error) {
	name, err := cleanListName(name)
	if err != nil {
		return List{}, err
	}
	res, err := s.db.Exec(`INSERT INTO lists (name, owner_id, invite_code) VALUES (?, ?, ?)`,
		name, nullID(ownerID), newInviteCode())
	if err != nil {
		return List{}, err
	}
	id, _ := res.LastInsertId()
	if ownerID != 0 {
		if err := s.AddMember(id, ownerID); err != nil {
			return List{}, err
		}
	}
	s.changed(id)
	return s.firstList(`WHERE l.id = ?`, id)
}

func cleanListName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxListNameLen {
		return "", ErrInvalid
	}
	return name, nil
}

func (s *Store) RenameList(id int64, name string) error {
	name, err := cleanListName(name)
	if err != nil {
		return err
	}
	if err := s.affected(s.db.Exec(`UPDATE lists SET name = ? WHERE id = ?`, name, id)); err != nil {
		return err
	}
	s.changed(id)
	return nil
}

// DeleteList moves a list, and everything on it, to the trash. Like a deleted
// task it can be brought back for a day, after which it is gone for good.
func (s *Store) DeleteList(id int64) error {
	ids, all := s.recipients(id) // gather before the list stops being visible
	if err := s.affected(s.db.Exec(
		`UPDATE lists SET deleted_at = CURRENT_TIMESTAMP WHERE id = ? AND deleted_at IS NULL`, id)); err != nil {
		return err
	}
	s.purgeTrash()
	s.publish(ids, all)
	return nil
}

// RestoreList undoes a delete, if userID is the one who could delete it: its
// owner, or anyone at all when it is public.
func (s *Store) RestoreList(id, userID int64) (List, error) {
	l, err := s.firstList(`WHERE l.id = ? AND l.deleted_at IS NOT NULL`, id)
	if err != nil {
		return List{}, err
	}
	if !l.Public() && l.OwnerID != userID {
		return List{}, ErrNotFound
	}
	if err := s.affected(s.db.Exec(`UPDATE lists SET deleted_at = NULL WHERE id = ?`, id)); err != nil {
		return List{}, err
	}
	s.changed(id)
	return s.firstList(`WHERE l.id = ?`, id)
}

// ClaimList makes userID the owner of a public list, making it private.
func (s *Store) ClaimList(listID, userID int64) error {
	if err := s.affected(s.db.Exec(`UPDATE lists SET owner_id = ? WHERE id = ? AND owner_id IS NULL`, userID, listID)); err != nil {
		return err
	}
	if err := s.AddMember(listID, userID); err != nil {
		return err
	}
	s.publish(nil, true) // everyone who could see it as public must re-check
	return nil
}

// ResetInvite replaces the list's invite code, invalidating the old link.
func (s *Store) ResetInvite(listID int64) (string, error) {
	code := newInviteCode()
	if err := s.affected(s.db.Exec(`UPDATE lists SET invite_code = ? WHERE id = ?`, code, listID)); err != nil {
		return "", err
	}
	s.changed(listID)
	return code, nil
}

// ---- members ----------------------------------------------------------

func (s *Store) AddMember(listID, userID int64) error {
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO list_members (list_id, user_id) VALUES (?, ?)`, listID, userID); err != nil {
		return err
	}
	s.changed(listID)
	return nil
}

// RemoveMember removes someone from a list. The owner cannot be removed.
// Anything that was assigned to them goes back to being nobody's job, since
// they can no longer see it.
func (s *Store) RemoveMember(listID, userID int64) error {
	if err := s.affected(s.db.Exec(`
		DELETE FROM list_members WHERE list_id = ? AND user_id = ?
		AND user_id != (SELECT COALESCE(owner_id, 0) FROM lists WHERE id = ?)`, listID, userID, listID)); err != nil {
		return err
	}
	if _, err := s.db.Exec(`UPDATE tasks SET assigned_to = NULL WHERE list_id = ? AND assigned_to = ?`, listID, userID); err != nil {
		return err
	}
	s.changed(listID, userID)
	return nil
}

func (s *Store) Members(listID int64) ([]Member, error) {
	rows, err := s.db.Query(`
		SELECT u.id, u.name, m.joined_at, u.id = COALESCE(l.owner_id, 0)
		FROM list_members m
		JOIN users u ON u.id = m.user_id
		JOIN lists l ON l.id = m.list_id
		WHERE m.list_id = ?
		ORDER BY u.id = COALESCE(l.owner_id, 0) DESC, m.joined_at, u.id`, listID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	members := []Member{}
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.ID, &m.Name, &m.JoinedAt, &m.Owner); err != nil {
			return nil, err
		}
		members = append(members, m)
	}
	return members, rows.Err()
}

// ---- tasks ------------------------------------------------------------

const taskSelect = `
	SELECT t.id, t.list_id, l.name, t.title, t.description, t.done, t.created_at,
	       COALESCE(t.due_date, ''), COALESCE(a.name, ''), COALESCE(d.name, ''),
	       COALESCE(t.assigned_to, 0), COALESCE(g.name, '')
	FROM tasks t
	JOIN lists l ON l.id = t.list_id
	LEFT JOIN users a ON a.id = t.added_by
	LEFT JOIN users d ON d.id = t.done_by
	LEFT JOIN users g ON g.id = t.assigned_to `

func scanTask(sc interface{ Scan(...any) error }) (Task, error) {
	var t Task
	err := sc.Scan(&t.ID, &t.ListID, &t.ListName, &t.Title, &t.Description, &t.Done, &t.CreatedAt, &t.DueDate, &t.AddedBy, &t.DoneBy,
		&t.AssigneeID, &t.Assignee)
	return t, err
}

func (s *Store) queryTasks(where string, args ...any) ([]Task, error) {
	rows, err := s.db.Query(taskSelect+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// Tasks returns a list's tasks: open ones first, soonest due date first.
func (s *Store) Tasks(listID int64) ([]Task, error) {
	return s.queryTasks(`WHERE t.list_id = ? AND t.deleted_at IS NULL
		ORDER BY t.done, t.due_date IS NULL, t.due_date, t.id`, listID)
}

func (s *Store) oneTask(where string, id int64) (Task, error) {
	t, err := scanTask(s.db.QueryRow(taskSelect+where, id))
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

func (s *Store) Task(id int64) (Task, error) {
	return s.oneTask(`WHERE t.id = ? AND t.deleted_at IS NULL`, id)
}

// TaskAccess returns a task and its list if userID may see that list.
func (s *Store) TaskAccess(taskID, userID int64) (Task, List, error) {
	t, err := s.Task(taskID)
	if err != nil {
		return Task{}, List{}, err
	}
	l, err := s.Access(t.ListID, userID)
	if err != nil {
		return Task{}, List{}, err
	}
	return t, l, nil
}

// DueTasks returns the open tasks with a due date on or before until, across
// every list userID can see, soonest first.
func (s *Store) DueTasks(userID int64, until string) ([]Task, error) {
	return s.queryTasks(`WHERE t.deleted_at IS NULL AND t.done = 0
		AND t.due_date IS NOT NULL AND t.due_date <= ? AND `+visibleLists+`
		ORDER BY t.due_date, l.id, t.id`, until, userID)
}

// DueCounts counts open tasks that are overdue (due before today) and due
// today, across the lists userID can see.
func (s *Store) DueCounts(userID int64, today string) (overdue, dueToday int, err error) {
	err = s.db.QueryRow(`
		SELECT COALESCE(SUM(t.due_date < ?), 0), COALESCE(SUM(t.due_date = ?), 0)
		FROM tasks t JOIN lists l ON l.id = t.list_id
		WHERE t.deleted_at IS NULL AND t.done = 0 AND t.due_date IS NOT NULL
		AND t.due_date <= ? AND `+visibleLists, today, today, today, userID).Scan(&overdue, &dueToday)
	return
}

func (s *Store) AddTask(listID, userID int64, title, description, due string) (Task, error) {
	title, description = strings.TrimSpace(title), strings.TrimSpace(description)
	if !okLength(title, maxTitleLen) || !within(description, maxDescLen) {
		return Task{}, ErrInvalid
	}
	dueVal, err := cleanDue(due)
	if err != nil {
		return Task{}, err
	}
	res, err := s.db.Exec(`INSERT INTO tasks (list_id, title, description, added_by, due_date) VALUES (?, ?, ?, ?, ?)`,
		listID, title, description, nullID(userID), dueVal)
	if err != nil {
		return Task{}, err
	}
	id, _ := res.LastInsertId()
	s.changed(listID)
	return s.Task(id)
}

// UpdateTask applies a partial edit to a task.
func (s *Store) UpdateTask(id int64, u TaskUpdate) (Task, error) {
	t, err := s.Task(id)
	if err != nil {
		return Task{}, err
	}
	title, desc := t.Title, t.Description
	if u.Title != nil {
		if title = strings.TrimSpace(*u.Title); !okLength(title, maxTitleLen) {
			return Task{}, ErrInvalid
		}
	}
	if u.Description != nil {
		if desc = strings.TrimSpace(*u.Description); !within(desc, maxDescLen) {
			return Task{}, ErrInvalid
		}
	}
	var dueVal any
	if t.DueDate != "" {
		dueVal = t.DueDate
	}
	if u.DueDate != nil {
		if dueVal, err = cleanDue(*u.DueDate); err != nil {
			return Task{}, err
		}
	}
	if _, err := s.db.Exec(`UPDATE tasks SET title = ?, description = ?, due_date = ? WHERE id = ?`, title, desc, dueVal, id); err != nil {
		return Task{}, err
	}
	s.changed(t.ListID)
	return s.Task(id)
}

// SetDone marks a task done or not done, remembering who finished it.
func (s *Store) SetDone(id int64, done bool, userID int64) (Task, error) {
	var by any
	if done {
		by = nullID(userID)
	}
	if err := s.affected(s.db.Exec(`UPDATE tasks SET done = ?, done_by = ? WHERE id = ? AND deleted_at IS NULL`, done, by, id)); err != nil {
		return Task{}, err
	}
	t, err := s.Task(id)
	if err == nil {
		s.changed(t.ListID)
	}
	return t, err
}

// AssignTask makes a task somebody's job. Pass assigneeID 0 to leave it for
// anyone. The assignee must be a member of the task's list: assigning work to
// someone who cannot see it would only hide it.
func (s *Store) AssignTask(id, assigneeID int64) (Task, error) {
	t, err := s.Task(id)
	if err != nil {
		return Task{}, err
	}
	if assigneeID != 0 && !s.isMember(t.ListID, assigneeID) {
		return Task{}, ErrInvalid
	}
	if _, err := s.db.Exec(`UPDATE tasks SET assigned_to = ? WHERE id = ? AND deleted_at IS NULL`,
		nullID(assigneeID), id); err != nil {
		return Task{}, err
	}
	// The old assignee is told too: the task has just left their own list.
	s.changed(t.ListID, t.AssigneeID, assigneeID)
	return s.Task(id)
}

// isMember reports whether userID belongs to a list. A public list has no
// members, so nothing can be assigned in one until somebody claims it.
func (s *Store) isMember(listID, userID int64) bool {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM list_members WHERE list_id = ? AND user_id = ?`, listID, userID).Scan(&n)
	return err == nil && n > 0
}

// AssignedTasks returns the open tasks assigned to userID, across every list
// they can see, soonest due first and undated last.
func (s *Store) AssignedTasks(userID int64) ([]Task, error) {
	return s.queryTasks(`WHERE t.deleted_at IS NULL AND t.done = 0 AND t.assigned_to = ?
		AND `+visibleLists+`
		ORDER BY t.due_date IS NULL, t.due_date, l.id, t.id`, userID, userID)
}

// AssignedCount counts the open tasks assigned to userID.
func (s *Store) AssignedCount(userID int64) (int, error) {
	var n int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM tasks t JOIN lists l ON l.id = t.list_id
		WHERE t.deleted_at IS NULL AND t.done = 0 AND t.assigned_to = ?
		AND `+visibleLists, userID, userID).Scan(&n)
	return n, err
}

// DeleteTask moves a task to the trash. It can be restored for a day.
func (s *Store) DeleteTask(id int64) error {
	t, err := s.Task(id)
	if err != nil {
		return err
	}
	if err := s.affected(s.db.Exec(`UPDATE tasks SET deleted_at = CURRENT_TIMESTAMP WHERE id = ? AND deleted_at IS NULL`, id)); err != nil {
		return err
	}
	s.purgeTrash()
	s.changed(t.ListID)
	return nil
}

// RestoreTask brings a deleted task back, if userID may see its list.
func (s *Store) RestoreTask(id, userID int64) (Task, error) {
	t, err := s.oneTask(`WHERE t.id = ? AND t.deleted_at IS NOT NULL`, id)
	if err != nil {
		return Task{}, err
	}
	if _, err := s.Access(t.ListID, userID); err != nil {
		return Task{}, err
	}
	if err := s.affected(s.db.Exec(`UPDATE tasks SET deleted_at = NULL WHERE id = ?`, id)); err != nil {
		return Task{}, err
	}
	s.changed(t.ListID)
	return s.Task(id)
}

// purgeTrash permanently removes tasks deleted more than a day ago.
func (s *Store) purgeTrash() error {
	if _, err := s.db.Exec(
		`DELETE FROM tasks WHERE deleted_at IS NOT NULL AND deleted_at < datetime('now', ?)`, trashRetention); err != nil {
		return err
	}
	// Tasks and memberships go with the list: they are ON DELETE CASCADE.
	_, err := s.db.Exec(
		`DELETE FROM lists WHERE deleted_at IS NOT NULL AND deleted_at < datetime('now', ?)`, trashRetention)
	return err
}

// okLength reports whether s is neither empty nor longer than max runes.
func okLength(s string, max int) bool { return s != "" && within(s, max) }

// within reports whether s is at most max runes; empty is allowed.
func within(s string, max int) bool { return utf8.RuneCountInString(s) <= max }

func (s *Store) affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
