// Package client talks to a Doables server's JSON API, as one person.
//
// It is what the Discord bot uses. The CLI predates it and has its own calls,
// written with Resty; both speak the same API, which the README documents.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"doables/internal/store"
)

// Client makes requests as whoever its token belongs to. It is cheap to make,
// so there is one per person rather than one token swapped in and out.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// New returns a client for the server at base (as in "http://localhost:8080")
// acting as the person token belongs to; an empty token acts anonymously.
func New(base, token string) *Client {
	return &Client{base: strings.TrimRight(base, "/"), token: token, http: &http.Client{Timeout: 15 * time.Second}}
}

// As returns a client for the same server acting as somebody else.
func (c *Client) As(token string) *Client {
	return &Client{base: c.base, token: token, http: c.http}
}

// Error is what the server said when it turned a request down.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("doables: %d %s", e.Status, http.StatusText(e.Status))
	}
	return fmt.Sprintf("doables: %d %s", e.Status, e.Message)
}

// IsStatus reports whether err is the server answering with that HTTP status.
func IsStatus(err error, status int) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == status
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&e)
		return &Error{Status: res.StatusCode, Message: e.Error}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

func id(n int64) string { return strconv.FormatInt(n, 10) }

// Register makes a new person and returns them with their token. The server
// allows only a few of these in a row from one address.
func (c *Client) Register(ctx context.Context, name string) (store.User, string, error) {
	var u struct {
		store.User
		Token string `json:"token"`
	}
	err := c.do(ctx, "POST", "/api/users", map[string]string{"name": name}, &u)
	return u.User, u.Token, err
}

func (c *Client) Me(ctx context.Context) (store.User, error) {
	var u store.User
	err := c.do(ctx, "GET", "/api/me", nil, &u)
	return u, err
}

func (c *Client) CreateList(ctx context.Context, name string) (store.List, error) {
	var l store.List
	err := c.do(ctx, "POST", "/api/lists", map[string]string{"name": name}, &l)
	return l, err
}

func (c *Client) Lists(ctx context.Context) ([]store.List, error) {
	var ls []store.List
	err := c.do(ctx, "GET", "/api/lists", nil, &ls)
	return ls, err
}

// Join makes the caller a member of the list with that invite code. Joining a
// list you are already on is not an error.
func (c *Client) Join(ctx context.Context, code string) (store.List, error) {
	var l store.List
	err := c.do(ctx, "POST", "/api/join/"+url.PathEscape(code), nil, &l)
	return l, err
}

// Tasks returns a list's tasks: view "" for every one, "todo" or "history".
func (c *Client) Tasks(ctx context.Context, listID int64, view string) ([]store.Task, error) {
	path := "/api/lists/" + id(listID) + "/tasks"
	if view != "" {
		path += "?view=" + url.QueryEscape(view)
	}
	var ts []store.Task
	err := c.do(ctx, "GET", path, nil, &ts)
	return ts, err
}

// Mine returns the open tasks assigned to the caller, from every list.
func (c *Client) Mine(ctx context.Context) ([]store.Task, error) {
	var ts []store.Task
	err := c.do(ctx, "GET", "/api/mine", nil, &ts)
	return ts, err
}

func (c *Client) AddTask(ctx context.Context, listID int64, title, description string) (store.Task, error) {
	var t store.Task
	err := c.do(ctx, "POST", "/api/lists/"+id(listID)+"/tasks",
		map[string]string{"title": title, "description": description}, &t)
	return t, err
}

func (c *Client) SetDone(ctx context.Context, taskID int64, done bool) (store.Task, error) {
	var t store.Task
	err := c.do(ctx, "PATCH", "/api/tasks/"+id(taskID), map[string]bool{"done": done}, &t)
	return t, err
}

// Assign gives a task to a member of its list, or to nobody when userID is 0.
func (c *Client) Assign(ctx context.Context, taskID, userID int64) (store.Task, error) {
	var t store.Task
	err := c.do(ctx, "PATCH", "/api/tasks/"+id(taskID), map[string]int64{"assignee_id": userID}, &t)
	return t, err
}

// DeleteTask moves a task to the trash, from where it can be restored for a day.
func (c *Client) DeleteTask(ctx context.Context, taskID int64) error {
	return c.do(ctx, "DELETE", "/api/tasks/"+id(taskID), nil, nil)
}

// Comments returns what has been said about a task, oldest first.
func (c *Client) Comments(ctx context.Context, taskID int64) ([]store.Comment, error) {
	var cs []store.Comment
	err := c.do(ctx, "GET", "/api/tasks/"+id(taskID)+"/comments", nil, &cs)
	return cs, err
}

func (c *Client) AddComment(ctx context.Context, taskID int64, body string) (store.Comment, error) {
	var cm store.Comment
	err := c.do(ctx, "POST", "/api/tasks/"+id(taskID)+"/comments", map[string]string{"body": body}, &cm)
	return cm, err
}

// Events listens for changes to any list the caller is on and calls changed
// for each one, until ctx ends or the connection drops. A signal says only
// that something changed; what changed is for the caller to fetch.
func (c *Client) Events(ctx context.Context, changed func()) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+"/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "text/event-stream")
	// No timeout: the stream is meant to stay open. The server pings it every
	// 25 seconds, so a dead connection still ends, through the read failing.
	res, err := (&http.Client{Transport: c.http.Transport}).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		// /events sends a browser without a valid token to the sign-in page.
		return &Error{Status: res.StatusCode, Message: "no live updates for this token"}
	}
	sc := bufio.NewScanner(res.Body)
	for sc.Scan() {
		if sc.Text() == "event: changed" {
			changed()
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return io.ErrUnexpectedEOF
}
