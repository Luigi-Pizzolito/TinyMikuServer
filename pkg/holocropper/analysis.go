package holocropper

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"sync"
	"time"

	vidio "github.com/AlexEidt/Vidio"
)

/* Analysis pass (HOLOCROPPER-API-PLAN.md story A4): prepare + detect, cached
under the caller's TempDir, live-queryable while running.

	<TempDir>/holocropper-analysis-<key12>/
		analysis.mp4    the frame-aligned (passthrough, §4/S13) re-encode the
		                detect pass and every render execute against
		stream.bin.gz   the DetectionStream: per frame 4x int16 master bbox +
		                uint16 subject count (~11 KB per 1000 frames)
		meta.json       cache identity (input path/mtime/size, analysis height,
		                geometry, frame alignment)

 The cache key is sha256(input path | mtime | size | analysis height | "pt1")
so a re-Analyze of an unchanged input hits the cache and a modified input
misses. Analyze is last-call-wins per cache key: a new Analyze supersedes the
running one (Task slot semantics), and while a pass runs PartialStream()
exposes the frames detected so far for live planning.

Mode law (plan §2/U1): AnalyzeRequest.Mode selects the pass - crop (default)
runs prepare+detect; fit runs prepare + a geometry probe and NO detection,
streaming one subject-less sample per analysis frame instead of measured
boxes. The fit transform never reads the detection, so fit plan replay,
previews and every render product run against the synthetic stream unchanged.
The cache is resumable across modes: meta.json records detected:true|false,
and a crop Analyze over a fit cache skips prepare and runs only the detect
phase over the already-cached analysis video. Switching the other way needs
no work at all: any complete cache answers a fit Analyze immediately. */

// ---------------------------------------------------------------------------
// Detection stream

// streamSample is the packed per-frame detection record (10 bytes): master
// subject bbox corners in analysis px (zero when lost) + subject count.
type streamSample struct {
	x0, y0, x1, y1 int16
	subjects       uint16
}

func newStreamSample(bbox image.Rectangle, subjects int) streamSample {
	if bbox.Empty() {
		return streamSample{}
	}
	s := streamSample{
		x0:       int16(bbox.Min.X),
		y0:       int16(bbox.Min.Y),
		x1:       int16(bbox.Max.X),
		y1:       int16(bbox.Max.Y),
		subjects: uint16(subjects),
	}
	return s
}

func (s streamSample) bbox() image.Rectangle {
	if s.x1 <= s.x0 || s.y1 <= s.y0 {
		return image.ZR
	}
	return image.Rect(int(s.x0), int(s.y0), int(s.x1), int(s.y1))
}

// streamData is the growable backing store; a running detect pass appends
// while PartialStream consumers read concurrently under the RWMutex.
type streamData struct {
	mu      sync.RWMutex
	samples []streamSample

	analysisW, analysisH int
	srcW, srcH           int
	fps                  float64 // analysis video frame rate (source timing preserved)
	complete             bool

	// videoPath is the canonical cached analysis video this stream was
	// detected from ("" for synthetic streams). Set once the prepare pass
	// renames the scratch encode into place; Snapshot/ContactSheet read it.
	videoPath string
}

func newStreamData() *streamData { return &streamData{} }

func (d *streamData) setGeometry(analysisW, analysisH, srcW, srcH int, fps float64) {
	d.mu.Lock()
	d.analysisW, d.analysisH, d.srcW, d.srcH, d.fps = analysisW, analysisH, srcW, srcH, fps
	d.mu.Unlock()
}

func (d *streamData) append(s streamSample) {
	d.mu.Lock()
	d.samples = append(d.samples, s)
	d.mu.Unlock()
}

func (d *streamData) markComplete() {
	d.mu.Lock()
	d.complete = true
	d.mu.Unlock()
}

func (d *streamData) setVideoPath(p string) {
	d.mu.Lock()
	d.videoPath = p
	d.mu.Unlock()
}

func (d *streamData) len() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.samples)
}

// snapshot returns a consistent copy of everything detected so far: the
// replay engine runs against a fixed slice while the stream may still grow.
func (d *streamData) snapshot() (samples []streamSample, analysisW, analysisH, srcW, srcH int, fps float64, complete bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	samples = make([]streamSample, len(d.samples))
	copy(samples, d.samples)
	return samples, d.analysisW, d.analysisH, d.srcW, d.srcH, d.fps, d.complete
}

// DetectionFrame is one stream frame: the master subject box covering ALL
// detected subjects (empty when the subject was lost) and the subject count.
type DetectionFrame struct {
	BBox     Rect `json:"bbox"` // analysis px
	Subjects int  `json:"subjects"`
}

// DetectionStream is the immutable-by-contract view of a detect pass's
// per-frame output. Safe for any number of concurrent readers, including
// while the producing Analyze task is still running (a partial stream
// grows; frames beyond Len() read as empty).
type DetectionStream struct {
	data *streamData
}

// Len reports how many frames have been detected so far.
func (s DetectionStream) Len() int {
	if s.data == nil {
		return 0
	}
	return s.data.len()
}

// Frame returns stream frame i (0-based).
func (s DetectionStream) Frame(i int) DetectionFrame {
	if s.data == nil {
		return DetectionFrame{}
	}
	s.data.mu.RLock()
	defer s.data.mu.RUnlock()
	if i < 0 || i >= len(s.data.samples) {
		return DetectionFrame{}
	}
	sm := s.data.samples[i]
	return DetectionFrame{BBox: rectOf(sm.bbox()), Subjects: int(sm.subjects)}
}

// Complete reports whether the producing detect pass finished all frames.
func (s DetectionStream) Complete() bool {
	if s.data == nil {
		return false
	}
	s.data.mu.RLock()
	defer s.data.mu.RUnlock()
	return s.data.complete
}

// AnalysisWidth/Height report the analysis frame geometry the stream was
// detected at (0 before the pass has probed the input).
func (s DetectionStream) AnalysisWidth() int {
	s.data.mu.RLock()
	defer s.data.mu.RUnlock()
	return s.data.analysisW
}

func (s DetectionStream) AnalysisHeight() int {
	s.data.mu.RLock()
	defer s.data.mu.RUnlock()
	return s.data.analysisH
}

// SourceWidth/Height report the true input video dimensions.
func (s DetectionStream) SourceWidth() int {
	s.data.mu.RLock()
	defer s.data.mu.RUnlock()
	return s.data.srcW
}

func (s DetectionStream) SourceHeight() int {
	s.data.mu.RLock()
	defer s.data.mu.RUnlock()
	return s.data.srcH
}

// FPS reports the analysis video frame rate (prepare preserves source
// timing via -fps_mode passthrough).
func (s DetectionStream) FPS() float64 {
	s.data.mu.RLock()
	defer s.data.mu.RUnlock()
	return s.data.fps
}

// AnalysisVideoPath is the cached analysis video file these frames were
// detected from ("" for synthetic streams). Snapshot/ContactSheet on Plans
// replayed from this stream composite their frames from it.
func (s DetectionStream) AnalysisVideoPath() string {
	if s.data == nil {
		return ""
	}
	s.data.mu.RLock()
	defer s.data.mu.RUnlock()
	return s.data.videoPath
}

// ---------------------------------------------------------------------------
// Cache identity

// analysisFormatVersion bumps when meta.json or stream.bin.gz layout changes;
// caches of older formats are treated as misses.
// v2 (mode-aware analysis, plan §2/U1): meta.json carries "detected"
// (true = the stream holds measured boxes; false = fit-mode synthetic
// geometry samples, resumable with a detect-only pass).
const analysisFormatVersion = 2

// frameAlignmentTag names the prepare encoding mode (§4/S13): frame indices
// map 1:1 between source, analysis video and stream. Part of the cache key
// so a mode change never silently reuses stale streams.
const frameAlignmentTag = "pt1"

const (
	analysisVideoName  = "analysis.mp4"
	analysisStreamName = "stream.bin.gz"
	analysisMetaName   = "meta.json"
)

type analysisMeta struct {
	FormatVersion  int     `json:"format_version"`
	Key            string  `json:"key"`
	Input          string  `json:"input"`
	InputMTimeNS   int64   `json:"input_mtime_ns"`
	InputSizeBytes int64   `json:"input_size_bytes"`
	AnalysisHeight int     `json:"analysis_height"` // the rounded-even value actually encoded
	AnalysisW      int     `json:"analysis_w"`
	AnalysisH      int     `json:"analysis_h"`
	SourceW        int     `json:"source_w"`
	SourceH        int     `json:"source_h"`
	Frames         int     `json:"frames"`
	FPS            float64 `json:"fps"`
	Alignment      string  `json:"frame_alignment"`
	// Detected is the resume law (v2): true when the stream carries measured
	// subject boxes, false when it carries fit-mode synthetic samples - the
	// analysis video is still the full frame-aligned encode either way, so a
	// later crop Analyze runs only the detect phase over it.
	Detected  bool      `json:"detected"`
	CreatedAt time.Time `json:"created_at"`
}

// resolvedAnalyze is the validated, cache-keyed form of an AnalyzeRequest.
type resolvedAnalyze struct {
	input      string // absolute
	mtimeNS    int64
	sizeBytes  int64
	analysisH  int // rounded even
	key        string
	dir        string
	srcW, srcH int // probed dimensions (Analyze path fills; CachedAnalysis reads meta)
}

// resolveAnalyze validates paths and computes the cache identity.
func resolveAnalyze(req AnalyzeRequest) (resolvedAnalyze, error) {
	if req.TempDir == "" {
		return resolvedAnalyze{}, errors.New("holocropper: Analyze requires TempDir")
	}
	input, err := convertToAbsolutePath(req.Input)
	if err != nil {
		return resolvedAnalyze{}, fmt.Errorf("holocropper: analyze input %s: %w", req.Input, err)
	}
	st, err := os.Stat(input)
	if err != nil {
		return resolvedAnalyze{}, fmt.Errorf("holocropper: analyze input %s: %w", input, err)
	}
	if st.IsDir() {
		return resolvedAnalyze{}, fmt.Errorf("holocropper: analyze input %s is a directory", input)
	}

	ah := req.AnalysisHeight
	if ah <= 0 {
		ah = defaultAnalysisHeight
	}
	if ah%2 != 0 {
		ah++
	}

	key := analysisCacheKey(input, st.ModTime().UnixNano(), st.Size(), ah)
	dir := filepath.Join(mustAbs(req.TempDir), "holocropper-analysis-"+key)
	return resolvedAnalyze{
		input: input, mtimeNS: st.ModTime().UnixNano(), sizeBytes: st.Size(),
		analysisH: ah, key: key, dir: dir,
	}, nil
}

func mustAbs(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

func analysisCacheKey(input string, mtimeNS, size int64, analysisH int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d|%d|%s", input, mtimeNS, size, analysisH, frameAlignmentTag)))
	return hex.EncodeToString(sum[:])[:12]
}

// analysisDir holds one cache entry. The registry canonicalises Analysis
// objects per cache dir so preview bookkeeping and the render slot are
// shared across CachedAnalysis/Analyze handles for the same input.
type analysisEntry struct {
	dir      string
	slot     taskSlot // analyze slot: last Analyze call wins
	mu       sync.Mutex
	analysis *Analysis
}

var analysisRegistry = struct {
	mu      sync.Mutex
	entries map[string]*analysisEntry
}{entries: make(map[string]*analysisEntry)}

func getAnalysisEntry(dir string) *analysisEntry {
	analysisRegistry.mu.Lock()
	defer analysisRegistry.mu.Unlock()
	e, ok := analysisRegistry.entries[dir]
	if !ok {
		e = &analysisEntry{dir: dir}
		analysisRegistry.entries[dir] = e
	}
	return e
}

func dropAnalysisEntry(dir string) {
	analysisRegistry.mu.Lock()
	defer analysisRegistry.mu.Unlock()
	delete(analysisRegistry.entries, dir)
}

// liveAnalyze reports the entry's non-terminal analyze Task (what Status()
// shows as the busy analyze indicator).
func (e *analysisEntry) liveAnalyze() *taskBase { return e.slot.live() }

// canonical returns the shared Analysis for this cache dir if a complete
// one exists in memory or on valid disk, else nil (cache miss).
func (e *analysisEntry) canonical(ra resolvedAnalyze) *Analysis {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.analysis != nil && e.analysis.matches(ra) {
		return e.analysis
	}
	a, err := loadAnalysis(ra)
	if err != nil {
		return nil
	}
	e.analysis = a
	a.entry = e
	return a
}

func (e *analysisEntry) setCanonical(a *Analysis) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.analysis = a
	a.entry = e
}

// ---------------------------------------------------------------------------
// Analysis (the cached pass result)

// Analysis is a completed prepare+detect pass: the cached analysis video
// plus the DetectionStream. Renders and Plans execute against it without
// re-detection.
type Analysis struct {
	key        string
	tempDir    string // the caller's scratch parent (render scratch lives here)
	dir        string // cache dir under tempDir
	meta       analysisMeta
	stream     *streamData
	entry      *analysisEntry
	renderSlot taskSlot // shared by RenderPreview and RenderFinal (B1/B2)
}

func (a *Analysis) matches(ra resolvedAnalyze) bool {
	return a.meta.Input == ra.input &&
		a.meta.InputMTimeNS == ra.mtimeNS &&
		a.meta.InputSizeBytes == ra.sizeBytes &&
		a.meta.AnalysisHeight == ra.analysisH
}

// Stream returns the immutable detection stream (safe for any number of
// concurrent Plans).
func (a *Analysis) Stream() DetectionStream { return DetectionStream{data: a.stream} }

// AnalysisVideoPath is the cached analysis mp4 under TempDir; the app may
// serve or inspect it.
func (a *Analysis) AnalysisVideoPath() string { return filepath.Join(a.dir, analysisVideoName) }

// Dir is the analysis cache dir under the caller's TempDir.
func (a *Analysis) Dir() string { return a.dir }

// Detected reports whether this analysis ran the subject-detect pass. A
// fit-mode analysis (false) still holds the full frame-aligned analysis video
// and a complete geometry stream - a later crop Analyze on the same cache key
// resumes with the detect phase only.
func (a *Analysis) Detected() bool { return a.meta.Detected }

// Evict deletes the cache dir and forgets the registry entry (cancelling
// any analyze pass still running for it).
func (a *Analysis) Evict() error {
	if e := a.entry; e != nil {
		if t := e.slot.current(); t != nil {
			t.Cancel()
		}
		e.mu.Lock()
		if e.analysis == a {
			e.analysis = nil
		}
		e.mu.Unlock()
		if e.dir == a.dir {
			dropAnalysisEntry(a.dir)
		}
	}
	return os.RemoveAll(a.dir)
}

// Status is the one cheap call that reconciles every UI spinner: the live
// analyze Task (nil if idle), the live render Task on this Analysis (nil if
// idle), and whether a complete cached stream exists.
func (a *Analysis) Status() AnalysisStatus {
	st := AnalysisStatus{}
	a.stream.mu.RLock()
	st.Cached = a.stream.complete
	a.stream.mu.RUnlock()
	if e := a.entry; e != nil {
		if t := e.liveAnalyze(); t != nil {
			info := t.Progress()
			st.Analysis = &info
		}
	}
	if t := a.renderSlot.live(); t != nil {
		info := t.Progress()
		st.Render = &info
	}
	return st
}

// ---------------------------------------------------------------------------
// Analyze pass

// AnalysisTask is the live handle of an Analyze pass. It implements the
// Task API (ID/State/Progress/Cancel via the embedded taskBase).
type AnalysisTask struct {
	*taskBase

	data *streamData // the partial stream, valid from Task creation

	mu       sync.Mutex
	analysis *Analysis // set just before the runner finishes successfully
}

// Wait blocks until the Task reaches a terminal state. On completion it
// returns the cached Analysis; superseded/cancelled/failure surface through
// the error channel (*SupersededError, ErrCancelled, pass errors).
func (t *AnalysisTask) Wait() (*Analysis, error) {
	if err := t.waitTerminal(); err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.analysis, nil
}

// PartialStream returns the frames detected SO FAR - live while the pass
// runs (Len() grows during the detect phase).
func (t *AnalysisTask) PartialStream() DetectionStream { return DetectionStream{data: t.data} }

// Plan replays framing over the partial stream while detection still runs,
// so overlay tuning can begin before the pass finishes.
func (t *AnalysisTask) Plan(s Settings) (*Plan, error) { return NewPlan(t.PartialStream(), s) }

// Analysis returns the finished Analysis (nil until completion).
func (t *AnalysisTask) completedAnalysis() *Analysis {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.analysis
}

// Analyze starts (or reuses) the analysis pass for req. Last call wins per
// cache key: a running Analyze for the same input + analysis height is
// superseded by this one. A valid cache hit completes the Task immediately
// without running any pass. Mode-aware (plan §2/U1):
//   - fit over any complete cache: immediate hit (geometry is there; fit
//     never needed the detection).
//   - crop over a detected cache: immediate hit, as before.
//   - crop over a fit-skipped cache: RESUME - the cached analysis video is
//     already the frame-aligned encode this key produced, so only the detect
//     phase runs. If the analysis video vanished, the pass re-runs whole.
func Analyze(ctx context.Context, req AnalyzeRequest) (*AnalysisTask, error) {
	if errs := req.Validate(); len(errs) > 0 {
		return nil, &SettingsError{Errors: errs}
	}
	ra, err := resolveAnalyze(req)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(ra.dir, 0o755); err != nil {
		return nil, fmt.Errorf("holocropper: analysis cache dir: %w", err)
	}

	entry := getAnalysisEntry(ra.dir)
	task := &AnalysisTask{
		taskBase: newTaskBase(ctx, TaskAnalyze, req.Observer),
		data:     newStreamData(),
	}
	entry.slot.install(task.taskBase)

	resumeFrom := (*Analysis)(nil)
	if a := entry.canonical(ra); a != nil {
		if req.normalizedMode() == AnalyzeFit || a.meta.Detected {
			// Cache hit: adopt the cached stream into the Task's partial
			// stream and complete immediately - no ffmpeg, no detect.
			src := a.stream
			src.mu.RLock()
			task.data.samples = make([]streamSample, len(src.samples))
			copy(task.data.samples, src.samples)
			task.data.analysisW, task.data.analysisH = src.analysisW, src.analysisH
			task.data.srcW, task.data.srcH = src.srcW, src.srcH
			task.data.fps, task.data.complete = src.fps, src.complete
			task.data.videoPath = src.videoPath
			src.mu.RUnlock()
			task.mu.Lock()
			task.analysis = a
			task.mu.Unlock()
			task.finish(nil)
			return task, nil
		}
		// crop over a fit-skipped cache: resume where the fit pass stopped.
		if st, serr := os.Stat(a.AnalysisVideoPath()); serr == nil && st.Size() > 0 {
			resumeFrom = a
		}
		// No usable analysis video (partial state): fall through to a full pass.
	}

	go runAnalyze(entry, task, req, ra, resumeFrom)
	return task, nil
}

// CachedAnalysis loads a valid cached Analysis (no Task, no pass runs).
// ErrAnalysisCacheMiss means re-Analyze is required (new input, changed
// input, different analysis height, evicted or half-finished cache).
func CachedAnalysis(input, tempDir string, analysisHeight int) (*Analysis, error) {
	ra, err := resolveAnalyze(AnalyzeRequest{Input: input, TempDir: tempDir, AnalysisHeight: analysisHeight})
	if err != nil {
		return nil, err
	}
	if a := getAnalysisEntry(ra.dir).canonical(ra); a != nil {
		return a, nil
	}
	return nil, fmt.Errorf("%w (%s, analysis height %d)", ErrAnalysisCacheMiss, ra.input, ra.analysisH)
}

// runAnalyze is the Task runner: prepare (ffmpeg re-encode, frame
// passthrough), detect (streamed subject detection) or the fit-mode geometry
// probe, persistence. resumeFrom carries a fit-skipped cache whose analysis
// video the detect phase replays over - prepare is skipped (nil = run the
// whole pass).
func runAnalyze(entry *analysisEntry, task *AnalysisTask, req AnalyzeRequest, ra resolvedAnalyze, resumeFrom *Analysis) {
	tb := task.taskBase
	defer func() {
		if r := recover(); r != nil {
			tb.finish(fmt.Errorf("holocropper: analyze pass panicked: %v", r))
		}
	}()
	tb.begin()

	a, err := analyzeWork(task.ctx, entry, task, req, ra, resumeFrom)
	if err != nil {
		if !req.KeepWorkDir {
			// Leave no half-written cache: this Task's own scratch files are
			// removed; the cache dir stays consistent (a later hit still misses).
			cleanupScratch(ra.dir, tb.id)
		}
		tb.finish(err)
		return
	}
	entry.setCanonical(a)
	task.mu.Lock()
	task.analysis = a
	task.mu.Unlock()
	tb.finish(nil)
}

// scratchName is a per-Task scratch filename in the cache dir. Two Analyze
// passes for the same key can overlap briefly (the newer one supersedes the
// older, which then unwinds); shared tmp filenames would let the loser's
// cleanup delete the winner's in-flight file. The task tag goes before the
// extension so ffmpeg still infers the muxer from the suffix.
func scratchName(dir, name string, taskID int) string {
	ext := filepath.Ext(name)
	stem := name[:len(name)-len(ext)]
	return filepath.Join(dir, fmt.Sprintf("%s.tmp-%d%s", stem, taskID, ext))
}

func cleanupScratch(dir string, taskID int) {
	os.Remove(scratchName(dir, analysisVideoName, taskID))
	os.Remove(scratchName(dir, analysisStreamName, taskID))
	os.Remove(scratchName(dir, analysisMetaName, taskID))
}

// analyzeWork runs the pass selected by req's mode. resumeFrom != nil is the
// detect-resume law (plan §2/U1): the cached analysis video is already the
// frame-aligned encode this cache key produced, so prepare is skipped and
// only the detect phase runs over it.
func analyzeWork(ctx context.Context, entry *analysisEntry, task *AnalysisTask, req AnalyzeRequest, ra resolvedAnalyze, resumeFrom *Analysis) (*Analysis, error) {
	tb := task.taskBase
	sd := task.data

	// Source geometry + frame count (metadata probe only).
	srcVideo, err := vidio.NewVideo(ra.input)
	if err != nil {
		return nil, fmt.Errorf("holocropper: analyze open %s: %w", ra.input, err)
	}
	srcW, srcH, srcFrames := srcVideo.Width(), srcVideo.Height(), srcVideo.Frames()
	srcVideo.Close()
	if srcW <= 0 || srcH <= 0 || srcFrames <= 0 {
		return nil, fmt.Errorf("holocropper: analyze unreadable input %s", ra.input)
	}

	// Analysis frame size follows the true source aspect (no output-height
	// floor here: Analyze does not know the output; renders that need a
	// taller analysis video re-Analyze - see Plan's ErrStreamIncompatible).
	ah := ra.analysisH
	aw := ah * srcW / srcH
	if aw < 16 {
		aw = 16
	}
	if aw%2 != 0 {
		aw++
	}
	sd.setGeometry(aw, ah, srcW, srcH, 0)

	videoPath := filepath.Join(ra.dir, analysisVideoName)
	if resumeFrom != nil {
		// Detect resume (plan §2/U1): the fit pass's analysis video is already
		// in place under this exact cache key - same analysis height input,
		// same source dims, so the geometry computed above is the geometry it
		// was encoded at. Prepare is skipped entirely.
		if resumeFrom.AnalysisVideoPath() != videoPath {
			return nil, fmt.Errorf("holocropper: detect resume cache mismatch (%s vs %s)",
				resumeFrom.AnalysisVideoPath(), videoPath)
		}
	} else {
		// Prepare: frame-passthrough re-encode (-fps_mode passthrough, §4/S13)
		// with no audio; written to .tmp and renamed so a cancelled pass never
		// leaves a half-file named analysis.mp4.
		tb.setPhase(PhasePrepare, srcFrames)
		tmpVideo := scratchName(ra.dir, analysisVideoName, tb.id)
		args := []string{
			"-hide_banner",
			"-i", ra.input,
			"-vf", fmt.Sprintf("scale=%d:%d", aw, ah),
			// lanczos+bitexact: the analysis scaler must be CPU-dispatch
			// independent. SIMD lanczos paths round differently on different
			// CPU flag sets (a 1 px edge-ring difference was observed between
			// a desktop and a GH runner on the SAME ffmpeg build), which
			// would silently shift detect bboxes and break the golden parity
			// gate. bitexact forces swscale's deterministic algorithm.
			"-sws_flags", "lanczos+accurate_rnd+bitexact",
			// cpu-independent x264: same rationale - subpel search and mode
			// decisions must not follow the CPU's asm, or the analysis
			// bitstream (and therefore detect) differs per machine.
			"-x264-params", "cpu-independent=1",
			"-fps_mode", "passthrough",
			"-an",
			tmpVideo,
			"-y",
		}
		if err := runFFmpegProgress(ctx, req.FFmpegPath, args, PhasePrepare, tb); err != nil {
			return nil, err
		}
		if err := os.Rename(tmpVideo, videoPath); err != nil {
			return nil, fmt.Errorf("holocropper: analysis video finalize: %w", err)
		}
	}
	// From here on the stream knows which video file its frames came from,
	// so Snapshot/ContactSheet on Plans built from PartialStream work too.
	sd.setVideoPath(videoPath)

	detected := req.normalizedMode() == AnalyzeCrop
	var frames int
	if detected {
		// Detect: stream the analysis video through the shared single-frame
		// detector (detectFrame): grayscale -> Otsu hysteresis -> binary ->
		// subjectsFromBinary, the same measurements the corpus framing
		// decisions were tuned on.
		tb.setPhase(PhaseDetect, srcFrames)
		batcher := newFrameBatcher(req.Observer)
		frames, err = runDetectStream(ctx, videoPath, tb, sd, batcher)
		if err != nil {
			return nil, err
		}
	} else {
		// Fit mode: no subject detection - the fit transform scales the full
		// frame, so every consumer downstream needs only geometry and frame
		// count from the stream. One subject-less sample per analysis frame,
		// counted through the same vidio reader the detect pass and the
		// render loops use: the stream/records/decoded-frame invariant holds
		// by construction.
		tb.setPhase(PhaseProbe, srcFrames)
		frames, err = runFitProbe(ctx, videoPath, tb, sd)
		if err != nil {
			return nil, err
		}
	}
	// A supersede/cancel that landed while the last frames decoded still
	// wins here: never persist a stream for a pass that no longer owns the
	// slot.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sd.markComplete()

	// Persist stream + meta (per-Task scratch, renamed into place, keeps the
	// cache all-or-nothing).
	tmpStream := scratchName(ra.dir, analysisStreamName, tb.id)
	if err := writeStreamFile(tmpStream, sd); err != nil {
		return nil, err
	}
	if err := os.Rename(tmpStream, filepath.Join(ra.dir, analysisStreamName)); err != nil {
		return nil, err
	}
	meta := analysisMeta{
		FormatVersion:  analysisFormatVersion,
		Key:            ra.key,
		Input:          ra.input,
		InputMTimeNS:   ra.mtimeNS,
		InputSizeBytes: ra.sizeBytes,
		AnalysisHeight: ah,
		AnalysisW:      aw,
		AnalysisH:      ah,
		SourceW:        srcW,
		SourceH:        srcH,
		Frames:         frames,
		FPS:            sd.fpsLocked(),
		Alignment:      frameAlignmentTag,
		Detected:       detected,
		CreatedAt:      time.Now().UTC(),
	}
	tmpMeta := scratchName(ra.dir, analysisMetaName, tb.id)
	metaBytes, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(tmpMeta, metaBytes, 0o644); err != nil {
		return nil, err
	}
	if err := os.Rename(tmpMeta, filepath.Join(ra.dir, analysisMetaName)); err != nil {
		return nil, err
	}

	return &Analysis{
		key:     ra.key,
		tempDir: filepath.Dir(ra.dir),
		dir:     ra.dir,
		meta:    meta,
		stream:  sd,
	}, nil
}

// runDetectStream decodes the analysis video frame by frame and appends
// detection samples; returns the number of frames processed.
func runDetectStream(ctx context.Context, videoPath string, tb *taskBase, sd *streamData, batcher *frameBatcher) (int, error) {
	video, err := vidio.NewVideo(videoPath)
	if err != nil {
		return 0, fmt.Errorf("holocropper: detect open %s: %w", videoPath, err)
	}
	defer video.Close()

	w, h := video.Width(), video.Height()
	buf := image.NewRGBA(image.Rect(0, 0, w, h))
	if err := video.SetFrameBuffer(buf.Pix); err != nil {
		return 0, err
	}
	sd.setGeometry(w, h, sd.srcWLocked(), sd.srcHLocked(), video.FPS())

	lastThr := -1.0
	count := 0
	for video.Read() {
		if err := ctx.Err(); err != nil {
			batcher.flush()
			return count, err
		}
		count++
		bbox, subj := detectFrame(buf, &lastThr)
		sd.append(newStreamSample(bbox, subj))
		if batcher.observer != nil && batcher.observer.OnFrames != nil {
			r := rectOf(bbox)
			batcher.add(FrameRecord{
				Frame:    count,
				Subjects: subj,
				BBox:     r,
				Norm:     SpaceRects{BBox: normalizeRect(r, float64(w), float64(h))},
			})
		}
		tb.setDone(count)
	}
	batcher.flush()
	if count == 0 {
		return 0, fmt.Errorf("holocropper: detect decoded zero frames from %s", videoPath)
	}
	return count, nil
}

// runFitProbe is the fit-mode geometry pass (mode-aware analysis law, plan
// §2/U1): it opens the prepared analysis video with the same vidio reader the
// detect pass and the render loops use, decodes it with NO detection work,
// and appends one subject-less sample per frame. Measuring the frame count
// through this reader (not the container metadata) keeps the invariant every
// render pass checks - decoded frames == stream samples == plan records -
// true by construction.
func runFitProbe(ctx context.Context, videoPath string, tb *taskBase, sd *streamData) (int, error) {
	video, err := vidio.NewVideo(videoPath)
	if err != nil {
		return 0, fmt.Errorf("holocropper: fit probe open %s: %w", videoPath, err)
	}
	defer video.Close()

	w, h := video.Width(), video.Height()
	buf := image.NewRGBA(image.Rect(0, 0, w, h))
	if err := video.SetFrameBuffer(buf.Pix); err != nil {
		return 0, err
	}
	sd.setGeometry(w, h, sd.srcWLocked(), sd.srcHLocked(), video.FPS())

	count := 0
	for video.Read() {
		if err := ctx.Err(); err != nil {
			tb.setDone(count)
			return count, err
		}
		count++
		sd.append(streamSample{})
		if count%512 == 0 {
			tb.setDone(count)
		}
	}
	if count == 0 {
		return 0, fmt.Errorf("holocropper: fit probe decoded zero frames from %s", videoPath)
	}
	tb.setDone(count)
	return count, nil
}

func (d *streamData) srcWLocked() int { d.mu.RLock(); defer d.mu.RUnlock(); return d.srcW }

func (d *streamData) srcHLocked() int { d.mu.RLock(); defer d.mu.RUnlock(); return d.srcH }
func (d *streamData) fpsLocked() float64 {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.fps
}

// ---------------------------------------------------------------------------
// Stream persistence

func writeStreamFile(path string, sd *streamData) error {
	samples, _, _, _, _, _, _ := sd.snapshot()
	var raw bytes.Buffer
	var scratch [10]byte
	for _, s := range samples {
		binary.LittleEndian.PutUint16(scratch[0:2], uint16(s.x0))
		binary.LittleEndian.PutUint16(scratch[2:4], uint16(s.y0))
		binary.LittleEndian.PutUint16(scratch[4:6], uint16(s.x1))
		binary.LittleEndian.PutUint16(scratch[6:8], uint16(s.y1))
		binary.LittleEndian.PutUint16(scratch[8:10], s.subjects)
		raw.Write(scratch[:])
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		zw.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, gz.Bytes(), 0o644)
}

func readStreamFile(path string) ([]streamSample, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var raw bytes.Buffer
	if _, err := raw.ReadFrom(zr); err != nil {
		return nil, err
	}
	b := raw.Bytes()
	if len(b)%10 != 0 {
		return nil, fmt.Errorf("corrupt detection stream %s: %d bytes not a multiple of 10", path, len(b))
	}
	samples := make([]streamSample, len(b)/10)
	for i := range samples {
		o := i * 10
		samples[i] = streamSample{
			x0:       int16(binary.LittleEndian.Uint16(b[o:])),
			y0:       int16(binary.LittleEndian.Uint16(b[o+2:])),
			x1:       int16(binary.LittleEndian.Uint16(b[o+4:])),
			y1:       int16(binary.LittleEndian.Uint16(b[o+6:])),
			subjects: binary.LittleEndian.Uint16(b[o+8:]),
		}
	}
	return samples, nil
}

// loadAnalysis reconstructs a complete Analysis from a valid on-disk cache.
// Any identity/format/file mismatch is a miss (nil, error).
func loadAnalysis(ra resolvedAnalyze) (*Analysis, error) {
	metaBytes, err := os.ReadFile(filepath.Join(ra.dir, analysisMetaName))
	if err != nil {
		return nil, err
	}
	var meta analysisMeta
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return nil, err
	}
	if meta.FormatVersion != analysisFormatVersion ||
		meta.Key != ra.key ||
		meta.Input != ra.input ||
		meta.InputMTimeNS != ra.mtimeNS ||
		meta.InputSizeBytes != ra.sizeBytes ||
		meta.AnalysisHeight != ra.analysisH ||
		meta.Alignment != frameAlignmentTag {
		return nil, fmt.Errorf("holocropper: stale or mismatched analysis cache metadata")
	}

	vst, err := os.Stat(filepath.Join(ra.dir, analysisVideoName))
	if err != nil || vst.Size() == 0 {
		return nil, fmt.Errorf("holocropper: analysis video missing from cache")
	}
	samples, err := readStreamFile(filepath.Join(ra.dir, analysisStreamName))
	if err != nil {
		return nil, err
	}
	if meta.Frames > 0 && len(samples) != meta.Frames {
		return nil, fmt.Errorf("holocropper: cached stream has %d frames, meta says %d", len(samples), meta.Frames)
	}

	sd := newStreamData()
	sd.samples = samples
	sd.analysisW, sd.analysisH = meta.AnalysisW, meta.AnalysisH
	sd.srcW, sd.srcH = meta.SourceW, meta.SourceH
	sd.fps, sd.complete = meta.FPS, true
	sd.videoPath = filepath.Join(ra.dir, analysisVideoName)

	return &Analysis{
		key:     ra.key,
		tempDir: filepath.Dir(ra.dir),
		dir:     ra.dir,
		meta:    meta,
		stream:  sd,
	}, nil
}
