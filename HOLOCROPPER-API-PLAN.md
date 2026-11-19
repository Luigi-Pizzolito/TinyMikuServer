# HoloCropper Embeddable API Plan

Status: **revised after owner review (2026-11-19)** — the first proposal included an HTTP `/v1` layer, job workspaces and CSV artifacts. The owner rejected all of it: the consumer is a **Wails app (Go backend, web UI via bindings)**, so the API is **Go public funcs only**. No HTTP, no network, no job queue or persistence inside holocropper — the calling app tracks jobs. What holocropper must provide is **real-time data for previews, bbox visualisation, settings, inputs and outputs, pulled or pushed live through Go function calls**. No offline CSV requirement.

## 1. What the Wails app needs

- **Settings**: read/set every rendering parameter (output W/H, fps, analysis height, zoom margin, max zoom, auto-orient) — the same set the CLI exposes as flags.
- **Probe**: run the auto-orient probe *before* a render and get the data to preview it (per-sample subject boxes, orientation scores) in real time.
- **Live render data**: phases, progress, and per-frame framing records (subject bbox, smoothed box, applied crop, walk target) streamed while the render runs.
- **Live preview images**: annotated preview frames during the render, and snapshot/contact-sheet images for the UI, retrievable live — as bytes the app can hand straight to Wails bindings, not as files the library decides to write.
- **Result**: applied settings, chosen orientation, dimensions, frame count, output path — structured, not log-scraped.
- **Control**: cancel mid-render (including the ffmpeg children, which today keep running after `Cancel()`).
- **Metrics**: coverage %, edge overshoot, slack, walk range, dropout count — computed in-process from the live record stream, so the UI can show quality badges without any offline analysis pass.

## 2. Current seams to open

Audited surface: `NewHoloCropper()` + fields `Height Width FPS AnalysisHeight ZoomMargin MaxZoom`; `Crop(height, width, fps, input, output) error`; `AutoOrient(...)`; `Progress()`/`Done()` (unguarded counters, `Done()` busy-waits); `Cancel()`; env `HOLOCROPPER_DEBUG_BOXES` → 16-col CSV written inside STEP 4.

- **S1 — no context.** `Crop` is uncancellable via `context.Context`; `resizeTiny` and `convertImagesToMP4` use plain `exec.Command`, so cancelling closes pipeline channels while ffmpeg keeps running.
- **S2 — stdout coupling.** Startup prints, full ffmpeg command lines, and terminal progress bars are hardcoded in the package. A Wails backend has no terminal to own.
- **S3 — env-var telemetry only.** The framing log reaches the outside *only* through a global env var writing a file. The binding API needs typed records delivered live.
- **S4 — unguarded counters.** `currentFrame++` runs in the STEP 4 goroutine; `tickPipeline`/`Progress`/`Done` read it unguarded from another goroutine.
- **S5 — no structured result.** Applied output dims (after auto-orient + even rounding), analysis/source dims, frame count, elapsed time and the actual output path (`outputDir/<basename>.mp4` is convention) are never returned.
- **S6 — probe output too thin.** `OrientationStat` has aggregates only; the UI wants the per-sample bboxes to draw on a poster frame.
- **S7 — nothing live.** No way to ask a running render "what frame are you on / what does the current analysis frame look like".
- **S8 — wire-unfriendly types.** `image.Rectangle`, `[]string` warnings etc. are fine in Go but every value crossing a Wails binding is JSON-serialised for JS; records should use flat numeric structs with precomputed normalized/output-space coordinates so the UI never recomputes geometry.

`cmd/holocropper` (CLI, Makefile) and the env-var debug CSV stay working for the measurement workflows in `HIGH-ASPECT-RATIO-FIXES.md` — but they are **not** part of the API surface.

## 3. API design (in `pkg/holocropper`)

The whole surface below uses plain JSON-serialisable types so any returned struct crosses a Wails binding as-is. `Crop`, `AutoOrient`, `Progress`, `Done`, `Cancel` remain unchanged for the CLI; the new surface is additive.

### 3.1 Settings

```go
// Settings mirrors every CLI flag. Zero = built-in default, as today.
type Settings struct {
    Width, Height  int     // output dims; rounded up to even, as Crop does now
    FPS            float64
    AnalysisHeight int     // 0 = 480, never below Height
    ZoomMargin     float64 // 0 = 1.12
    MaxZoom        float64 // 0 = 2.0
    AutoOrient     bool
}

func (s Settings) Validate() error
func (s Settings) Resolve(srcW, srcH int) Resolved // concrete values + analysis size, for reporting

type RenderRequest struct {
    Input     string // video file (single file; the app owns directory scanning)
    OutputDir string
    Settings  Settings
    Preview   PreviewOptions // live preview behaviour, see 3.4
}
```

### 3.2 Render lifecycle: `StartRender` + `RenderHandle`

The app owns job tracking, so holocropper exposes a **start-and-run handle** rather than a blocking one-shot: `Run` blocks in the app's own goroutine, while the handle answers live queries and cancel from any other goroutine (Wails binding calls come from the Wails runtime goroutine, not the render goroutine).

```go
func (hc *HoloCropper) StartRender(req RenderRequest) (*RenderHandle, error)

type RenderHandle struct{ /* unexported */ }

// Run drives the pipeline; blocks until finished. Call it from the app's job goroutine.
func (h *RenderHandle) Run(ctx context.Context) (*Result, error)
func (h *RenderHandle) Cancel()                 // ctx cancel and Cancel() are equivalent

// live queries (safe from any goroutine while Run is active):
func (h *RenderHandle) Progress() (done, total int)
func (h *RenderHandle) Phase() Phase            // probe | scale | detect | mux
func (h *RenderHandle) LiveFrame() (*LiveFrame, error)     // newest live preview, see 3.4
func (h *RenderHandle) Snapshot(frame int) (*Snapshot, error)
func (h *RenderHandle) Records(from, to int) ([]FrameRecord, error) // window of already-processed frames

// finished-only:
func (h *RenderHandle) ContactSheet() (jpeg []byte, err error)
func (h *RenderHandle) Metrics() (*Metrics, error)
```

`Run` with a nil-Preview `RenderRequest` behaves like today's `Crop` (minus stdout, see 3.5). One `HoloCropper` instance runs one render at a time; the app may run many instances concurrently (same as the CLI's goroutine-per-file fan-out) — documented, not policed.

### 3.3 Frame records: push (Observer) and pull (`Records`)

```go
// Wire-friendly rectangle: top-left + size, float64 so every space uses one type.
type Rect struct{ X, Y, W, H float64 }

// One record per processed frame. All raw fields are analysis-pixel units.
type FrameRecord struct {
    Frame    int
    Subjects int

    BBox  Rect  // master subject bbox (analysis px)
    EMA   Rect  // smoothed subject box (analysis px)
    Crop  Rect  // applied crop rectangle (analysis px)
    TargetH float64
    WalkTgt float64  // 0 = walk leash inactive
    WalkActive bool

    // Precomputed derived spaces, so the UI draws anywhere without math:
    Norm   SpaceRects // BBox/EMA/Crop normalized 0..1 over the analysis frame
    Out    SpaceRects // BBox/EMA/Crop mapped into output-frame pixels
}

type SpaceRects struct{ BBox, EMA, Crop Rect }

type Observer struct {
    OnPhase    func(Phase)
    OnProgress func(done, total int)   // ≤ 4 Hz from the library
    OnFrames   func([]FrameRecord)     // batched: ≤ 24 records or 250 ms
    OnWarning  func(string)            // ffmpeg stderr lines, probe fallbacks...
}
```

Delivery is both ways because Wails apps use both idioms: `Observer` callbacks **push** (app forwards them to `runtime.EventsEmit` for the JS frontend), and `Records(from, to)` / handle queries **pull** (app bound methods). The library keeps a bounded ring of recent records (`Preview.RecordRing`, default 4096) so `Records` answers mid-render without a file.

Batching rules (protect the UI from 24 fps × thousands of frames): frame batches ≤ 24 records or 250 ms; progress ≤ 4 Hz; callbacks are invoked from pipeline goroutines and **must not block** (documented hard requirement; the app's job is `EventsEmit` or a channel send). If a ring overwrites records before they were read, `Result.RecordsDropped` reports the count.

### 3.4 Live preview: images, live, from the running render

Preview data is **generated during the render from the real pipeline** (no separate preview renderer, no offline annotation mode). `PreviewOptions` on the request:

```go
type PreviewOptions struct {
    LiveFrames    bool // enable LiveFrame(); default true
    LiveMaxWidth  int  // analysis-frame preview downscales to this width (default 320)
    LiveRateHz    int  // live frame delivery cap (default 10)
    SampleEvery   int  // annotated snapshot every N frames (0 = auto: ~64 across the video)
    KeepSnapshots int  // snapshots retained for Snapshot()/ContactSheet (default 64)
}

// Newest analysis frame + its framing, JPEG bytes, safe to return across a binding.
type LiveFrame struct {
    Frame    int
    JPEG     []byte  // current analysis frame (downscaled to LiveMaxWidth)
    W, H     int     // JPEG dimensions (normalized coords in the record scale to these)
    Record   FrameRecord
}

// Annotated output-resolution frame: subject box, EMA box, crop rectangle drawn in
// output px, frame number + walk marker legend strip.
type Snapshot struct {
    Frame int
    JPEG  []byte
    W, H  int
    Record FrameRecord
}
```

Mechanics: STEP 1 already holds every analysis RGBA frame (feed `LiveFrame`), and STEP 5/6 hold the actual cropped output frame with the boxes in hand in the same goroutine chain (feed `Snapshot`, composited in memory before JPEG). Nothing is written to disk by the library; the app receives bytes and decides what to persist. `ContactSheet` composes the retained snapshots into one JPEG on request after the render.

### 3.5 Result

```go
type Result struct {
    InputPath, OutputPath string   // OutputPath = the file actually written
    SourceW, SourceH     int
    AnalysisW, AnalysisH int
    OutputW, OutputH     int      // after auto-orient + even rounding
    AppliedSettings      Resolved
    Frames               int
    ElapsedSeconds       float64
    Orientation          *OrientationStat // nil unless the probe ran
    RecordsAvailable     int
    RecordsDropped       int
    Warnings             []string
}
```

Fixes folded in: **S1** (`exec.CommandContext` for `resizeTiny` + `convertImagesToMP4`; pipeline tick selects on `ctx.Done()`; cancel kills children and cleans temp dirs), **S2** (no stdout/progressbar when driven via `StartRender`; the CLI keeps its bar by staying on `Crop` or by using an observer), **S4** (atomic counters; `Progress()`/`Done()` remain for CLI compat), **S5** (everything above in `Result`).

### 3.6 Probe

Extend the auto-orient probe internals into a public report without changing `AutoOrient`'s behaviour:

```go
type ProbeSample struct {
    FrameIdx int
    Subjects int
    BBox     Rect    // analysis px at the probe's analysis size
    Norm     Rect    // 0..1, UI-drawable on a poster frame
    AR       float64
}

type ProbeReport struct {
    OrientationStat             // existing struct embedded
    SourceW, SourceH       int
    AnalysisW, AnalysisH   int
    Samples                []ProbeSample
    ElapsedSeconds         float64
}

func (hc *HoloCropper) Probe(input string, w, h int) (*ProbeReport, error)
```

This is the UI's pre-render preview: probe at the requested resolution, draw sampled bboxes on a poster frame (the app pulls one JPEG via an exported `PosterFrame(input) ([]byte, error)` helper or ffmpeg screenshot at the app level — decision D3 below), show the keep-vs-flip scores. `AutoOrient` delegates to `Probe` internally.

### 3.7 Metrics, in-process and live

Port the measured quantities from the Python tooling (`/tmp/opencode/analysis/worst_slack.py`, `walk_metrics.py`, `auto_metrics.py`) as a pure function over records — no files:

```go
func Analyze(records []FrameRecord, geo Geometry) Metrics

type Metrics struct {
    CoverageMeanPct, CoverageMedianPct float64 // subject height / crop height
    EdgeOvershootFrames  int    // frames where the subject crosses a crop edge
    MinSlackPctPerSide   float64
    WalkRangePx          float64 // horizontal walk range in output px
    DropoutFrames        int
    SceneCuts            int
}
```

`RenderHandle.Metrics()` runs it over the ring at any time (mid-render gives partial metrics, flagged with `Complete bool`). The app can also call `Analyze` itself on pushed batches.

## 4. Coordinate mapping (analysis px → normalized → output px)

All framing numbers are **analysis-frame pixels**: `AH = analysis_height` (even), `AW = AH × srcW/srcH` (even) — the analysis frame preserves the true source aspect ratio.

- **Normalized** (`0..1`): `u = x/AW`, `v = y/AH`. The stable UI space: resolution-independent, comparable across renders and settings, directly drawable on any scaled preview image (the library scales normalized rects to the delivered JPEG's own W×H).
- **Source px**: multiply by `srcW/AW` (uniform — aspect-preserving resize; < 1 px even-rounding error). Used for boxes over source poster frames.
- **Output px**: the applied crop rectangle is aspect-locked (`cropW = cropH × OutputW/OutputH`) and scales to exactly fill the output frame, so for analysis-space point `p`:
  `out = ((p.x - cropX) × OutputW/cropW , (p.y - cropY) × OutputH/cropH)`
  Stage-padding crops (landscape spilling past frame edges) map the same way and legitimately produce negative or > OutputW coordinates — `FrameRecord.Out` carries them raw; the UI clamps for display. Grounded crops keep the crop bottom == output bottom exactly.

`FrameRecord` ships analysis (`BBox/EMA/Crop`), `Norm`, and `Out` all precomputed, so the binding payload answers "draw it anywhere" with zero client-side geometry.

## 5. Concurrency and real-time policy

- **No queue in the library.** The app owns job scheduling. One `HoloCropper` instance = one render at a time; multiple instances may render concurrently (CPU-heavy: detection + 2 ffmpeg passes per render — the app should decide its own parallelism; serial renders also stay frame-deterministic run-to-run, which the verification workflow depends on).
- **Probe is lighter** (≤ 128 decoded frames + one ffmpeg pass) and may run concurrently with a render.
- **Real-time ceilings** (library-side, protect the binding path): frames batches ≤ 24 recs / 250 ms; progress ≤ 4 Hz; `LiveFrame` ≤ `LiveRateHz` (default 10 Hz); rings bounded (`RecordRing` 4096, `KeepSnapshots` 64) — memory stays < ~50 MB per render regardless of video length; drop counters surface in `Result`.
- Pipeline memory unchanged (streaming). Snapshots are composited in memory; JPEGs held only up to `KeepSnapshots`.
- Temp dirs remain `os.MkdirTemp` per render, removed on completion/cancel; **no durable artifacts by default** — outputs go to the app's `OutputDir`, everything else is bytes the app pulls.

## 6. Wails integration shape (reference example for the app side)

```go
type App struct{ ctx context.Context; hc *holocropper.HoloCropper; h *holocropper.RenderHandle }

func (a *App) StartRender(req holocropper.RenderRequest) error {
    h, err := a.hc.StartRender(req)             // observer callbacks emit Wails events
    if err != nil { return err }
    a.h = h
    go func() {
        res, err := h.Run(a.runCtx)             // cancellable via a.runCtx
        if err != nil { wailsruntime.EventsEmit(a.ctx, "failed", err.Error()); return }
        wailsruntime.EventsEmit(a.ctx, "completed", res)
    }()
    return nil
}

// Bound methods the JS frontend calls for live/pull data:
func (a *App) Progress() (int, int)                       { return a.h.Progress() }
func (a *App) LivePreview() (*holocropper.LiveFrame, error){ return a.h.LiveFrame() }
func (a *App) Snapshot(n int) (*holocropper.Snapshot, error){ return a.h.Snapshot(n) }
func (a *App) CancelRender()                               { a.h.Cancel() }

// Push side: an Observer installed on the handle forwards OnFrames/OnProgress/OnPhase
// straight to wailsruntime.EventsEmit for the canvas renderer.
```

`LiveFrame`/`Snapshot` JPEGs arrive as `[]byte` and serialize to base64 across the binding; the frontend paints them on a canvas and draws `Record.Norm`/`Record.Out` rects as overlays.

## 7. Epics, milestones, stories

Estimates: story points (1, 2, 3, 5, 8). Milestone A makes the package embeddable; Milestone B is the live-preview layer the UI needs; Milestone C is the integration proof.

### Milestone A — embedded API core

| ID | Story | Est | Depends on | Acceptance criteria |
|---|---|---|---|---|
| A1 | Context-aware render: `exec.CommandContext` for both ffmpeg calls, ctx-aware pipeline tick, cancel kills children + temp dirs | 5 | — | kill mid-render leaves no temp dirs, no zombie ffmpeg; `Cancel()` unchanged |
| A2 | `Settings`/`Validate`/`Resolve`; `Crop` wraps the new core; zero=default semantics preserved | 2 | — | CLI output unchanged on the corpus videos |
| A3 | `RenderHandle` lifecycle: `StartRender`/`Run`/`Cancel`, atomic counters, phase tracking; `Progress()/Done()` kept for compat | 3 | A1, A2 | `go test -race` clean, incl. a concurrent-query-while-rendering test |
| A4 | `FrameRecord` (wire types, `Norm`, `Out` precomputed) + record ring + batched `Observer` + `Records(from,to)` | 5 | A3 | tbaka 16:9 render: ring answers arbitrary windows mid-render; `Out` boxes match the applied crop within < 1 px on sampled frames |
| A5 | `Probe`/`ProbeReport` with per-sample boxes; `AutoOrient` delegates to it | 3 | — | probe aggregates on the corpus match current `OrientationStat` exactly |
| A6 | `Result` (paths, dims, orientation, warnings from captured ffmpeg stderr, drop counters) | 2 | A1, A3 | `Result.OutputPath` == file on disk for all 4 corpus renders |

### Milestone B — live preview and metrics

| ID | Story | Est | Depends on | Acceptance criteria |
|---|---|---|---|---|
| B1 | `LiveFrame` stream: analysis frame downscale + JPEG, rate-capped, `LiveFrame()` pull + observer push | 5 | A4 | live frame latency < 500 ms behind the pipeline on a 1080p source; ≤ LiveRateHz deliveries |
| B2 | `Snapshot` compositor: subject/EMA/crop boxes + legend drawn on real output frames, sampled, ring-kept; `Snapshot(n)` pull | 8 | A4 | tbaka 16:9 yields ~64 snapshots; drawn boxes match `Record.Out` |
| B3 | `ContactSheet` composer from retained snapshots (tiles + frame numbers + walk markers) | 3 | B2 | single JPEG ≤ ~4 MB for a 5-min video |
| B4 | `Analyze` metrics port (coverage, overshoot, slack, walk range, dropout, cuts) + `RenderHandle.Metrics()` with `Complete` flag | 5 | A4 | reproduces the §14/§14.1 measured numbers (tbaka overshoot 47, slack ≥ 3.5 %/side) from live records |

### Milestone C — integration proof

| ID | Story | Est | Depends on | Acceptance criteria |
|---|---|---|---|---|
| C1 | `examples/wails-bindings/` reference app: the §6 shape end to end (start/probe/progress/live preview/snapshot/cancel) | 5 | A1–A6, B1–B3 | runs against one corpus video in Wails; screenshots show live overlays |
| C2 | Golden end-to-end test with a tiny fixture video asserting `Result`, metrics shape, and ring/`Records` behaviour; no ffmpeg corpus needed | 3 | C1 | suite runs in CI |

### Deferred (explicitly out of scope)

The Wails UI itself; input uploads; anything HTTP/network; job persistence or job history inside holocropper; offline MP4-annotation mode; supersampling above source height.

## 8. What changed from the first proposal (owner-driven)

| First proposal | Now |
|---|---|
| HTTP `/v1` REST + SSE + error envelopes | **Removed** — Go binding funcs + observer callbacks into Wails events |
| `pkg/holocropper/server` package + `cmd/holocropper-server` | **Removed** |
| Job workspace (`job.json`, `framing.jsonl/csv`, frames on disk) | **Removed** — app owns jobs; artifacts are bytes pulled from the handle |
| CSV/JSONL sinks as the telemetry contract | Frame ring + batched callbacks are the contract; the env-var CSV stays only as the existing internal debug tool, not API |
| Server-side queue (1 render, cap 20) | **Removed** — no queue; per-instance single render documented, app decides parallelism |
| SSE batching ceilings | Same numbers, re-expressed as library-side delivery ceilings (≤ 24 recs/250 ms, ≤ 4 Hz progress, ≤ 10 Hz live frames) |
| Metrics as post-render file analysis | `Analyze([]FrameRecord, geo)` in-process, callable mid-render |
| D1–D8 decision list | Settled by owner (2026-11-19); remaining tunables D1–D4 below |

## 9. Remaining decisions (defaults proposed, tunable — not blocking)

- **D1 — defaults**: live preview 320 px wide @ 10 Hz, ~64 snapshots, 4096-record ring. Confirm or retune when the UI shell lands.
- **D2 — push vs pull default**: library provides both; observer batching ceilings above apply to both. Confirm.
- **D3 — poster frame for the probe preview**: library exports `PosterFrame(input) ([]byte, error)` (first-frame JPEG at source resolution) vs the app doing its own ffmpeg screenshot. Recommend library export.
- **D4 — snapshot retention**: retain snapshots in memory only (pulled as bytes) vs optional write-to-`OutputDir` flag. Recommend in-memory only, matching "no offline artifacts".

## 10. Sequencing

`A1 → A2 → A3 → {A4, A5 in parallel} → A6 → {B1, B2, B4} → B3 → {C1, C2}`. Milestone A alone makes the package embeddable for the app's non-preview flows (settings, probe, render, cancel, result). Nothing depends on Wails until C1. Implementation starts with A1 on owner go.
