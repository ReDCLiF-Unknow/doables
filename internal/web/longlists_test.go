package web

import (
	"fmt"
	"strings"
	"testing"
)

// rows counts the tasks a page shows.
func rows(page string) int { return strings.Count(page, `class="list-group-item task-row`) }

// A list used for a long time is mostly finished tasks. Every row carries its
// own menus and forms, and every change re-sends the page to everyone looking
// at it, so a list shows a page of tasks at a time rather than all of them.
func TestLongListsAreShownAPageAtATime(t *testing.T) {
	e := newEnv(t)
	alex := e.register("Alex")
	l := e.newList(alex, "Shopping")
	for i := 1; i <= 120; i++ {
		title := fmt.Sprintf("Thing %d", i)
		if i%2 == 0 {
			title += " #food"
		}
		task, err := e.st.AddTask(l.ID, 1, title, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if i > 10 { // ten still to buy, the rest bought long ago
			e.st.SetDone(task.ID, true, 1)
		}
	}
	listPage := "/lists/" + itoa(l.ID)

	_, page := e.page(alex, listPage)
	if n := rows(page); n != pageSize {
		t.Fatalf("the list shows %d tasks, want the first %d", n, pageSize)
	}
	for _, want := range []string{
		`<a class="btn" href="` + listPage + `?show=100" data-more>`,
		"Show 50 more", "50 of 120 shown",
		// The open tasks come first, then the most recently finished.
		"Thing 1<", "Thing 119<", // odd numbers have no tag after the title
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the first page does not contain %q", want)
		}
	}
	if strings.Index(page, "Thing 9<") > strings.Index(page, "Thing 119<") {
		t.Error("finished tasks are shown before open ones")
	}
	if strings.Contains(page, "Thing 11<") {
		t.Error("the first page shows the task finished longest ago, not the latest ones")
	}

	_, page = e.page(alex, listPage+"?show=100")
	if n := rows(page); n != 100 || !strings.Contains(page, "Show 20 more") || !strings.Contains(page, "?show=150") {
		t.Errorf("?show=100 shows %d tasks; want 100, and an offer of the last 20", n)
	}
	// Actions return to the longer page, so ticking something on it does not
	// fold the list back up.
	if !strings.Contains(page, `name="next" value="`+listPage+`?show=100"`) {
		t.Error("forms on the longer page do not come back to it")
	}
	_, page = e.page(alex, listPage+"?show=150")
	if n := rows(page); n != 120 || strings.Contains(page, `" data-more>`) {
		t.Errorf("?show=150 shows %d tasks and offers more: %v; want all 120 and no offer", n, strings.Contains(page, `" data-more>`))
	}
	for _, junk := range []string{"-5", "abc", "10"} {
		if _, page = e.page(alex, listPage+"?show="+junk); rows(page) != pageSize {
			t.Errorf("?show=%s shows %d tasks, want the usual %d", junk, rows(page), pageSize)
		}
	}

	// A tag or Open/Done filter is kept when asking for more.
	_, page = e.page(alex, listPage+"?tag=food&filter=done")
	if n := rows(page); n != pageSize || !strings.Contains(page, `href="`+listPage+`?filter=done&amp;tag=food&amp;show=100"`) {
		t.Errorf("done #food tasks: %d shown, want %d with a link that keeps both filters", n, pageSize)
	}
	// A short list offers nothing.
	_, page = e.page(alex, listPage+"?filter=open")
	if rows(page) != 10 || strings.Contains(page, `" data-more>`) {
		t.Error("the ten open tasks should all show, with nothing more to offer")
	}
}
