package store

import (
	"errors"
	"strings"
	"testing"
)

func TestCommentsAddUpInOrder(t *testing.T) {
	s := openTest(t)
	alex, _, _ := s.CreateUser("Alex")
	sam, _, _ := s.CreateUser("Sam")
	l, _ := s.CreateList("Weekend", alex.ID)
	s.AddMember(l.ID, sam.ID)
	task, _ := s.AddTask(l.ID, alex.ID, "Book the ferry", "", "")

	for _, c := range []struct {
		who  int64
		body string
	}{{alex.ID, "Which day?"}, {sam.ID, "Friday, 7pm"}, {alex.ID, "  Perfect  "}} {
		if _, err := s.AddComment(task.ID, c.who, c.body); err != nil {
			t.Fatal(err)
		}
	}

	cs, err := s.Comments(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range cs {
		got = append(got, c.Author+": "+c.Body)
	}
	want := "Alex: Which day? | Sam: Friday, 7pm | Alex: Perfect"
	if strings.Join(got, " | ") != want {
		t.Errorf("the conversation reads\n  %s\nwant\n  %s", strings.Join(got, " | "), want)
	}

	// The task knows how many there are, wherever it is shown.
	if again, _ := s.Task(task.ID); again.Comments != 3 {
		t.Errorf("the task says it has %d comments, want 3", again.Comments)
	}
}

func TestCommentsHaveLimits(t *testing.T) {
	s := openTest(t)
	alex, _, _ := s.CreateUser("Alex")
	l, _ := s.CreateList("Weekend", alex.ID)
	task, _ := s.AddTask(l.ID, alex.ID, "Book the ferry", "", "")

	for name, body := range map[string]string{
		"empty":          "",
		"only spaces":    "   ",
		"one char over":  strings.Repeat("x", maxCommentLen+1),
		"accented, over": strings.Repeat("é", maxCommentLen+1),
	} {
		if _, err := s.AddComment(task.ID, alex.ID, body); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want ErrInvalid", name, err)
		}
	}
	// Characters, not bytes: 500 accented letters is 1000 bytes and fine.
	if _, err := s.AddComment(task.ID, alex.ID, strings.Repeat("é", maxCommentLen)); err != nil {
		t.Errorf("a comment exactly at the limit was refused: %v", err)
	}
	if _, err := s.AddComment(999, alex.ID, "hello"); !errors.Is(err, ErrNotFound) {
		t.Errorf("commenting on a task that does not exist: got %v", err)
	}
}

// What somebody said is theirs to take back, not anybody else's to erase.
func TestOnlyTheAuthorDeletesAComment(t *testing.T) {
	s := openTest(t)
	alex, _, _ := s.CreateUser("Alex")
	sam, _, _ := s.CreateUser("Sam")
	l, _ := s.CreateList("Weekend", alex.ID)
	s.AddMember(l.ID, sam.ID)
	task, _ := s.AddTask(l.ID, alex.ID, "Book the ferry", "", "")
	c, _ := s.AddComment(task.ID, sam.ID, "Friday, 7pm")

	// Not even the list's owner.
	if err := s.DeleteComment(c.ID, alex.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("the owner deleting Sam's comment: got %v, want ErrForbidden", err)
	}
	if err := s.DeleteComment(c.ID, sam.ID); err != nil {
		t.Fatalf("Sam deleting his own comment: %v", err)
	}
	if again, _ := s.Task(task.ID); again.Comments != 0 {
		t.Errorf("after deleting, the task still counts %d comments", again.Comments)
	}
	if err := s.DeleteComment(c.ID, sam.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting it twice: got %v, want ErrNotFound", err)
	}
}

// A comment belongs to its task: it goes to the trash with it, comes back with
// it, and is gone for good when the task is.
func TestCommentsFollowTheirTask(t *testing.T) {
	s := openTest(t)
	alex, _, _ := s.CreateUser("Alex")
	l, _ := s.CreateList("Weekend", alex.ID)
	task, _ := s.AddTask(l.ID, alex.ID, "Book the ferry", "", "")
	other, _ := s.AddTask(l.ID, alex.ID, "Pack", "", "")
	s.AddComment(task.ID, alex.ID, "Friday")
	s.AddComment(other.ID, alex.ID, "The big bag")

	byTask, _ := s.ListComments(l.ID)
	if len(byTask[task.ID]) != 1 || len(byTask[other.ID]) != 1 {
		t.Fatalf("a list's threads were not grouped by task: %+v", byTask)
	}

	s.DeleteTask(task.ID)
	if byTask, _ = s.ListComments(l.ID); len(byTask[task.ID]) != 0 {
		t.Error("a deleted task's comments still show on its list")
	}
	if _, err := s.RestoreTask(task.ID, alex.ID); err != nil {
		t.Fatal(err)
	}
	if byTask, _ = s.ListComments(l.ID); len(byTask[task.ID]) != 1 {
		t.Error("restoring a task did not bring its comments back")
	}

	// Once the trash is emptied, the comments go too.
	s.DeleteTask(task.ID)
	s.db.Exec(`UPDATE tasks SET deleted_at = datetime('now', '-2 days') WHERE id = ?`, task.ID)
	if err := s.purgeTrash(); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM comments WHERE task_id = ?`, task.ID); n != 0 {
		t.Errorf("%d comment(s) outlived their task", n)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM comments WHERE task_id = ?`, other.ID); n != 1 {
		t.Error("purging one task took another task's comments with it")
	}
}

// If an account disappears, what it said stays, without a name, and nobody
// can delete it on its behalf.
func TestCommentsOutliveTheirAuthor(t *testing.T) {
	s := openTest(t)
	alex, _, _ := s.CreateUser("Alex")
	sam, _, _ := s.CreateUser("Sam")
	l, _ := s.CreateList("Weekend", alex.ID)
	task, _ := s.AddTask(l.ID, alex.ID, "Book the ferry", "", "")
	c, _ := s.AddComment(task.ID, sam.ID, "Friday, 7pm")

	if _, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, sam.ID); err != nil {
		t.Fatal(err)
	}
	cs, _ := s.Comments(task.ID)
	if len(cs) != 1 || cs[0].AuthorID != 0 || cs[0].Author != "" || cs[0].Body != "Friday, 7pm" {
		t.Fatalf("after its author left, the comment is %+v", cs)
	}
	if err := s.DeleteComment(c.ID, 0); !errors.Is(err, ErrForbidden) {
		t.Errorf("deleting an ownerless comment as nobody: got %v, want ErrForbidden", err)
	}
}
