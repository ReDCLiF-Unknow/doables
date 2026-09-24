package web

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"doables/internal/store"
)

func TestTalkingAboutATask(t *testing.T) {
	e := newEnv(t)
	alex, sam := e.register("Alex"), e.register("Sam")
	l := e.newList(alex, "Weekend")
	e.call("POST", "/api/join/"+l.InviteCode, sam, "", nil)
	e.call("POST", "/api/lists/"+itoa(l.ID)+"/tasks", alex, `{"title":"Book the ferry"}`, nil)
	listPage := "/lists/" + itoa(l.ID)

	// Alex asks through the page; Sam answers through the API.
	resp := e.form(alex, "/tasks/1/comments", url.Values{"body": {"Which day works?"}, "next": {listPage}})
	want(t, "commenting from the page", resp.StatusCode, 303)
	if loc := resp.Header.Get("Location"); loc != listPage {
		t.Errorf("commenting sent us to %q, want back to the list", loc)
	}
	var c store.Comment
	want(t, "commenting through the API", e.call("POST", "/api/tasks/1/comments", sam, `{"body":"Friday, 7pm"}`, &c), 201)
	if c.Author != "Sam" || c.Body != "Friday, 7pm" {
		t.Errorf("the API returned %+v", c)
	}

	// Both show on the list page, in order, under the task.
	_, page := e.page(alex, listPage)
	for _, want := range []string{"2 comments", "Which day works?", "Friday, 7pm", "Sam", `data-thread="1"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the list page does not show %q", want)
		}
	}
	if strings.Index(page, "Which day works?") > strings.Index(page, "Friday, 7pm") {
		t.Error("the conversation is shown out of order")
	}
	// Your own comments say "You", and only they have a delete button.
	if !strings.Contains(page, "<strong>You</strong>") {
		t.Error("Alex's own comment is not marked as his")
	}
	if strings.Count(page, `action="/comments/`) != 1 {
		t.Errorf("Alex should be able to delete exactly one comment (his own), the page offers %d",
			strings.Count(page, `action="/comments/`))
	}

	// Elsewhere the row just says how many, without the thread.
	e.call("PATCH", "/api/tasks/1", alex, `{"due_date":"`+time.Now().Format("2006-01-02")+`"}`, nil)
	_, today := e.page(alex, "/today")
	if !strings.Contains(today, "2 comments") {
		t.Error("Today does not say the task has comments")
	}
	if strings.Contains(today, `action="/tasks/1/comments"`) {
		t.Error("Today shows a comment box; threads belong on the list's own page")
	}

	// The API reads them back, oldest first.
	var cs []store.Comment
	e.call("GET", "/api/tasks/1/comments", sam, "", &cs)
	if len(cs) != 2 || cs[0].Author != "Alex" || cs[1].Author != "Sam" {
		t.Errorf("the API read back %+v", cs)
	}
}

// Comments are text one person writes and everyone else on the list reads, so
// they must be shown as text, never run as markup.
func TestCommentsCannotInjectMarkup(t *testing.T) {
	e := newEnv(t)
	mallory, alex := e.register("Mallory"), e.register("Alex")
	l := e.newList(alex, "Weekend")
	e.call("POST", "/api/join/"+l.InviteCode, mallory, "", nil)
	e.call("POST", "/api/lists/"+itoa(l.ID)+"/tasks", alex, `{"title":"Book the ferry"}`, nil)

	evil := `<script>fetch("/me/token")</script><img src=x onerror=alert(1)>`
	e.form(mallory, "/tasks/1/comments", url.Values{"body": {evil}})

	_, page := e.page(alex, "/lists/"+itoa(l.ID))
	if strings.Contains(page, `<script>fetch`) || strings.Contains(page, `<img src=x`) {
		t.Fatal("a comment's markup reached the page unescaped")
	}
	if !strings.Contains(page, "&lt;script&gt;fetch") {
		t.Error("the comment is not shown at all, rather than shown as text")
	}
}

func TestCommentsRespectWhoCanSeeWhat(t *testing.T) {
	e := newEnv(t)
	alex, sam, carol := e.register("Alex"), e.register("Sam"), e.register("Carol")
	l := e.newList(alex, "Weekend")
	e.call("POST", "/api/join/"+l.InviteCode, sam, "", nil)
	e.call("POST", "/api/lists/"+itoa(l.ID)+"/tasks", alex, `{"title":"Book the ferry"}`, nil)
	var c store.Comment
	e.call("POST", "/api/tasks/1/comments", sam, `{"body":"Friday"}`, &c)

	// Carol is not on the list: to her the task and its comments do not exist.
	want(t, "stranger reads comments", e.call("GET", "/api/tasks/1/comments", carol, "", nil), 404)
	want(t, "stranger comments", e.call("POST", "/api/tasks/1/comments", carol, `{"body":"hi"}`, nil), 404)
	want(t, "stranger comments from a page", e.form(carol, "/tasks/1/comments", url.Values{"body": {"hi"}}).StatusCode, 404)
	want(t, "stranger deletes a comment", e.call("DELETE", "/api/comments/"+itoa(c.ID), carol, "", nil), 404)

	// Alex owns the list but not Sam's words.
	want(t, "owner deletes Sam's comment", e.call("DELETE", "/api/comments/"+itoa(c.ID), alex, "", nil), 403)
	want(t, "owner deletes it from a page", e.form(alex, "/comments/"+itoa(c.ID)+"/delete", url.Values{}).StatusCode, 403)
	want(t, "Sam deletes his own", e.call("DELETE", "/api/comments/"+itoa(c.ID), sam, "", nil), 204)

	// Limits hold through the API as well as the form.
	want(t, "an overlong comment", e.call("POST", "/api/tasks/1/comments", alex,
		`{"body":"`+strings.Repeat("x", 501)+`"}`, nil), 400)
	want(t, "an empty comment", e.call("POST", "/api/tasks/1/comments", alex, `{"body":"  "}`, nil), 400)

	// A comment needs someone to have said it, even on a public list.
	old := e.legacyPublicList("From an old database")
	var task store.Task
	e.call("POST", "/api/lists/"+itoa(old.ID)+"/tasks", "", `{"title":"Paint the shed"}`, &task)
	var refusal struct {
		Error string `json:"error"`
	}
	want(t, "an anonymous comment", e.call("POST", "/api/tasks/"+itoa(task.ID)+"/comments", "", `{"body":"hi"}`, &refusal), 401)
	if !strings.Contains(refusal.Error, "register") {
		t.Errorf("the refusal does not say what to do: %q", refusal.Error)
	}
}

// Everyone on the list sees a new comment appear, like any other change.
func TestACommentReachesEveryoneLive(t *testing.T) {
	e := newEnv(t)
	alex, sam := e.register("Alex"), e.register("Sam")
	l := e.newList(alex, "Weekend")
	e.call("POST", "/api/join/"+l.InviteCode, sam, "", nil)
	e.call("POST", "/api/lists/"+itoa(l.ID)+"/tasks", alex, `{"title":"Book the ferry"}`, nil)

	samEvents, stop := e.events(sam)
	defer stop()
	time.Sleep(100 * time.Millisecond)
	e.call("POST", "/api/tasks/1/comments", alex, `{"body":"Friday?"}`, nil)
	expectSignal(t, "Sam sees Alex's comment arrive", samEvents)
}

func TestWhenReadsLikeAConversation(t *testing.T) {
	now := time.Date(2026, 9, 24, 15, 30, 0, 0, time.Local)
	for _, c := range []struct {
		ago  time.Time
		want string
	}{
		{now.Add(-20 * time.Second), "just now"},
		{now.Add(-5 * time.Minute), "5 min ago"},
		{time.Date(2026, 9, 24, 9, 5, 0, 0, time.Local), "09:05"},
		{time.Date(2026, 9, 23, 21, 40, 0, 0, time.Local), "yesterday 21:40"},
		{time.Date(2026, 9, 2, 12, 0, 0, 0, time.Local), "Sep 2"},
		{time.Date(2025, 12, 31, 12, 0, 0, 0, time.Local), "Dec 31, 2025"},
	} {
		if got := when(c.ago, now); got != c.want {
			t.Errorf("when(%s) = %q, want %q", c.ago.Format(time.DateTime), got, c.want)
		}
	}
}
