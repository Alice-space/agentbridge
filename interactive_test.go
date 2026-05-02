package agentbridge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestInteractiveSession_NativeSteer(t *testing.T) {
	driver := newFakeInteractiveDriver(SteerModeNative)
	session := NewInteractiveSession(driver)
	defer session.Close()

	first, err := session.Submit(context.Background(), RunRequest{UserText: "first"})
	if err != nil {
		t.Fatalf("first submit failed: %v", err)
	}
	if first.Mode != SubmitStarted {
		t.Fatalf("first mode = %q, want %q", first.Mode, SubmitStarted)
	}

	second, err := session.Submit(context.Background(), RunRequest{UserText: "second"})
	if err != nil {
		t.Fatalf("second submit failed: %v", err)
	}
	if second.Mode != SubmitSteered {
		t.Fatalf("second mode = %q, want %q", second.Mode, SubmitSteered)
	}
	if len(driver.steered) != 1 || driver.steered[0] != "second" {
		t.Fatalf("steered = %#v, want second", driver.steered)
	}
	if len(driver.started) != 1 {
		t.Fatalf("started = %#v, want only first start", driver.started)
	}
}

func TestInteractiveSession_NativeEnqueue(t *testing.T) {
	driver := newFakeInteractiveDriver(SteerModeNativeEnqueue)
	session := NewInteractiveSession(driver)
	defer session.Close()

	first, err := session.Submit(context.Background(), RunRequest{UserText: "first"})
	if err != nil {
		t.Fatalf("first submit failed: %v", err)
	}
	if first.Mode != SubmitStarted {
		t.Fatalf("first mode = %q, want %q", first.Mode, SubmitStarted)
	}

	second, err := session.Submit(context.Background(), RunRequest{UserText: "second"})
	if err != nil {
		t.Fatalf("second submit failed: %v", err)
	}
	if second.Mode != SubmitSteered {
		t.Fatalf("second mode = %q, want %q", second.Mode, SubmitSteered)
	}
	if len(driver.steered) != 1 || driver.steered[0] != "second" {
		t.Fatalf("steered = %#v, want second", driver.steered)
	}
	if len(driver.started) != 1 {
		t.Fatalf("started = %#v, want only first start", driver.started)
	}
}

func TestInteractiveSession_QueueWhenBusy(t *testing.T) {
	driver := newFakeInteractiveDriver(SteerModeQueueWhenBusy)
	session := NewInteractiveSession(driver)
	defer session.Close()

	first, err := session.Submit(context.Background(), RunRequest{UserText: "first"})
	if err != nil {
		t.Fatalf("first submit failed: %v", err)
	}
	if first.Mode != SubmitStarted {
		t.Fatalf("first mode = %q, want %q", first.Mode, SubmitStarted)
	}

	second, err := session.Submit(context.Background(), RunRequest{UserText: "second"})
	if err != nil {
		t.Fatalf("second submit failed: %v", err)
	}
	if second.Mode != SubmitQueued || second.QueueDepth != 1 {
		t.Fatalf("second result = %#v, want queued depth 1", second)
	}
	if len(driver.steered) != 0 {
		t.Fatalf("steered = %#v, want none", driver.steered)
	}

	driver.emit(TurnEvent{Kind: TurnEventCompleted, ThreadID: "thread_after_first", TurnID: first.TurnID})
	waitFor(t, time.Second, func() bool {
		return len(driver.started) == 2
	}, "queued turn should start")
	if driver.started[1] != "second" {
		t.Fatalf("second started prompt = %q", driver.started[1])
	}
	if driver.startedThreadIDs[1] != "thread_after_first" {
		t.Fatalf("second started thread id = %q, want completed thread id", driver.startedThreadIDs[1])
	}
}

func TestInteractiveSession_SteerWithoutActiveTurn(t *testing.T) {
	driver := newFakeInteractiveDriver(SteerModeNative)
	session := NewInteractiveSession(driver)
	defer session.Close()

	_, err := session.Steer(context.Background(), RunRequest{UserText: "second"})
	if !errors.Is(err, ErrNoActiveTurn) {
		t.Fatalf("Steer error = %v, want ErrNoActiveTurn", err)
	}
}

type fakeInteractiveDriver struct {
	mode             SteerMode
	events           chan TurnEvent
	nextID           int
	started          []string
	startedThreadIDs []string
	steered          []string
}

func newFakeInteractiveDriver(mode SteerMode) *fakeInteractiveDriver {
	return &fakeInteractiveDriver{mode: mode, events: make(chan TurnEvent, 16)}
}

func (d *fakeInteractiveDriver) SteerMode() SteerMode {
	return d.mode
}

func (d *fakeInteractiveDriver) StartTurn(_ context.Context, req RunRequest) (TurnRef, error) {
	d.nextID++
	d.started = append(d.started, req.UserText)
	d.startedThreadIDs = append(d.startedThreadIDs, strings.TrimSpace(req.ThreadID))
	return TurnRef{ThreadID: "thread", TurnID: "turn-" + string(rune('0'+d.nextID))}, nil
}

func (d *fakeInteractiveDriver) SteerTurn(_ context.Context, _ TurnRef, req RunRequest) error {
	d.steered = append(d.steered, req.UserText)
	return nil
}

func (d *fakeInteractiveDriver) InterruptTurn(context.Context, TurnRef) error {
	return nil
}

func (d *fakeInteractiveDriver) Events() <-chan TurnEvent {
	return d.events
}

func (d *fakeInteractiveDriver) Close() error {
	close(d.events)
	return nil
}

func (d *fakeInteractiveDriver) emit(event TurnEvent) {
	d.events <- event
}

func waitFor(t *testing.T, timeout time.Duration, ok func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(message)
}
