package store

import (
	"path/filepath"
	"sync"
	"testing"
)

// Every change makes each connected browser fetch its page again, so reads and
// writes genuinely overlap: a household ticking things off at the same time is
// the normal case, not a stress test. This checks that overlapping work does
// not produce "database is locked", and that nothing is lost when it does.
func TestConcurrentReadsAndWrites(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "busy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	u, _, err := s.CreateUser("Alex")
	if err != nil {
		t.Fatal(err)
	}
	l, err := s.CreateList("Weekend", u.ID)
	if err != nil {
		t.Fatal(err)
	}

	const writers, each = 8, 25
	errs := make(chan error, writers*each*3)
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				task, err := s.AddTask(l.ID, u.ID, "Something to do", "", "2026-10-01")
				if err != nil {
					errs <- err
					continue
				}
				// Read while other goroutines are writing.
				if _, err := s.Tasks(l.ID); err != nil {
					errs <- err
				}
				if _, _, err := s.DueCounts(u.ID, "2026-10-01"); err != nil {
					errs <- err
				}
				if _, err := s.SetDone(task.ID, true, u.ID); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)

	failures := 0
	for err := range errs {
		if failures++; failures < 4 {
			t.Errorf("concurrent use failed: %v", err)
		}
	}
	if failures >= 4 {
		t.Errorf("...and %d more failures", failures-3)
	}

	tasks, err := s.Tasks(l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != writers*each {
		t.Errorf("ended up with %d tasks, want %d: writes were lost", len(tasks), writers*each)
	}
	for _, task := range tasks {
		if !task.Done {
			t.Errorf("task %d was added but never finished", task.ID)
			break
		}
	}
}
