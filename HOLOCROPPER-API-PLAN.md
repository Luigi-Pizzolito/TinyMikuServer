# HoloCropper Embeddable API Plan

Status: **scheme v5 — approved 2026-11-19. Implementation in progress on branch `feat/holocropper-api` (see the implementation record in §11).**
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
    FitWithBars    bool    `json:"fit_with_bars"`    // fit mode for non-hologram videos (§6.10)
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
func NewPlan(stream DetectionStream, s Settings) (*Plan, error)   // renamed from Plan: Go forbids a type and func of the same name (see §11)

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
type PreviewMux string  // "mjpeg" | "vp9-webm"
type PreviewOptions struct {
    Kind        PreviewKind `json:"kind"`          // which videos to render
    DrawSubject bool        `json:"draw_subject"`  // overlay video: raw subject + EMA boxes
    DrawCrop    bool        `json:"draw_crop"`     // overlay video: applied crop window
    MuxVideo    PreviewMux  `json:"mux_video"`     // "" = mjpeg (.mp4); "vp9-webm" = VP9 + Opus (.webm)
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
    MuxVideo PreviewMux  `json:"mux_video"`     // what both preview files actually hold
    Width, Height int   `json:"width"`         // OUTPUT target res - previews are final framing at final size
    Frames int; FPS float64; ElapsedSeconds float64
    Removed []string   `json:"removed"`        // superseded preview files the sweep deleted
}

// The UI can always ask what previews currently exist and which matches its settings:
type PreviewRef struct {
    Version int; Kind PreviewKind; Path, SettingsHash string; MuxVideo PreviewMux
    CreatedAt time.Time; Width, Height, Frames int; FPS float64
}
func (a *Analysis) Previews() []PreviewRef     // newest first; empty if none
```

**Refresh design decision (requirement 11): versioned files, never overwrite.** Overwriting a file a browser is mid-streaming (Range requests, player cache) is fragile; versioned filenames make refresh an atomic swap: the UI loads `v<N+1>`, players keep playing `v<N>` until the swap, stale URLs are simply gone after the sweep. Each `Analysis` keeps a preview dir `<TempDir>/holocropper-analysis-<key>/previews/`; the version counter is monotonic per kind; a retention sweep keeps the **newest 3 versions** per kind plus whatever `Wait` just returned, and reports deleted paths in `Removed`. `Previews()` + `SettingsHash` let the UI answer "is the newest preview already correct for these settings? skip the render" without bookkeeping.

**Supersede semantics (requirement 12):** `RenderPreview` is last-write-wins. Calling it while a preview Task is running on the same `Analysis` cancels the old Task (its `State()` → `superseded`, its `Wait()` → `*SupersededError` carrying the new Task ID) and starts the new one. The UI never handles "already busy" errors: it just spins on the newest Task until it completes. Preview and final render share one **render slot** per `Analysis` — starting a final render supersedes a running preview and vice versa (one crop-style render at a time per analysis; the `Analyze` pass has its own slot).

Mechanics: crops execute from the cached analysis video via the same `cropToBBox`/`fillResize` code as STEP 5, **at output resolution**, in one frame loop that writes both the crop sequence and the composited overlay sequence (boxes drawn from `Plan.Records()` in output px — same visual as the UI's canvas overlays); then two ffmpeg muxes with progress. JPEG scratch lives in `<TempDir>/holocropper-render-<renderID>/frames/` (renderID-isolated, cancel-safe); finished preview videos move into the analysis preview dir. `Snapshot`/`ContactSheet` (§6.8) remain for scrubber thumbs.

**Preview mux (`Options.MuxVideo`):** `""`/`mjpeg` is the engine default — the rendered JPEG sequence goes into an MP4 the way the CLI and the PC render put it there, video only. `vp9-webm` encodes VP9 video and brings the analyzed input's audio track along as Opus in a **WebM** container, which is the pairing every browser decodes: a preview you can actually watch with sound. The codec spike behind this item measured MJPEG-in-MP4 as playable by WebKitGTK and *refused* by Chromium (`DEMUXER_ERROR_NO_SUPPORTED_STREAMS`), while VP9+Opus in WebM plays in both — so WebM, not VP9-in-MP4, is VP9's home in this engine. It costs a real video encode, so it is opt-in and the default does not change for any existing caller. The extension follows the mux (`…-v<N>.mp4` / `…-v<N>.webm`) because the name is what picks the container for ffmpeg and for every player; version numbering stays per kind and target size *across* muxes, so a mux change lands as the next version rather than over a file a player may be streaming. The choice is recorded in the sidecar (`PreviewRef.MuxVideo`) and in `PreviewResult`, because a UI that skips a render on `SettingsHash` must also want the codec and container the retained file holds. The sidecar likewise carries the engine's ENCODE law at build time (`PreviewRef.Law`, `PreviewLaw`): a file built under an older law - or under none, the pre-field sidecars read as 0 - is never reuse-current, so a build-law change self-heals by re-encoding the preview on its next use instead of serving a retained file players choke on. A source without an audio track muxes the VP9 previews without sound (with a warning).

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

`RenderFinal` executes planned crops against the cached analysis video, JPEG sequence into the render scratch dir, ffmpeg mux **with the source audio track** exactly as today, output at the requested WxH, and cleans scratch on completion/cancel (`Result.WorkDir` reported; a `KeepWorkDir` debug flag available). Output naming is **dimension-tagged** — `<basename>-<WxH>.mp4` — so re-rendering the same input at a different target never silently clobbers a previous production file; the player swap story is the same versioned swap the UI already uses for previews (the app controls its own OutputDir files). It shares the `Analysis` render slot: a running preview is superseded when final starts, and vice versa (`*SupersededError` on the loser). `Crop` (CLI) remains untouched until the CLI is ported to this surface. The PC mux is fixed: mjpeg video, source audio copied through — that pairing *is* the format the UMP PC player takes.

**Frame clock (every output pass).** An `Analysis` holds the **source's** frame clock: prepare re-encodes with `-fps_mode passthrough`, and a `Plan` carries one record per analysis frame. The render clock is `Settings.FPS`. So the frame loop plays the plan out on a schedule — output frame *i* replays analysis frame `floor(i × analysisFPS / renderFPS)` — and the pass writes `seconds × Settings.FPS` frames: a faster render clock repeats frames, a slower one drops them, order never changes, and nothing is blended (a repeated frame is the same pixels, which is what a fixed per-frame budget expects). Without the mapping, a 12 fps source rendered at a 24 fps preset wrote 48 frames = 2 s of picture under a 4 s audio track, and a MikV cut its sound to `sampleRate/fps` per frame — half the song. The clock it maps from is the cache metadata's recorded `fps`; when the decoded analysis video reports something different, or when a pass resamples at all, the pass says so in its `Warnings`. A cache that never recorded a clock keeps the old one-record-per-frame behaviour rather than guessing.

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

Stills need no Task: they composite from the cached analysis video + `Plan` records on demand (fast: one video seek). All bytes cross bindings directly. *(As built: `ComputeMetrics(records []FrameRecord) *Metrics` — records already carry all three coordinate spaces; seek and sheet conventions in §11.)*

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

### 6.10 Fit mode — bars, not crops (non-hologram videos)

Crop mode exists for hologram sources: find the subject, frame it tight. A non-hologram music video is the opposite request - *keep the whole 16:9 or 4:3 picture* and land it in a 1:1 or portrait output. Fit mode is that mode: an **aspect-preserving scale-to-fit resize with no cropping at all**, centered, the remainder filled with black bars (letterbox/pillarbox).

```go
// Entry points: Settings{FitWithBars: true} or the explicit constructor - identical plans.
plan, err := holocropper.NewFitPlan(analysis.Stream(), holocropper.Settings{
    Width: 240, Height: 240, FPS: 24, AutoOrient: true,
})
plan.FitWithBars() // true for either entry point; false for NewPlan defaults
```

Semantics, stated against the crop-mode invariants:

- **Detection still runs.** `Analyze` is untouched; records, normalized/output rects, overlays, stills and metrics all stay populated. Subject boxes map through the fit transform into `Out`-space, so an overlay preview draws them at the right spot *inside* the bars.
- **No framing controller.** `framingDecider`/walk never runs. Every frame's crop is the full analysis frame: `Crop` = analysis frame, `Norm.Crop` = the unit square, `Out.Crop` = the fitted image rect (its complement *is* the bars). `EMA` mirrors `BBox` - fit mode applies no smoothing and the records do not pretend otherwise. `TargetH` = analysis height, `WalkTgt` = 0, walk never active. Dropouts still list the no-subject spans so a timeline can shade absence, even though framing ignores it.
- **Rendering is `fitResizeTo`:** scale to fit, center on black. Matching source/output aspects degenerate it to an exact resize with no bars.
- **Auto-orient becomes pure geometry.** With `AutoOrient` in fit mode the quality measure is the black-bar area: the containment score evaluated on the *source frame's own aspect ratio* equals the output fill fraction, so keep/flip compares `WxH` against `HxW` and picks whichever gets the least bars (`fitOrientationStats`). The decision is identical on subjectless clips - unlike crop-mode auto-orient, which keeps the request when there is no subject evidence. The CLI prints both orientations' bar coverage.
- **Guards stay.** `ErrStreamIncompatible` still fires when the requested output height exceeds the analyzed frame height (the render scales from the analysis video and never upscales past it); `AnalysisHeight` rules are unchanged.
- **Mode is part of the Plan identity.** `SettingsHash` includes the fit flag: a fit preview and a crop preview of the same dimensions never satisfy each other's hash and can never cross-serve from cache.

`Schema` advertises `fit_with_bars`; validation, preview versioning, stills, final render and the Task/observer contract apply unchanged. CLI: `-fit`, optionally with `-auto-orient` for the least-bars orientation. Tests (`fit_test.go`): replay records/transform, the incompatible-stream guard, the least-bars orientation incl. the subjectless contrast with crop mode, `fitResizeTo` pixels, an end-to-end bars render with audio mux through `Analyze → NewFitPlan → RenderFinal`, hash separation, and the debug-CSV shim in fit mode.

### 6.11 MikV output — the render writes the firmware container (D2/D3)

The asset pipeline's last hop is `mikv-converter`. The MikV output pass folds it into the render: `analysis.RenderMikV(ctx, plan, MikVRequest{...})` replays the same Plan against the same cached analysis video and writes the planned frames **straight into a MikV container** - no intermediate mp4. Cropping, fit mode, orientation and walk behavior are untouched; only the output container changes. The pass shares the `Analysis` render slot with previews and the mp4 render (last call supersedes).

Payload encodings ride the v5 header: **byte 13 = video encoding** (`0x00` DEFLATEd RGB565, the legacy default; `0x01` one complete JPEG per frame - MJPEG-in-MikV), **byte 14 = audio encoding** (`0x00` raw PCM at `sampleRate / fps` samples per frame; `0x01` OGG-Opus page segments). Byte 15 stays reserved zero. Legacy files wrote zeros into the padding, so `0/0` is the legacy encoding *by construction* and default output is byte-identical to the original format; readers must reject unknown encoding IDs.

```go
task, err := analysis.RenderMikV(ctx, plan, holocropper.MikVRequest{
    OutputDir: "assets/mp4", TempDir: "./tmp",
    Options: holocropper.MikVOptions{
        VideoJPEG: true, AudioOpus: true, OpusBitrateKbps: 32, // header 0x01/0x01
    },
}) // default Options = the legacy encodings (0x00/0x00, 16000 Hz mono 16-bit)
```

- **The Opus track is one ordinary OGG-Opus stream**, encoded at the packet duration `mikvformat.OGGOpusFrameDurationMs(fps)` picks for the fps and cut at Ogg page boundaries into one chunk per video frame (`SplitOGGOpus`). Packets keep their bytes, order and granule timeline; concatenating the chunks in file order reproduces a byte-valid `.ogg` - the firmware's ogg_pages/opus decoder consumes the chunks sequentially with no codec changes. Uniformity is verified against the container's own page-granule arithmetic (accepting both preroll conventions and the final flush packet).
- **The audio is as long as the picture.** Whatever completes past the last frame window is dropped, not squeezed into the final chunk: raw PCM by the fixed `sampleRate / fps` budget, OGG-Opus by `SplitOGGOpus` refusing packets outside the schedule. A container therefore plays exactly the frames it ships with, and when the source's own track runs past its video track the pass warns that the tail was cut.
- **Integral fps required** (the header stores 8 bits; the PCM/Opus frame budgets derive from it). Raw PCM additionally requires `sampleRate % fps == 0` - PCM chunks are fixed-length whole samples. The compressed pairing the firmware is designed for is **24 fps + 48 kHz** (40 ms packets).
- **The frames come from the same schedule as every other output pass** (§6.6): `analysis.RenderMikV` asks the frame schedule for its frame count first and cuts the audio track to exactly that many chunks, so `AddFrame` never runs short. A 12 fps source exported at the 24 fps pairing carries 4 s of picture *and* the 4 s under it, where the old frame clock gave 2 s of picture against the same track.
- Naming is dimension-tagged like the mp4 render: `<basename>-<WxH>.mikv`, `OutputName` (a validated bare `.mikv`) overrides. Cancel/supersede/scratch semantics are the render contract unchanged: the container is staged in scratch and published in one `moveFile`.
- A source without an audio track renders the PCM path as silence (with a warning, like the converter's); OGG-Opus has nothing to encode and fails loudly.

CLI: `-mikv` plus `-mikv-jpeg`/`-jpeg-quality` and `-mikv-opus`/`-opus-bitrate`. Tests (`mikv_test.go` + `pkg/mikvformat/ogg_opus_test.go`): default-encoding container round-trip (header bytes, RGB565 payloads, the fixed PCM budget covering the whole resampled picture), the JPEG+Opus pairing (per-frame JPEGs, chunks reassembling into a stream ffmpeg itself demuxes), `SplitOGGOpus` against real `ffmpeg -c:a libopus` output on both supported ffmpeg builds, the audio budget clamped to the picture, naming, cancel mid-render, supersede on the shared slot, and the option-validation plane. CI runs two MikV CLI smokes with the header bytes checked raw on the rendered file.

### 6.12 Mode-aware analysis - fit never detects, crop resumes (D4)

The fit transform scales the FULL analysis frame into the output on black bars; it never reads a subject box. Yet until now a fit video still paid for the whole prepare+detect pass before its first preview could render - minutes of subject detection a bars-fit output cannot use. `AnalyzeRequest.Mode` (plan §2/U1) makes the pass honest about what the mode needs:

```go
task, err := holocropper.Analyze(ctx, holocropper.AnalyzeRequest{
    Input: "clip.mp4", TempDir: "./tmp",
    Mode: holocropper.AnalyzeFit, // "" or AnalyzeCrop = prepare + detect, unchanged
}) // fit = prepare + the geometry probe; the detect pass never runs
```

- **The mode is not part of the cache identity.** One cache dir per (input, analysis height), as before. `meta.json` (format v2) records `detected: true|false`, and the pass that runs depends on what the cache already holds:
  - **fit over any complete cache** → immediate hit (geometry exists; fit never needed boxes - a detected cache is reused as-is);
  - **crop over a detected cache** → immediate hit, exactly the pre-mode behavior;
  - **crop over a fit-skipped cache** → RESUME: the cached `analysis.mp4` IS the frame-aligned encode this key produced, so prepare is skipped entirely and only the detect phase runs over it (the test pins the analysis video's mtime across the resume to prove it);
  - **no cache** → the full pass in the requested mode.
- **The fit stream is synthetic but complete**: prepare re-encodes the full frame-aligned analysis video exactly as the crop pass does, and the geometry probe (new phase `probe`) streams ONE subject-less sample per frame, counted through the same vidio reader the detect pass and the render loops use. That keeps the invariant every render pass checks - decoded frames == stream samples == plan records - true by construction, so `NewFitPlan`, previews, the PC render and `RenderMikV` all execute against a fit analysis with zero changes. Only the subject readouts differ: boxes are empty, subjects 0, the whole video shades as one dropout span (honest - nothing was measured), and the auto-orient decision comes from the SOURCE frame's aspect (`fitOrientationStats`), which needs no samples at all.
- **`Analysis.Detected()`** reports which kind of cache you have, so the embedding app can tell "this analysis can frame" from "this analysis is geometry-only" and issue the right mode instead of guessing.
- **`RenderWhole` picks the mode**: fit `Settings` (FitWithBars) with an unset `Mode` analyze in fit mode automatically - a one-shot bars-fit render never detects.
- `ErrStreamIncompatible` semantics are unchanged: a fit render whose output height exceeds the analyzed height still re-Analyzes taller.

Tests (`fit_analysis_test.go`): the fit pass end-to-end (synthetic stream, `meta.detected=false`, analysis video present), the mode validation plane, the resume law (fit → crop re-detects in place with the prepare phase skipped → fit is then an immediate hit), plan parity (a fit plan over the synthetic stream has the same settings hash, resolved geometry, crop rects, output mapping and auto-orient flip as a fit plan over a detected stream of the same fixture), the full render chain against a fit analysis (preview + PC final + MikV), and the `RenderWhole` mode auto-pick.

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

`examples/wails-bindings/` implements exactly this flow as the acceptance reference for the data plane — built per the §9.2 brief, not inside this repo's milestones.

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

### 9.2 C1 implementation brief (the Wails reference app — written for the app agent)

C1 is delivered as this brief, not as code in this repository. The app is built by a separate agent/checkout; `pkg/holocropper` is consumed as-is and **must not be edited** to make the app work — if the surface seems to be missing something, report it instead of patching the library. §9.1 is the UI design guidance; this section is the concrete build order and contract.

**Deliverable:** `examples/wails-bindings/` — a Wails v2 desktop app (Go backend + a JS frontend; pick vanilla JS or Svelte and stay with it) implementing the §9 flow end to end against the shipped surface. It is the acceptance reference for the data plane: if the app can drive the flow with pure `TaskInfo`/`Status`/`OnTask` wiring, the API is done.

**Module wiring:** its own `go.mod` (Go `go 1.24.1` line like the other modules) with
`require github.com/Luigi-Pizzolito/TinyMikuServer/pkg/holocropper` and
`replace github.com/Luigi-Pizzolito/TinyMikuServer/pkg/holocropper => ../../pkg/holocropper`
(holocropper's own go.mod already replaces `pkg/mikvformat` relative to itself — keep that shape, don't re-declare it). Never import the root module. ffmpeg is a runtime dependency: surface `FFmpegPath` in app settings.

**The Go side is a thin binding layer — an `App` struct that owns exactly one `TempDir` (app data dir) and forwards to the library. Bindings to expose (names/signatures are the shipped ones):**

| Binding | Call | Notes |
|---|---|---|
| `Inputs(dir)` | `holocropper.ListInputs(dir)` | input picker. `PosterFrame` (§6.2) is **not shipped**: draw thumbnails client-side, e.g. a `<video preload="metadata">` element per `InputInfo.Path` |
| `SettingsSchema()` | `holocropper.Schema()` | build the form from `ParamSpec`s |
| `ValidateSettings(s)` | `holocropper.Settings.Validate()` | per-field inline errors, all at once |
| `StartAnalyze(input)` | `holocropper.Analyze(ctx, AnalyzeRequest{Input, TempDir, Observer})` | the analyze slot: calling it again supersedes the old task — just call |
| `PlanNow(streamToken, s)` | `AnalysisTask.Plan(s)` while running, `NewPlan(analysis.Stream(), s)` after | the ~10 ms re-plan path; `AnalysisTask.Plan` works over the partial stream |
| `RenderPreview(planID, s)` | `analysis.RenderPreview(ctx, plan, PreviewRequest{Options:{Kind}})` | fire-and-supersede; never check "is a render running" |
| `RenderFinal(planID, s)` | `analysis.RenderFinal(ctx, plan, RenderRequest{OutputDir, TempDir})` | explicit button only |
| `Previews()` | `analysis.Previews()` | `[]PreviewRef` with version + `SettingsHash` match test |
| `CancelTask(id)` / `StatusNow()` | the Task's `Cancel()` / `analysis.Status()` | `StatusNow()` once at startup/hot-reload to restore spinners |
| `ContactSheet(maxWidth)` | `plan.ContactSheet(...)` | timeline thumbnails |

**Event contract (the whole live-data plane is three emissions):** build the `Observer` once in `NewApp` and emit through Wails events — `OnTask` → `events:Emit("task", TaskInfo)` (drives *all* spinners/progress bars, keyed by `TaskInfo.ID` + `Kind`), `OnFrames` → `"frames"` batched `FrameRecord`s (drives overlays during detect), `OnWarning` → `"warning"` toast. `SupersededError.NewTaskID` tells the frontend to move the spinner to the new ID — never show supersede as an error. Task waits run in goroutines; bind `Wait()` results to `"task_complete"`/`"task_error"` events rather than blocking the binding call.

**Build order (each step is shippable and testable on its own):**

1. **Form + picker:** `Schema()` form with live `Validate()` errors; `ListInputs` grid with posters. No processing yet.
2. **Analyze:** start button → spinners purely from `task` events (prepare → detect phases, `Done/Total` bars); canvas overlays begin immediately from `frames` events + `AnalysisTask.Plan` replays.
3. **Tuning loop:** slider `input` → `PlanNow` → redraw overlays/metrics (`plan.Metrics()`, `plan.Walk()`, `plan.DropoutFrames()`, `plan.Orientation()` score bars). Debounce ~400 ms → `Previews()` `SettingsHash` check → `RenderPreview` if stale. Players **preload-then-swap** to the new `-vN` file (§9.1) via a custom `app://` asset handler rooted *only* at `Previews()`/`Result.OutputPath` paths.
4. **Final render:** button → `RenderFinal`, progress bar, `Result.OutputPath` appended to a renders list (dimension-tagged names, so different targets coexist).
5. **Cancel + reconciliation:** per-zone cancel buttons → `CancelTask`; on app start `StatusNow()` restores every spinner. `cancelled`/`superseded` render as quiet states, not errors. `ErrStreamIncompatible` → "re-analyze with analysis height ≥ H" action.

**Acceptance (= the C1 row, checkable in the app):** setting changes visibly move overlays/metrics within a frame of the slider; preview players swap to new version files without blanking; every spinner/progress bar is driven purely by `task` events + `StatusNow()` (no client-side state guessing); cancelling detect, a preview, or the final render each leaves the app usable, scratch cleaned, and no zombie ffmpeg; re-opening the app mid-run restores correct spinners from `Status()` alone.

**Non-goals (restating §10 deferred):** no HTTP server (Wails' own asset handler suffices), no uploads, no job history/persistence, no mikv streaming integration, no supersampling above source height.

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
| C1 | `examples/wails-bindings/` reference app implementing the §9 flow end to end (live detect progress, instant re-plan on slider changes, output-res preview player swaps with spinner overlays, final render, cancel) — **delivered as the §9.2 implementation brief; the app itself is built by an external agent, not in this repo** | 8 | A1–A6, B1–B4 | interactive setting changes visibly update overlays/metrics immediately; preview players swap to new version files; spinners driven purely by `TaskInfo`/`Status`/`OnTask` |
| C2 | CI test suite: golden pipeline test on a tiny fixture video (no corpus), replay equivalence, Task cancel cases, benchmark gate — **delivered: `.github/workflows/ci.yml`** | 5 | C3 (built without C1) | suite green without corpus videos; benchmark gate enforced |
| C3 | Port `cmd/holocropper` CLI onto the new surface (`Analyze → Plan → RenderFinal`), delete the fused one-shot path; env-var CSV kept as internal debug shim inside the detect pass | 3 | B2 | CLI corpus renders match current framing decision streams |

### Milestone D — non-hologram output (fit mode + MikV container)

| ID | Story | Est | Depends on | Acceptance criteria |
|---|---|---|---|---|
| D1 | Fit mode (plan §6.10): whole-frame aspect-preserving resize onto black bars for non-hologram videos - `Settings.FitWithBars` / `NewFitPlan` / `Plan.FitWithBars`, `fitReplay` (the framingDecider never runs), `fitResizeTo` in the render + stills paths, geometry-only auto-orient (`fitOrientationStats`), mode-aware `SettingsHash`, CLI `-fit` | 2 | C3 | `fit_test.go` pins the replay records/transform, the incompatible-stream guard, the least-bars orientation incl. the subjectless contrast with crop mode, `fitResizeTo` pixels, an end-to-end bars render with audio, hash separation and the debug-CSV shim; CI runs a `-fit` + fit-auto-orient smoke |
| D2 | MikV payload encodings in `pkg/mikvformat`: header byte 13 (video: DEFLATE RGB565 / JPEG) and byte 14 (audio: raw PCM / OGG-Opus), byte 15 reserved zero; `SplitOGGOpus`/`JoinOGGChunks` (one OGG-Opus stream cut at page boundaries into per-frame chunks that reassemble byte-exactly for the packets their frame windows keep - see E2); `LoadFromMP4` encoding paths; converter `-jpeg`/`-jpeg-quality`/`-opus`/`-opus-bitrate`; mikvserver `X-Video-Encoding`/`X-Audio-Encoding` response headers | 2 | C3 | `ogg_opus_test.go` + `mikvformat_test.go`: synthetic split/join round-trips, granule-grid uniformity rejections, corruption rejections, real-encoder streams on both supported ffmpeg builds, MikV container round-trips whose reassembled audio ffmpeg itself demuxes; default output byte-identical to the legacy format |
| D3 | MikV output pass (plan §6.11): `Analysis.RenderMikV` writes the planned frames straight into a MikV container - shared `plannedFrames` loop, audio track via ffmpeg (PCM or one libopus OGG stream), `MikVOptions`/`MikVRequest`/`MikVResult`, integral-fps + PCM divisibility validation, render-slot supersede, `<basename>-<WxH>.mikv` naming; CLI `-mikv`/`-mikv-jpeg`/`-mikv-opus` | 2 | D1, D2 | `mikv_test.go`: legacy-default container end-to-end (payloads, fixed PCM budgets, silence tail), the MJPEG+Opus 24 fps/48 kHz pairing, naming + OutputName override, cancel mid-render, supersede vs the mp4 render, validation plane; CI smokes check the rendered header bytes raw |

### Milestone E — output-truth fixes from the embedding app's audit (2026-11-20)

| ID | Story | Est | Depends on | Acceptance criteria |
|---|---|---|---|---|
| E1 | Preview mux plane (plan §6.5): `PreviewOptions.MuxVideo` (`""`/`mjpeg` default, `vp9-webm` = VP9 video + the source's audio as Opus in a WebM container), recorded on `PreviewRef`/`PreviewResult`, gated on the analyzed input actually having an audio track, with the file extension following the mux | 1 | B1 | `TestRenderPreviewMuxChoice`: the default file is unchanged (mjpeg in an MP4, no audio track), `vp9-webm` probes as a WebM container with a vp9 video stream and an opus audio stream, the choice lands as a new version beside the default, both labels appear in `Previews()`, `vp9-mp4` is a validation error, and the naming law covers both extensions |
| E2 | Frame clock (plan §6.6/§6.11): output passes play the Plan on a schedule — `frameSchedule` maps the analysis (source) clock onto `Settings.FPS`, so a pass writes `seconds × FPS` frames; the MikV audio track is cut to exactly that many chunks and `SplitOGGOpus` drops what completes past the last frame window | 2 | B1, B2, D3 | `frameclock_test.go`: schedule arithmetic (repeat/drop, never reorder, unknown clocks stay one-record-per-frame, product length ≈ analysis length), and an end-to-end 12 fps source at a 24 fps plan where previews decode 96 frames measuring ~4 s, the MikV container holds 96 frames, and its joined Opus track measures ~4 s under them |
| E3 | Strict MikV reading in `pkg/mikvformat`: `Load` reports a record cut mid-payload, a missing length field, or an over-long declared payload as `ErrTruncatedRecord` and keeps no frames; a file ending at a record boundary is complete | 1 | D2 | `TestLoadRefusesTruncatedRecords`: every measured cut inside a record refuses as `ErrTruncatedRecord` with a message naming the field and the shortfall, the over-long-length lie is caught before allocating, and 1-record and 2-record boundary files load normally |

### Milestone F — mode-aware analysis (batch 64 U1, 2026-11-21)

| ID | Story | Est | Depends on | Acceptance criteria |
|---|---|---|---|---|
| F1 | `AnalyzeRequest.Mode` (plan §6.12): `crop` (default) = prepare + detect unchanged; `fit` = prepare + geometry probe, detect never runs; the synthetic stream carries one subject-less sample per analysis frame through the same vidio reader, so replay, previews, PC and MikV renders execute against it unchanged; `meta.json` v2 records `detected`; crop over a fit cache RESUMES (prepare skipped, detect only); `Analysis.Detected()`; `RenderWhole` auto-picks fit for fit settings | 2 | A4, A5, D1 | `fit_analysis_test.go`: fit pass end-to-end, mode validation, resume with unchanged analysis-video mtime, fit-plan parity against a detected plan (hash/geometry/crop rects/Out/orientation flip), preview+PC+MikV chain on a fit analysis, `RenderWhole` mode auto-pick |

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

### v5 → implementation record (Milestone A, branch `feat/holocropper-api`)

What changed while building it, and why. Everything here is the shipped shape; the §6 surface above is otherwise as designed.

- **`Plan(stream, s)` became `NewPlan(stream, s)`.** Go forbids a package-level type `Plan` and a function `Plan` in the same scope. The type and all its methods are unchanged; only the constructor is renamed.
- **`PreviewRequest` has no `TempDir`.** Previews are written inside the `Analysis`'s own cache dir, under the `TempDir` that produced that analysis — a preview can never be pointed at a scratch root that does not hold its analysis video. `RenderRequest` keeps its own `TempDir` + `OutputDir` (the final file is the app's, not cache).
- **`Resolved.AnalysisReused` removed.** Cache-hit vs re-analyze is already visible without it: a cached `Analyze` Task completes without ever entering a phase, and `Analysis.Status().Cached` reports the stream.
- **Orientation is computed from the stream** (A5 as designed): `orientationStats` is shared by the CLI probe (`AutoOrient`, ≤128 probe frames) and `NewPlan`, which feeds it the master-bbox aspect ratio of *every* detected frame — strictly more evidence, identical decision machinery. `Plan.Orientation().FramesExamined` is the stream length.
- **Detection and geometry became instance-free** (A3 groundwork). The STEP 4 controller lives in `framingDecider.step()` and `calcVideo` is a thin driver of it, so live and replayed framing share one implementation. The geometry/plant functions (`aspectLockedTargetCalc`, `cropRectFromFramingCalc`, `cropToBBoxCalc`, `fillResizeTo`, `applyAxisPIDCalc`, `applyZoomPIDCalc`) and the detection stages (`convertToMonochrome`, `otsuThreshold`, `applyThreshold`, `connectedComponentLabeling`, `dfs`, `combineBounds`, `expandRectangle`, `totalBoundingBox`) no longer take an `HoloCropper` receiver; the `hc.` methods remain as wrappers where the CLI path and the pre-existing tests call them. One subject-extraction implementation (`subjectsFromBinary`) and one per-frame detector (`detectFrame`) now serve the pipeline, the probe and the detect pass, so a cached stream and a live pipeline cannot disagree frame for frame.
- **A1 landed as sequential ctx-aware passes, not channel teardown.** Prepare/detect (and the render passes in Milestone B) are cancel-safe loops with `exec.CommandContext` + shared `runFFmpegProgress` (stderr `frame=` counters into the Task phase, error tokens to `Observer.OnWarning`), and per-Task scratch names under `TempDir`. The fused CLI pipeline keeps its channel stages until C3 ports the CLI onto this surface. A killed ffmpeg or a cancelled decode loop maps to the `cancelled` Task state.
- **Cache layout and key, as built** (§6.3): `<TempDir>/holocropper-analysis-<key12>/` holding `analysis.mp4` (prepare: `-vf scale=… -sws_flags lanczos+accurate_rnd -fps_mode passthrough -an`), `stream.bin.gz` (10 bytes/frame: 4×`int16` master bbox + `uint16` subject count → ~10 KB per 1 000 frames) and `meta.json`. Key = `sha256(abs input | mtime ns | size | rounded-even analysis height | "pt1")[:12]`. Scratch is task-tagged before the extension (`analysis.tmp-<taskID>.mp4`) so ffmpeg still infers the muxer, and renamed into place; a cancelled or failed pass leaves a directory that is still a consistent **miss** (no `meta.json`, no `stream.bin.gz`).
- **`DetectionStream` also exposes `Complete()` and `FPS()`**; frame alignment is carried in `meta.json` (the `pt1` tag in the cache key) rather than as a stream getter. `AnalyzeRequest` additionally has `KeepWorkDir` (leave scratch behind on failure/cancel for debugging).
- **`ErrStreamIncompatible` is fps-independent.** Detection has no fps dependence (fps feeds only the controller `dt` and the mux), so the sole incompatibility is an effective output height above the analyzed height — checked *after* the auto-orient flip.
- **A3 equivalence is proven on a synthetic fixture**, because the corpus clips are not in the repository: `TestReplayParityVsDebugCSV` compares the framing decisions against the fused pipeline's own debug CSV. As built at A3 it ran `Crop` live and replayed that CSV through `NewPlan`; since C3 the CSV is the committed golden (see the C3 entry below) and the same comparison runs against it. Worst drift 0.005 px over 96 frames, i.e. the CSV's own 2-decimal rounding. The same comparison can be re-run against any corpus clip on demand.
- **Frame batching flushes at 24 records or 250 ms from the batch's start** (not from the previous flush), so a stalled trickle still delivers.
- **Task IDs come from a package-wide counter**, unique across every slot and `Analysis`, so a UI can key spinners by ID alone.
- **A6: `ComputeMetrics(records []FrameRecord) *Metrics` dropped the §6.8 `Geometry` argument** — every `FrameRecord` already carries its analysis-, normalized- and output-space rects, so a second geometry object was redundant. Metric semantics are pinned to the debug-CSV measurements the §14/§14.1 review tables used: coverage = bbox height / crop height; *edge overshoot* = negative X slack (the subject clipped at a crop side edge — the tbaka 16:9 "raw bbox outside crop" column); `MinSlackPctPerSide` = per-frame `min(slackLeft, slackRight)` as a % of crop width, minimum over frames (the §14.1 ≥3.5%/side fit guarantee reads straight off it); `WalkRangePx` = range of the subject bbox center X in output px (the §14 travel column, tbaka 3.24: 244 px of 648). Dropout frames are **excluded** from coverage/slack/travel (an empty bbox is not a framing measurement) and counted in `DropoutFrames`; overshoot frames instead report a negative `MinSlackPctPerSide` and an extra `WorstOvershootPx`. `TestComputeMetrics*` pins this arithmetic synthetically — the corpus clips themselves are not in the repository.
- **B3: the analysis video path rides on the stream.** `DetectionStream.AnalysisVideoPath()` (set when the prepare pass renames `analysis.mp4` into place, and on cache load) flows into `Plan.videoPath` at `NewPlan` time, so `Snapshot`/`ContactSheet` stay `*Plan` methods as designed and still work for Plans replayed from an `AnalysisTask.PartialStream()`. Plans built from synthetic streams have no video and return a typed "re-plan from an Analyze result" error.
- **B3 frame seeking bypasses vidio's `ReadFrame`/`ReadFrames`** — they pass `-vsync 0`, which ffmpeg 8+ removed outright (the spawn dies and vidio surfaces a bare EOF). The shipped seek is the library's own single ffmpeg pass: `select='eq(n\,i)+…'` → `rawvideo rgba` pipe with `-fps_mode passthrough`, one pass per still request. (vidio's own frame decode elsewhere uses `video.Read`, unaffected; the same `-vsync` bug sits latent in vidio 1.5.1 should any code call `ReadFrame`.)
- **B3 stills conventions:** contact sheets sample with a constant stride, ≤36 tiles, ≤6 columns, first frame always included and the last sample forced onto the final frame, so `TileFrames` is a regular scrubber (a tile click maps to a player seek). Each tile carries a 3 px status bar (dropout red > scene-cut amber > walk-active green > neutral gray) and a caption rendered from a built-in 3×5 bitmap digit face — deterministic bytes, no font dependency. Tiles floor at 80 px wide (a narrower `maxWidth` is honored only down to that floor). Snapshots annotate at full output resolution and then scale, so capped boxes are exactly the full-res framing shrunk, never re-derived. JPEG q85 sheets / q90 snapshots; the §6.8 ~4 MB sheet budget is asserted in `TestContactSheetFixture`.
- **B4: the warnings plane reaches every Task.** Every ffmpeg diagnostic token now travels three ways: live via `Observer.OnWarning`, snapshot via `Task.Warnings()` (queryable mid-run, after completion, *and* after cancel/failure — the app can surface diagnostics for a Task it never waited on), and embedded in the finished results (`Result.Warnings` on the final render, `PreviewResult.Warnings` on previews). Hard errors were already never silent: `TaskInfo.Err` + the typed error from `Wait()` (`*FFmpegError` carries the command, exit code and a stderr tail). The stderr classifier fires on ffmpeg's **marker words alone** (`Error`/`error`/`Warning`/`failed`…, trimmed of glued punctuation) — word-substring matching fired on mux summary lines, which echo every output file path, so a work directory merely *named* with "error"/"Warnings" produced phantom warnings. The per-Task warning list is capped at the newest 200 entries.
- **C3: `RenderRequest.OutputName` — the fixed-name channel.** A validated bare `.mp4` name (no path separators, never outside `OutputDir`) overrides the dimension-tagged default. The API default is unchanged; the override exists so a caller can keep one stable production filename, which is how the CLI holds its legacy `<basename>.mp4` naming including overwrite-on-re-render (`moveFile` renames the staged render onto that name).
- **C3: `HOLOCROPPER_DEBUG_BOXES` rides the replay.** `NewPlan` re-emits the framing CSV the corpus tooling reads — same 16 columns, one line per frame, values straight from the records it returns — so the debug hook survived the pipeline's removal. `TestPlanDebugCSVMatchesRecords` pins the shim against the records; the parity test double-checks shim vs golden.
- **C3: the fused pipeline is deleted** (`pipeline.go`, `preprocess.go`, the six `STEP` stage goroutines, `Crop`/`Progress`/`Done`/`Cancel`, the mask structs, the CLI-path temp dirs and its own `ffmpeg` invocations, and the auto-orient probe decode with its private copy of the detection stages). `HoloCropper` survives as the framing-parameters holder (`Height`, `ZoomMargin`, `MaxZoom`, `srcH` → `framingParams()`); its geometry/PID wrapper methods are gone and the framing tests call the instance-free `*Calc` functions, so the corpus-calibrated behaviour is now pinned directly on the functions the API actually runs. The probe's degenerate-mask guard moved to the AR level (`degenerateMasterBBox` in the orientation aggregates): a frame whose thresholding collapses to a full-frame box still cannot skew the flip decision. `cmd/holocropper` is a thin front end over `Analyze → NewPlan → RenderFinal` with the same flags, messages, per-file goroutines, Ctrl-C-cancels-everything, progress bar and output naming, plus `-temp-dir` and `-ffmpeg-path`.
- **C3: parity is golden-based, not live-fused.** The fused CSV was generated from the fused path itself and committed as `pkg/holocropper/testdata/holocropper-parity-fixture-240-*.csv` (C2 later made goldens per-ffmpeg-major, so today's file for a 9.x build is `...-240-ffmpeg9.csv`); `TestReplayParityVsDebugCSV` now checks three layers against it — `Analyze`'s per-frame subject boxes and subject counts, the Plan replay's crop/target/walk/EMA numbers (worst crop drift 0.005 px over 96 frames = the CSV's own 2-decimal rounding), and the shim CSV the replay writes. `HOLOCROPPER_REGENERATE_GOLDEN=1` rebuilds the golden from the API chain when an ffmpeg upgrade shifts decode drift. Independent end-to-end check: running the shipped CLI over the same fixture at the golden's fps writes a decision stream **byte-identical** to the golden.
- **C2: CI suite without the corpus** (`.github/workflows/ci.yml`, two jobs). `holocropper`: pinned `ubuntu-24.04` + apt ffmpeg, `go-version-file` from the module's go.mod, gofmt gate, `go vet ./...`, CLI build, `go test -count=1 -race -timeout 600s ./...` — the whole suite runs on lavfi-synthesized fixtures and the golden CSVs (the corpus lives in untracked `assets/`/`test/`, so a CI checkout has no video to read by construction) — plus a **CLI backwards-compatibility smoke**: fixture rendered with legacy flags, exactly one output named `<basename>.mp4`, ffprobe-dims checked at 240x240, re-render at 420x240 still exactly one legacy-named file. `rest-of-repo`: build + vet of the root module and `pkg/mikvformat`/`pkg/mikvserver`. The benchmark gate (A5) and the cancel/Task cases ride the `-race` run. **`requireFFmpeg` got a strict gate:** with `HOLOCROPPER_REQUIRE_FFMPEG` set (what CI exports) a missing ffmpeg *fails* the run instead of skipping — the suite can never go green by silently skipping every fixture test. Verified both ways locally: ffmpeg absent + gate set → `FAIL` with the gate message; gate unset → skip as before.
- **C2, what the first CI run caught (the suite is ffmpeg-version-sensitive).** The first green attempt failed on exactly two tests, both pinned to one ffmpeg build: the golden parity test (CI's Ubuntu ffmpeg 6.1 vs the 9.x desktop that recorded the golden drifts master bboxes ~1 px — the decision stream follows the encode/decode round trip and the analysis scaler, which are ffmpeg-build artifacts) and the warnings-on-failure test (it failed ffmpeg on purpose with the `-vsync` option ffmpeg 8 removed — Ubuntu 6.1 still accepts it). Fixes: **goldens are tagged per ffmpeg major version** (`holocropper-parity-fixture-240-ffmpeg<N>.csv`, chosen from `ffmpeg -version`); a missing golden for the running ffmpeg **fails with the exact regeneration command** instead of weakening the comparison — `HOLOCROPPER_REGENERATE_GOLDEN=1` writes the correctly-named file, review the diff, commit it. The CI job pins `ubuntu-24.04` so its ffmpeg matches the committed ffmpeg6 golden (generated in an Ubuntu 24.04 container). The failure-injection test now fails on **any** ffmpeg build (unreadable input). The drift turns out to be *build*-level, not major-level: the 6.1.1 container and the 9.0.2 desktop produced byte-identical goldens, while ubuntu-latest's 8.0.1-3ubuntu2 was 1 px off — so a golden always records the exact pinned CI build, and the workflow gained a `workflow_dispatch` input `regenerate_golden` that re-records the goldens on the runner itself and uploads them as an artifact. Net effect: version drift is an explicit, one-command maintenance event — never a silent skip and never a silent tolerance widening.

- **C2, what the second CI run caught (the drift is CPU asm dispatch, not ffmpeg-version).** The pinned `ubuntu-24.04` job ran the *exact* apt build that recorded the golden (`7:6.1.1-3ubuntu5`), yet frame 15's master bbox was still 1 px off between the GH runner and the container/desktop pair that produced byte-identical goldens. Same build, different CPU, different pixels: libx264's subpel/mode decisions (and swscale's lanczos rounding) have CPU-asm paths that diverge per CPU flag set. The first fix pass hardened only the library's prepare pass - the drift survived on the next run, which exposed the second producer: the **lavfi fixture encode inside the test itself** is x264 output too. Every encode in the parity chain is now deterministic: prepare scales with `-sws_flags lanczos+accurate_rnd+bitexact` and encodes with `-x264-params cpu-independent=1`, and `buildFixture` adds `-x264-params cpu-independent=1` + `-sws_flags accurate_rnd+bitexact`. On the desktop those deterministic decisions produce the *same* bitstream it always did (goldens regenerate byte-identical), so the committed goldens are the CPU-independent decision stream, and any CPU running the pipeline lands on the same one. Goldens stay per-ffmpeg-major: that tag is a version guard, not a CPU guard.
- **D1: fit mode lands (§6.10), the non-hologram path.** `Settings.FitWithBars` (+ `Schema`'s `fit_with_bars`), the explicit `NewFitPlan`, and `Plan.FitWithBars()`. The replay swaps the framing controller for a constant fit transform (`fitReplay`): every frame's crop is the full analysis frame (`Crop` = analysis frame, `Norm.Crop` = unit square, `Out.Crop` = the fitted image rect - its complement *is* the bars), `EMA` mirrors the raw detection box (no smoothing is claimed), `TargetH` = analysis height, walk never active, dropouts stay the no-subject spans. Subject rects map through scale-to-fit/center so overlays still land inside the bars. The shared frame loop and `composeFrame` branch to `fitResizeTo` (aspect-preserving Lanczos scale centered on black; exact resize when aspects match). Fit auto-orient is pure geometry (`fitOrientationStats`): the containment score on the SOURCE aspect equals bar coverage, so keep/flip picks the least-bars orientation and decides identically on subjectless clips - the contrast with crop mode (which keeps the request there) is pinned in `TestFitAutoOrientPicksLeastBars`. `planSettingsHash` gained `fit=%d`: fit and crop previews of identical dimensions can never cross-serve. `ErrStreamIncompatible` (output height above analyzed height) applies unchanged. CLI `-fit` (+ fit-flavored `-auto-orient` print: `bar coverage 0.321 at 240x420 vs 0.984 at 420x240 -> FLIPPING to 420x240` in the smoke run). The `HOLOCROPPER_DEBUG_BOXES` shim reports what the fit path actually applies (full-frame crop, EMA = bbox, no leash).
- **D2: MikV payload encodings (the v5 header).** Header byte 13 records the video encoding (`0x00` DEFLATEd RGB565, `0x01` JPEG-per-frame), byte 14 the audio encoding (`0x00` raw PCM, `0x01` OGG-Opus page segments); byte 15 stays reserved zero. Legacy writers left the padding zero, so every pre-existing file decodes as the legacy encodings *by construction* and default output is byte-identical to the original format; `readHeader` rejects unknown IDs before any frame is touched, and every `MikVFrame` carries its `VideoEncoding`/`AudioEncoding` stamp. `MikVHeader` became a template with per-call copies in Load/Save (a latent data race a `-race` concurrent-file-IO test caught). The Opus path (`ogg_opus.go`) is pure Go: `SplitOGGOpus` cuts ONE ordinary OGG-Opus stream into one chunk per video frame by rewriting only the page boundaries - packets keep their bytes, order and granule timeline, OpusHead/OpusTags pass through untouched, so `JoinOGGChunks` reproduces a byte-valid `.ogg` the firmware's ogg_pages/opus decoder plays with zero codec changes. Uniformity is verified from the container's own page-granule arithmetic against ffmpeg's oggenc granule law (`pts + duration + rescaled initial_padding`): the grid accepts both preroll conventions (ffmpeg cancels the libopus delay out of the pts; strict encoders bake it in) and the final flush packet that lands short. Converter flags `-jpeg`/`-jpeg-quality`/`-opus`/`-opus-bitrate` (the `-opus` path forces the 48000 Hz granule clock the frame-window arithmetic needs); mikvserver announces the encodings via `X-Video-Encoding`/`X-Audio-Encoding`. Verified against real `ffmpeg -c:a libopus` output on both supported builds (6.1.1 and 9.0.2): 51 packets at 40 ms over 2 s split, re-joined, and accepted by ffmpeg's own demuxer.
- **D2 follow-up, what the pinned runner's golden record proved.** The deterministic encodes (C2 follow-up) made the parity chain CPU-independent *between the container and the desktop* - but the pinned runner still produced its own stable decision stream: four CI runs failed at the identical frame-15 bbox, and the `regenerate_golden` dispatch recorded the *same* values. The pinned runner is deterministic run-to-run, and the golden is the runner's contract, so the ffmpeg6 golden was re-recorded on the runner (dispatch run 44655024309) - exactly the escape hatch the workflow documents. The container/desktop pair reproduces the ffmpeg9 golden and keeps passing; the ffmpeg6 golden records what the pinned runner produces. Machine-independence is thus held where it matters (CI determinism + an explicit one-command re-record when the pinned image changes), not as an untestable promise about every CPU on earth.
- **D3: the MikV output pass (RenderMikV).** The frame loop split into `plannedFrames` (decode the analysis video, apply the Plan's framing, hand each finished frame to a callback) with `runCropLoop` becoming the JPEG-file callback - previews, the mp4 render and the MikV render now share one framing loop byte for byte. `Analysis.RenderMikV` stages the audio track first (raw PCM to scratch via ffmpeg, or ONE libopus OGG stream at `OGGOpusFrameDurationMs(fps)` cut by `SplitOGGOpus`), feeds the planned frames straight into `AddFrame` (Go-side JPEG encoding for `0x01`, RGB565 little-endian packing identical to `LoadFromMP4` for the DEFLATE path), and publishes `<basename>-<WxH>.mikv` from scratch with the usual `moveFile`. Validation is container-first: integral fps (the header's 8-bit field), `sampleRate % fps == 0` for PCM (chunks are whole fixed-length samples; the error names the fps values a rate divides), OGG-Opus pinned to 48000 Hz with an fps the Opus packet table supports, channels 1-2, bits 8/16, no-audio sources = silence-with-a-warning for PCM and a loud error for Opus. The pass shares the render slot (supersede like the mp4 render), the cancel contract (nothing published, scratch cleaned), and the Observer phases (`mikv_audio`/`mikv_frames`/`mikv_write`). CLI: `-mikv` with `-mikv-jpeg`/`-jpeg-quality`/`-mikv-opus`/`-opus-bitrate`. CI smokes: legacy-encoding container (header 0/0, 16000 Hz) and the MJPEG+Opus pairing (header 1/1, 48000 Hz, ≥96 JPEG SOI markers, Ogg pages present); `make test-mikv` now runs the `pkg/mikvformat` suite too.
- **E1: the preview mux plane.** `PreviewOptions.MuxVideo` (`PreviewMux`: `""`/`mjpeg` default, `vp9-webm`), validated on `PreviewRequest`, resolved once per pass, and recorded per file (`PreviewRef.MuxVideo` in the sidecar, `PreviewResult.MuxVideo`). `muxFrameSequence` gained the mux parameter: mjpeg keeps the CLI's exact `-c:v mjpeg -q:v 6` video-only MP4 mux, `vp9-webm` runs `-c:v libvpx-vp9 -pix_fmt yuv420p -crf 30 -b:v 0 -deadline good -cpu-used 4 -g min(fps, 24)` with `-c:a libopus -b:a 128k` into a `.webm`, so the preview pane plays the clip with sound in either webview. The `-g` is decode law, not tuning: Chromium's VP9 decoder refuses some engine previews outright (`MEDIA_ERR_DECODE` right after `loadedmetadata` while libvpx/ffvp9 read the same bytes), and the refusal follows the keyframe interval — measured on the QA rig's 96-frame preview content, GOPs of 24 frames or less play, GOPs of 48 or more do not — so the encode caps its key interval at one second of frames, ceilinged at `previewGOPMaxFrames` (`previewGOPFrames`). WebM rather than VP9-in-MP4 is the choice the embedding app's codec spike measured (MJPEG-in-MP4: WebKitGTK plays it, Chromium refuses the stream outright; VP9+Opus WebM: both play it), and the extension is part of the law — `previewFileExt`/`previewFileName` land `.mp4` or `.webm`, ffmpeg picks the muxer from that name, and `readPreviewRefs` reads both sidecar shapes. The audio source is resolved through `sourceHasAudio` against the analyzed input, and a source without a track muxes the VP9 previews silently (with a warning) rather than failing the preview. `RenderFinal` now names its mux explicitly (`PreviewMuxMJPEG`): the PC product's pairing is not a tunable. Naming, versioning and the retention sweep are otherwise untouched — version numbering stays per kind and target size *across* muxes, so a mux change lands as the next version rather than over a file a player may be streaming.
- **E2: the frame clock, the defect the embedding app measured.** The analysis cache preserves the source's frame rate (`-fps_mode passthrough`) while a Plan replays at `Settings.FPS`, and every output pass used to emit exactly one frame per Plan record. On a 12 fps source at the 24 fps preset that was 48 frames: 2.000 s of picture muxed under a copied 4.001 s audio track, and the MikV export — which budgets `sampleRate/fps` per frame — shipped half the song. `frameSchedule`/`frameScheduleFor` (renders.go) is now the answer every pass asks: output frame *i* replays analysis frame `floor(i × analysisFPS / renderFPS)`, `Frames = round(records × renderFPS / analysisFPS)`, identity when the clocks match or the cache never recorded one. `plannedFrames` takes the schedule (and still decodes every analysis frame, so the decode-vs-record count check that catches a stale cache keeps working); `RenderMikV` sizes its audio chunks from `sched.Frames`, so `AddFrame` can never run short. Repeats are exact frame replays — no interpolation, no reordering — which is what a fixed per-frame PCM/Opus budget expects. `SplitOGGOpus`'s other half: a packet completing past the last frame window is dropped instead of squeezed into the final chunk, so a container's sound is exactly as long as its frames (the raw-PCM path already cut to that budget; `warnAudioCutToPicture` states it when the source's track outlived its video). `buildFixtureAt` makes the fixture's *source* rate a test variable, which is what let this be pinned end to end rather than arithmetically.
- **E3: strict MikV reading.** `Load` used to return `nil` when a frame read failed (it returned the outer `err`, which was `nil` at that point) and kept the frames that survived — a half-written file reported itself as a shorter valid container. `readFrame` now distinguishes a clean `io.EOF` at a record boundary (complete file) from every other shortfall, reports each as `ErrTruncatedRecord` with the field and the byte arithmetic, and `Load` drops what it read before the cut. The length fields are checked against the bytes actually left in the file before allocating, so a corrupt length can no longer ask for a 1 GiB buffer. `errors.Is(err, mikvformat.ErrTruncatedRecord)` is the consumer-facing contract; the format spec and README now state the same rule for browser-side readers.

## 12. Remaining tunables (defaults; not blocking approval)

- Preview retention: newest 3 versions per kind (raise if the UI wants an A/B compare of consecutive settings).
- Debounce 300–500 ms (app-side); frame batch ceilings (24 recs / 250 ms) retuned with the UI.
- Analysis cache key granularity (mtime-based; content-hash later if needed).

## 13. Sequencing (starts only after approval)

`A1 → A2 → A3 → A4 → A5 → {A6, B1, B2} → {B3, B4} → C1 → C2 → C3`.
A3 is the architectural keystone: everything in B and the interactive flow stands on the replay engine. First deliverable checkpoint: A5 (interactive re-planning proven on corpus streams, before any video is re-rendered).

**Status 2026-11-20:** the A5 checkpoint is reached on `feat/holocropper-api` — A3 (replay engine + CLI delegation), A4 (analysis pass, cache, partial stream) and A5 (`NewPlan` with records/orientation/walk/dropouts and the < 100 ms benchmark gate) are implemented and tested, A2 shipped with them and A1 delivered as scoped in the implementation record. Milestone B is complete: B1/B2 (output-resolution previews with versioned files + supersede, final render with CLI parity), A6 (framing metrics report), B3 (snapshots + contact sheets) and B4 (unified Task/status plane with the warnings feed on every pass). **C3 is complete: the CLI runs `Analyze → NewPlan → RenderFinal`, the fused one-shot pipeline is deleted, and framing parity is held by the golden decision CSV** (see the C3 records above). **C2 is complete: the corpus-free CI suite ships in `.github/workflows/ci.yml`** — the full `-race` suite on lavfi fixtures + golden CSV, benchmark gate and cancel cases enforced, CLI legacy-naming smoke, and a strict `HOLOCROPPER_REQUIRE_FFMPEG` gate so fixture tests can never pass by skipping (see the C2 record). **C1 is delivered as guidelines: §9.2 is the implementable brief for the Wails reference app, which a separate agent builds outside this story.** Milestone C is closed on this repo's side. **The C2 follow-up made the suite machine-independent:** the golden drift the pinned runner still showed turned out to be CPU asm dispatch, and every encode in the parity chain - the prepare pass (`-sws_flags lanczos+accurate_rnd+bitexact` + `-x264-params cpu-independent=1`) and the test fixture build alike - now runs deterministic, so analysis pixels - and the whole decision stream - are identical across CPUs. **Milestone D is open and D1 is complete: fit mode (§6.10) renders non-hologram videos as whole-frame scale-to-fit onto black bars**, reachable via `NewFitPlan`/`Settings.FitWithBars` and the CLI `-fit`, with geometry-only auto-orient that picks the least-bars orientation even on subjectless clips. **D2 and D3 are complete: the MikV container is a first-class render output.** `pkg/mikvformat` records the payload encodings in the v5 header (byte 13 video: DEFLATE RGB565 / JPEG; byte 14 audio: raw PCM / OGG-Opus; byte 15 reserved zero - legacy files land on the legacy encodings by construction, default output byte-identical to the original format) and cuts ONE ordinary OGG-Opus stream into per-frame page-segment chunks that reassemble byte-exactly for the packets their frame windows keep (E2 dropped the tail that outlived the picture), verified against real libopus output on both supported ffmpeg builds. `Analysis.RenderMikV` (plan §6.11) writes the planned frames straight into the container, the CLI reaches it with `-mikv` (+ `-mikv-jpeg`/`-mikv-opus`), and CI smokes check the rendered header bytes raw. The firmware pairing for compressed output is **24 fps + 48 kHz OGG-Opus** (40 ms packets); the firmware's existing ogg_pages/opus decoder plays the chunks with no codec change. **The ffmpeg6 golden was re-recorded on the pinned runner** via the documented `regenerate_golden` escape hatch (dispatch run 44655024309): the runner is deterministic run-to-run, and the golden records the runner's own decision stream (see the D2 follow-up record). **Milestone E is complete (2026-11-20): the three output-truth fixes the embedding app's QA walk found.** E1 gives previews an opt-in `vp9-webm` mux — VP9 video plus the source's audio as Opus in a WebM file, `.webm` extension and all, labelled per retained file — while the mjpeg default and the PC render's mux are unchanged. E2 makes every output pass follow the **plan's** frame clock instead of the analysis cache's (`frameSchedule`: repeat/drop nearest-frame, `seconds × Settings.FPS` frames), and cuts a MikV's audio to exactly the frames it ships with; a 12 fps source at a 24 fps plan now renders 96 frames and 4 s of sound where it used to render 48 frames under that same 4 s track. E3 makes `mikvformat.Load` refuse a frame record cut mid-payload as `ErrTruncatedRecord` rather than report the surviving frames as a shorter valid file. **Milestone F is complete (2026-11-21): U1 mode-aware analysis (F1, plan §6.12).** A fit-mode `Analyze` no longer pays for subject detection at all — prepare + the geometry probe, a synthetic one-sample-per-frame stream, `meta.detected` recording what the cache holds, and a crop `Analyze` over a fit cache resuming in place with the detect phase only. The embedding app's MV/fit mode can now plan, preview and render in seconds instead of after a full detect pass.
