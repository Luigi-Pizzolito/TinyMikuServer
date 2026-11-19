package holocropper

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

/* Task foundation tests (stories A1/A2 spinner contract).

A fake runner stands in for a real pass: it observes the Task context and
calls finish() with whatever error it saw, exactly like runAnalyze and the
render runners do. Everything here is pure — no ffmpeg, no video. */

// fakePass models a pipeline runner goroutine: begin, open a phase, then
// block on gate. The test closes gate when it wants the pass to end, and the
// pass finishes with the context error it observes - exactly how a real
// runner notices cancellation between frames. Deterministic by construction.
func fakePass(tb *taskBase, gate <-chan struct{}, fail error) {
	tb.begin()
	tb.setPhase(PhaseDetect, 1)
	<-gate
	if err := tb.ctx.Err(); err != nil {
		tb.finish(err)
		return
	}
	tb.finish(fail)
}

func fakeObserver(tasks *[]TaskInfo, mu *sync.Mutex) *Observer {
	return &Observer{
		OnTask: func(info TaskInfo) {
			mu.Lock()
			defer mu.Unlock()
			*tasks = append(*tasks, info)
		},
	}
}

// A newer Task in the same slot supersedes the running one: the old Task
// reaches the superseded terminal state, its Wait reports *SupersededError
// carrying the new ID, and no "already busy" error is ever returned.
func TestTaskSupersedeLastCallWins(t *testing.T) {
	var slot taskSlot
	ctx := context.Background()

	t1 := newTaskBase(ctx, TaskAnalyze, nil)
	slot.install(t1)
	gate := make(chan struct{})
	go fakePass(t1, gate, nil)

	t2 := newTaskBase(ctx, TaskAnalyze, nil)
	prev := slot.install(t2)
	close(gate) // t1's runner now observes the cancellation
	if prev != t1 {
		t.Fatalf("install must report the superseded task, got %v", prev)
	}

	err := t1.waitTerminal()
	var se *SupersededError
	if !errors.As(err, &se) {
		t.Fatalf("want *SupersededError, got %v", err)
	}
	if se.NewTaskID != t2.ID() {
		t.Errorf("SupersededError.NewTaskID = %d, want %d", se.NewTaskID, t2.ID())
	}
	if got := t1.State(); got != StateSuperseded {
		t.Errorf("old task state = %s, want %s", got, StateSuperseded)
	}
	if info := t1.Progress(); info.SupersededTo != t2.ID() {
		t.Errorf("old task snapshot SupersededTo = %d, want %d", info.SupersededTo, t2.ID())
	}

	// The superseded runner's own context error is swallowed by the state
	// precedence: superseded beats cancelled beats failed.
	finished := make(chan struct{})
	close(finished) // a pass that runs to the end
	fakePass(t2, finished, nil)
	if err := t2.waitTerminal(); err != nil {
		t.Fatalf("superseding task should complete normally, got %v", err)
	}
	if t2.State() != StateCompleted {
		t.Errorf("new task state = %s, want %s", t2.State(), StateCompleted)
	}

	// A terminal Task cannot be superseded again, and re-installing the same
	// Task in its own slot is a no-op.
	if prev := slot.install(t2); prev != nil {
		t.Errorf("reinstalling the current task must not report a supersede, got %v", prev)
	}
	if slot.install(newTaskBase(ctx, TaskAnalyze, nil)) == nil {
		t.Error("installing over a terminal task must report it")
	}
	if err := t2.waitTerminal(); err != nil {
		t.Errorf("terminal task wait must stay stable, got %v", err)
	}
}

// Cancel() reaches the cancelled terminal state; so does the caller's own
// context being cancelled (ffmpeg kill / detect loop exit), and Wait returns
// ErrCancelled in both cases.
func TestTaskCancelAndCallerContext(t *testing.T) {
	t.Run("Cancel", func(t *testing.T) {
		tb := newTaskBase(context.Background(), TaskPreview, nil)
		gate := make(chan struct{})
		go fakePass(tb, gate, nil)
		tb.Cancel()
		close(gate) // the runner wakes and sees the cancelled context

		if err := tb.waitTerminal(); !errors.Is(err, ErrCancelled) {
			t.Fatalf("want ErrCancelled, got %v", err)
		}
		if got := tb.State(); got != StateCancelled {
			t.Errorf("state = %s, want %s", got, StateCancelled)
		}
		// Cancel on a terminal task is a no-op (no double close, no state churn).
		tb.Cancel()
		if tb.State() != StateCancelled {
			t.Errorf("state after redundant Cancel = %s", tb.State())
		}
	})

	t.Run("CallerContext", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		tb := newTaskBase(ctx, TaskRenderFinal, nil)
		gate := make(chan struct{})
		go fakePass(tb, gate, nil)
		cancel() // the app cancels its own context, not the Task
		close(gate)

		if err := tb.waitTerminal(); !errors.Is(err, ErrCancelled) {
			t.Fatalf("caller context cancel: want ErrCancelled, got %v", err)
		}
		if tb.State() != StateCancelled {
			t.Errorf("state = %s, want %s", tb.State(), StateCancelled)
		}
	})

	// A runner failure reaches failed and surfaces its own error.
	t.Run("Failure", func(t *testing.T) {
		boom := errors.New("ffmpeg exploded")
		tb := newTaskBase(context.Background(), TaskRenderFinal, nil)
		finished := make(chan struct{})
		close(finished)
		fakePass(tb, finished, boom)
		err := tb.waitTerminal()
		if !errors.Is(err, boom) {
			t.Fatalf("want the runner error, got %v", err)
		}
		if tb.State() != StateFailed {
			t.Errorf("state = %s, want %s", tb.State(), StateFailed)
		}
		if info := tb.Progress(); info.Err != boom.Error() {
			t.Errorf("snapshot Err = %q, want %q", info.Err, boom.Error())
		}
	})
}

// Progress/State queries are cheap and non-blocking from any goroutine, and
// each phase carries its own totals (a bar consumer never reconciles phases).
func TestTaskProgressSnapshotPerPhase(t *testing.T) {
	tb := newTaskBase(context.Background(), TaskAnalyze, nil)
	if info := tb.Progress(); info.State != StateQueued || info.Done != 0 || info.Total != 0 {
		t.Errorf("fresh task snapshot = %+v", info)
	}
	tb.begin()
	tb.setPhase(PhasePrepare, 100)
	tb.setDone(40)
	tb.setDone(30) // never goes backwards
	info := tb.Progress()
	if info.Phase != PhasePrepare || info.Done != 40 || info.Total != 100 {
		t.Errorf("prepare snapshot = %+v", info)
	}
	if info.ID != tb.ID() || info.Kind != TaskAnalyze {
		t.Errorf("snapshot identity mismatch: %+v", info)
	}

	tb.setPhase(PhaseDetect, 100)
	tb.setDone(7)
	info = tb.Progress()
	if info.Phase != PhaseDetect || info.Done != 7 || info.Total != 100 {
		t.Errorf("detect snapshot = %+v (totals must reset per phase)", info)
	}
	if info.ElapsedSeconds < 0 {
		t.Errorf("elapsed = %v", info.ElapsedSeconds)
	}

	// Concurrent queries while the pass runs must not block or race.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = tb.Progress()
				_ = tb.State()
			}
		}()
	}
	wg.Wait()
}

// Observer.OnTask: state/phase/terminal events are immediate, plain progress
// advances are throttled to progressEmitInterval (spinner-friendly, <= 4 Hz).
func TestTaskObserverThrottling(t *testing.T) {
	var mu sync.Mutex
	var events []TaskInfo
	tb := newTaskBase(context.Background(), TaskAnalyze, fakeObserver(&events, &mu))

	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(events)
	}

	if n := count(); n != 1 {
		t.Fatalf("creation must publish one queued event, got %d", n)
	}
	tb.begin()
	tb.setPhase(PhasePrepare, 1000)
	tb.setPhase(PhaseDetect, 1000) // phase changes are never throttled
	if n := count(); n != 4 {
		t.Fatalf("begin + two phase changes must each emit, got %d events", n)
	}

	// 200 rapid progress advances inside the throttle window: at most one
	// gets through.
	for i := 1; i <= 200; i++ {
		tb.setDone(i)
	}
	burst := count()
	if burst > 5 {
		t.Errorf("progress burst emitted %d events, want <= 5 (creation+begin+2 phases+1)", burst)
	}

	// After the interval, progress flows again; the terminal event is forced.
	time.Sleep(progressEmitInterval + 30*time.Millisecond)
	tb.setDone(201)
	tb.setDone(202)
	if n := count(); n < burst+1 {
		t.Errorf("progress after the throttle window must emit, events %d -> %d", burst, n)
	}
	tb.finish(nil)
	if n := count(); n != burst+2 { // one allowed advance + the forced terminal event
		t.Errorf("terminal event must emit exactly once: got %d events", n)
	}

	mu.Lock()
	defer mu.Unlock()
	last := events[len(events)-1]
	if last.State != StateCompleted {
		t.Errorf("last event state = %s, want %s", last.State, StateCompleted)
	}
	// Events are ordered snapshots of the same Task.
	for _, e := range events {
		if e.ID != tb.ID() {
			t.Fatalf("event for a different task: %+v", e)
		}
	}
}

// Warning events reach Observer.OnWarning (ffmpeg diagnostics feed).
func TestTaskObserverWarning(t *testing.T) {
	var got atomic.Value
	tb := newTaskBase(context.Background(), TaskAnalyze, &Observer{
		OnWarning: func(m string) { got.Store(m) },
	})
	tb.warn("bad input stream")
	if v, _ := got.Load().(string); v != "bad input stream" {
		t.Errorf("OnWarning got %q", v)
	}

	// A Task with no observer must not panic.
	quiet := newTaskBase(context.Background(), TaskAnalyze, nil)
	quiet.warn("ignored")
	quiet.finish(nil)
}

// Slot bookkeeping: current() keeps the latest task (terminal or not, so a
// UI can show the final state), live() only reports a non-terminal one
// (the busy indicator Status() shows).
func TestTaskSlotLiveAndCurrent(t *testing.T) {
	var slot taskSlot
	if slot.current() != nil || slot.live() != nil {
		t.Fatal("unused slot must report nothing")
	}

	t1 := newTaskBase(context.Background(), TaskAnalyze, nil)
	slot.install(t1)
	if slot.current() != t1 || slot.live() != t1 {
		t.Fatal("running task must be both current and live")
	}
	finished := make(chan struct{})
	close(finished)
	fakePass(t1, finished, nil)
	if slot.current() != t1 {
		t.Error("completed task must stay current until replaced")
	}
	if slot.live() != nil {
		t.Error("completed task must not be live")
	}

	t2 := newTaskBase(context.Background(), TaskAnalyze, nil)
	slot.install(t2)
	if slot.current() != t2 || slot.live() != t2 {
		t.Fatal("new task must take the slot")
	}
	t2.Cancel()
	t2.finish(context.Canceled)
	if slot.live() != nil {
		t.Error("cancelled task must not be live")
	}
}

// Task IDs come from a package-wide counter: unique across every slot and
// pass kind, so a UI can key spinners by ID alone.
func TestTaskIDsAreUnique(t *testing.T) {
	const n = 200
	seen := make(map[int]bool, n)
	for i := 0; i < n; i++ {
		tb := newTaskBase(context.Background(), TaskKind("kind"+string(rune('a'+i%3))), nil)
		if seen[tb.ID()] {
			t.Fatalf("duplicate task id %d", tb.ID())
		}
		seen[tb.ID()] = true
		tb.finish(nil)
	}
}

// Observer.OnFrames batching: flush at frameBatchMaxRecords, flush again
// frameBatchMaxAge after a batch started, always flush at stream end, order
// preserved, nothing lost.
func TestFrameBatcherLimits(t *testing.T) {
	var batches [][]FrameRecord
	b := newFrameBatcher(&Observer{OnFrames: func(recs []FrameRecord) {
		batches = append(batches, recs)
	}})

	for i := 1; i <= 100; i++ {
		b.add(FrameRecord{Frame: i})
	}
	b.flush()

	var got []int
	for _, batch := range batches {
		if len(batch) > frameBatchMaxRecords {
			t.Errorf("batch of %d exceeds the cap %d", len(batch), frameBatchMaxRecords)
		}
		for _, r := range batch {
			got = append(got, r.Frame)
		}
	}
	if len(batches) < 4 {
		t.Errorf("expected several batches for 100 records, got %d", len(batches))
	}
	if len(got) != 100 {
		t.Fatalf("batcher lost records: %d of 100 delivered", len(got))
	}
	for i, f := range got {
		if f != i+1 {
			t.Fatalf("record order broken at position %d: frame %d", i, f)
		}
	}

	// Age-based flush: a trickle of records must arrive within the age window
	// instead of waiting for the record cap.
	batches = nil
	b2 := newFrameBatcher(&Observer{OnFrames: func(recs []FrameRecord) {
		batches = append(batches, recs)
	}})
	b2.add(FrameRecord{Frame: 1})
	if len(batches) != 0 {
		t.Errorf("one record must not flush immediately (cap is %d)", frameBatchMaxRecords)
	}
	time.Sleep(frameBatchMaxAge + 30*time.Millisecond)
	b2.add(FrameRecord{Frame: 2}) // the stale batch is over age: flush
	if len(batches) == 0 {
		t.Error("aged batch must flush on the next add")
	}
	b2.flush()
	total := 0
	for _, batch := range batches {
		total += len(batch)
	}
	if total != 2 {
		t.Errorf("trickle delivered %d records, want 2", total)
	}

	// No observer / no callback: add and flush are no-ops.
	quiet := newFrameBatcher(nil)
	quiet.add(FrameRecord{Frame: 1})
	quiet.flush()
	quiet2 := newFrameBatcher(&Observer{})
	quiet2.add(FrameRecord{Frame: 1})
	quiet2.flush()
}
