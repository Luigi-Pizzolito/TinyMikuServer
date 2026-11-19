package holocropper

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"image"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	vidio "github.com/AlexEidt/Vidio"
)

/* Analysis pass tests (stories A4/A5 + the A3 equivalence acceptance).

These need a real ffmpeg, so they skip when it is not on PATH. The fixture is
a deterministic synthetic clip - a white box walking right across a black
field, with an audio track - which keeps detection unambiguous (no dark-scene
confounds) while exercising the full prepare -> detect -> cache -> replay
chain. The parity test compares the embeddable replay against the fused CLI
pipeline's debug CSV, which is the A3 acceptance criterion. */

const (
	fixtureW, fixtureH = 960, 540
	fixtureFPS         = 24
	fixtureSeconds     = 4
)

func requireFFmpeg(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("analysis pass tests need ffmpeg on PATH")
	}
	return path
}

// buildFixture renders the synthetic clip: lavfi black field + a white
// 120x60 box overlaid on a linear rightward path, plus a sine audio track.
func buildFixture(t *testing.T, ffmpegPath, dir string) string {
	t.Helper()
	out := filepath.Join(dir, "fixture.mp4")
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("color=c=black:s=%dx%d:d=%d:r=%d", fixtureW, fixtureH, fixtureSeconds, fixtureFPS),
		"-f", "lavfi", "-i", fmt.Sprintf("color=c=white:s=120x60:d=%d:r=%d", fixtureSeconds, fixtureFPS),
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:duration=%d", fixtureSeconds),
		"-filter_complex", "[0:v][1:v]overlay=x='60+90*t':y=240,format=yuv420p[v]",
		"-map", "[v]", "-map", "2:a",
		"-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", "-shortest",
		out,
	}
	cmd := exec.Command(ffmpegPath, args...)
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture build failed: %v\n%s", err, combined)
	}
	return out
}

func probeFrames(t *testing.T, path string) (w, h, frames int, fps float64) {
	t.Helper()
	v, err := vidio.NewVideo(path)
	if err != nil {
		t.Fatalf("probe %s: %v", path, err)
	}
	defer v.Close()
	return v.Width(), v.Height(), v.Frames(), v.FPS()
}

// scratchLeftovers lists this cache dir's per-Task scratch files: a finished,
// cancelled or superseded pass must leave none (unless KeepWorkDir was set).
func scratchLeftovers(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*.tmp-*"))
	if err != nil {
		t.Fatalf("scratch glob: %v", err)
	}
	return matches
}

type eventLog struct {
	mu      sync.Mutex
	tasks   []TaskInfo
	frames  []FrameRecord
	warns   []string
	batches [][]FrameRecord
}

func (e *eventLog) observer() *Observer {
	return &Observer{
		OnTask: func(info TaskInfo) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.tasks = append(e.tasks, info)
		},
		OnFrames: func(recs []FrameRecord) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.batches = append(e.batches, recs)
			e.frames = append(e.frames, recs...)
		},
		OnWarning: func(m string) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.warns = append(e.warns, m)
		},
	}
}

// waitForDetect blocks until the pass is visibly inside the detect phase
// (Done > 0), the Task is terminal (test failure: the pass beat us to it),
// or the deadline passes.
func waitForDetect(t *testing.T, task *AnalysisTask) TaskInfo {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		info := task.Progress()
		if info.Phase == PhaseDetect && info.Done > 0 {
			return info
		}
		if info.State == StateCompleted || info.State == StateFailed ||
			info.State == StateCancelled || info.State == StateSuperseded {
			t.Fatalf("pass reached %s before the test could act on it: %+v", info.State, info)
		}
		if time.Now().After(deadline) {
			t.Fatalf("detect phase never started: %+v", info)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestAnalyzeCacheRoundTrip is the story A4 acceptance run: prepare + detect
// produce the cached analysis video, stream and meta; the live pass exposes
// a growing partial stream, status and frame events; a second Analyze of the
// same input + analysis height hits the cache with the canonical Analysis;
// a different height or an evict is a miss.
func TestAnalyzeCacheRoundTrip(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	inputDir, tempDir := t.TempDir(), t.TempDir()
	fixture := buildFixture(t, ffmpeg, inputDir)
	srcW, srcH, srcFrames, _ := probeFrames(t, fixture)

	log := &eventLog{}
	req := AnalyzeRequest{Input: fixture, TempDir: tempDir, FFmpegPath: ffmpeg, Observer: log.observer()}

	task, err := Analyze(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	// Live view while the detect pass runs (requirement 8).
	live := waitForDetect(t, task)
	if live.Total != srcFrames {
		t.Errorf("detect total = %d, source frames = %d", live.Total, srcFrames)
	}
	stream := task.PartialStream()
	detected := stream.Len()
	if detected == 0 {
		t.Fatal("partial stream empty while detect is running")
	}
	if info := task.Progress(); info.State != StateRunning {
		t.Errorf("Status while running: state = %s", info.State)
	}
	// Reading frames concurrently with the producing append must be safe and
	// must never see a half-written sample.
	for i := 0; i < detected; i++ {
		f := stream.Frame(i)
		if f.Subjects == 0 && !f.BBox.toImage().Empty() {
			t.Fatalf("frame %d: bbox without subjects", i+1)
		}
	}
	if p, err := NewPlan(stream, Settings{Width: 240, Height: 240, FPS: fixtureFPS}); err != nil {
		t.Fatalf("plan over the partial stream: %v", err)
	} else if len(p.Records()) == 0 {
		t.Error("partial Plan produced no records")
	}

	a1, err := task.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if task.State() != StateCompleted {
		t.Errorf("task state = %s, want completed", task.State())
	}
	if a1 == nil {
		t.Fatal("completed Analyze returned no Analysis")
	}

	// Cache artifacts under the caller's TempDir, frame-aligned.
	dir := a1.Dir()
	if filepath.Dir(dir) != mustAbs(tempDir) {
		t.Errorf("cache dir %s not under TempDir %s", dir, tempDir)
	}
	if !strings.HasPrefix(filepath.Base(dir), "holocropper-analysis-") {
		t.Errorf("cache dir name %s", filepath.Base(dir))
	}
	vw, vh, vframes, vfps := probeFrames(t, a1.AnalysisVideoPath())
	if vw == 0 || vh != defaultAnalysisHeight {
		t.Errorf("analysis video %dx%d, want height %d", vw, vh, defaultAnalysisHeight)
	}
	if vframes != srcFrames {
		t.Errorf("frame alignment broken: analysis video has %d frames, source has %d", vframes, srcFrames)
	}
	if vfps <= 0 {
		t.Errorf("analysis video fps = %v", vfps)
	}
	for _, name := range []string{analysisStreamName, analysisMetaName} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("cache artifact %s: %v", name, err)
		}
	}
	if left := scratchLeftovers(t, dir); len(left) != 0 {
		t.Errorf("scratch left behind: %v", left)
	}

	s := a1.Stream()
	if !s.Complete() {
		t.Error("completed pass must report a complete stream")
	}
	if s.Len() != srcFrames {
		t.Errorf("stream length %d, source frames %d", s.Len(), srcFrames)
	}
	if s.AnalysisWidth() != vw || s.AnalysisHeight() != vh {
		t.Errorf("stream analysis geometry %dx%d, video %dx%d", s.AnalysisWidth(), s.AnalysisHeight(), vw, vh)
	}
	if s.SourceWidth() != srcW || s.SourceHeight() != srcH {
		t.Errorf("stream source geometry %dx%d, fixture %dx%d", s.SourceWidth(), s.SourceHeight(), srcW, srcH)
	}
	if s.FPS() <= 0 {
		t.Errorf("stream fps = %v", s.FPS())
	}

	// Detection sanity: the box is visible in every frame and walks right.
	first, last := s.Frame(0), s.Frame(s.Len()-1)
	for i := 0; i < s.Len(); i++ {
		f := s.Frame(i)
		if f.Subjects < 1 || f.BBox.W <= 0 {
			t.Fatalf("frame %d: no subject detected (subjects=%d bbox=%+v)", i+1, f.Subjects, f.BBox)
		}
		if f.BBox.W > 220 || f.BBox.H > 160 || f.BBox.W < 60 || f.BBox.H < 30 {
			t.Errorf("frame %d: implausible subject box %+v (fixture box is 120x60 source px)", i+1, f.BBox)
		}
	}
	if dx := (last.BBox.X + last.BBox.W/2) - (first.BBox.X + first.BBox.W/2); dx < 50 {
		t.Errorf("walking box moved only %.1f px: first %+v last %+v", dx, first, last)
	}

	// Frame events: every detected frame delivered, batches capped, order kept.
	log.mu.Lock()
	gotFrames := len(log.frames)
	maxBatch := 0
	for _, b := range log.batches {
		if len(b) > maxBatch {
			maxBatch = len(b)
		}
	}
	for i, r := range log.frames {
		if r.Frame != i+1 {
			t.Fatalf("frame event order broken at %d: frame %d", i, r.Frame)
		}
	}
	log.mu.Unlock()
	if gotFrames != srcFrames {
		t.Errorf("OnFrames delivered %d records, want %d", gotFrames, srcFrames)
	}
	if maxBatch > frameBatchMaxRecords {
		t.Errorf("frame batch of %d exceeds cap %d", maxBatch, frameBatchMaxRecords)
	}

	// Status reconciles: idle analyze slot, complete cached stream.
	status := a1.Status()
	if !status.Cached {
		t.Error("Status().Cached = false after a completed pass")
	}
	if status.Analysis != nil {
		t.Errorf("Status().Analysis = %+v, want nil when idle", status.Analysis)
	}
	if status.Render != nil {
		t.Errorf("Status().Render = %+v, want nil when idle", status.Render)
	}

	// Cache hit: same canonical Analysis, no prepare/detect ever runs.
	start := time.Now()
	task2, err := Analyze(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := task2.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("cached Analyze took %v - it must not run a pass", elapsed)
	}
	if a2 != a1 {
		t.Error("cached Analyze must return the canonical Analysis, not a copy")
	}
	if task2.State() != StateCompleted {
		t.Errorf("cached task state = %s", task2.State())
	}
	if phase := task2.Progress().Phase; phase != "" {
		t.Errorf("cached task ran phase %q - a cache hit must not touch ffmpeg or detect", phase)
	}
	if task2.PartialStream().Len() != srcFrames {
		t.Errorf("cached task partial stream length = %d, want %d", task2.PartialStream().Len(), srcFrames)
	}

	// CachedAnalysis handles the same cache entry, and a different analysis
	// height or a modified input is a miss.
	cached, err := CachedAnalysis(fixture, tempDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if cached != a1 {
		t.Error("CachedAnalysis must return the canonical Analysis")
	}
	if _, err := CachedAnalysis(fixture, tempDir, 720); !errors.Is(err, ErrAnalysisCacheMiss) {
		t.Errorf("different analysis height: want ErrAnalysisCacheMiss, got %v", err)
	}
	touched := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(fixture, touched, touched); err != nil {
		t.Fatal(err)
	}
	if _, err := CachedAnalysis(fixture, tempDir, 0); !errors.Is(err, ErrAnalysisCacheMiss) {
		t.Errorf("modified input mtime: want ErrAnalysisCacheMiss, got %v", err)
	}

	// Evict drops the dir and the registry entry.
	if err := a1.Evict(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("Evict left %s in place (%v)", dir, err)
	}
	if _, err := CachedAnalysis(fixture, tempDir, 0); !errors.Is(err, ErrAnalysisCacheMiss) {
		t.Errorf("after Evict: want ErrAnalysisCacheMiss, got %v", err)
	}
}

// TestAnalyzeCancelMidDetect: cancelling during detect unwinds cleanly -
// ErrCancelled, no persistence, no scratch, and the half-finished cache dir
// is still a consistent miss.
func TestAnalyzeCancelMidDetect(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	inputDir, tempDir := t.TempDir(), t.TempDir()
	fixture := buildFixture(t, ffmpeg, inputDir)
	_, _, srcFrames, _ := probeFrames(t, fixture)

	// A tall analysis frame makes detect the slow phase, so the cancellation
	// reliably lands inside the decode loop.
	req := AnalyzeRequest{Input: fixture, TempDir: tempDir, FFmpegPath: ffmpeg, AnalysisHeight: 720}
	task, err := Analyze(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	live := waitForDetect(t, task)
	task.Cancel()

	a, err := task.Wait()
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("want ErrCancelled, got %v", err)
	}
	if a != nil {
		t.Error("cancelled Analyze must not return an Analysis")
	}
	if task.State() != StateCancelled {
		t.Errorf("state = %s, want cancelled", task.State())
	}
	if got := task.PartialStream().Len(); got == 0 || got >= srcFrames {
		t.Errorf("partial stream kept %d frames of %d: a cancelled pass should hold its progress", got, srcFrames)
	}
	if task.PartialStream().Complete() {
		t.Error("cancelled stream must not report Complete")
	}
	if info := task.Progress(); info.Phase != PhaseDetect || info.Done == 0 {
		t.Errorf("cancelled task snapshot lost its phase progress: %+v (was %+v)", info, live)
	}

	dir := cachedDirFor(t, req)
	// Prepare finished (we saw detect running), so the analysis video is the
	// only artifact allowed to survive; the cache is still unusable without
	// stream + meta.
	if _, err := os.Stat(filepath.Join(dir, analysisVideoName)); err != nil {
		t.Errorf("analysis video after a mid-detect cancel: %v", err)
	}
	for _, name := range []string{analysisStreamName, analysisMetaName} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("cancelled pass persisted %s: %v", name, err)
		}
	}
	if left := scratchLeftovers(t, dir); len(left) != 0 {
		t.Errorf("cancel left scratch files: %v", left)
	}
	if _, err := CachedAnalysis(fixture, tempDir, 720); !errors.Is(err, ErrAnalysisCacheMiss) {
		t.Errorf("half-finished cache: want ErrAnalysisCacheMiss, got %v", err)
	}
}

// TestAnalyzeSupersedeLastCallWins: re-running Analyze for the same input and
// analysis height replaces the running pass - the old Task reports the new
// Task ID instead of a busy error, and the replacement produces the cache.
func TestAnalyzeSupersedeLastCallWins(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	inputDir, tempDir := t.TempDir(), t.TempDir()
	fixture := buildFixture(t, ffmpeg, inputDir)
	req := AnalyzeRequest{Input: fixture, TempDir: tempDir, FFmpegPath: ffmpeg}

	first, err := Analyze(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Analyze(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID() == first.ID() {
		t.Fatal("the replacement pass must be a new Task")
	}

	a, err := first.Wait()
	var se *SupersededError
	if !errors.As(err, &se) {
		t.Fatalf("want *SupersededError, got %v", err)
	}
	if a != nil {
		t.Error("superseded Analyze must not return an Analysis")
	}
	if se.NewTaskID != second.ID() {
		t.Errorf("SupersededError.NewTaskID = %d, want %d", se.NewTaskID, second.ID())
	}
	if first.State() != StateSuperseded {
		t.Errorf("superseded task state = %s", first.State())
	}

	replacement, err := second.Wait()
	if err != nil {
		t.Fatalf("replacement pass failed: %v", err)
	}
	if second.State() != StateCompleted {
		t.Errorf("replacement state = %s", second.State())
	}
	if replacement == nil || !replacement.Stream().Complete() {
		t.Fatal("replacement pass must produce a complete cached stream")
	}

	dir := replacement.Dir()
	if left := scratchLeftovers(t, dir); len(left) != 0 {
		t.Errorf("superseded pass left scratch files behind: %v", left)
	}
	if _, err := os.Stat(filepath.Join(dir, analysisMetaName)); err != nil {
		t.Errorf("replacement cache: %v", err)
	}
	if _, err := CachedAnalysis(fixture, tempDir, 0); err != nil {
		t.Errorf("after supersede, CachedAnalysis must hit the replacement: %v", err)
	}
}

// TestAnalyzeInvalidRequests: validation and unreadable inputs surface before
// any Task exists.
func TestAnalyzeInvalidRequests(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	inputDir, tempDir := t.TempDir(), t.TempDir()

	cases := []struct {
		name string
		req  AnalyzeRequest
	}{
		{"no input", AnalyzeRequest{TempDir: tempDir}},
		{"no temp dir", AnalyzeRequest{Input: filepath.Join(inputDir, "x.mp4")}},
		{"tiny analysis height", AnalyzeRequest{Input: filepath.Join(inputDir, "x.mp4"), TempDir: tempDir, AnalysisHeight: 8}},
	}
	for _, c := range cases {
		if _, err := Analyze(context.Background(), c.req); err == nil {
			t.Errorf("%s: want a validation error", c.name)
		} else if !strings.Contains(err.Error(), "invalid") && !strings.Contains(err.Error(), "required") {
			t.Errorf("%s: unexpected error %v", c.name, err)
		}
	}

	// A path that does not exist resolves as a failure, not a panic.
	missing := AnalyzeRequest{Input: filepath.Join(inputDir, "nope.mp4"), TempDir: tempDir, FFmpegPath: ffmpeg}
	if _, err := Analyze(context.Background(), missing); err == nil {
		t.Error("missing input: want an error")
	}
	// A directory is not an input.
	if _, err := Analyze(context.Background(), AnalyzeRequest{Input: inputDir, TempDir: tempDir}); err == nil {
		t.Error("directory input: want an error")
	}
	// Cancelled before the pass even starts: terminal cancelled, no cache.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	task, err := Analyze(ctx, AnalyzeRequest{Input: buildFixture(t, ffmpeg, inputDir), TempDir: tempDir, FFmpegPath: ffmpeg})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := task.Wait(); !errors.Is(err, ErrCancelled) {
		t.Errorf("pre-cancelled context: want ErrCancelled, got %v", err)
	}
}

// cachedDirFor recomputes the cache dir a request maps to (the same identity
// the pass uses) so tests can inspect it.
func cachedDirFor(t *testing.T, req AnalyzeRequest) string {
	t.Helper()
	ra, err := resolveAnalyze(req)
	if err != nil {
		t.Fatalf("resolve cache dir: %v", err)
	}
	return ra.dir
}

/* A3 equivalence acceptance: the embeddable replay over the recorded bbox
stream must reproduce the fused CLI pipeline's framing. The debug CSV is the
ground truth (HIGH-ASPECT-RATIO-FIXES.md Phase 0), so the comparison is
CSV-derived framing vs Plan records on the same fixture. */

type csvFrameRow struct {
	frame, subjects          int
	bboxX0, bboxY0           int
	bboxW, bboxH             int
	emaX0, emaY0, emaW, emaH int
	cropCx, cropCy           float64
	cropW, cropH             float64
	targetH, walkTgt         float64
}

func readDebugCSV(t *testing.T, path string) []csvFrameRow {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open debug csv: %v", err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("read debug csv: %v", err)
	}
	if len(rows) < 2 {
		t.Fatalf("debug csv has no data rows: %d lines", len(rows))
	}
	out := make([]csvFrameRow, 0, len(rows)-1)
	for _, r := range rows[1:] {
		if len(r) != 16 {
			t.Fatalf("csv row has %d columns: %v", len(r), r)
		}
		i := func(c int) int {
			v, err := strconv.Atoi(r[c])
			if err != nil {
				t.Fatalf("csv column %d = %q: %v", c, r[c], err)
			}
			return v
		}
		f := func(c int) float64 {
			v, err := strconv.ParseFloat(r[c], 64)
			if err != nil {
				t.Fatalf("csv column %d = %q: %v", c, r[c], err)
			}
			return v
		}
		out = append(out, csvFrameRow{
			frame: i(0), subjects: i(1),
			bboxX0: i(2), bboxY0: i(3), bboxW: i(4), bboxH: i(5),
			emaX0: i(6), emaY0: i(7), emaW: i(8), emaH: i(9),
			cropCx: f(10), cropCy: f(11), cropW: f(12), cropH: f(13),
			targetH: f(14), walkTgt: f(15),
		})
	}
	return out
}

func TestReplayParityVsDebugCSV(t *testing.T) {
	ffmpeg := requireFFmpeg(t)

	// Keep the fused CLI path's own scratch inside the test's temp tree.
	t.Setenv("TMPDIR", t.TempDir())

	inputDir, outDir := t.TempDir(), t.TempDir()
	fixture := buildFixture(t, ffmpeg, inputDir)
	csvPath := filepath.Join(t.TempDir(), "boxes.csv")
	t.Setenv("HOLOCROPPER_DEBUG_BOXES", csvPath)

	const (
		outH, outW = 240, 240
		fps        = fixtureFPS
	)
	hc := &HoloCropper{}
	if err := hc.Crop(outH, outW, fps, fixture, outDir); err != nil {
		t.Fatalf("CLI crop on the fixture: %v", err)
	}
	rows := readDebugCSV(t, csvPath)

	// The CLI analysis frame for this fixture: height 480, width follows the
	// source aspect (even). Same geometry the embeddable pass produces.
	aw := defaultAnalysisHeight * fixtureW / fixtureH
	if aw%2 != 0 {
		aw++
	}
	samples := make([]streamSample, len(rows))
	for i, r := range rows {
		samples[i] = newStreamSample(image.Rect(r.bboxX0, r.bboxY0, r.bboxX0+r.bboxW, r.bboxY0+r.bboxH), r.subjects)
	}
	stream := newTestStream(samples, aw, defaultAnalysisHeight, fixtureW, fixtureH)

	p, err := NewPlan(stream, Settings{Width: outW, Height: outH, FPS: fps})
	if err != nil {
		t.Fatal(err)
	}
	recs := p.Records()
	if len(recs) != len(rows) {
		t.Fatalf("replay length %d, CSV rows %d", len(recs), len(rows))
	}
	if r := p.Resolved(); r.AnalysisW != aw || r.AnalysisH != defaultAnalysisHeight ||
		r.OutputW != outW || r.OutputH != outH || r.SourceW != fixtureW || r.SourceH != fixtureH {
		t.Errorf("resolved geometry mismatch: %+v", r)
	}

	// CSV floats are written with 2 decimals, so the parity tolerance is the
	// rounding band, not drift: 0.01 px plus a hair.
	const tol = 0.011
	var maxCropDrift, maxTargetDrift float64
	worstCrop, worstTarget := 0, 0
	for i, r := range rows {
		rec := recs[i]
		if rec.Frame != r.frame {
			t.Fatalf("record %d: Frame = %d, CSV frame %d", i, rec.Frame, r.frame)
		}
		if rec.Subjects != r.subjects {
			t.Errorf("frame %d: subjects %d vs CSV %d", r.frame, rec.Subjects, r.subjects)
		}
		// Inputs and the integer tracking box must match exactly.
		if rec.BBox.X != float64(r.bboxX0) || rec.BBox.Y != float64(r.bboxY0) ||
			rec.BBox.W != float64(r.bboxW) || rec.BBox.H != float64(r.bboxH) {
			t.Fatalf("frame %d: bbox %+v vs CSV {%d %d %d %d}", r.frame, rec.BBox, r.bboxX0, r.bboxY0, r.bboxW, r.bboxH)
		}
		if rec.EMA.X != float64(r.emaX0) || rec.EMA.Y != float64(r.emaY0) ||
			rec.EMA.W != float64(r.emaW) || rec.EMA.H != float64(r.emaH) {
			t.Fatalf("frame %d: EMA %+v vs CSV {%d %d %d %d} - tracking replay diverged",
				r.frame, rec.EMA, r.emaX0, r.emaY0, r.emaW, r.emaH)
		}

		cropCx, cropCy := rec.Crop.X+rec.Crop.W/2, rec.Crop.Y+rec.Crop.H/2
		for _, d := range []struct {
			name string
			got  float64
			want float64
		}{
			{"crop_cx", cropCx, r.cropCx},
			{"crop_cy", cropCy, r.cropCy},
			{"crop_w", rec.Crop.W, r.cropW},
			{"crop_h", rec.Crop.H, r.cropH},
		} {
			drift := d.got - d.want
			if absF(drift) > maxCropDrift {
				maxCropDrift, worstCrop = absF(drift), r.frame
			}
			if absF(drift) > tol {
				t.Fatalf("frame %d: %s = %.4f, CSV %.4f (drift %.4f > %.3f)",
					r.frame, d.name, d.got, d.want, drift, tol)
			}
		}
		targetDrift := rec.TargetH - r.targetH
		if absF(targetDrift) > maxTargetDrift {
			maxTargetDrift, worstTarget = absF(targetDrift), r.frame
		}
		if absF(targetDrift) > tol {
			t.Fatalf("frame %d: target_h = %.4f, CSV %.4f (drift %.4f)", r.frame, rec.TargetH, r.targetH, targetDrift)
		}
		if absF(rec.WalkTgt-r.walkTgt) > tol {
			t.Fatalf("frame %d: walk_tgt = %.4f, CSV %.4f", r.frame, rec.WalkTgt, r.walkTgt)
		}
		if r.walkTgt == 0 && rec.WalkActive {
			t.Errorf("frame %d: replay says an active leash, CSV says none", r.frame)
		}
	}
	t.Logf("parity over %d frames: worst crop drift %.4f px (frame %d), worst target drift %.4f px (frame %d)",
		len(rows), maxCropDrift, worstCrop, maxTargetDrift, worstTarget)

	// The crop must sit fully inside the analysis frame for a square output.
	for _, rec := range recs {
		if rec.Crop.X < -0.01 || rec.Crop.Y < -0.01 ||
			rec.Crop.X+rec.Crop.W > float64(aw)+0.01 || rec.Crop.Y+rec.Crop.H > float64(defaultAnalysisHeight)+0.01 {
			t.Fatalf("frame %d: replayed crop escapes the analysis frame: %+v", rec.Frame, rec.Crop)
		}
	}

	// The HOLOCROPPER_DEBUG_BOXES shim on the replay path (story C3): the
	// NewPlan above re-emitted the CSV (the env is still set), so the file
	// now holds the replay's own lines. They must match the fused path's
	// ground truth exactly the same way - this is the comparison C3 will
	// run against golden CSVs once the fused path is gone.
	shim := readDebugCSV(t, csvPath)
	if len(shim) != len(rows) {
		t.Fatalf("shim CSV has %d rows, ground truth %d", len(shim), len(rows))
	}
	for i, r := range rows {
		s := shim[i]
		if s.frame != r.frame || s.subjects != r.subjects ||
			s.bboxX0 != r.bboxX0 || s.bboxY0 != r.bboxY0 || s.bboxW != r.bboxW || s.bboxH != r.bboxH ||
			s.emaX0 != r.emaX0 || s.emaY0 != r.emaY0 || s.emaW != r.emaW || s.emaH != r.emaH {
			t.Fatalf("shim CSV row %d disagrees with the fused ground truth: %+v vs %+v", i, s, r)
		}
		for _, pr := range [][2]float64{
			{s.cropCx, r.cropCx}, {s.cropCy, r.cropCy}, {s.cropW, r.cropW}, {s.cropH, r.cropH},
			{s.targetH, r.targetH}, {s.walkTgt, r.walkTgt},
		} {
			if absF(pr[0]-pr[1]) > tol {
				t.Fatalf("shim CSV row %d: %.4f vs ground truth %.4f (drift > %.3f)", i, pr[0], pr[1], tol)
			}
		}
	}
}

func absF(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
