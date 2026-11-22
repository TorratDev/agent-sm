// Tests for the job scheduler in runner.go (Enqueue, loop, slots, Stop). The file is not named
// runner_test.go so that scripts/test-affected.sh keeps running the full suite when runner.go changes.
package main

import (
	"slices"
	"sync"
	"testing"
	"testing/synctest"
)

// schedRunner returns a runner whose jobs block until finish(issue) ends the running job for
// that issue. Call it inside a synctest bubble: finish fails the bubble with a deadlock if no
// job for that issue is running.
func schedRunner(limit int) (r *Runner, finish func(issue int)) {
	var mu sync.Mutex
	gates := map[int]chan struct{}{}
	gate := func(n int) chan struct{} {
		mu.Lock()
		defer mu.Unlock()
		if gates[n] == nil {
			gates[n] = make(chan struct{})
		}
		return gates[n]
	}
	r = &Runner{slots: newSlots(limit)}
	r.run = func(j *Job) { <-gate(j.Issue) }
	return r, func(n int) { gate(n) <- struct{}{} }
}

func issues(ns ...int) []Job {
	jobs := make([]Job, len(ns))
	for i, n := range ns {
		jobs[i].Issue = n
	}
	return jobs
}

// wantSched waits until every scheduler goroutine is blocked, then checks the running issues,
// the queue length and whether the queue loop is alive (it exits once nothing is queued,
// while started jobs keep running).
func wantSched(t *testing.T, r *Runner, running []int, queued int, working bool) {
	t.Helper()
	synctest.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	var got []int
	for _, j := range r.running {
		got = append(got, j.Issue)
	}
	slices.Sort(got)
	if !slices.Equal(got, running) || len(r.queue) != queued || r.working != working {
		t.Fatalf("running %v, queued %d, working %v; want %v, %d, %v", got, len(r.queue), r.working, running, queued, working)
	}
}

func TestSchedulerRespectsSlotLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, finish := schedRunner(2)
		r.Enqueue(issues(1, 2, 3, 4))
		wantSched(t, r, []int{1, 2}, 2, true)
		finish(1)
		wantSched(t, r, []int{2, 3}, 1, true)
		finish(2)
		finish(3)
		wantSched(t, r, []int{4}, 0, false) // the queue loop exits once nothing is queued
		finish(4)
		wantSched(t, r, nil, 0, false)
		if len(r.history) != 4 {
			t.Fatalf("history has %d jobs, want 4", len(r.history))
		}
	})
}

func TestSchedulerNeverRunsAnIssueTwiceAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, finish := schedRunner(3) // a free slot must not let the second job for #1 start early
		r.Enqueue(issues(1, 1, 2))
		wantSched(t, r, []int{1, 2}, 1, true)
		finish(1)
		wantSched(t, r, []int{1, 2}, 0, false)
		finish(1)
		finish(2)
		wantSched(t, r, nil, 0, false)
	})
}

func TestSchedulerResize(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, finish := schedRunner(1)
		r.Enqueue(issues(1, 2, 3))
		wantSched(t, r, []int{1}, 2, true)
		r.slots.resize(3) // raising the limit starts waiting jobs at once
		wantSched(t, r, []int{1, 2, 3}, 0, false)

		r.slots.resize(1) // lowering it lets running jobs finish and holds new ones back
		r.Enqueue(issues(4))
		wantSched(t, r, []int{1, 2, 3}, 1, true)
		finish(1)
		wantSched(t, r, []int{2, 3}, 1, true)
		finish(2)
		wantSched(t, r, []int{3}, 1, true)
		finish(3)
		wantSched(t, r, []int{4}, 0, false)
		finish(4)
		wantSched(t, r, nil, 0, false)
	})
}

func TestSchedulerStopDropsTheQueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, finish := schedRunner(1)
		r.Enqueue(issues(1, 2, 3))
		wantSched(t, r, []int{1}, 2, true)
		r.Stop()
		wantSched(t, r, []int{1}, 0, true)
		finish(1) // the loop wakes for the freed slot, finds nothing queued and exits
		wantSched(t, r, nil, 0, false)
		if len(r.history) != 1 {
			t.Fatalf("history has %d jobs, want 1: queued jobs ran after Stop", len(r.history))
		}
	})
}
