package web

import (
	"net/url"
	"strings"
	"testing"
)

// What other people say should be hard to miss: the task, the list in the
// sidebar and on Overview, and the browser tab all say so until it is read.
func TestNewCommentsStandOutUntilRead(t *testing.T) {
	e := newEnv(t)
	alex, sam := e.register("Alex"), e.register("Sam")
	l := e.newList(alex, "Weekend")
	e.call("POST", "/api/join/"+l.InviteCode, sam, "", nil)
	e.call("POST", "/api/lists/"+itoa(l.ID)+"/tasks", alex, `{"title":"Book the ferry"}`, nil)
	e.call("POST", "/api/tasks/1/comments", sam, `{"body":"Is the <b>7pm</b> one still running?"}`, nil)
	listPage := "/lists/" + itoa(l.ID)

	_, page := e.page(alex, listPage)
	for _, want := range []string{
		`task-row has-unread`,                                // a bar beside the task
		`<summary class="unread-line">`,                      // its thread quotes what is new
		`<strong>Sam</strong> Is the &lt;b&gt;7pm&lt;/b&gt;`, // escaped like everything else
		`<span class="badge bg-blue text-white">1 new</span>`,
		`class="comment comment-new" data-comment="1"`,    // and picks it out inside
		`<span class="unread-dot" title="1 new comment">`, // the sidebar marks the list
		`<title>(1) Weekend · Doables</title>`,            // and so does the browser tab
	} {
		if !strings.Contains(page, want) {
			t.Errorf("Alex's list page does not contain %s", want)
		}
	}
	if _, overview := e.page(alex, "/"); !strings.Contains(overview, "1 new comment</span>") {
		t.Error("Overview does not say the list has a new comment")
	}
	// Sam wrote it, so it is not news to him.
	if _, samsPage := e.page(sam, listPage); strings.Contains(samsPage, "task-row has-unread") || strings.Contains(samsPage, "<title>(1)") {
		t.Error("Sam's own comment is marked new for Sam")
	}

	// Opening the thread marks it read, and Alex's other devices hear so,
	// while Sam's do not.
	alexEvents, stopA := e.events(alex)
	defer stopA()
	samEvents, stopS := e.events(sam)
	defer stopS()
	resp := e.form(alex, "/tasks/1/seen", url.Values{"next": {listPage}})
	want(t, "marking the thread seen", resp.StatusCode, 303)
	expectSignal(t, "Alex's other pages after he read the thread", alexEvents)
	expectQuiet(t, "Sam's pages after Alex read the thread", samEvents)

	_, page = e.page(alex, listPage)
	for _, gone := range []string{"task-row has-unread", "comment comment-new", `class="unread-dot"`, "<title>(1)"} {
		if strings.Contains(page, gone) {
			t.Errorf("after reading, the page still has %s", gone)
		}
	}
	if !strings.Contains(page, "1 comment</summary>") {
		t.Error("a read thread should go back to saying how many comments it has")
	}

	// Nobody can mark a task they cannot see.
	stranger := e.register("Stranger")
	want(t, "a stranger marking a task seen", e.form(stranger, "/tasks/1/seen", url.Values{}).StatusCode, 404)
}
