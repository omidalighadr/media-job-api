package jobs

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, e := Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func createTest(t *testing.T, s *Store, id string) {
	t.Helper()
	if _, _, e := s.Create(id, "", "hash", []byte("audio"), 100); e != nil {
		t.Fatal(e)
	}
}
func TestIdempotencyAndQueueLimit(t *testing.T) {
	s := openTest(t)
	first, created, e := s.Create("one", "key", "digest", []byte("x"), 1)
	if e != nil || !created {
		t.Fatalf("create: %v", e)
	}
	j, created, e := s.Create("two", "key", "digest", []byte("x"), 1)
	if e != nil || created || j.ID != first.ID {
		t.Fatalf("replay: %v %+v", e, j)
	}
	if _, _, e = s.Create("three", "key", "changed", []byte("y"), 1); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	if _, _, e = s.Create("four", "", "digest", []byte("x"), 1); !errors.Is(e, ErrBusy) {
		t.Fatal(e)
	}
}
func TestCancellationWinsLateCompletion(t *testing.T) {
	s := openTest(t)
	createTest(t, s, "one")
	if _, e := s.Claim(); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Cancel("one"); e != nil {
		t.Fatal(e)
	}
	if e := s.Finish("one", "succeeded", "", []byte(`{"ok":true}`)); e != nil {
		t.Fatal(e)
	}
	j, e := s.Get("one")
	if e != nil || j.Status != "canceled" || len(j.Result) != 0 {
		t.Fatalf("%+v %v", j, e)
	}
	var b []byte
	if e = s.db.QueryRow("SELECT input FROM jobs WHERE id='one'").Scan(&b); e != nil || b != nil {
		t.Fatalf("payload not released: %v", e)
	}
}
func TestExclusiveLockAndCrashRecovery(t *testing.T) {
	dir := t.TempDir()
	s, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	createTest(t, s, "one")
	if _, e = s.Claim(); e != nil {
		t.Fatal(e)
	}
	if second, e := Open(dir); e == nil {
		second.Close()
		t.Fatal("second process acquired same data directory")
	}
	s.Close()
	s, e = Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	j, e := s.Claim()
	if e != nil || j.ID != "one" || string(j.Input) != "audio" {
		t.Fatalf("recovery: %+v %v", j, e)
	}
}
func TestConcurrentClaimsDoNotDuplicate(t *testing.T) {
	s := openTest(t)
	for _, id := range []string{"a", "b", "c", "d"} {
		createTest(t, s, id)
	}
	results := make(chan string, 16)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j, e := s.Claim()
			if e == nil {
				results <- j.ID
			} else if !errors.Is(e, ErrNotFound) {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	close(results)
	seen := map[string]bool{}
	for id := range results {
		if seen[id] {
			t.Fatal("duplicate claim", id)
		}
		seen[id] = true
	}
	if len(seen) != 4 {
		t.Fatal(seen)
	}
}
func TestDeleteRequiresTerminalState(t *testing.T) {
	s := openTest(t)
	createTest(t, s, "one")
	if e := s.Delete("one"); !errors.Is(e, ErrActive) {
		t.Fatal(e)
	}
	s.Cancel("one")
	if e := s.Delete("one"); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Get("one"); !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}
}

type processorFunc func(context.Context, []byte) ([]byte, error)

func (f processorFunc) Verify(c context.Context, b []byte) ([]byte, error) { return f(c, b) }
func waitStatus(t *testing.T, s *Store, id, status string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		j, e := s.Get(id)
		if e == nil && j.Status == status {
			return
		}
		time.Sleep(time.Millisecond * 5)
	}
	j, _ := s.Get(id)
	t.Fatalf("expected %s, got %+v", status, j)
}
func TestWorkerSuccessAndFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			s := openTest(t)
			createTest(t, s, "one")
			p := NewPool(s, processorFunc(func(context.Context, []byte) ([]byte, error) {
				if fail {
					return nil, errors.New("bad audio")
				}
				return []byte(`{"ok":true}`), nil
			}), time.Second)
			ctx, cancel := context.WithCancel(context.Background())
			p.Start(ctx, 2)
			defer func() { cancel(); p.Wait() }()
			status := "succeeded"
			if fail {
				status = "failed"
			}
			waitStatus(t, s, "one", status)
		})
	}
}
func TestWorkerCancelAndTimeout(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "timeout"}[timeout], func(t *testing.T) {
			s := openTest(t)
			createTest(t, s, "one")
			started := make(chan struct{})
			stopped := make(chan struct{})
			p := NewPool(s, processorFunc(func(ctx context.Context, _ []byte) ([]byte, error) {
				close(started)
				<-ctx.Done()
				close(stopped)
				return nil, ctx.Err()
			}), 100*time.Millisecond)
			ctx, cancel := context.WithCancel(context.Background())
			p.Start(ctx, 1)
			defer func() { cancel(); p.Wait() }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("not started")
			}
			status := "failed"
			if !timeout {
				s.Cancel("one")
				p.Cancel("one")
				status = "canceled"
			}
			waitStatus(t, s, "one", status)
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("processor not canceled")
			}
			if timeout {
				j, _ := s.Get("one")
				if j.Error != "verification_timeout" {
					t.Fatal(j)
				}
			}
		})
	}
}
func TestShutdownRequeuesInterruptedWork(t *testing.T) {
	s := openTest(t)
	createTest(t, s, "one")
	started := make(chan struct{})
	p := NewPool(s, processorFunc(func(ctx context.Context, _ []byte) ([]byte, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}), time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx, 1)
	defer func() { cancel(); p.Wait() }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("not started")
	}
	cancel()
	p.Wait()
	j, e := s.Claim()
	if e != nil || string(j.Input) != "audio" {
		t.Fatalf("lost work: %+v %v", j, e)
	}
}

func TestClaimOrderDoesNotDependOnWallClock(t *testing.T) {
	s := openTest(t)
	createTest(t, s, "first")
	createTest(t, s, "second")
	if _, e := s.db.Exec("UPDATE jobs SET created_at='1900-01-01T00:00:00Z' WHERE id='second'"); e != nil {
		t.Fatal(e)
	}
	j, e := s.Claim()
	if e != nil || j.ID != "first" {
		t.Fatalf("claim order: %+v %v", j, e)
	}
}

func TestCancelBetweenClaimAndRegistration(t *testing.T) {
	s := openTest(t)
	createTest(t, s, "one")
	j, e := s.Claim()
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Cancel(j.ID); e != nil {
		t.Fatal(e)
	}
	p := NewPool(s, processorFunc(func(context.Context, []byte) ([]byte, error) { t.Fatal("canceled job was processed"); return nil, nil }), time.Second)
	p.run(context.Background(), j)
	current, e := s.Get(j.ID)
	if e != nil || current.Status != "canceled" {
		t.Fatalf("%+v %v", current, e)
	}
}
