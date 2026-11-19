# HoloCropper Embeddable API Plan

Status: **scheme v4 — awaiting owner approval. No implementation starts before sign-off.**
Consumer: the Wails app (Go backend, web UI via bindings). No HTTP layer, no queue: the app owns jobs and serves files itself; holocropper provides the data, previews, passes and progress through Go binding funcs.

## 1. Owner requirements on record

Standing rulings (2026-11-19):

1. Go public funcs only — no HTTP, no network; all data real time.
2. The app tracks jobs; no queue/persistence inside holocropper.
3. All scratch on disk lives in **caller-supplied directories** (`TempDir`); the library may return paths under it for the app to serve.
4. The surface must be well-designed and easy to build a UI against.

Feature requirements added in approval review (owner, v4 drivers):

5. **Full video previews from the API**: crop preview video, bbox-overlay video, and the original video — returned as servable file paths.
6. **Changing any setting updates everything in real time.**
7. **Final render output call** — explicit, separate from previews.
8. **Real-time progress bars for every pass**: preprocessing analysis re-encode, the detection pass, the preview render, the final render conversion.
9. A full end-to-end UI-ready data plane (the reference app proves it; the UI itself is app-side).

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

PLAN ──► RENDER PREVIEW   (fast, low-res, servable mp4s: crop video + bbox-overlay video)
PLAN ──► RENDER FINAL     (output-res crops from the cached analysis video + source audio, no re-detect)
```

Key consequences:

- **Real-time setting changes**: output dims, zoom margin, max zoom, auto-orient, walk behaviour, preview size → re-`Plan` (instant) → UI redraws overlays, re-plans metrics → debounced `RenderPreview` refreshes the preview videos. Only **analysis height** (and fps, see §4) invalidates the cached detection stream.
- **Final render skips detection**: crops execute against the cached analysis video using the planned records — the expensive part was already paid once.
- **Determinism preserved**: preview, final and CLI all drive the *same* controller code path; replay output is byte-identical to streaming output (asserted by a new equivalence test against corpus CSVs).
- The auto-orient `Probe` becomes a derived view of the same `DetectionStream` — no separate probe decode.

## 3. Passes, progress contract, invalidation

Progress contract (requirement 8): every async pass is a **Task** with `State()`, `Progress() ProgressState`, `Cancel()`, and typed `Wait()`; optionally an `Observer` pushes the same events. Phases are task-scoped, each with its own totals (fixes the current conflation where three different bars share `totalFrames`):

| Pass | API | Phases (Done/Total = frames) | Progress source |
|---|---|---|---|
| Prepare (analysis re-encode) | part of `Analyze` | `prepare` | ffmpeg stderr frame counter |
| Detect | part of `Analyze` | `detect` | decode loop counter |
| Plan | `Plan(...)` — synchronous | none (expected < 100 ms for a 5-min video; benchmark test enforces) | — |
| Render preview | `RenderPreview` | `preview_crop`, `preview_overlay`, `preview_mux` | Go crop loop counter, ffmpeg mux counter |
| Render final | `RenderFinal` | `render_frames`, `render_mux` | Go crop loop counter, ffmpeg mux counter |

Invalidation rules (what a settings change costs):

| Changed setting | Re-run needed | Typical cost |
|---|---|---|
| output W/H, zoom margin, max zoom, auto-orient, fps | `Plan` only | ms |
| preview max width / overlay options | `RenderPreview` only | seconds, low-res |
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
func Analyze(ctx context.Context, req AnalyzeRequest) (*AnalysisTask, error)
func CachedAnalysis(input, tempDir string, analysisHeight int) (*Analysis, error) // cache hit, no Task

type AnalysisTask struct{ /* unexported */ }
func (t *AnalysisTask) State() TaskState              // queued|running|cancelling|completed|failed|cancelled
func (t *AnalysisTask) Progress() ProgressState       // prepare phase, then detect phase
func (t *AnalysisTask) Cancel()
func (t *AnalysisTask) Wait() (*Analysis, error)
func (t *AnalysisTask) PartialStream() DetectionStream // frames detected SO FAR - live while running

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
```

`Plan` is pure and synchronous — the UI can call it on every slider tick (benchmark acceptance: < 100 ms for 5 558 frames; typical ~10 ms). `AutoOrient` is no longer a separate decode: the flip decision is `Plan`'s orientation aggregates. Preview == final == CLI framing are guaranteed identical because all three execute the same replay.

### 6.5 Preview renders (full videos, servable paths — requirement 5)

```go
type PreviewOptions struct {
    MaxWidth    int  `json:"max_width"`     // 0 = 480: preview render width
    CropVideo   bool `json:"crop_video"`    // the framed/cropped preview
    OverlayVideo bool `json:"overlay_video"`// analysis-res frames + drawn boxes
    DrawSubject bool `json:"draw_subject"`  // overlay video: subject + EMA boxes
    DrawCrop    bool `json:"draw_crop"`     // overlay video: crop window
    Observer    *Observer
}

func (p *Plan) RenderPreview(ctx context.Context, req PreviewRequest) (*PreviewTask, error)
// PreviewRequest{ TempDir, FFmpegPath, PreviewOptions, OutputName }

type PreviewTask struct{ ... State/Progress/Cancel ... }
func (t *PreviewTask) Wait() (*PreviewResult, error)

type PreviewResult struct {
    CropVideoPath   string `json:"crop_video_path"`   // under TempDir - servable; "" if not requested
    OverlayVideoPath string `json:"overlay_video_path"`
    Width, Height, Frames int
    FPS float64
    ElapsedSeconds float64
}
```

Mechanics: crops execute from the cached analysis video via the same `cropToBBox`/`fillResize` code as STEP 5 at preview resolution; the overlay video composites boxes (from `Plan.Records()`, drawn in analysis px) onto analysis frames; both mux through ffmpeg `mjpeg` with progress. JPEG scratch lives in `<TempDir>/holocropper-render-<renderID>/frames/` (renderID-isolated, cancel-safe). `Snapshot`/`ContactSheet` (annotated stills + bytes, §6.8) remain for scrubber thumbs alongside the preview videos.

### 6.6 Final render (the explicit output call — requirement 7)

```go
type RenderRequest struct {
    OutputDir  string // required, exists; final <basename>.mp4 lands here
    TempDir    string // required scratch parent
    FFmpegPath string // "" = PATH
    Observer   *Observer
}
func (p *Plan) RenderFinal(ctx context.Context, req RenderRequest) (*RenderTask, error)

type RenderTask struct{ ... State/Progress/Cancel ... }
func (t *RenderTask) Wait() (*Result, error)

// One-shot convenience (no cached analysis, no interactive flow) — wraps the three passes:
func RenderWhole(ctx context.Context, req AnalyzeRequest, settings Settings, renderReq RenderRequest) (*Result, error)
```

`RenderFinal` executes planned crops against the cached analysis video, JPEG sequence into the render scratch dir, ffmpeg mux **with the source audio track** exactly as today, output at the requested WxH, and cleans scratch on completion/cancel (`Result.WorkDir` reported; a `KeepWorkDir` debug flag available). `Crop` (CLI) remains untouched until the CLI is ported to this surface.

### 6.7 Records, observer, progress (the live data plane)

```go
type Rect struct{ X, Y, W, H float64 }
type SpaceRects struct{ BBox, EMA, Crop Rect }
type FrameRecord struct {
    Frame int; Subjects int
    BBox, EMA, Crop Rect      // analysis px
    TargetH, WalkTgt float64; WalkActive bool
    Norm, Out SpaceRects      // precomputed 0..1 and output-px spaces
}

type Phase string // prepare | detect | preview_crop | preview_overlay | preview_mux | render_frames | render_mux
type ProgressState struct{ Task, Phase string; Done, Total int; ElapsedSeconds float64 }

type Observer struct {
    OnPhase    func(ProgressState)
    OnProgress func(ProgressState)  // ≤ 4 Hz per phase
    OnFrames   func([]FrameRecord)  // ≤ 24 records or 250 ms (Detect partials + live renders)
    OnWarning  func(string)         // ffmpeg stderr diagnostics
}
// Callbacks fire from pipeline goroutines; they must not block (app job: EventsEmit / buffered channel).
```

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

- **No queue in the library.** App owns scheduling. Per instance: one async Task at a time (detect or render); `Plan` is pure, concurrent-safe against an immutable `DetectionStream`, unlimited. Multiple instances may run Tasks concurrently (CPU-heavy; serial stays frame-deterministic run-to-run — the property the §13/§14 verification workflow depends on; the app decides).
- Debounce guidance (app-side): slider drag → `Plan` each tick (ms), overlay redraw immediate; `RenderPreview` re-trigger debounced ~300–500 ms and skipped while a preview Task is still running (app cancels the stale one).
- Cache lifecycle: analysis cache keyed by (input path+mtime+size, analysis height); lives under the app's `TempDir`; app may delete the whole dir tree any time. Render scratch (`renderID`-isolated) is removed on completion/cancel unless `KeepWorkDir`.
- Memory ceilings: detection stream ~150 KB/5-min video; `Plan` records ~16 numbers/frame (numbers only); preview JPEGs streaming (one in flight) — per-flow memory stays tens of MB.
- Live ceilings on the binding path: frame batches ≤ 24 recs/250 ms; progress ≤ 4 Hz/phase; no unbounded queues anywhere.

## 9. End-to-end UI flow this surface enables (requirement 9)

```
Schema() ──────────► settings form (per-field errors from Validate())
ListInputs(dir) ───► input picker;  PosterFrame / InputInfo.Path ► player + poster
Analyze(...) ──────► progress bar (prepare, detect); PartialStream ► overlays start immediately
Plan(stream, s) ───► canvas overlays (Norm rects over original video), Metrics badges,
                     orientation decision, walk summary — re-run instantly on any setting change
RenderPreview(...) ► progress bar; CropVideoPath ► "what the crop looks like" video
                     OverlayVideoPath ► "why" video (boxes over analysis frames)
RenderFinal(...) ──► progress bars (render_frames, render_mux); Result.OutputPath ► player
Cancel on any Task ► progress bar stops, scratch cleaned, ErrCancelled surfaces
```

`examples/wails-bindings/` implements exactly this flow as the acceptance reference for the data plane.

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
| B1 | `RenderPreview` Task: crop preview video + bbox-overlay video from cached analysis + Plan, ffmpeg mux with progress, servable paths under TempDir | 8 | A4, A5 | 5-min 1080p source → preview pair in < ~30 s at 480 px; boxes in overlay video match `Plan.Records` |
| B2 | `RenderFinal` Task: crop sequence + audio mux from cached analysis + Plan; `RenderWhole` one-shot; parity test vs `Crop` output (same framing decisions, frame-deterministic) | 5 | A4, A5 | final mp4 frame count/AR correct; parity test green |
| B3 | `Snapshot`/`ContactSheet` from cached analysis + Plan (in-memory compositing) | 3 | A5 | tbaka 16:9 contact sheet ≤ ~4 MB, boxes match `Record.Out` |
| B4 | Progress/Observer plumbing across all Tasks (rate ceilings, batching, warnings from ffmpeg stderr) | 3 | A1–A5 | live progress ≤ 4 Hz, frames ≤ 24/250 ms observed on a corpus run |

### Milestone C — integration proof

| ID | Story | Est | Depends on | Acceptance criteria |
|---|---|---|---|---|
| C1 | `examples/wails-bindings/` reference app implementing the §9 flow end to end (live detect progress, instant re-plan on slider changes, preview videos, final render, cancel) | 8 | A1–A6, B1–B4 | interactive setting changes visibly update overlays/metrics immediately; previews and final render work from the same analysis |
| C2 | CI test suite: golden pipeline test on a tiny fixture video (no corpus), replay equivalence, Task cancel cases, benchmark gate | 5 | C1 | suite green without corpus videos; benchmark gate enforced |
| C3 | Port `cmd/holocropper` CLI onto the new surface (`Analyze → Plan → RenderFinal`), delete the fused one-shot path; env-var CSV kept as internal debug shim inside the detect pass | 3 | B2 | CLI corpus renders match current framing decision streams |

### Deferred (explicitly out of scope)

The Wails UI itself; uploads; HTTP/network of any kind; job persistence/history; supersampling above source height as a default.

## 11. What changed v3 → v4

- **Pass model replaces the fused pipeline** (S12): Prepare/Detect cached → Plan replay → Preview/Final renders. Real-time setting changes are replays, not re-renders.
- `StartRender/Run` monolith replaced by Task-per-pass with `State/Progress/Cancel/Wait` on each.
- Full **preview videos** (crop + bbox overlay) as servable TempDir paths replace JPEG-only live preview as the primary preview plane; `LiveFrame` folded away — during-detect feedback comes from `PartialStream` + the UI drawing over the original video.
- `Probe` folded into the model: the detection stream is the probe; orientation is a `Plan` aggregate.
- Frame-alignment fix (`-fps_mode passthrough`, S13) added as story A4.
- New milestones: A3 (replay engine, the keystone), C3 (CLI port onto the new surface).
- Kept from v3: TempDir ownership, FFmpegPath, Validate-all-errors + Schema, typed errors, wire-friendly records with three coordinate spaces, no queue in the library, the env-var CSV as internal tooling.

## 12. Remaining tunables (defaults; not blocking approval)

- Preview width 480; overlay video at analysis resolution; debounce 300–500 ms (app-side).
- Analysis cache key granularity (mtime-based; content-hash later if needed).
- Frame batch ceilings (24 recs / 250 ms) retuned with the UI.

## 13. Sequencing (starts only after approval)

`A1 → A2 → A3 → A4 → A5 → {A6, B1, B2} → {B3, B4} → C1 → C2 → C3`.
A3 is the architectural keystone: everything in B and the interactive flow stands on the replay engine. First deliverable checkpoint: A5 (interactive re-planning proven on corpus streams, before any video is re-rendered).
