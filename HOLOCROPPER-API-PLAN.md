# HoloCropper Embeddable API Plan

Status: **scheme v3 — awaiting owner approval. No implementation starts before sign-off.**
Consumer: the Wails app (Go backend, web UI via bindings). Owner rulings so far (2026-11-19): Go binding funcs only — no HTTP, no network; the app owns jobs (no queue/persistence in holocropper); all preview/framing data delivered in real time; **all disk scratch space lives in directories the caller passes in**; the surface must be well-designed and easy to build a UI against.

## 1. What the Wails app needs

- **Settings**: read/set every render parameter (the CLI flag set), get a machine-readable schema for the settings form, and get *all* validation errors at once for per-field UI errors.
- **Inputs**: list the videos in a directory the app chose; get a poster frame JPEG for any input.
- **Probe**: run the auto-orient probe without rendering; get per-sample subject boxes + orientation scores for a pre-render preview.
- **Live render**: phases with per-phase progress, batched framing records, live preview frames — while the render runs.
- **Bbox visualisation**: every box precomputed in analysis px, normalized 0..1, and output px, so the UI draws anywhere with zero geometry math.
- **Preview images**: annotated snapshots sampled during the render + a contact sheet, pulled as bytes.
- **Control**: cancel anytime — must actually stop the ffmpeg children and clean up its scratch dirs (today `Cancel()` does neither).
- **Result + metrics**: applied settings, orientation chosen, dims, output path; coverage/overshoot/slack/walk/dropout badges computed live, no offline pass.
- **Scratch space on the app's terms**: the library needs two temp areas during a render (the downscaled analysis video, the output-size JPEG frame sequence). Today these are `os.MkdirTemp("", ...)` in the system temp with one of them *not even cleaned up on cancel*. New scheme: the app passes `TempDir`; the library creates uniquely-named subdirectories there, uses only those, and removes exactly those on completion/cancel (unless `KeepWorkDir` is set for debugging).

## 2. Current seams to open

Audited from the actual code (`holocropper.go`, `pipeline.go`, `process.go`, `preprocess.go`, `file_io.go`, `box_debug.go`, `auto_orient.go`, CLI):

- **S1 — no context.** `Crop` is uncancellable via `context.Context`; `resizeTiny` and `convertImagesToMP4` use plain `exec.Command`, so cancel closes pipeline channels while ffmpeg keeps running. Pipeline stages never close their own output channels (termination relies on `closePipes` from outside), so mid-render cancel can deadlock or panic on send-to-closed.
- **S2 — stdout coupling.** Startup text, `cmd.String()` of every ffmpeg call, and terminal progress bars (three separate bars: scale, detect, mux) are hardcoded in the package.
- **S3 — env-var telemetry only.** Framing records leave the package only via `HOLOCROPPER_DEBUG_BOXES` → CSV file.
- **S4 — unguarded counters.** `currentFrame++` in the save stage is read unguarded by `tickPipeline`/`Progress()`/`Done()` (data race under `-race`).
- **S5 — no structured result.** Applied dims, analysis dims, source dims, frame count, elapsed, actual output path — never returned.
- **S6 — probe output too thin.** `OrientationStat` has aggregates only, no per-sample bboxes.
- **S7 — nothing live.** No handle, no way to query a running render.
- **S8 — wire-unfriendly types.** `image.Rectangle` and inline `fmt.Errorf("Error: ...")` strings; Wails serializes everything to JSON for JS.
- **S9 — phase progress is conflated.** `totalFrames` is set by `resizeTiny` (source frame count) then overwritten by `startPipeline` (analysis video frame count); the scale bar, detect counter and mux bar each have their own notion of total. The API needs **phase-scoped progress**: `{phase, done, total}` per phase, with the library owning phase transitions.
- **S10 — scratch dirs opaque and leaky.** Two `os.MkdirTemp` dirs the caller never sees; `rmTempDirs` removes only the analysis dir — the JPEG frames dir is removed only on the success path inside `convertImagesToMP4`. Cancel leaves it behind.
- **S11 — ffmpeg path hardcoded.** Both calls exec `"ffmpeg"` from PATH; an embedded app (Wails packaged for distribution) may ship its own binary and must be able to point at it.

`cmd/holocropper` (CLI + Makefile) and the env-var CSV remain untouched as the internal measurement toolchain for `HIGH-ASPECT-RATIO-FIXES.md`; they are **not** API surface.

## 3. The complete API surface (the scheme)

Grouped by the flow the UI will actually implement. All exported types carry `json:"..."` snake_case tags so anything returned crosses a Wails binding predictably. All live-query handle methods are safe to call from the Wails runtime goroutine while `Run` executes in the app's job goroutine.

### 3.1 Settings, validation, schema (the settings form)

```go
type Settings struct {
    Width, Height  int     `json:"width"`   // 0/default = 240; always rounded up to even
    FPS            float64 `json:"fps"`     // 0 = 20
    AnalysisHeight int     `json:"analysis_height"` // 0 = 480, never below Height
    ZoomMargin     float64 `json:"zoom_margin"`     // 0 = 1.12
    MaxZoom        float64 `json:"max_zoom"`        // 0 = 2.0
    AutoOrient     bool    `json:"auto_orient"`
}

// ValidationError names the field, so the UI highlights inputs individually.
type ValidationError struct{ Field, Reason string }
func (s Settings) Validate() []ValidationError   // ALL violations at once, empty = valid

// ParamSpec powers auto-generated forms: defaults, ranges, help text.
type ParamSpec struct {
    Name, Type string   // "integer" | "float" | "bool"
    Default any
    Min, Max any        // nil = unbounded; nil Max on analysis_height means "source height"
    Help string
}
func Schema() []ParamSpec                        // covers every Settings field
```

Ranges (enforced by `Validate`, advertised by `Schema`): `width`/`height` 16–4096; `fps` 1–120; `analysis_height` 0 or ≥ height (values above source height are allowed but `Probe`/`Result` report the supersampling cost); `zoom_margin` 1.0–10.0 (the 1.18 landscape/portrait padding floor still applies); `max_zoom` 1.0–8.0.

`Resolved` (returned inside `Result` and `ProbeReport`) is `Settings` after defaults + even-rounding + probe flip, plus `SourceW/H`, `AnalysisW/H` — what *actually* got used, for display.

### 3.2 Inputs and poster (the input picker)

```go
type InputInfo struct {
    Name string   `json:"name"`   // base name, e.g. "sweet.mp4"
    Path string   `json:"path"`   // absolute path
    SizeBytes int64 `json:"size_bytes"`
    ModTime time.Time `json:"mod_time"`
}
func ListInputs(dir string) ([]InputInfo, error)      // sorted, .mp4 only
func PosterFrame(input string, maxWidth int) (jpeg []byte, err error) // first frame, downscaled
```

No inputs *directory* concept is baked into the library — the app passes any dir/path it owns.

### 3.3 Probe (the pre-render preview)

```go
type ProbeSample struct {
    FrameIdx int    `json:"frame_idx"`
    Subjects int    `json:"subjects"`
    BBox     Rect   `json:"bbox"` // analysis px
    Norm     Rect   `json:"norm"` // 0..1 over the probe analysis frame - draws directly on PosterFrame
    AR       float64 `json:"ar"`
}

type ProbeReport struct {
    OrientationStat                     // existing struct: mean/median/geo AR, keep/flip scores, Flip decision
    SourceW, SourceH   int  `json:"source_w"`
    AnalysisW, AnalysisH int
    Samples            []ProbeSample `json:"samples"`
    ElapsedSeconds     float64       `json:"elapsed_seconds"`
    Settings           Resolved      `json:"settings"` // what a render with these settings would use
}

func (hc *HoloCropper) Probe(ctx context.Context, input string, s Settings) (*ProbeReport, error)
```

`AutoOrient` delegates to `Probe` internally (existing CLI behaviour unchanged). Normalized sample rects scale to the poster JPEG's own dimensions, so the UI overlays them with no math.

### 3.4 Render request and scratch space (the core of the owner ruling)

```go
type RenderRequest struct {
    Input     string   `json:"input"`      // video file the app chose
    OutputDir string   `json:"output_dir"` // must exist; final <basename>.mp4 lands here
    TempDir   string   `json:"temp_dir"`   // REQUIRED for library mode: parent for all scratch
    KeepWorkDir bool   `json:"keep_work_dir"` // debug: leave scratch dirs for inspection
    FFmpegPath string  `json:"ffmpeg_path"` // "" = "ffmpeg" from PATH
    Settings  Settings `json:"settings"`
    Preview   PreviewOptions `json:"preview"`
    Observer  *Observer `json:"-"`         // Go callbacks; nil = pull-only
}

// RenderRequest.Validate() returns []ValidationError, covering input-file-exists,
// output-dir-exists, TempDir (created via MkdirAll if absent), settings fields.

// Scratch layout inside the app's TempDir (all created/removed by the library):
//   <TempDir>/holocropper-<renderID>/analysis/   downscaled analysis video
//   <TempDir>/holocropper-<renderID>/frames/     output-size JPEG sequence
// renderID is a random suffix, so concurrent renders in one TempDir never collide.
// Cleanup: removed on completion AND on cancel, unless KeepWorkDir. Result reports
// WorkDir so the app can inspect kept dirs.
```

`Crop` (CLI path) keeps today's behavior; the new `StartRender` path requires `TempDir` (validation error if empty) — the library never writes outside `OutputDir` and its own scratch subdirs.

### 3.5 Render lifecycle (job control owned by the app)

```go
func (hc *HoloCropper) StartRender(req RenderRequest) (*RenderHandle, error) // validate + prepare, no pipeline yet

type RenderHandle struct{ /* unexported */ }

// Drive it in the app's job goroutine; blocks until terminal:
func (h *RenderHandle) Run(ctx context.Context) (*Result, error)
func (h *RenderHandle) Cancel() // equivalent to cancelling the Run ctx; safe from any goroutine

// Live queries (callable before/during/after Run):
func (h *RenderHandle) State() RenderState   // not_started | running | cancelling | completed | failed | cancelled
func (h *RenderHandle) Progress() ProgressState
func (h *RenderHandle) Records(from, to int) ([]FrameRecord, error)
func (h *RenderHandle) LiveFrame() (*LiveFrame, error)
func (h *RenderHandle) Snapshot(frame int) (*Snapshot, error)

// Terminal-only:
func (h *RenderHandle) ContactSheet() (*ContactSheet, error)
func (h *RenderHandle) Metrics() (*Metrics, error)   // partial mid-render: Metrics.Complete=false
```

```go
type Phase string // "probe" | "scale" | "detect" | "mux"

type ProgressState struct {
    Phase Phase `json:"phase"`
    Done  int   `json:"done"`   // phase-scoped (fixes S9: each phase has its own total)
    Total int   `json:"total"`
    ElapsedSeconds float64 `json:"elapsed_seconds"`
}
```

Pipeline teardown redesign (fixes S1 fully): every stage goroutine closes its own output channel when its input drains or ctx cancels; on cancel, stages **drain-and-discard** so no producer blocks on a dead consumer; both ffmpeg calls move to `exec.CommandContext` with the request's `FFmpegPath`; `Run` waits for all stages before removing scratch; `Cancel()` mid-scale and mid-mux kill ffmpeg children and return `context.Canceled`-based errors. The legacy `Cancel()`/`Progress()`/`Done()` on `HoloCropper` stay for CLI compatibility, marked deprecated.

### 3.6 Frame records: push (Observer) and pull (Records)

```go
type Rect struct{ X, Y, W, H float64 }
type SpaceRects struct{ BBox, EMA, Crop Rect }

type FrameRecord struct {
    Frame    int        `json:"frame"`
    Subjects int        `json:"subjects"`
    BBox  Rect          `json:"bbox"`  // master subject box, analysis px
    EMA   Rect          `json:"ema"`   // smoothed subject box
    Crop  Rect          `json:"crop"`  // applied crop rect
    TargetH   float64   `json:"target_h"`
    WalkTgt   float64   `json:"walk_tgt"`  // 0 = leash inactive
    WalkActive bool     `json:"walk_active"`
    Norm SpaceRects     `json:"norm"`  // all three rects, 0..1 over the analysis frame
    Out  SpaceRects     `json:"out"`   // all three rects mapped into output-frame px
}

type Observer struct {
    OnPhase    func(ProgressState)
    OnProgress func(ProgressState)  // ≤ 4 Hz per phase
    OnFrames   func([]FrameRecord)  // ≤ 24 records or 250 ms per batch
    OnWarning  func(string)         // ffmpeg stderr diagnostics
}
// Observer callbacks fire from pipeline goroutines and must not block
// (app-side job: wailsruntime.EventsEmit or a buffered channel send).
```

Pull side: `Records(from, to)` answers from a bounded ring (`Preview.RecordRing`, default 4096) after clamping to what's retained; `Result.RecordsDropped` counts evictions. Both paths feed the same typed records — the app picks push, pull, or both.

### 3.7 Live preview (images, real time, from the real render)

```go
type PreviewOptions struct {
    LiveFrames    bool `json:"live_frames"`      // enable LiveFrame()
    LiveMaxWidth  int  `json:"live_max_width"`   // 0 = 320
    LiveRateHz    int  `json:"live_rate_hz"`     // 0 = 10
    SampleEvery   int  `json:"sample_every"`     // 0 = auto: ~64 snapshots across the video
    KeepSnapshots int  `json:"keep_snapshots"`   // 0 = 64
    RecordRing    int  `json:"record_ring"`      // 0 = 4096
}

type LiveFrame struct { // current analysis frame + its framing
    Frame int  `json:"frame"`
    W, H  int  `json:"w"`
    JPEG  []byte `json:"jpeg"`
    Record FrameRecord `json:"record"`
}

type Snapshot struct { // actual output frame, boxes + legend composited during render
    Frame int `json:"frame"`
    W, H  int `json:"w"`
    JPEG  []byte `json:"jpeg"`
    Record FrameRecord `json:"record"`
}

type ContactSheet struct {
    W, H int `json:"w"`
    JPEG []byte `json:"jpeg"`
    Tiles []SnapshotRef `json:"tiles"` // frame idx + tile rect within the sheet
}
```

`LiveFrame` comes from the STEP 1 analysis frames (downscaled, JPEG, rate-capped); `Snapshot` from the STEP 5/6 output frames with subject/EMA/crop boxes and a legend composited in memory before encoding. Nothing written to disk (unless `KeepWorkDir` debug); all bytes delivered through bindings.

### 3.8 Result and errors

```go
type Result struct {
    InputPath, OutputPath string  `json:"output_path"` // <OutputDir>/<basename>.mp4 actually written
    WorkDir string  `json:"work_dir"`      // scratch dir (deleted unless KeepWorkDir)
    SourceW, SourceH int   `json:"source_w"`
    AnalysisW, AnalysisH int
    OutputW, OutputH int   `json:"output_w"` // after auto-orient + even rounding
    AppliedSettings Resolved `json:"applied_settings"`
    Frames int             `json:"frames"`
    ElapsedSeconds float64 `json:"elapsed_seconds"`
    Orientation *OrientationStat `json:"orientation,omitempty"`
    RecordsAvailable int  `json:"records_available"`
    RecordsDropped  int   `json:"records_dropped"`
    Warnings []string     `json:"warnings"` // ffmpeg stderr tails, probe fallbacks
}
```

Error taxonomy (UI-mappable): `[]ValidationError` from every `Validate()`; sentinel errors `ErrRenderNotStarted`, `ErrRenderTerminal` (live query on a cancelled/failed render), `ErrArtifactNotReady` (Snapshot/ContactSheet/Metrics too early), `ErrRecordsEvicted` window semantics folded into clamping + `RecordsDropped`; cancel yields `context.Canceled`; ffmpeg failure wrapped as `*FFmpegError{Phase, Command, ExitCode, StderrTail}`. No bare `fmt.Errorf("Error: ...")` strings in the new surface.

### 3.9 Metrics (live quality badges)

```go
func Analyze(records []FrameRecord, geo Geometry) *Metrics
func (h *RenderHandle) Metrics() (*Metrics, error)

type Metrics struct {
    Complete bool `json:"complete"`
    CoverageMeanPct, CoverageMedianPct float64 `json:"coverage_mean_pct"`
    EdgeOvershootFrames int    `json:"edge_overshoot_frames"`
    MinSlackPctPerSide  float64 `json:"min_slack_pct_per_side"`
    WalkRangePx float64 `json:"walk_range_px"`
    DropoutFrames int   `json:"dropout_frames"`
    SceneCuts int       `json:"scene_cuts"`
}
```

Ported from the Python tooling (`/tmp/opencode/analysis/*.py`), pure in-process over records; callable mid-render on the ring.

## 4. Coordinate mapping (analysis px → normalized → output px)

Analysis frame: `AH` = analysis height (even), `AW = AH × srcW/srcH` (even, preserves true source AR).

- **Normalized** `0..1`: `u = x/AW`, `v = y/AH` — the UI-stable space, comparable across renders/settings; scales to any delivered JPEG's own W×H.
- **Source px**: × `srcW/AW` (uniform; < 1 px even-rounding error) — poster-frame overlays.
- **Output px**: crop rect is aspect-locked and scales to fill the output exactly: `out = ((p.x - crop.X) × OutputW/CropW, (p.y - crop.Y) × OutputH/CropH)`. Stage-padding crops (landscape spill) map identically and may yield negative/out-of-bounds values — `Out` carries them raw, the UI clamps for display. Grounded crops: crop bottom == output bottom exactly.

`FrameRecord` ships all three spaces precomputed (`BBox/EMA/Crop`, `Norm`, `Out`) so "draw it anywhere" needs zero client-side geometry.

## 5. Concurrency and real-time ceilings

- **No queue in the library.** App owns scheduling. One `HoloCropper` instance = one render at a time; the app may run multiple instances (documented as CPU-heavy: detection + 2 ffmpeg passes each). Serial renders stay frame-deterministic run-to-run (the property the verification workflow depends on); the app decides if it wants that.
- **Probe** (≤ 128 decoded frames + one ffmpeg pass) may run concurrently with a render.
- **Library-side ceilings** protecting the binding path: frame batches ≤ 24 recs / 250 ms; progress ≤ 4 Hz per phase; `LiveFrame` ≤ `LiveRateHz`; bounded rings (4096 records, 64 snapshots) → memory < ~50 MB per render regardless of video length; eviction counted, never silently lost.
- Pipeline stays streaming; JPEG scratch only inside the app's scratch subdirs; annotated preview composited in memory.

## 6. Wails integration shape (reference, app side)

```go
type App struct{ ctx context.Context; hc *holocropper.HoloCropper; h *holocropper.RenderHandle; runCtx context.Context }

func (a *App) StartRender(req holocropper.RenderRequest) error {   // JS sends settings JSON
    h, err := a.hc.StartRender(req)   // Observer here: OnFrames/OnProgress -> EventsEmit
    if err != nil { return err }
    a.h = h
    a.runCtx, cancel = context.WithCancel(a.ctx)
    go func() {
        res, err := h.Run(a.runCtx)
        if err != nil { wailsruntime.EventsEmit(a.ctx, "failed", err); return }
        wailsruntime.EventsEmit(a.ctx, "completed", res)
    }()
    return nil
}

// Bound methods the JS UI calls:
Schema() []holocropper.ParamSpec
ListInputs(dir string) ([]holocropper.InputInfo, error)
Probe(input string, s holocropper.Settings) (*holocropper.ProbeReport, error)
Progress() holocropper.ProgressState
LivePreview() (*holocropper.LiveFrame, error)
Snapshot(n int) (*holocropper.Snapshot, error)
CancelRender()
```

The UI flow maps 1:1: form from `Schema()` → per-field errors from `Validate()` → input picker from `ListInputs`/`PosterFrame` → pre-render overlay preview from `Probe` + poster → live canvas from `Observer` batches + `LiveFrame` → scrubber from `Snapshot`/`ContactSheet` → badges from `Metrics` → player from `Result.OutputPath`.

## 7. Epics, milestones, stories

Estimates: story points (1,2,3,5,8). No implementation before this scheme is approved.

### Milestone A — embedded API core

| ID | Story | Est | Depends on | Acceptance criteria |
|---|---|---|---|---|
| A1 | Context teardown: per-stage output-channel closing + drain-discard on cancel, `exec.CommandContext` (both calls) with configurable `FFmpegPath`, `Run` waits for stages, scratch fully removed on cancel | 5 | — | kill mid-scale/mid-detect/mid-mux: no zombie ffmpeg, no leaked scratch, `errors.Is(err, context.Canceled)` |
| A2 | `Settings`/`Validate()` (all-errors)/`Schema()`/`Resolved`; `RenderRequest` + validation; **`TempDir` scratch under the caller's dir with renderID isolation + guaranteed cleanup (`KeepWorkDir` opt-out)**; fix S4 (atomic counters) | 5 | — | CLI `Crop` output unchanged on corpus; scratch dirs appear only under the passed `TempDir`; cancel removes them |
| A3 | `RenderHandle` lifecycle: `StartRender`/`Run`/`Cancel`/`State`, phase-scoped `ProgressState`, phase transitions incl. probe/scale/detect/mux | 3 | A1, A2 | concurrent `Progress`/`Records` during render under `-race`; each phase reports its own done/total |
| A4 | `FrameRecord` (wire types, precomputed `Norm`/`Out`) + ring + batched `Observer` + `Records(from,to)` + drop counters | 5 | A3 | tbaka 16:9: arbitrary mid-render windows answered; `Out` boxes match applied crop within < 1 px |
| A5 | `Probe(ctx, input, settings)` + `ProbeReport` with per-sample boxes; `AutoOrient` delegates; `PosterFrame`; `ListInputs` | 3 | A2 | probe aggregates on corpus match current `OrientationStat` exactly |
| A6 | `Result` (+ `WorkDir`, `Resolved`, ffmpeg stderr `Warnings`, `*FFmpegError` typing) | 2 | A1, A3 | `Result.OutputPath` on disk for all 4 corpus renders |

### Milestone B — live preview and metrics

| ID | Story | Est | Depends on | Acceptance criteria |
|---|---|---|---|---|
| B1 | `LiveFrame` stream (STEP 1 frames downscaled + JPEG, `LiveRateHz`-capped, pull + push) | 5 | A4 | < 500 ms behind pipeline on 1080p; rate ceiling holds |
| B2 | `Snapshot` compositor (subject/EMA/crop + legend on real output frames, `SampleEvery`, ring-kept) | 8 | A4 | tbaka 16:9 → ~64 snapshots; drawn boxes match `Record.Out` |
| B3 | `ContactSheet` composer from retained snapshots | 3 | B2 | single JPEG ≤ ~4 MB for a 5-min video |
| B4 | `Analyze` metrics port + `Metrics()` with `Complete` flag | 5 | A4 | reproduces §14/§14.1 numbers (tbaka overshoot 47, slack ≥ 3.5 %/side) from live records |

### Milestone C — integration proof

| ID | Story | Est | Depends on | Acceptance criteria |
|---|---|---|---|---|
| C1 | `examples/wails-bindings/` reference app exercising the §6 flow end to end | 5 | A1–A6, B1–B3 | live overlays visible against one corpus video in Wails |
| C2 | Golden end-to-end test on a tiny fixture video: Result + Metrics + ring/Records behaviour, cancel case included; no corpus needed in CI | 3 | C1 | suite green in CI |

### Deferred (explicitly out of scope)

The Wails UI itself; uploads; anything HTTP/network; job persistence/history; supersampling above source height as a default.

## 8. What changed across scheme versions

- v1 → v2 (owner ruling): HTTP layer, server package, job queue/workspaces/CSV persistence removed; Go bindings + real-time handle model; the env-var CSV stays as internal tooling only.
- v2 → v3 (this revision, per owner notes): **caller-supplied `TempDir`** with renderID isolation and cancel-safe cleanup (new story A2); configurable `FFmpegPath`; phase-scoped `ProgressState` (S9); all-at-once `Validate() []ValidationError` + `Schema()` for form building; typed errors instead of error strings; `ListInputs`/`PosterFrame` helpers; `KeepWorkDir` debug option; `ContactSheet` returns tiles metadata, not just bytes.

## 9. Remaining tunables (defaults proposed; not blocking approval)

- Live preview 320 px @ 10 Hz, ~64 snapshots, 4096-record ring — retune with the UI.
- Push/pull both provided; batching ceilings apply to both.
- Snapshots memory-only by default; `KeepWorkDir` is the only disk debug path.

## 10. Sequencing (starts only after approval)

`A1 → A2 → A3 → {A4, A5} → A6 → {B1, B2, B4} → B3 → {C1, C2}`. Milestone A alone makes the package embeddable for settings/probe/render/cancel/result flows; nothing depends on Wails until C1.
