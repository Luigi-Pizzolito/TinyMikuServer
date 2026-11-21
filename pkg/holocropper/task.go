package holocropper

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

/* Task foundation shared by every async pass (HOLOCROPPER-API-PLAN.md
stories A1/B4, spinner contract §6.7).

A Task is a cancellable background pass that answers cheap non-blocking
queries from any goroutine: State(), Progress(), Cancel(). Typed tasks
(AnalysisTask, PreviewTask, RenderTask) embed taskBase and add their own
Wait(); the base handles the state machine, progress bookkeeping and the
Observer event feed.

Slot semantics (last-write-wins, requirement 12): each Analysis has an
analyze slot and a shared render slot. Installing a task in a slot
supersedes the slot's previous running task - the old task reaches the
superseded terminal state and its Wait() returns *SupersededError with the
new Task ID, so a UI relocates its spinner instead of handling "already
busy" errors. There are no busy errors.

IDs come from a package-wide monotonic counter: unique across every slot and
Analysis, so a UI can key spinners by ID alone. */

// progressEmitInterval caps throttled OnTask progress events at ~4 Hz.
// Terminal and phase-change events bypass the throttle.
const progressEmitInterval = 250 * time.Millisecond

var taskIDCounter atomic.Int64

func nextTaskID() int { return int(taskIDCounter.Add(1)) }

type taskBase struct {
	id   int
	kind TaskKind

	// ctx/cancel exist from creation: the runner works under tb.ctx, and
	// Cancel()/supersede() just cancel it.
	ctx    context.Context
	cancel context.CancelFunc

	mu           sync.Mutex
	state        TaskState
	phase        Phase
	done         int
	total        int
	start        time.Time
	end          time.Time
	runErr       error // failure surfaced by the runner (nil = success)
	errMsg       string
	cancelReq    bool
	superTo      int
	terminalOpen bool // true until finish() closes the terminal channel

	observer *Observer
	lastEmit time.Time
	warnings []string       // distinct ffmpeg diagnostics, in the order they first appeared
	warnSeen map[string]int // per-Task repeat counts behind those distinct entries
	terminal chan struct{}
}

// maxTaskWarnings bounds the distinct diagnostic list a Task keeps.
const maxTaskWarnings = 200

// newTaskBase creates a queued Task under a cancellable context derived
// from ctx. The runner must call begin() when it starts and finish(err)
// exactly once when it returns.
func newTaskBase(ctx context.Context, kind TaskKind, observer *Observer) *taskBase {
	ctx, cancel := context.WithCancel(ctx)
	tb := &taskBase{
		id:           nextTaskID(),
		kind:         kind,
		state:        StateQueued,
		ctx:          ctx,
		cancel:       cancel,
		observer:     observer,
		start:        time.Now(),
		terminalOpen: true,
		terminal:     make(chan struct{}),
	}
	tb.publish(tb.snapshot(), true)
	return tb
}

// ID reports the Task ID the UI keys its spinner to.
func (tb *taskBase) ID() int { return tb.id }

// Kind reports which pass this Task runs.
func (tb *taskBase) Kind() TaskKind { return tb.kind }

// State reports the current terminal-or-live state, never blocking.
func (tb *taskBase) State() TaskState {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	return tb.state
}

// Progress returns the current status snapshot (Done/Total are scoped to
// the current Phase), never blocking. Completed Tasks keep their final
// snapshot until the owning Analysis is dropped.
func (tb *taskBase) Progress() TaskInfo { return tb.snapshot() }

// Warnings returns a copy of the ffmpeg diagnostics this Task has recorded
// (Observer.OnWarning mirrors them live). It never blocks and works during
// a run, after completion, and after a cancel or supersede - so an app can
// surface diagnostics for a Task even when Wait() returned an error.
// Bounded: a chatty ffmpeg cannot grow it without limit.
func (tb *taskBase) Warnings() []string {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	if len(tb.warnings) == 0 {
		return nil
	}
	out := make([]string, len(tb.warnings))
	copy(out, tb.warnings)
	return out
}

// Cancel requests cancellation. The Task reaches the cancelled terminal
// state when its runner observes the cancellation; completed Tasks ignore
// it. Never blocks.
func (tb *taskBase) Cancel() {
	tb.mu.Lock()
	if !tb.terminalOpen {
		tb.mu.Unlock()
		return
	}
	tb.cancelReq = true
	tb.cancel()
	tb.mu.Unlock()
}

// waitTerminal blocks until the Task reaches a terminal state and returns
// the wait error contract: nil (completed), *SupersededError, ErrCancelled,
// or the runner's failure. Safe to call multiple times and from several
// goroutines.
func (tb *taskBase) waitTerminal() error {
	<-tb.terminal
	tb.mu.Lock()
	defer tb.mu.Unlock()
	switch tb.state {
	case StateCompleted:
		return nil
	case StateSuperseded:
		return &SupersededError{NewTaskID: tb.superTo}
	case StateCancelled:
		return ErrCancelled
	default: // failed
		if tb.runErr != nil {
			return tb.runErr
		}
		return errors.New(tb.errMsg)
	}
}

// --- runner-side plumbing -------------------------------------------------

// begin marks the Task running (call from the runner goroutine).
func (tb *taskBase) begin() {
	tb.mu.Lock()
	tb.state = StateRunning
	info := tb.snapshotLocked()
	tb.mu.Unlock()
	tb.publish(info, true)
}

// setPhase moves the Task to a new phase with its own totals (each pass
// carries per-phase totals; a bar consumer never has to reconcile phases).
func (tb *taskBase) setPhase(phase Phase, total int) {
	tb.mu.Lock()
	tb.phase = phase
	tb.total = total
	tb.done = 0
	info := tb.snapshotLocked()
	tb.mu.Unlock()
	tb.publish(info, true)
}

// setDone sets the current phase's progress counter; total stays as set.
func (tb *taskBase) setDone(done int) { tb.advanceTo(done) }

// advanceTo is the shared implementation of setDone.
func (tb *taskBase) advanceTo(done int) {
	tb.mu.Lock()
	if done <= tb.done {
		tb.mu.Unlock()
		return
	}
	tb.done = done
	info := tb.snapshotLocked()
	tb.mu.Unlock()
	tb.publish(info, false) // throttled
}

// warn surfaces an ffmpeg diagnostic through Observer.OnWarning and records it
// for Result.Warnings. A chatty ffmpeg repeats the same line per frame — a
// decode error is a per-frame event — so each DISTINCT message fires ONCE per
// Task (the plan U3 "once per pass" law) and its repeat count is available
// from WarningCounts(). Bounded, so a chatty ffmpeg cannot grow it without
// limit.
func (tb *taskBase) warn(msg string) {
	tb.mu.Lock()
	if tb.warnSeen == nil {
		tb.warnSeen = make(map[string]int)
	}
	if n, seen := tb.warnSeen[msg]; seen {
		tb.warnSeen[msg] = n + 1
		tb.mu.Unlock()
		return // already surfaced this pass
	}
	tb.warnSeen[msg] = 1
	tb.warnings = append(tb.warnings, msg)
	if len(tb.warnings) > maxTaskWarnings {
		// Keep the newest ones; a dropped message may surface again later.
		dropped := tb.warnings[0]
		tb.warnings = tb.warnings[1:]
		delete(tb.warnSeen, dropped)
	}
	tb.mu.Unlock()
	if tb.observer != nil && tb.observer.OnWarning != nil {
		tb.observer.OnWarning(msg)
	}
}

// WarningCounts reports how many times each distinct diagnostic fired during
// this Task, keyed by the same text Warnings() returns. A message that fired
// once maps to 1; the count is how loud ffmpeg actually was.
func (tb *taskBase) WarningCounts() map[string]int {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	if len(tb.warnSeen) == 0 {
		return nil
	}
	out := make(map[string]int, len(tb.warnSeen))
	for k, v := range tb.warnSeen {
		out[k] = v
	}
	return out
}

// finish closes out the Task exactly once, deriving the terminal state:
// superseded (replaced by a newer Task in the slot) > cancelled (Cancel was
// requested) > failed (runner error) > completed. runErr is surfaced to
// Wait(); a cancelled/superseded runner error (context noise from ffmpeg
// kills etc.) is swallowed by the state precedence above.
func (tb *taskBase) finish(runErr error) {
	tb.mu.Lock()
	if !tb.terminalOpen {
		tb.mu.Unlock()
		return
	}
	switch {
	case tb.superTo != 0:
		tb.state = StateSuperseded
	case tb.cancelReq, errors.Is(runErr, context.Canceled):
		// Cancel() was requested, or the caller's own context was cancelled
		// downstream (ffmpeg kill, detect loop exit): same terminal state.
		tb.state = StateCancelled
	case runErr != nil:
		tb.state = StateFailed
		tb.runErr = runErr
		tb.errMsg = runErr.Error()
	default:
		tb.state = StateCompleted
	}
	tb.end = time.Now()
	tb.terminalOpen = false
	close(tb.terminal)
	info := tb.snapshotLocked()
	tb.mu.Unlock()
	tb.publish(info, true)
	tb.cancel() // release the context even on success
}

// supersede marks the Task replaced by newID and requests cancellation.
// A terminal Task cannot be superseded (the slot check handles that).
func (tb *taskBase) supersede(newID int) {
	tb.mu.Lock()
	if !tb.terminalOpen {
		tb.mu.Unlock()
		return
	}
	tb.superTo = newID
	tb.cancel()
	tb.mu.Unlock()
}

// supersededBy reports the Task that replaced this one (0 if none). Used by
// the pass runners to keep scratch cleanup consistent after a supersede.
func (tb *taskBase) supersededBy() int {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	return tb.superTo
}

// --- snapshot + event publishing ------------------------------------------

func (tb *taskBase) snapshot() TaskInfo {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	return tb.snapshotLocked()
}

func (tb *taskBase) snapshotLocked() TaskInfo {
	elapsed := time.Since(tb.start)
	if !tb.terminalOpen && !tb.end.IsZero() {
		elapsed = tb.end.Sub(tb.start)
	}
	var errMsg string
	if tb.state == StateFailed && tb.runErr != nil {
		errMsg = tb.runErr.Error()
	}
	return TaskInfo{
		ID:             tb.id,
		Kind:           tb.kind,
		State:          tb.state,
		Phase:          tb.phase,
		Done:           tb.done,
		Total:          tb.total,
		ElapsedSeconds: elapsed.Seconds(),
		Err:            errMsg,
		SupersededTo:   tb.superTo,
	}
}

// publish feeds Observer.OnTask. State/phase/terminal events are immediate;
// plain progress advances are throttled to progressEmitInterval. The
// callback runs on the calling pipeline goroutine (it must not block -
// contract is on the app side), and outside the task mutex so a slow
// observer can never stall Progress()/State() queries.
func (tb *taskBase) publish(info TaskInfo, force bool) {
	if tb.observer == nil || tb.observer.OnTask == nil {
		return
	}
	tb.mu.Lock()
	now := time.Now()
	if !force && !tb.lastEmit.IsZero() && now.Sub(tb.lastEmit) < progressEmitInterval {
		tb.mu.Unlock()
		return
	}
	tb.lastEmit = now
	tb.mu.Unlock()
	tb.observer.OnTask(info)
}

// --- slots ----------------------------------------------------------------

// taskSlot serializes one family of passes (analyze slot, render slot).
// Last call wins: installing a Task supersedes the slot's previous running
// Task instead of reporting a busy error.
type taskSlot struct {
	mu  sync.Mutex
	cur *taskBase
}

// install puts t in the slot and supersedes the previous running Task (if
// any). Returns the superseded Task - nil when the slot was empty or already
// held this same Task - for caller-side bookkeeping (usually nil handling:
// the superseded task's Wait() carries the SupersededError).
func (s *taskSlot) install(t *taskBase) *taskBase {
	s.mu.Lock()
	prev := s.cur
	s.cur = t
	s.mu.Unlock()
	if prev != nil && prev != t {
		prev.supersede(t.id)
		return prev
	}
	return nil
}

// current reports the slot's latest Task (nil if unused). A terminal Task
// stays reported so Status() can show the final state until the slot is
// replaced.
func (s *taskSlot) current() *taskBase {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

// live reports the slot's non-terminal Task, if any (what Status() shows
// as the busy indicator).
func (s *taskSlot) live() *taskBase {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != nil && s.cur.State() != StateCompleted && s.cur.State() != StateFailed &&
		s.cur.State() != StateCancelled && s.cur.State() != StateSuperseded {
		return s.cur
	}
	return nil
}

// --- frame batching ---------------------------------------------------------

// frameBatcher groups FrameRecords for Observer.OnFrames: it flushes at
// frameBatchMaxRecords or frameBatchMaxAge after the batch started, and
// always flushes at stream end. Batches preserve stream order, and every
// batcher method is O(1) apart from the callback.
const (
	frameBatchMaxRecords = 24
	frameBatchMaxAge     = 250 * time.Millisecond
)

type frameBatcher struct {
	observer *Observer
	buf      []FrameRecord
	started  time.Time // when the current batch began (first record added)
}

func newFrameBatcher(o *Observer) *frameBatcher {
	return &frameBatcher{observer: o}
}

func (b *frameBatcher) add(r FrameRecord) {
	if b.observer == nil || b.observer.OnFrames == nil {
		return
	}
	if len(b.buf) == 0 {
		b.started = time.Now()
	}
	b.buf = append(b.buf, r)
	if len(b.buf) >= frameBatchMaxRecords || time.Since(b.started) >= frameBatchMaxAge {
		b.flush()
	}
}

func (b *frameBatcher) flush() {
	if b.observer == nil || b.observer.OnFrames == nil || len(b.buf) == 0 {
		b.buf = b.buf[:0]
		return
	}
	batch := b.buf
	b.buf = make([]FrameRecord, 0, frameBatchMaxRecords)
	b.observer.OnFrames(batch)
}
