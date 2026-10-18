package discordbot

import (
	"database/sql"
	"errors"

	_ "modernc.org/sqlite"
)

// state is what the bot remembers about how Discord and Doables line up. The
// tasks themselves live in Doables; this is only the mapping between the two,
// plus each person's Doables token, which is why the file must be kept private.
type state struct{ db *sql.DB }

const stateSchema = `
CREATE TABLE IF NOT EXISTS people (
	discord_id TEXT PRIMARY KEY,
	doables_id INTEGER NOT NULL UNIQUE,
	token      TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS boards (
	channel_id TEXT PRIMARY KEY,
	guild_id   TEXT NOT NULL,
	list_id    INTEGER NOT NULL UNIQUE,
	owner      TEXT NOT NULL REFERENCES people(discord_id),
	invite     TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS joined (
	discord_id TEXT NOT NULL,
	list_id    INTEGER NOT NULL,
	PRIMARY KEY (discord_id, list_id)
);
CREATE TABLE IF NOT EXISTS cards (
	task_id    INTEGER PRIMARY KEY,
	list_id    INTEGER NOT NULL,
	channel_id TEXT NOT NULL,
	message_id TEXT NOT NULL,
	thread_id  TEXT NOT NULL DEFAULT '',
	render     TEXT NOT NULL DEFAULT '',
	assignee   INTEGER NOT NULL DEFAULT 0,
	comments   INTEGER NOT NULL DEFAULT 0,
	gone       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS cards_by_list ON cards(list_id);
CREATE INDEX IF NOT EXISTS cards_by_thread ON cards(thread_id);
-- Comments that are in both places already, whichever side they started on,
-- so that none is copied back to where it came from.
CREATE TABLE IF NOT EXISTS mirrored (comment_id INTEGER PRIMARY KEY);
`

func openState(path string) (*state, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(stateSchema); err != nil {
		db.Close()
		return nil, err
	}
	return &state{db: db}, nil
}

func (s *state) close() error { return s.db.Close() }

// person is somebody on Discord, and who they are in Doables.
type person struct {
	DiscordID string
	DoablesID int64
	Token     string
}

func (s *state) person(discordID string) (person, bool, error) {
	p := person{DiscordID: discordID}
	err := s.db.QueryRow(`SELECT doables_id, token FROM people WHERE discord_id = ?`, discordID).Scan(&p.DoablesID, &p.Token)
	if errors.Is(err, sql.ErrNoRows) {
		return p, false, nil
	}
	return p, err == nil, err
}

func (s *state) savePerson(p person) error {
	_, err := s.db.Exec(`INSERT INTO people (discord_id, doables_id, token) VALUES (?, ?, ?)`, p.DiscordID, p.DoablesID, p.Token)
	return err
}

// discordID is who a Doables person is on Discord, or "" if they only use
// Doables itself.
func (s *state) discordID(doablesID int64) string {
	var id string
	s.db.QueryRow(`SELECT discord_id FROM people WHERE doables_id = ?`, doablesID).Scan(&id)
	return id
}

// board is a Discord channel whose tasks are a Doables list.
type board struct {
	ChannelID string
	GuildID   string
	ListID    int64
	Owner     string // the Discord ID of whoever set it up, who owns the list
	Invite    string
}

const boardCols = `channel_id, guild_id, list_id, owner, invite`

func scanBoard(row interface{ Scan(...any) error }) (board, error) {
	var b board
	err := row.Scan(&b.ChannelID, &b.GuildID, &b.ListID, &b.Owner, &b.Invite)
	return b, err
}

func (s *state) board(channelID string) (board, bool, error) {
	return found(scanBoard(s.db.QueryRow(`SELECT `+boardCols+` FROM boards WHERE channel_id = ?`, channelID)))
}

func (s *state) boardByList(listID int64) (board, bool, error) {
	return found(scanBoard(s.db.QueryRow(`SELECT `+boardCols+` FROM boards WHERE list_id = ?`, listID)))
}

func (s *state) boards() ([]board, error) {
	rows, err := s.db.Query(`SELECT ` + boardCols + ` FROM boards ORDER BY list_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var bs []board
	for rows.Next() {
		b, err := scanBoard(rows)
		if err != nil {
			return nil, err
		}
		bs = append(bs, b)
	}
	return bs, rows.Err()
}

func (s *state) saveBoard(b board) error {
	_, err := s.db.Exec(`INSERT INTO boards (`+boardCols+`) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (channel_id) DO UPDATE SET invite = excluded.invite`,
		b.ChannelID, b.GuildID, b.ListID, b.Owner, b.Invite)
	return err
}

func (s *state) joined(discordID string, listID int64) bool {
	var n int
	s.db.QueryRow(`SELECT 1 FROM joined WHERE discord_id = ? AND list_id = ?`, discordID, listID).Scan(&n)
	return n == 1
}

func (s *state) markJoined(discordID string, listID int64) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO joined (discord_id, list_id) VALUES (?, ?)`, discordID, listID)
	return err
}

// card is the Discord message that shows one task, and the thread under it
// where people talk about it.
type card struct {
	TaskID    int64
	ListID    int64
	ChannelID string
	MessageID string
	ThreadID  string
	Render    string // what the message last showed, to tell whether it needs editing
	Assignee  int64  // whose it was when last shown, to tell when that changes
	Comments  int    // how many comments the task had when they were last copied
	Gone      bool   // the task has been deleted
}

const cardCols = `task_id, list_id, channel_id, message_id, thread_id, render, assignee, comments, gone`

func scanCard(row interface{ Scan(...any) error }) (card, error) {
	var c card
	err := row.Scan(&c.TaskID, &c.ListID, &c.ChannelID, &c.MessageID, &c.ThreadID, &c.Render, &c.Assignee, &c.Comments, &c.Gone)
	return c, err
}

func (s *state) card(taskID int64) (card, bool, error) {
	return found(scanCard(s.db.QueryRow(`SELECT `+cardCols+` FROM cards WHERE task_id = ?`, taskID)))
}

func (s *state) cardByThread(threadID string) (card, bool, error) {
	if threadID == "" {
		return card{}, false, nil
	}
	return found(scanCard(s.db.QueryRow(`SELECT `+cardCols+` FROM cards WHERE thread_id = ?`, threadID)))
}

func (s *state) cards(listID int64) (map[int64]card, error) {
	rows, err := s.db.Query(`SELECT `+cardCols+` FROM cards WHERE list_id = ?`, listID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cs := map[int64]card{}
	for rows.Next() {
		c, err := scanCard(rows)
		if err != nil {
			return nil, err
		}
		cs[c.TaskID] = c
	}
	return cs, rows.Err()
}

func (s *state) saveCard(c card) error {
	_, err := s.db.Exec(`INSERT INTO cards (`+cardCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (task_id) DO UPDATE SET message_id = excluded.message_id, thread_id = excluded.thread_id,
			render = excluded.render, assignee = excluded.assignee, comments = excluded.comments, gone = excluded.gone`,
		c.TaskID, c.ListID, c.ChannelID, c.MessageID, c.ThreadID, c.Render, c.Assignee, c.Comments, c.Gone)
	return err
}

func (s *state) mirrored(commentID int64) bool {
	var n int
	s.db.QueryRow(`SELECT 1 FROM mirrored WHERE comment_id = ?`, commentID).Scan(&n)
	return n == 1
}

func (s *state) markMirrored(commentID int64) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO mirrored (comment_id) VALUES (?)`, commentID)
	return err
}

// found turns "no such row" into ok == false.
func found[T any](v T, err error) (T, bool, error) {
	if errors.Is(err, sql.ErrNoRows) {
		return v, false, nil
	}
	return v, err == nil, err
}
