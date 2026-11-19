# HoloCropper Embeddable API Plan

Status: **scheme v5 — awaiting owner approval. No implementation starts before sign-off.**
Consumer: the Wails app (Go backend, web UI via bindings). No HTTP layer, no queue: the app owns jobs and serves files itself; holocropper provides the data, previews, passes and progress through Go binding funcs.

## 1. Owner requirements on record

Standing rulings (2026-11-19):

1. Go public funcs only — no HTTP, no network; all data real time.
2. The app tracks jobs; no queue/persistence inside holocropper.
3. All scratch on disk lives in **caller-supplied directories** (`TempDir`); the library may return paths under it for the app to serve.
4. The surface must be well-designed and easy to build a UI against.

Feature requirements added in approval review (owner, v4/v5 drivers):

5. **Full video previews from the API**: crop preview video, bbox-overlay video, and the original video — returned as servable file paths.
6. **Changing any setting updates everything in real time.**
7. **Final render output call** — explicit, separate from previews.
8. **Real-time progress bars for every pass**: preprocessing analysis re-encode, the detection pass, the preview render, the final render conversion.
9. A full end-to-end UI-ready data plane (the reference app proves it; the UI itself is app-side).
10. **Previews render at the OUTPUT target resolution**, not a low-res proxy.
11. **Refresh-friendly preview files**: the UI must be able to repoint its `<video>` players when a preview is re-rendered. Decision taken in this scheme: **versioned files (`-v<N>.mp4`), never overwrite** (§6.5) — atomic swap, safe while the old file is still being streamed, no browser-cache games.
12. **Spinner-friendly state**: every blocking pass exposes cheap, non-blocking status queries and events so the UI can show spinner overlays — designed with **last-write-wins supersede semantics** so the UI never has to handle "already running" errors (§6.7).

## 2. The architectural core: passes + cached detection + replay

Requirement 6 forbids the current fused pipeline (one pass: scale → decode → detect → frame → crop → mux, re-running everything for every change). The controller facts that make the redesign possible:

- The framing controller (STEP 4) is a **pure per-frame decision function over the detection stream** — `framing_test.go` already tunes it by replaying the controller against recorded corpus bbox streams (§12.7). Streaming and replay are the same arithmetic.
- Detection depends only on the *analysis video* (resolution + frame alignment), **not** on output size, zoom margin, max zoom, or auto-orient. Those settings affect only the framing replay, which costs **milliseconds** for a full video (5 558 frames ≈ trivial arithmetic).

So the pipeline splits into addressable passes:

```
PREPARE ──► DETECT ──► [DetectionStream: cached, per-frame master bbox + subject count]
   │           │
   └─ ffmpeg    └─ decode + Otsu + components, one pass over the analysis video

DETECTIONSTREAM × SETTINGS ──► PLAN  (in-process replay, ~ms, re-run on every setting change)
   ├─► FrameRecords (analysis / normalized / output px)
   ├─► orientation decision + probe stats (auto-orient = stream aggregates)
   ├─► Metrics
   └─► Resolved settings actually used

PLAN ──► RENDER PREVIEW   (servable mp4s AT OUTPUT TARGET RES: crop video + bbox-overlay video,
│                          versioned files -v1/-v2/... - re-rendered whenever settings change)
PLAN ──► RENDER FINAL     (same framing at output res, with source audio, into OutputDir - production file)
```

Key consequences:

- **Real-time setting changes**: output dims, zoom margin, max zoom, auto-orient, walk behaviour → re-`Plan` (instant) → UI redraws overlays and metrics immediately → debounced `RenderPreview` regenerates the output-res preview videos as versioned files the players swap to. Only **analysis height** (and fps, see §4) invalidates the cached detection stream.
- **Final render skips detection**: crops execute against the cached analysis video using the planned records — the expensive part was already paid once.
- **Determinism preserved**: preview, final and CLI all drive the *same* controller code path; replay output is byte-identical to streaming output (asserted by a new equivalence test against corpus CSVs).
- The auto-orient `Probe` becomes a derived view of the same `DetectionStream` — no separate probe decode.

## 3. Passes, progress contract, invalidation

Progress contract (requirement 8): every async pass is a **Task** with `ID()`, `State()`, `Progress() TaskInfo`, `Cancel()`, and typed `Wait()` (full Task API in §6.7); optionally an `Observer` pushes the same events. Phases are task-scoped, each with its own totals (fixes the current conflation where three different bars share `totalFrames`):

| Pass | API | Phases (Done/Total = frames) | Progress source |
|---|---|---|---|
| Prepare (analysis re-encode) | part of `Analyze` | `prepare` | ffmpeg stderr frame counter |
| Detect | part of `Analyze` | `detect` | decode loop counter |
| Plan | `Plan(...)` — synchronous | none (expected < 100 ms for a 5-min video; benchmark test enforces) | — |
| Render preview | `RenderPreview` | `preview_frames` (one crop pass writing BOTH the crop and overlay JPEG sequences at output res), `preview_mux` (crop mux, then overlay mux) | Go crop loop counter, ffmpeg mux counter |
| Render final | `RenderFinal` | `render_frames`, `render_mux` | Go crop loop counter, ffmpeg mux counter |

Invalidation rules (what a settings change costs):

| Changed setting | Re-run needed | Typical cost |
|---|---|---|
| output W/H, zoom margin, max zoom, auto-orient, fps | `Plan` only | ms |
| overlay draw options, or any setting change needing fresh preview video | `RenderPreview` (output res) | crop+mux pass, no detection |
| analysis height | `Analyze` (prepare + detect) | full detection pass once |
| new input video | `Analyze` | full detection pass once |

Cache: the detection stream + analysis video live in `<TempDir>/holocropper-analysis-<key>/` (key = input path + mtime + analysis height + stream frame-alignment mode). The app owns `TempDir`; `Analysis.Evict()` and `Analysis.Dir()` are exposed. Streams are compact (per frame: 4× int16 bbox + subject count → ~150 KB for 5 558 frames).

## 4. Frame alignment fix (needed by the pass model)

Today `resizeTiny` re-encodes the analysis video with `-r <fps>`, which can duplicate/drop frames relative to the source — harmless when everything is one fused pass, but the pass model requires **1:1 frame indices** between source, analysis video, detection stream, previews and the final mux (which maps *source* audio against the crop sequence). Prepare therefore encodes with frame passthrough (`-fps_mode passthrough`); output fps is applied at mux time, exactly as today. The detection stream is keyed by frame index. (Note for the corpus: sources are ≥24 fps; where source fps equals the old `-r` value the stream is unchanged, so existing measurements stay comparable.)

## 5. Current seams to open (from the pipeline audit)

- **S1 — no context**: `Crop` uncancellable; `resizeTiny`/`convertImagesToMP4` use plain `exec.Command`; stages never close their own output channels, so mid-render cancel can deadlock (send-to-closed panic). Fix: per-stage channel closing + drain-discard on cancel + `exec.CommandContext`.
- **S2 — stdout coupling**: startup prints, `cmd.String()` per ffmpeg call, three terminal progress bars.
- **S3 — env-var telemetry only** (`HOLOCROPPER_DEBUG_BOXES`): stays as internal tooling for `HIGH-ASPECT-RATIO-FIXES.md` measurement, not API.
- **S4 — unguarded counters**: `currentFrame++` read unguarded by `tickPipeline`/`Progress()`/`Done()`.
- **S5 — no structured result** (applied dims, analysis dims, output path, elapsed).
- **S6 — probe aggregates only**, no per-sample boxes.
- **S7 — nothing live**: no handle to query a running pass.
- **S8 — wire-unfriendly types**: `image.Rectangle`, error strings, no JSON tags.
- **S9 — phase progress conflated** (see §3).
- **S10 — scratch dirs opaque and leaky**: `os.MkdirTemp` in system temp; cancel leaks the frames dir entirely.
- **S11 — ffmpeg path hardcoded** (`"ffmpeg"` from PATH).
- **S12 — fused pipeline**: every setting change re-runs detection (minutes) — incompatible with requirement 6. The pass split (§2) is the fix.
- **S13 — `-r` resampling** risks frame-index drift between analysis video and source (see §4).

`cmd/holocropper` CLI + Makefile stay as-is until the new surface is proven (then the CLI is reimplemented on `Analyze → Plan → RenderFinal`, keeping identical output).

## 6. The complete API surface (the scheme)

All exported types carry snake_case `json` tags for Wails. Live-query methods are safe from any goroutine while a Task runs.

### 6.1 Settings, validation, schema (settings form)

```go
type Settings struct {
    Width, Height  int     `json:"width"`            // 0 = 240; rounded up to even
    FPS            float64 `json:"fps"`              // 0 = 20; controller dt + mux rate
    AnalysisHeight int     `json:"analysis_height"`  // 0 = 480, never below Height
    ZoomMargin     float64 `json:"zoom_margin"`      // 0 = 1.12
    MaxZoom        float64 `json:"max_zoom"`         // 0 = 2.0
    AutoOrient     bool    `json:"auto_orient"`
}
type ValidationError struct{ Field, Reason string }
func (s Settings) Validate() []ValidationError   // ALL violations at once
type ParamSpec struct { Name, Type string; Default any; Min, Max any; Help string }
func Schema() []ParamSpec                        // covers every Settings field
type Resolved struct { Settings; SourceW, SourceH, AnalysisW, AnalysisH, OutputW, OutputH int }
```

Ranges (`Schema` advertises, `Validate` enforces): width/height 16–4096; fps 1–120; analysis_height 0 or ≥ height; zoom_margin 1.0–10.0 (1.18 landscape/portrait padding floor always applies); max_zoom 1.0–8.0.

### 6.2 Inputs and poster (input picker)

```go
type InputInfo struct { Name, Path string; SizeBytes int64; ModTime time.Time }
func ListInputs(dir string) ([]InputInfo, error)   // sorted, .mp4 only
func PosterFrame(input string, maxWidth int) (jpeg []byte, err error)
```

The **original video** for the UI player is simply `InputInfo.Path` (or the app's own path) — the app serves it; the library never copies inputs.

### 6.3 Analysis pass (prepare + detect, cached, live)

```go
type AnalyzeRequest struct {
    Input          string // input video path
    TempDir        string // cache + scratch parent (required)
    FFmpegPath     string // "" = "ffmpeg" from PATH
    AnalysisHeight int    // 0 = 480; part of the analysis cache key
    Observer       *Observer
}
type AnalysisTask struct{ /* implements the Task API of 6.7: ID/State/Progress/Cancel */ }
func (t *AnalysisTask) Wait() (*Analysis, error)      // blocks until terminal state
func (t *AnalysisTask) PartialStream() DetectionStream // frames detected SO FAR - live while running
func (t *AnalysisTask) Plan(s Settings) (*Plan, error) // replay over the partial stream while running

// Analyze is keyed by its cache key: a second Analyze for the same (input, tempDir,
// analysis height) supersedes the running task (Task API semantics of 6.7) - last call wins.
func Analyze(ctx context.Context, req AnalyzeRequest) (*AnalysisTask, error)
func CachedAnalysis(input, tempDir string, analysisHeight int) (*Analysis, error) // cache hit, no Task

type Analysis struct{ /* cached pass result */ }
func (a *Analysis) Stream() DetectionStream  // immutable, safe for any number of concurrent Plans
func (a *Analysis) AnalysisVideoPath() string // the cached analysis mp4 (TempDir - app may serve/inspect)
func (a *Analysis) Dir() string               // its cache dir
func (a *Analysis) Evict() error

type DetectionStream struct{ /* frames + geometry */ }
func (s DetectionStream) Frame(i int) DetectionFrame
func (s DetectionStream) Len() int
type DetectionFrame struct {
    BBox     Rect `json:"bbox"`     // master subject box, analysis px (empty when subject lost)
    Subjects int  `json:"subjects"`
}
// Geometry exposed by the stream: SourceW/H, AnalysisW/H, frame alignment.
```

Live behaviour during Detect (requirement 8): `PartialStream()` returns the frames detected so far, so framing previews (§6.4) start while detection is still running — the UI shows overlays over the original video before the analysis pass finishes.

### 6.4 Plan (the real-time replay — requirement 6)

```go
func Plan(stream DetectionStream, s Settings) (*Plan, error)

type Plan struct{ /* replayed framing, immutable */ }
func (p *Plan) Records() []FrameRecord   // full per-frame stream, all three coordinate spaces
func (p *Plan) Record(i int) FrameRecord
func (p *Plan) Orientation() OrientationStat  // probe stats + flip decision computed from the stream
func (p *Plan) Resolved() Resolved
func (p *Plan) Metrics() *Metrics
func (p *Plan) Walk() WalkSummary      // active? range px, fallback-frame count (§14 telemetry)
func (p *Plan) DropoutFrames() []IntRange // subject-lost spans
func (p *Plan) SettingsHash() string   // stable hash of the settings that produced this Plan
```

`Plan` is pure and synchronous — the UI can call it on every slider tick (benchmark acceptance: < 100 ms for 5 558 frames; typical ~10 ms). `AutoOrient` is no longer a separate decode: the flip decision is `Plan`'s orientation aggregates. Preview == final == CLI framing are guaranteed identical because all three execute the same replay.

### 6.5 Preview renders (output-resolution videos, versioned servable files — requirements 5, 10, 11)

```go
type PreviewKind string // "crop" | "overlay" | "crop_overlay"
type PreviewOptions struct {
    Kind        PreviewKind `json:"kind"`          // which videos to render
    DrawSubject bool        `json:"draw_subject"`  // overlay video: raw subject + EMA boxes
    DrawCrop    bool        `json:"draw_crop"`     // overlay video: applied crop window
    Observer    *Observer
}
type PreviewRequest struct {
    TempDir    string // preview files live under the analysis cache dir in here
    FFmpegPath string
    Options    PreviewOptions
}

// Method on Analysis (it owns the cached analysis video + preview slot):
func (a *Analysis) RenderPreview(ctx context.Context, p *Plan, req PreviewRequest) (*PreviewTask, error)

type PreviewTask struct{ /* ID(), State(), Progress(), Cancel() - see 6.7 */ }
func (t *PreviewTask) Wait() (*PreviewResult, error)

type PreviewResult struct {
    Version      int    `json:"version"`        // monotonic per Analysis+kind: v1, v2, v3...
    SettingsHash string `json:"settings_hash"`  // == Plan.SettingsHash()
    CropVideoPath string `json:"crop_video_path"`    // <basename>-<WxH>-v<N>.mp4   ("" if kind=overlay)
    OverlayVideoPath string `json:"overlay_video_path"` // <basename>-<WxH>-overlay-v<N>.mp4
    Width, Height int   `json:"width"`         // OUTPUT target res - previews are final framing at final size
    Frames int; FPS float64; ElapsedSeconds float64
    Removed []string   `json:"removed"`        // superseded preview files the sweep deleted
}

// The UI can always ask what previews currently exist and which matches its settings:
type PreviewRef struct {
    Version int; Kind PreviewKind; Path, SettingsHash string
    CreatedAt time.Time; Width, Height, Frames int; FPS float64
}
func (a *Analysis) Previews() []PreviewRef     // newest first; empty if none
```

**Refresh design decision (requirement 11): versioned files, never overwrite.** Overwriting a file a browser is mid-streaming (Range requests, player cache) is fragile; versioned filenames make refresh an atomic swap: the UI loads `v<N+1>`, players keep playing `v<N>` until the swap, stale URLs are simply gone after the sweep. Each `Analysis` keeps a preview dir `<TempDir>/holocropper-analysis-<key>/previews/`; the version counter is monotonic per kind; a retention sweep keeps the **newest 3 versions** per kind plus whatever `Wait` just returned, and reports deleted paths in `Removed`. `Previews()` + `SettingsHash` let the UI answer "is the newest preview already correct for these settings? skip the render" without bookkeeping.

**Supersede semantics (requirement 12):** `RenderPreview` is last-write-wins. Calling it while a preview Task is running on the same `Analysis` cancels the old Task (its `State()` → `superseded`, its `Wait()` → `*SupersededError` carrying the new Task ID) and starts the new one. The UI never handles "already busy" errors: it just spins on the newest Task until it completes. Preview and final render share one **render slot** per `Analysis` — starting a final render supersedes a running preview and vice versa (one crop-style render at a time per analysis; the `Analyze` pass has its own slot).

Mechanics: crops execute from the cached analysis video via the same `cropToBBox`/`fillResize` code as STEP 5, **at output resolution**, in one frame loop that writes both the crop sequence and the composited overlay sequence (boxes drawn from `Plan.Records()` in output px — same visual as the UI's canvas overlays); then two ffmpeg `mjpeg` muxes with progress. JPEG scratch lives in `<TempDir>/holocropper-render-<renderID>/frames/` (renderID-isolated, cancel-safe); finished preview videos move into the analysis preview dir. `Snapshot`/`ContactSheet` (§6.8) remain for scrubber thumbs.

### 6.6 Final render (the explicit output call — requirement 7)

```go
type RenderRequest struct {
    OutputDir  string // required, exists; production output lands here
    TempDir    string // required scratch parent
    FFmpegPath string // "" = PATH
    Observer   *Observer
}
func (a *Analysis) RenderFinal(ctx context.Context, p *Plan, req RenderRequest) (*RenderTask, error)

type RenderTask struct{ /* ID(), State(), Progress(), Cancel() - see 6.7 */ }
func (t *RenderTask) Wait() (*Result, error)

// One-shot convenience (no cached analysis, no interactive flow) — wraps the three passes:
func RenderWhole(ctx context.Context, req AnalyzeRequest, settings Settings, renderReq RenderRequest) (*Result, error)
```

`RenderFinal` executes planned crops against the cached analysis video, JPEG sequence into the render scratch dir, ffmpeg mux **with the source audio track** exactly as today, output at the requested WxH, and cleans scratch on completion/cancel (`Result.WorkDir` reported; a `KeepWorkDir` debug flag available). Output naming is **dimension-tagged** — `<basename>-<WxH>.mp4` — so re-rendering the same input at a different target never silently clobbers a previous production file; the player swap story is the same versioned swap the UI already uses for previews (the app controls its own OutputDir files). It shares the `Analysis` render slot: a running preview is superseded when final starts, and vice versa (`*SupersededError` on the loser). `Crop` (CLI) remains untouched until the CLI is ported to this surface.

### 6.7 Records, tasks, status, observer (the live data plane + spinner contract — requirement 12)

```go
type Rect struct{ X, Y, W, H float64 }
type SpaceRects struct{ BBox, EMA, Crop Rect }
type FrameRecord struct {
    Frame int; Subjects int
    BBox, EMA, Crop Rect      // analysis px
    TargetH, WalkTgt float64; WalkActive bool
    Norm, Out SpaceRects      // precomputed 0..1 and output-px spaces
}

type Phase string // prepare | detect | preview_frames | preview_mux | render_frames | render_mux
type TaskKind string // analyze | preview | render_final
type TaskState string // queued | running | completed | failed | cancelled | superseded

// TaskInfo is the ONE status struct every Task speaks, cheap and non-blocking to fetch.
type TaskInfo struct {
    ID int    `json:"id"`      // monotonic per Analysis; UI maps spinner <-> ID
    Kind TaskKind `json:"kind"`
    State TaskState `json:"state"`
    Phase Phase  `json:"phase"`
    Done, Total int `json:"done"`
    ElapsedSeconds float64 `json:"elapsed_seconds"`
    Err string `json:"err,omitempty"`
}

// Every Task (AnalysisTask / PreviewTask / RenderTask) implements:
//   ID() int · State() TaskState · Progress() TaskInfo · Cancel() · <typed>Wait()
// All are safe from any goroutine at any time, never block, valid before/after Wait too
// (completed Tasks keep their final TaskInfo until the Analysis is dropped).

// Aggregate snapshot for spinner wiring - one call answers "what is busy right now":
type AnalysisStatus struct {
    Analysis *TaskInfo `json:"analysis,omitempty"` // running analyze, nil if idle/done-from-cache
    Render   *TaskInfo `json:"render,omitempty"`   // current preview OR final render (shared slot), nil if idle
    Cached   bool      `json:"cached"`
}
func (a *Analysis) Status() AnalysisStatus

type SupersededError struct{ NewTaskID int } // Wait() result when another call replaced this Task

type Observer struct {
    OnTask    func(TaskInfo)         // every state/phase change + progress ≤ 4 Hz, all Tasks
    OnFrames  func([]FrameRecord)    // ≤ 24 records or 250 ms (detect partials + framing)
    OnWarning func(string)           // ffmpeg stderr diagnostics
}
// Callbacks fire from pipeline goroutines; they must not block (app job: EventsEmit / buffered channel).
```

UI spinner recipe (the whole requirement): on any action call, remember `Task.ID()`; show a spinner keyed to that ID; remove it when `OnTask` reports `completed/failed/cancelled/superseded` — a superseded spinner just moves to `NewTaskID`, no error UI ever appears for overlapping requests. `Status()` is the polling fallback / startup reconciliation call after a UI reload mid-render: one non-blocking call restores every spinner to its true state.

### 6.8 Stills, contact sheets, metrics

```go
func (p *Plan) Snapshot(frame int, maxWidth int) (*Snapshot, error)      // annotated output-space frame from the analysis video, in-memory
func (p *Plan) ContactSheet(maxWidth int) (*ContactSheet, error)         // tiles + frame idx + walk markers, in-memory
type Metrics struct { CoverageMeanPct, CoverageMedianPct float64; EdgeOvershootFrames int;
    MinSlackPctPerSide, WalkRangePx float64; DropoutFrames, SceneCuts int }
func ComputeMetrics(records []FrameRecord, geo Geometry) *Metrics         // Python-tooling port
```

Stills need no Task: they composite from the cached analysis video + `Plan` records on demand (fast: one video seek). All bytes cross bindings directly.

### 6.9 Result and errors

```go
type Result struct {
    InputPath, OutputPath string; WorkDir string
    SourceW, SourceH, AnalysisW, AnalysisH, OutputW, OutputH int
    AppliedSettings Resolved; Orientation *OrientationStat
    Frames int; FPS float64; ElapsedSeconds float64
    Warnings []string
}
```

Error taxonomy: `[]ValidationError` (per-field, from every Validate); `context.Canceled`; sentinel `ErrAnalysisCacheMiss` (CachedAnalysis), `ErrStreamIncompatible` (Plan against a stream with mismatched geometry); `*FFmpegError{Phase, Command, ExitCode, StderrTail}`. No bare error strings in the new surface.

## 7. Coordinate mapping (analysis px → normalized → output px)

Analysis frame: `AH` = analysis height (even), `AW = AH × srcW/srcH` (even, true source AR). Normalized: `u = x/AW`, `v = y/AH` — UI-stable across settings, scales onto any served original/preview video. Output px: crop rect is aspect-locked and fills the output exactly: `out = ((p.x - crop.X) × OutputW/CropW, (p.y - crop.Y) × OutputH/CropH)`; landscape stage-padding crops may legitimately produce out-of-bounds `Out` values (raw, clamped by the UI). Grounded crops: crop bottom == output bottom exactly. Every `FrameRecord` ships all three spaces, so drawing over the **original video** in the UI = scale normalized rects to the player size — zero client-side geometry.

## 8. Concurrency, cache and real-time policy

- **No queue in the library.** App owns scheduling. Per `Analysis`: an **analyze slot** and a **render slot** (preview and final share it) — starting a pass in a slot supersedes that slot's running Task, last call wins; no "already busy" errors ever reach the UI. `Plan` is pure, concurrent-safe against an immutable `DetectionStream`, unlimited. Multiple instances may run Tasks concurrently (CPU-heavy; serial stays frame-deterministic run-to-run — the property the §13/§14 verification workflow depends on; the app decides).
- Debounce guidance (app-side): slider drag → `Plan` each tick (ms), overlay redraw immediate; `RenderPreview` re-trigger debounced ~300–500 ms — supersede semantics make the re-trigger safe even while an older preview is still rendering; skip entirely when `Previews()` already has a `SettingsHash` match. Preview cost note: previews render at **output resolution** (requirement 10) ≈ a crop+mux pass without detection — expect roughly a third of the fused pipeline's time on the corpus (measured in B1's acceptance run).
- Cache lifecycle: analysis cache keyed by (input path+mtime+size, analysis height); lives under the app's `TempDir`; app may delete the whole dir tree any time. Render scratch (`renderID`-isolated) is removed on completion/cancel unless `KeepWorkDir`.
- Memory ceilings: detection stream ~150 KB/5-min video; `Plan` records ~16 numbers/frame (numbers only); preview JPEGs streaming (one in flight) — per-flow memory stays tens of MB.
- Live ceilings on the binding path: frame batches ≤ 24 recs/250 ms; progress ≤ 4 Hz/phase; no unbounded queues anywhere.

## 9. End-to-end UI flow this surface enables (requirement 9)

```
Schema() ──────────► settings form (per-field errors from Validate())
ListInputs(dir) ───► input picker;  PosterFrame / InputInfo.Path ► player + poster
Analyze(...) ──────► TaskInfo spinner (prepare, detect); PartialStream+Plan ► overlays start immediately
Plan(stream, s) ───► canvas overlays (Norm rects over original video), Metrics badges,
                     orientation decision, walk summary — re-run instantly on any setting change
Previews() / SettingsHash ► "is the current preview already right?" — skip re-render when yes
RenderPreview(...) ► output-res videos; on completion players SWAP to -v<N+1>.mp4 files;
                     running preview superseded automatically by a newer request or by RenderFinal
RenderFinal(...) ──► spinner on its TaskInfo; Result.OutputPath (<basename>-<WxH>.mp4) ► player
Analysis.Status() ► startup/reload reconciliation: restores every spinner with one cheap call
Cancel on any Task ► spinner clears, scratch cleaned, cancelled state surfaces
```

`examples/wails-bindings/` implements exactly this flow as the acceptance reference for the data plane.

### 9.1 UI implementation suggestions (app side — the Wails frontend)

Concrete patterns for building the tuning UI on this surface. These are recommendations for the app, not API contracts.

**Layout (one screen, three zones):**

- **Left rail — settings form**, generated from `Schema()`: sliders for `zoom_margin`/`max_zoom`, number inputs for W/H/fps/analysis height, a switch for `auto_orient`. Inline per-field errors from `Validate()` (no modal dialogs). Output-dims presets as chips (1:1, portrait 200×648, landscape 648×200…) plus the orientation decision readout (keep-vs-flip score bars from `Plan.Orientation()`, with a "flipped to 200×648" badge when auto-orient transposes).
- **Center — video stack with one canvas:** the **original video** player with a canvas overlay drawing `FrameRecord.Norm` rects (subject bbox, EMA, crop window) — the primary tuning view. Tabs switch the source between original (canvas ON), the **crop preview** player (canvas OFF — boxes already baked into the overlay preview tab), and the **bbox-overlay preview** (canvas OFF — boxes baked). Tabs swap `video.src` only; nothing else changes.
- **Bottom — timeline strip:** frame-index scrubber over `Plan.Records()`: dropout spans shaded (`Plan.DropoutFrames()`), scene-cut ticks, walk-active segments highlighted (`Walk()`). Thumbnails from `ContactSheet()` tiles; clicking a tile seeks the player (`currentTime = frame / FPS`) and opens a record popover (bbox numbers, crop geometry, walk state). Right of it: `Metrics()` badges (coverage %, overshoot count, min slack) that recompute live on every re-plan.

**JS state model (small on purpose):**

```
state = { input, settings, plan, previews: {crop: PreviewRef, overlay: PreviewRef},
          tasks: Map<taskID, TaskInfo>, spinners: Set<taskID> }
```

Everything the UI renders comes from those five fields — the API was shaped so no other bookkeeping is needed.

**Overlay drawing:** `requestVideoFrameCallback` maps player position to a frame index (`frame = round(currentTime × FPS)`), look up `Plan.Record(frame)` once per video frame, scale `Norm` rects (0..1) to the canvas display size. That's the entire geometry story — never derive boxes client-side. During drag-scrubbing (no frame callbacks), interpolate against `Record(i)`/`Record(i+1)`.

**Real-time tuning loop:**

1. Slider `input` event → binding call `Plan(stream, settings)` (~10 ms) → redraw overlays + metrics immediately. This is the frame-rate path; nothing heavier runs here.
2. Debounce ~400 ms after the last change → check `Previews()` for a `SettingsHash` match; if none, call `RenderPreview` (supersede semantics make firing it mid-render safe — just fire, the old one dies).
3. On `OnTask` `completed` for the preview task: **preload then swap** — set `video.src = new -vN path` on the active player only after the new file's first frame loads (a hidden `<video preload="metadata">` or `fetch` HEAD); players keep playing the old version until the swap succeeds, so a superseded render never blanks the UI. Deleted `Removed` paths are simply dropped from state.

**Spinners and busy states (from §6.7 only):** spinner overlay per zone (analyze strip across timeline + timeline disabled while detect runs; preview corner spinner on the video stack; full-width bar for final render). All driven by `TaskInfo` via `OnTask`; on app start or hot reload call `Status()` once and restore spinners from it. `superseded` never shows an error — move the spinner to `NewTaskID`. Gate preview renders while the analysis slot is `running`: partial overlays/timeline yes (via `AnalysisTask.Plan`), preview *video* renders no (the analysis mp4 isn't finalized yet) — show a "partial analysis" badge instead.

**Progress bars:** each zone's bar binds to `TaskInfo.Done/Total` for its current slot task — prepare/detect for the analyze zone, preview_frames/preview_mux for previews, render_frames/render_mux for final. Phase name maps to label text; nothing needs to know *why* totals change — phases carry their own totals.

**File serving:** serve only the two roots the app owns — `TempDir` and `OutputDir` — through a custom Wails URL scheme (e.g. `app://previews/...` mapped onto `Analysis.Previews()` paths, `app://outputs/...` onto `Result.OutputPath`). Versioned filenames are natural cache-busting: serve preview files with `Cache-Control: immutable`. Never expose the whole TempDir — map only paths the API returned.

**Final render flow:** explicit button (not automatic) → spinner zone + progress bar → on completion add `Result.OutputPath` to a "renders" list under the input, player-able instantly and openable in the OS via the app. Dimension-tagged names make the list survive re-renders at different targets.

**Errors:** validation → inline field errors; `*FFmpegError` → toast with the `StderrTail`; `cancelled`/`superseded` → silent (they're normal flow); `ErrStreamIncompatible` → prompt "analysis outdated for these settings — re-analyze" action.

**Keyboard/quality-of-life:** space = play/pause player, `[`/`]` = step frame (`Record(i)` popover follows), `r` = re-plan+preview shortcut. Keep all Plan-driven views (canvas, timeline, metrics) subscribed to the *current Plan object* — swapping the Plan pointer is the single re-render trigger for the whole tuning UI.

## 10. Epics, milestones, stories

Estimates: story points (1,2,3,5,8). No implementation before approval.

### Milestone A — passes, replay engine, real-time planning

| ID | Story | Est | Depends on | Acceptance criteria |
|---|---|---|---|---|
| A1 | Teardown/context core: per-stage channel closing + drain-discard, `exec.CommandContext` with configurable `FFmpegPath`, cancel-safe scratch under caller `TempDir` (renderID isolation, `KeepWorkDir`), fix S4 atomics | 5 | — | kill mid-ffmpeg/mid-pipeline: no zombies, no leaked scratch, `context.Canceled` propagates; CLI corpus output unchanged |
| A2 | `Settings`/`Validate`/`Schema`/`Resolved`, `AnalysisRequest`/`RenderRequest`/`PreviewRequest` validation, typed error taxonomy | 3 | — | all invalid inputs surface as per-field `ValidationError` lists |
| A3 | **Replay engine**: extract the STEP 4 controller into a replayable per-frame decider; streaming calcVideo delegates to it; corpus equivalence test (replay of recorded bbox streams == streamed CSV framing, < 1 px drift) | 8 | A1 | existing 36 tests green; equivalence test passes on tbaka/sweet streams |
| A4 | Analysis pass: `Analyze` Task (prepare with `-fps_mode passthrough`, detect), cache under `TempDir`, `CachedAnalysis`, `PartialStream()`, progress phases | 5 | A1, A2 | re-`CachedAnalysis` hit skips the whole pass; partial stream grows during detect; stream ~150 KB for 5-min video |
| A5 | `Plan`: replay → `FrameRecord` list (Norm/Out precomputed), orientation/probe aggregates (replaces separate probe decode), `Resolved`, `Walk`, dropout; benchmark < 100 ms @ 5 558 frames | 5 | A3 | auto-orient decision matches current CLI probe decisions on the corpus |
| A6 | `ComputeMetrics` port from the Python tooling + `Plan.Metrics` | 5 | A5 | reproduces §14/§14.1 numbers (tbaka overshoot 47, slack ≥ 3.5 %/side) |

### Milestone B — preview and final renders

| ID | Story | Est | Depends on | Acceptance criteria |
|---|---|---|---|---|
| B1 | `RenderPreview` Task **at output target resolution**: crop + bbox-overlay videos from cached analysis + Plan; versioned `-vN` files + retention sweep + `Previews()`; supersede slot semantics; ffmpeg mux with progress | 8 | A4, A5 | 5-min 1080p source → output-res preview pair, measurably faster than the fused render (no detection); overlay boxes match `Plan.Records`; supersede cancels a running preview cleanly mid-pass |
| B2 | `RenderFinal` Task (`Analysis` method): crop sequence + audio mux from cached analysis + Plan; `<basename>-<WxH>.mp4` naming; shares render slot; `RenderWhole` one-shot; parity test vs `Crop` output (same framing decisions, frame-deterministic) | 5 | A4, A5 | final mp4 frame count/AR correct; parity test green |
| B3 | `Snapshot`/`ContactSheet` from cached analysis + Plan (in-memory compositing) | 3 | A5 | tbaka 16:9 contact sheet ≤ ~4 MB, boxes match `Record.Out` |
| B4 | Unified Task API + status plane: `TaskInfo`/`OnTask` across all Tasks, supersede states + `SupersededError`, `Analysis.Status()`, rate ceilings, warnings from ffmpeg stderr | 5 | A1–A5 | the §6.7 spinner recipe holds on a corpus run: overlapping requests never surface errors to the UI |

### Milestone C — integration proof

| ID | Story | Est | Depends on | Acceptance criteria |
|---|---|---|---|---|
| C1 | `examples/wails-bindings/` reference app implementing the §9 flow end to end (live detect progress, instant re-plan on slider changes, output-res preview player swaps with spinner overlays, final render, cancel) | 8 | A1–A6, B1–B4 | interactive setting changes visibly update overlays/metrics immediately; preview players swap to new version files; spinners driven purely by `TaskInfo`/`Status`/`OnTask` |
| C2 | CI test suite: golden pipeline test on a tiny fixture video (no corpus), replay equivalence, Task cancel cases, benchmark gate | 5 | C1 | suite green without corpus videos; benchmark gate enforced |
| C3 | Port `cmd/holocropper` CLI onto the new surface (`Analyze → Plan → RenderFinal`), delete the fused one-shot path; env-var CSV kept as internal debug shim inside the detect pass | 3 | B2 | CLI corpus renders match current framing decision streams |

### Deferred (explicitly out of scope)

The Wails UI itself; uploads; HTTP/network of any kind; job persistence/history; supersampling above source height as a default.

## 11. What changed across scheme versions

### v3 → v4

- **Pass model replaces the fused pipeline** (S12): Prepare/Detect cached → Plan replay → Preview/Final renders. Real-time setting changes are replays, not re-renders.
- `StartRender/Run` monolith replaced by Task-per-pass with `State/Progress/Cancel/Wait` on each.
- Full **preview videos** (crop + bbox overlay) as servable TempDir paths replace JPEG-only live preview as the primary preview plane; `LiveFrame` folded away — during-detect feedback comes from `PartialStream` + the UI drawing over the original video.
- `Probe` folded into the model: the detection stream is the probe; orientation is a `Plan` aggregate.
- Frame-alignment fix (`-fps_mode passthrough`, S13) added as story A4.
- New milestones: A3 (replay engine, the keystone), C3 (CLI port onto the new surface).
- Kept from v3: TempDir ownership, FFmpegPath, Validate-all-errors + Schema, typed errors, wire-friendly records with three coordinate spaces, no queue in the library, the env-var CSV as internal tooling.

### v4 → v5 (this revision, per owner notes)

- **Previews render at the OUTPUT target resolution** (req 10): the low-res proxy (`MaxWidth`) is gone. A preview is the final framing at final size minus audio, executed from the cached analysis video.
- **Refresh story decided: versioned files (`-v<N>.mp4`), never overwrite** (req 11) — monotonic version per kind, retention sweep (newest 3 per kind), `Analysis.Previews()` + `Plan.SettingsHash()` so the UI can skip redundant renders and atomically repoint players. Overwriting mid-streamed files rejected: Range requests + player caches make it fragile.
- **Slot + supersede semantics** (req 12): each `Analysis` has an analyze slot and a shared render slot; overlapping requests are last-write-wins, `superseded` state + `SupersededError{NewTaskID}` move the spinner to the new Task — the UI never handles "already busy" errors.
- **Unified status plane**: one `TaskInfo` across all Tasks (`ID/State/Phase/Done/Total/Err`), `Analysis.Status()` snapshot for startup/reload spinner reconciliation, single `Observer.OnTask` event feed, explicit spinner recipe spelled out in §6.7.
- `RenderPreview`/`RenderFinal` become **`Analysis` methods** (they need the cached analysis video); `Analyze` supersedes by cache key; `AnalysisTask.Plan(settings)` replays framing over partial streams while detection still runs.
- Final output naming dimension-tagged (`<basename>-<WxH>.mp4`): re-rendering a video at a new target never clobbers a previous production file.
- Added §9.1: UI implementation suggestions for the Wails frontend (layout, JS state model, `requestVideoFrameCallback` overlay loop, preload-then-swap preview refresh, spinner wiring, file serving, final render flow) — app-side guidance, not API contract.

## 12. Remaining tunables (defaults; not blocking approval)

- Preview retention: newest 3 versions per kind (raise if the UI wants an A/B compare of consecutive settings).
- Debounce 300–500 ms (app-side); frame batch ceilings (24 recs / 250 ms) retuned with the UI.
- Analysis cache key granularity (mtime-based; content-hash later if needed).

## 13. Sequencing (starts only after approval)

`A1 → A2 → A3 → A4 → A5 → {A6, B1, B2} → {B3, B4} → C1 → C2 → C3`.
A3 is the architectural keystone: everything in B and the interactive flow stands on the replay engine. First deliverable checkpoint: A5 (interactive re-planning proven on corpus streams, before any video is re-rendered).
