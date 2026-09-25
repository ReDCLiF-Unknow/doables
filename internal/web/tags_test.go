package web

import (
	"net/url"
	"strings"
	"testing"

	"doables/internal/store"
)

func TestTagsFilterAList(t *testing.T) {
	e := newEnv(t)
	alex := e.register("Alex")
	l := e.newList(alex, "Weekend")
	tasksAt := "/api/lists/" + itoa(l.ID) + "/tasks"
	e.call("POST", tasksAt, alex, `{"title":"Buy adapters #Shopping"}`, nil)
	e.call("POST", tasksAt, alex, `{"title":"Sunscreen","description":"the big one #shopping #beach"}`, nil)
	e.call("POST", tasksAt, alex, `{"title":"Call the hotel"}`, nil)
	e.call("PATCH", "/api/tasks/2", alex, `{"done":true}`, nil)
	listPage := "/lists/" + itoa(l.ID)

	// A tag in a title is a link to the list showing only that tag, and the
	// list says which tags it has, however each was written.
	_, page := e.page(alex, listPage)
	for _, want := range []string{
		`<a class="hashtag" href="` + listPage + `?tag=shopping" title="Show only tasks tagged #Shopping">#Shopping</a>`,
		`#beach<span class="tag-count">1</span>`,
		`#shopping<span class="tag-count">2</span>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the list page does not contain %s", want)
		}
	}

	// Filtered, it shows the tagged tasks, counts only them, and keeps every
	// tag above the list so you can jump to another one.
	_, page = e.page(alex, listPage+"?tag=SHOPPING")
	if !strings.Contains(page, "Buy adapters") || !strings.Contains(page, "Sunscreen") || strings.Contains(page, "Call the hotel") {
		t.Error("?tag=shopping did not show exactly the two shopping tasks")
	}
	for _, want := range []string{
		`To do <span class="badge bg-secondary-lt ms-1">1</span>`,
		`History <span class="badge bg-secondary-lt ms-1">1</span>`,
		`href="` + listPage + `?filter=history&amp;tag=shopping"`, // the History tab keeps the tag
		`href="` + listPage + `?tag=beach"`,                       // other tags are still offered
		`<a class="hashtag active" href="` + listPage + `"`,       // and a second click undoes the filter
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the filtered page does not contain %s", want)
		}
	}
	// History narrows it again, to the finished ones.
	_, page = e.page(alex, listPage+"?tag=shopping&filter=history")
	if strings.Contains(page, "Buy adapters") || !strings.Contains(page, "Sunscreen") {
		t.Error("finished tasks tagged #shopping should be just the sunscreen")
	}
	// A tag nobody uses any more says so rather than showing an empty list.
	_, page = e.page(alex, listPage+"?tag=gone")
	if !strings.Contains(page, "Nothing tagged #gone") {
		t.Error("a tag with no tasks does not say so")
	}
	// Nonsense in the address is ignored rather than shown as a tag.
	_, page = e.page(alex, listPage+"?tag="+url.QueryEscape("<b>"))
	if !strings.Contains(page, "Call the hotel") {
		t.Error("an invalid tag in the address filtered the list")
	}

	// Elsewhere, a tag leads to the task's own list.
	e.call("PATCH", "/api/tasks/1", alex, `{"assignee_id":1}`, nil)
	_, mine := e.page(alex, "/mine")
	if !strings.Contains(mine, `href="`+listPage+`?tag=shopping"`) {
		t.Error("My tasks does not link a task's tag to its list")
	}
}

// A task added while only #shopping is showing would disappear the moment it
// was added, so it is given the tag.
func TestAddingATaskWhileFilteredTagsIt(t *testing.T) {
	e := newEnv(t)
	alex := e.register("Alex")
	l := e.newList(alex, "Home")
	listPage := "/lists/" + itoa(l.ID)
	filtered := listPage + "?tag=shopping"

	_, page := e.page(alex, filtered)
	if !strings.Contains(page, `<input type="hidden" name="tag" value="shopping">`) {
		t.Fatal("the add form on a filtered list does not carry the tag")
	}
	add := func(title, desc string) {
		t.Helper()
		resp := e.form(alex, listPage+"/tasks", url.Values{"title": {title}, "description": {desc}, "tag": {"shopping"}, "next": {filtered}})
		want(t, "adding a task", resp.StatusCode, 303)
	}
	add("Milk", "")
	add("Bread #Shopping", "")          // already has it, whatever the case
	add("Eggs", "free range #shopping") // or has it in the description
	add("Soap  ", "")

	var titles []string
	for _, task := range e.tasks(alex, l.ID) {
		titles = append(titles, task.Title)
		if !task.HasTag("shopping") {
			t.Errorf("%q was added while #shopping was showing, but is not tagged", task.Title)
		}
	}
	if got := strings.Join(titles, " | "); got != "Milk #shopping | Bread #Shopping | Eggs | Soap #shopping" {
		t.Errorf("the tasks are titled %s", got)
	}

	// An unfiltered list adds exactly what was typed.
	e.form(alex, listPage+"/tasks", url.Values{"title": {"Call mum"}})
	if ts := e.tasks(alex, l.ID); ts[len(ts)-1].Title != "Call mum" {
		t.Errorf("a task added without a filter became %q", ts[len(ts)-1].Title)
	}
}

// A tag becomes a link, and nothing around it may become markup.
func TestTaggedTextIsEscaped(t *testing.T) {
	e := newEnv(t)
	alex := e.register("Alex")
	l := e.newList(alex, "Home")
	e.call("POST", "/api/lists/"+itoa(l.ID)+"/tasks", alex,
		`{"title":"<img src=x onerror=alert(1)> #a\"onclick=alert(1) #ok<b>"}`, nil)
	_, page := e.page(alex, "/lists/"+itoa(l.ID))
	if strings.Contains(page, "<img src=x") || strings.Contains(page, "<b>") || strings.Contains(page, `"onclick`) {
		t.Fatal("markup in a tagged title reached the page unescaped")
	}
	for _, want := range []string{`&lt;img src=x onerror=alert(1)&gt; `, `>#a</a>&#34;onclick=alert(1) `, `>#ok</a>&lt;b&gt;`} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not contain %s", want)
		}
	}
}

func TestTheAPIFiltersByTag(t *testing.T) {
	e := newEnv(t)
	alex := e.register("Alex")
	l := e.newList(alex, "Home")
	tasksAt := "/api/lists/" + itoa(l.ID) + "/tasks"
	e.call("POST", tasksAt, alex, `{"title":"Milk #shopping"}`, nil)
	e.call("POST", tasksAt, alex, `{"title":"Call mum"}`, nil)

	var all, tagged []store.Task
	want(t, "all tasks", e.call("GET", tasksAt, alex, "", &all), 200)
	if len(all) != 2 || len(all[0].Tags) != 1 || all[0].Tags[0] != "shopping" || all[1].Tags != nil {
		t.Errorf("the API gave %+v; want the first task tagged shopping, the second untagged", all)
	}
	want(t, "tasks tagged #shopping", e.call("GET", tasksAt+"?tag=%23Shopping", alex, "", &tagged), 200)
	if len(tagged) != 1 || tagged[0].Title != "Milk #shopping" {
		t.Errorf("?tag=#Shopping gave %+v", tagged)
	}
	want(t, "an invalid tag", e.call("GET", tasksAt+"?tag=two+words", alex, "", nil), 400)
}
