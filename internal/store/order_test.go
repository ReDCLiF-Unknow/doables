package store

import (
	"strings"
	"testing"
)

// Finished tasks come most recently finished first, and unticking one puts it
// back among the open ones.
func TestFinishedTasksAreNewestFirst(t *testing.T) {
	s := openTest(t)
	u, _, _ := s.CreateUser("Alex")
	l, _ := s.CreateList("Home", u.ID)
	add := func(title string) int64 {
		task, err := s.AddTask(l.ID, u.ID, title, "", "")
		if err != nil {
			t.Fatal(err)
		}
		return task.ID
	}
	// Tasks finished before the finish time was recorded have none, and go
	// last, newest first.
	s.SetDone(add("Old A"), true, u.ID)
	s.SetDone(add("Old B"), true, u.ID)
	if _, err := s.db.Exec(`UPDATE tasks SET done_at = NULL`); err != nil {
		t.Fatal(err)
	}
	first, second, third := add("First"), add("Second"), add("Third")
	s.SetDone(second, true, u.ID)
	s.SetDone(third, true, u.ID)
	s.SetDone(first, true, u.ID)
	add("Still to do")

	order := func() string {
		tasks, err := s.Tasks(l.ID)
		if err != nil {
			t.Fatal(err)
		}
		var titles []string
		for _, task := range tasks {
			titles = append(titles, task.Title)
		}
		return strings.Join(titles, ", ")
	}
	if got, want := order(), "Still to do, First, Third, Second, Old B, Old A"; got != want {
		t.Errorf("the tasks come back as %s; want %s", got, want)
	}
	s.SetDone(third, false, u.ID)
	if got, want := order(), "Third, Still to do, First, Second, Old B, Old A"; got != want {
		t.Errorf("after unticking Third: %s; want %s", got, want)
	}
	var doneAt *string
	s.db.QueryRow(`SELECT done_at FROM tasks WHERE id = ?`, third).Scan(&doneAt)
	if doneAt != nil {
		t.Errorf("an unticked task still has a finish time, %s", *doneAt)
	}
}
