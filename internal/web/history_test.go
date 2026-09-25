package web

import (
	"strings"
	"testing"
	"time"

	"doables/internal/store"
)

// A finished task stays on To do for a day, so everyone sees it was done and a
// mistaken tick can be taken back, then moves to History. Nothing is deleted.
func TestFinishedTasksMoveToHistoryADayLater(t *testing.T) {
	e := newEnv(t)
	alex := e.register("Alex")
	l := e.newList(alex, "Weekly shop")
	tasksAt := "/api/lists/" + itoa(l.ID) + "/tasks"
	e.call("POST", tasksAt, alex, `{"title":"Milk"}`, nil)
	e.call("POST", tasksAt, alex, `{"title":"Bread"}`, nil)
	e.call("PATCH", "/api/tasks/1", alex, `{"done":true}`, nil)
	listPage := "/lists/" + itoa(l.ID)
	later := func(d time.Duration) { e.app.now = func() time.Time { return time.Now().Add(d) } }

	// Just ticked: still on To do, under a heading that says where it is going.
	_, page := e.page(alex, listPage)
	if !strings.Contains(page, "Milk") || !strings.Contains(page, "Finished in the last day") {
		t.Error("a task ticked just now is not on To do, under its own heading")
	}
	if strings.Index(page, "Bread") > strings.Index(page, "Finished in the last day") {
		t.Error("the open task is not above the finished ones")
	}
	// History has everything finished, including today's.
	_, page = e.page(alex, listPage+"?filter=history")
	if !strings.Contains(page, "Milk") || strings.Contains(page, "Bread") || !strings.Contains(page, `list-day">Today<`) {
		t.Error("History should hold Milk, finished today, and not the open Bread")
	}

	later(23 * time.Hour)
	if _, page = e.page(alex, listPage); !strings.Contains(page, "Milk") {
		t.Error("a task finished 23 hours ago has already left To do")
	}
	later(25 * time.Hour)
	_, page = e.page(alex, listPage)
	if strings.Contains(page, "Milk") || !strings.Contains(page, "Bread") || strings.Contains(page, "Finished in the last day") {
		t.Error("a day later, To do should hold only Bread")
	}
	for _, want := range []string{`To do <span class="badge bg-secondary-lt ms-1">1</span>`, `History <span class="badge bg-secondary-lt ms-1">1</span>`} {
		if !strings.Contains(page, want) {
			t.Errorf("the tabs do not say %s", want)
		}
	}
	_, page = e.page(alex, listPage+"?filter=history")
	if !strings.Contains(page, "Milk") || !strings.Contains(page, `list-day">Yesterday<`) {
		t.Error("a day later, Milk is not in History under Yesterday")
	}
	// The old Done tab's links still work, and open History.
	if _, old := e.page(alex, listPage+"?filter=done"); !strings.Contains(old, `list-day">Yesterday<`) {
		t.Error("?filter=done does not open History")
	}

	// Unticking it in History puts it straight back on To do.
	e.call("PATCH", "/api/tasks/1", alex, `{"done":false}`, nil)
	if _, page = e.page(alex, listPage); !strings.Contains(page, "Milk") {
		t.Error("an unticked task did not come back to To do")
	}

	// Once everything is done and a day old, To do says where it all went.
	e.call("PATCH", "/api/tasks/1", alex, `{"done":true}`, nil)
	e.call("PATCH", "/api/tasks/2", alex, `{"done":true}`, nil)
	later(25 * time.Hour)
	_, page = e.page(alex, listPage)
	if !strings.Contains(page, "All caught up!") || !strings.Contains(page, `href="`+listPage+`?filter=history"`) {
		t.Error("an emptied To do does not point to History")
	}
}

func TestTheAPIGivesEitherTab(t *testing.T) {
	e := newEnv(t)
	alex := e.register("Alex")
	l := e.newList(alex, "Home")
	tasksAt := "/api/lists/" + itoa(l.ID) + "/tasks"
	e.call("POST", tasksAt, alex, `{"title":"Open"}`, nil)
	e.call("POST", tasksAt, alex, `{"title":"Done long ago"}`, nil)
	e.call("PATCH", "/api/tasks/2", alex, `{"done":true}`, nil)
	e.app.now = func() time.Time { return time.Now().Add(48 * time.Hour) }

	titles := func(query string) string {
		var ts []store.Task
		want(t, "tasks"+query, e.call("GET", tasksAt+query, alex, "", &ts), 200)
		var names []string
		for _, task := range ts {
			names = append(names, task.Title)
		}
		return strings.Join(names, ", ")
	}
	for query, want := range map[string]string{
		"":              "Open, Done long ago", // unchanged for anything already using it
		"?view=todo":    "Open",
		"?view=history": "Done long ago",
	} {
		if got := titles(query); got != want {
			t.Errorf("tasks%s = %q, want %q", query, got, want)
		}
	}
	var ts []store.Task
	e.call("GET", tasksAt+"?view=history", alex, "", &ts)
	if len(ts) != 1 || ts[0].DoneAt == nil {
		t.Errorf("a finished task does not say when it was finished: %+v", ts)
	}
	want(t, "an unknown view", e.call("GET", tasksAt+"?view=done", alex, "", nil), 400)
}

func TestOnToDo(t *testing.T) {
	now := time.Now()
	at := func(ago time.Duration) *time.Time { t := now.Add(-ago); return &t }
	for _, c := range []struct {
		task store.Task
		want bool
	}{
		{store.Task{}, true},
		{store.Task{Done: true, DoneAt: at(time.Minute)}, true},
		{store.Task{Done: true, DoneAt: at(FinishedStays - time.Second)}, true},
		{store.Task{Done: true, DoneAt: at(FinishedStays)}, false},
		// Finished before Doables recorded when: long gone.
		{store.Task{Done: true}, false},
	} {
		if got := onToDo(c.task, now); got != c.want {
			t.Errorf("onToDo(done=%v, at=%v) = %v", c.task.Done, c.task.DoneAt, got)
		}
	}
}

func TestDayTitles(t *testing.T) {
	now := time.Date(2026, 9, 24, 9, 0, 0, 0, time.Local) // a Thursday
	for at, want := range map[time.Time]string{
		time.Date(2026, 9, 24, 0, 5, 0, 0, time.Local):   "Today",
		time.Date(2026, 9, 23, 23, 55, 0, 0, time.Local): "Yesterday",
		time.Date(2026, 9, 21, 12, 0, 0, 0, time.Local):  "Monday",
		time.Date(2026, 9, 17, 12, 0, 0, 0, time.Local):  "Thu, Sep 17",
		time.Date(2025, 12, 31, 12, 0, 0, 0, time.Local): "Dec 31, 2025",
	} {
		if got := dayTitle(at, now); got != want {
			t.Errorf("dayTitle(%v) = %q, want %q", at, got, want)
		}
	}
	tasks := []store.Task{{ID: 1, DoneAt: &now}, {ID: 2, DoneAt: &now}, {ID: 3}}
	groups := byDayFinished(tasks, now)
	if len(groups) != 2 || groups[0].Title != "Today" || len(groups[0].Tasks) != 2 || groups[1].Title != "Earlier" {
		t.Errorf("byDayFinished gave %+v", groups)
	}
}
