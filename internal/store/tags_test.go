package store

import (
	"reflect"
	"testing"
)

func TestFindingTags(t *testing.T) {
	for _, c := range []struct {
		text string
		want []string
	}{
		{"Buy adapters #shopping", []string{"shopping"}},
		{"#urgent call the bank", []string{"urgent"}},
		{"#Shopping and #shopping, #SHOPPING.", []string{"shopping", "shopping", "shopping"}},
		{"Pack (#travel) and #trip-", []string{"travel", "trip"}},
		{"#flat-move #to_do #2026plan", []string{"flat-move", "to_do", "2026plan"}},
		{"Einkaufen #Getränke #über", []string{"getränke", "über"}},
		// Not tags: numbers, the middle of a word, a link's fragment, a lone #.
		{"Fix issue #12", nil},
		{"Read the C# book", nil},
		{"see example.com/#top", nil},
		{"a#b and ##x and # alone", nil},
		{"&#39; is an apostrophe", nil},
	} {
		var got []string
		for _, m := range FindTags(c.text) {
			got = append(got, m.Tag)
			if want := "#" + m.Tag; len(c.text[m.Start:m.End]) != len(want) {
				t.Errorf("%q: the mark for %q covers %q", c.text, m.Tag, c.text[m.Start:m.End])
			}
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("FindTags(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}

func TestNormalizingATag(t *testing.T) {
	for in, want := range map[string]string{"shopping": "shopping", "#Shopping": "shopping", " #flat-move ": "flat-move"} {
		if got, ok := NormalizeTag(in); !ok || got != want {
			t.Errorf("NormalizeTag(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "#", "12", "two words", "#a#b", "shop!"} {
		if got, ok := NormalizeTag(in); ok {
			t.Errorf("NormalizeTag(%q) accepted it as %q", in, got)
		}
	}
}

// Tags come from the title and the description, and are always up to date,
// because there is nothing to keep in step: they are read from the words.
func TestTasksKnowTheirTags(t *testing.T) {
	s := openTest(t)
	u, _, _ := s.CreateUser("Alex")
	l, _ := s.CreateList("Home", u.ID)
	task, err := s.AddTask(l.ID, u.ID, "Buy adapters #Shopping", "for the #trip, and more #shopping", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"shopping", "trip"}; !reflect.DeepEqual(task.Tags, want) {
		t.Errorf("the task's tags are %q, want %q", task.Tags, want)
	}
	title := "Buy adapters"
	task, _ = s.UpdateTask(task.ID, TaskUpdate{Title: &title})
	if want := []string{"shopping", "trip"}; !reflect.DeepEqual(task.Tags, want) {
		t.Errorf("after the title lost its tag: %q, want %q (still in the description)", task.Tags, want)
	}
	desc := ""
	task, _ = s.UpdateTask(task.ID, TaskUpdate{Description: &desc})
	if task.Tags != nil {
		t.Errorf("a task with no #words still has tags %q", task.Tags)
	}

	s.AddTask(l.ID, u.ID, "Milk #shopping", "", "")
	s.AddTask(l.ID, u.ID, "Call mum", "", "")
	tasks, _ := s.Tasks(l.ID)
	if got := WithTag(tasks, "shopping"); len(got) != 1 || got[0].Title != "Milk #shopping" {
		t.Errorf("WithTag kept %+v", got)
	}
	s.AddTask(l.ID, u.ID, "Soap #shopping #bathroom", "", "")
	tasks, _ = s.Tasks(l.ID)
	if got, want := CountTags(tasks), []TagCount{{"bathroom", 1}, {"shopping", 2}}; !reflect.DeepEqual(got, want) {
		t.Errorf("CountTags = %+v, want %+v", got, want)
	}
}
