package review

import (
	"context"
	"testing"
	"time"
)

func wait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for review job")
	}
}

func TestManagerDeduplicatesDelivery(t *testing.T) {
	m := NewManager(1)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	finished := make(chan struct{}, 1)
	runs := 0
	fn := func(ctx context.Context) {
		runs++
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		finished <- struct{}{}
	}
	if !m.Start(context.Background(), "repo#1", "delivery-1", fn) {
		t.Fatal("first delivery was rejected")
	}
	wait(t, started)
	if m.Start(context.Background(), "repo#1", "delivery-1", fn) {
		t.Fatal("duplicate delivery was accepted")
	}
	close(release)
	wait(t, finished)
	if runs != 1 {
		t.Fatalf("ran %d times", runs)
	}
	if m.Start(context.Background(), "repo#1", "delivery-1", fn) {
		t.Fatal("completed delivery was accepted a second time")
	}
}

func TestManagerCancelsOlderJobForSamePR(t *testing.T) {
	m := NewManager(1)
	started := make(chan struct{}, 2)
	cancelled := make(chan struct{}, 1)
	second := make(chan struct{}, 1)
	m.Start(context.Background(), "repo#2", "first", func(ctx context.Context) { started <- struct{}{}; <-ctx.Done(); cancelled <- struct{}{} })
	wait(t, started)
	m.Start(context.Background(), "repo#2", "second", func(ctx context.Context) { second <- struct{}{} })
	wait(t, cancelled)
	wait(t, second)
}
