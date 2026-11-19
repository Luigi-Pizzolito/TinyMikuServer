# HoloCropper Embeddable API Plan

Status: **proposed** — plan only, no implementation. Decisions in §9 need owner sign-off before any story starts.

Goal: make `pkg/holocropper` work as an embedded library inside another Go program (the future web UI server), so the UI can reach everything over HTTP: settings, probe results, per-frame framing data, bbox visualisations, inputs, outputs, previews, progress, cancellation. Today the package reports through stdout progress bars and an env-var CSV file; that has to become typed, callback-driven, context-aware API surface without breaking the CLI.

## 1. What the web UI needs

From the owner brief:

- **Settings**: read and change every rendering parameter (output W/H, fps, analysis height, zoom margin, max zoom, auto-orient) — the same set the CLI exposes as flags.
- **Data**: per-frame framing records (subject bbox, smoothed box, applied crop, walk target) — everything the debug CSV already writes, queryable and streamable.
- **Bbox visualisations**: overlay boxes drawn on real frames — for the source frame, for the chosen crop, and as a contact sheet / scrubber across the render.
- **Inputs and outputs**: list available videos, fetch the rendered MP4, fetch probe-only results before committing to a render.
- **Preview**: a fast probe/projection of framing before paying for a full render.
- **Progress and control**: live progress during a render, phase reporting, cancel.

## 2. Current surface audit (what exists today)

Exported API of `pkg/holocropper` (module `pkg/holocropper`):

| Surface | Today |
|---|---|
| `NewHoloCropper()` + struct fields `Height Width FPS AnalysisHeight ZoomMargin MaxZoom` | config via exported fields, zero = built-in default |
| `Crop(height, width, fps, input, output) error` | one-shot, no context, height-first arg order |
| `AutoOrient(input, w, h) (outW, outH, OrientationStat, error)` | probe already returns a structured stat |
| `Progress() float64`, `Done() bool` | read unguarded counters; `Done()` busy-waits 20 ms ticks |
| `Cancel()` | closes pipeline channels once + removes temp dirs |
| env `HOLOCROPPER_DEBUG_BOXES=<path>` | 16-col CSV written inside STEP 4 (`box_debug.go`) |

Seams that must open, numbered for reference throughout this plan:

- **S1 — no context.** `Crop` cannot be cancelled through `context.Context`. Both ffmpeg calls (`resizeTiny` in `preprocess.go`, `convertImagesToMP4` in `file_io.go`) use plain `exec.Command`; `Cancel()` closes channels but the ffmpeg child keeps running.
- **S2 — stdout coupling.** `Crop` prints startup text, `convertImagesToMP4` prints the full ffmpeg command line, and both create `progressbar` bars bound to a terminal. An embedded server has no terminal to own.
- **S3 — env-var telemetry hook.** The framing log is only reachable through an environment variable and a file path. Global process state, no way to stream or multiplex it.
- **S4 — unguarded counters.** `currentFrame` is incremented in the STEP 4 goroutine (`process.go`) and read by `tickPipeline`, `Progress()`, `Done()` from another goroutine with no synchronisation.
- **S5 — no structured result.** Nothing tells the caller the applied output dims (after auto-orient and even-rounding), source dims, analysis dims, frame count, elapsed time, or where the output MP4 landed (`outputDir/<basename>.mp4` is convention only).
- **S6 — probe output too thin.** `OrientationStat` carries aggregate means only; the UI wants the per-sample-frame bboxes to draw on a poster frame.
- **S7 — CLI owns input discovery.** Directory walking / `.mp4` filtering lives in `cmd/holocropper`; the package has no input inventory concept.
- **S8 — concurrent multi-file renders.** The CLI fires one goroutine per input file. Fine for a batch CLI; a server must control render concurrency deliberately.

Non-negotiable: the CLI (`cmd/holocropper`, Makefile target) keeps working unchanged, and the debug CSV stays available so `HIGH-ASPECT-RATIO-FIXES.md` measurement workflows keep running.

## 3. Layer 1 — embedded Go API (in `pkg/holocropper`)

Backward compatibility first: `Crop`, `AutoOrient`, `Progress`, `Done`, `Cancel`, the env-var CSV all remain. The new surface is additive, and the CLI gets ported to it once it is proven.

### 3.1 `Settings` and `Render`

```go
// Settings mirrors every CLI flag. Zero values select built-in defaults,
// exactly as today.
type Settings struct {
    Width, Height  int     // output dims; rounded up to even, as Crop does now
    FPS            float64
    AnalysisHeight int     // 0 = 480, never below Height
    ZoomMargin     float64 // 0 = 1.12
    MaxZoom        float64 // 0 = 2.0
    AutoOrient     bool
}

func (s Settings) Validate() error   // explicit errors instead of Crop's inline checks
func (s Settings) Resolve(srcW, srcH int) Resolved // concrete values + analysis size, for reporting

type RenderRequest struct {
    Input     string  // abs or cwd-relative file
    OutputDir string
    Settings  Settings
}

// Render is the context-aware replacement for Crop.
func (hc *HoloCropper) Render(ctx context.Context, req RenderRequest) (*Result, error)

type Result struct {
    InputPath, OutputPath string   // OutputPath = the actual <dir>/<basename>.mp4 written
    SourceW, SourceH    int
    AnalysisW, AnalysisH int
    OutputW, OutputH    int       // after auto-orient + even rounding
    AppliedSettings     Resolved
    Frames              int
    Elapsed             time.Duration
    Orientation         *OrientationStat // nil unless the probe ran
    DroppedFrameRecords int              // sink backpressure accounting
    Warnings            []string         // ffmpeg stderr tails, probe fallbacks, ...
}
```

`Crop` becomes a thin wrapper over `Render` with no observer, so behaviour and CLI output stay identical.

Fixes folded in here: **S1** (both ffmpeg calls move to `exec.CommandContext`; pipeline `tickPipeline` selects on `ctx.Done()`; cancel kills children and cleans temp dirs), **S4** (frame counters become atomic; `Done()`/`Progress()` stay for compatibility but the new code path never uses them), **S5** (everything the UI needs is in `Result`).

### 3.2 Observer and typed frame records

The observer replaces stdout/progressbar coupling (**S2**) and the env-var hook (**S3**) with one typed callback bundle. All callbacks are optional and called from the pipeline goroutines; implementations must not block (send to a channel / append under a mutex instead).

```go
type Phase string
const (
    PhaseProbe   Phase = "probe"    // auto-orient sampling
    PhaseScale   Phase = "scale"    // ffmpeg analysis pre-scale
    PhaseDetect  Phase = "detect"   // STEPS 1-4 streaming
    PhaseMux     Phase = "mux"      // ffmpeg frame + audio join
)

type Observer struct {
    OnPhase    func(Phase)
    OnProgress func(done, total int)      // rate-limited by the caller
    OnFrames   func([]FrameRecord)        // batched, see below
    OnWarning  func(string)
}

// FrameRecord mirrors the debug CSV columns 1:1 (analysis-pixel units).
type FrameRecord struct {
    Frame      int
    Subjects   int
    BBox       image.Rectangle // master subject bbox, analysis px
    EMA        image.Rectangle // smoothed subject box, analysis px
    CropCx, CropCy   float64   // applied crop centre, analysis px
    CropW, CropH     float64
    TargetH          float64   // smoothed zoom target
    WalkTgt          float64   // 0 = walk leash inactive
    WalkActive       bool      // derived: WalkTgt != 0
}
```

`FrameSink` is the persistence half: `NewCSVFileSink(path, legacyHeader bool) FrameSink` reproduces the exact 16-column CSV so existing analysis scripts (`/tmp/opencode/analysis/*.py`) keep working unchanged, and a `JSONLSink` feeds the HTTP layer. The env var becomes a compatibility shim that constructs a CSV sink.

Batching: `OnFrames` delivers slices — at most 24 records or 250 ms old, whichever comes first — so an SSE fan-out never emits one event per frame at 24 fps × thousands of frames. Sink backpressure is the consumer's problem: the package never blocks the pipeline; if a consumer signals a drop, `DroppedFrameRecords` counts it.

### 3.3 Probe as a first-class operation

**S6**: extend `probeSubjectARs` internals into a public report without changing `AutoOrient`'s behaviour.

```go
type ProbeSample struct {
    FrameIdx int
    Subjects int
    BBox     image.Rectangle // analysis px at the probe's analysis size
    AR       float64         // bbox width / height
}

type ProbeReport struct {
    OrientationStat            // the existing struct, embedded
    AnalysisW, AnalysisH int   // probe analysis geometry for overlay mapping
    SourceW, SourceH     int
    Samples              []ProbeSample
}

func (hc *HoloCropper) Probe(input string, w, h int) (*ProbeReport, error)
```

This is the UI's preview path: probe an input at a requested resolution, draw the sampled bboxes over a poster frame, show the keep-vs-flip decision (keep/flip scores, mean/median/geo AR) — all without rendering. `AutoOrient` internally calls `Probe` and adds the orientation decision on top.

## 4. Layer 2 — artifacts and preview model

A job workspace owned by whoever embeds the library (the server in layer 3). The package provides the parts; the workspace layout is:

```
<dataDir>/jobs/<jobID>/
  job.json              // settings, state, timestamps, Result when finished
  framing.jsonl         // full FrameRecord stream (typed)
  framing.csv           // legacy 16-col CSV, same numbers
  contact-sheet.jpg     // annotated overview tiles
  frames/<n>.jpg        // annotated snapshots at selected frame numbers
  out/<name>.mp4        // final render (rendered in place, recorded in job.json)
```

**Annotated snapshots (`frames/`, `contact-sheet.jpg`).** STEP 5/6 already hold the cropped output frame and STEP 4 holds the boxes in the same goroutine chain, so annotation is generated *during* the render, from real frames, for a sampled subset (adaptive K so a render yields ≤ ~48 snapshots). Each snapshot draws: subject bbox (raw detection), EMA box, and the applied crop rectangle mapped into output pixels (§6), with a small per-frame legend. The contact sheet tiles those snapshots with frame numbers and walk-active markers. Snapshots are written straight into the job workspace — the existing JPEG temp sequence is still deleted after muxing.

Fallback path: an offline `Annotate(outputVideo, framingCSV)` mode reconstructs overlays from a finished MP4 + CSV via ffmpeg `drawbox`. Keep it simple and defer it unless the UI needs it (decision D6).

**Metrics.** Port the measured quantities the analysis scripts already compute in Python (`/tmp/opencode/analysis/worst_slack.py`, `walk_metrics.py`, `auto_metrics.py`) into a small `pkg/holocropper/analysis` analyzer that consumes `framing.jsonl` and returns JSON: subject-height coverage %, edge-overshoot frame count, per-side slack min, walk range px, dropout frame count, orientation stats. These become the UI's quality badges (e.g. tbaka 16:9: edge overshoot 47, slack ≥ 3.5 %/side).

## 5. Layer 3 — HTTP API (`/v1`)

A `pkg/holocropper/server` package **inside the holocropper module** (so it reaches telemetry without env vars), plus a `cmd/holocropper-server` binary. The server depends on a small interface, not on `*HoloCropper` directly, so handler tests need no ffmpeg:

```go
type Renderer interface {
    Render(ctx context.Context, req RenderRequest) (*Result, error)
    Probe(input string, w, h int) (*ProbeReport, error)
}
```

`*HoloCropper` satisfies it; tests inject a fake.

### 5.1 Resources and endpoints

Plural nouns, hierarchical nesting, `/v1` path prefix. All request/response bodies `application/json` unless noted.

| Method | Path | Purpose | Success | Failure |
|---|---|---|---|---|
| GET | `/v1/inputs` | list renderable videos in the configured inputs dir (name, size, mtime, duration) | 200 | 500 |
| GET | `/v1/settings/schema` | parameter descriptors for auto-generated UI forms | 200 | — |
| POST | `/v1/probe` | probe-only report for an input + settings (no render) | 200 | 400, 404 |
| POST | `/v1/jobs` | create a render job (settings + input) → queued | 202 | 400, 404, 429 (queue full) |
| GET | `/v1/jobs` | list jobs, filter by state, cursor pagination (`data`, `next_cursor`, `has_more`) | 200 | — |
| GET | `/v1/jobs/{id}` | job state + Result when completed | 200 | 404 |
| POST | `/v1/jobs/{id}/cancel` | cancel a queued/running job | 202 | 404, 409 (terminal) |
| DELETE | `/v1/jobs/{id}` | delete the job workspace | 204 | 404, 409 (running) |
| GET | `/v1/jobs/{id}/events` | SSE event stream (§5.3) | 200 | 404 |
| GET | `/v1/jobs/{id}/frames` | page of FrameRecord (`cursor`=frame number, `limit` ≤ 500, `from`/`to` frame filters) | 200 | 400, 404 |
| GET | `/v1/jobs/{id}/metrics` | FramingMetrics summary (analyzer over framing.jsonl) | 200 | 404, 409 (not finished) |
| GET | `/v1/jobs/{id}/framing.csv` | full legacy CSV download | 200 text/csv | 404 |
| GET | `/v1/jobs/{id}/contact-sheet` | annotated contact sheet | 200 image/jpeg | 404, 409 |
| GET | `/v1/jobs/{id}/frames/{n}/snapshot` | annotated snapshot at frame n | 200 image/jpeg | 404, 409 |
| GET | `/v1/jobs/{id}/output` | rendered MP4, `Range` supported | 200/206 video/mp4 | 404, 409 |

Job states: `queued → running → completed | failed | cancelling → cancelled`. Artifact endpoints return **409 `not_ready`** until the artifact exists — the UI polls or waits on SSE instead of guessing.

Inputs are referenced by **name within the configured inputs directory** (e.g. `vids/`), never by arbitrary path: the server maps `name → dir/name.mp4` and rejects traversal attempts. Uploads are out of scope for v1 (decision D2).

### 5.2 Error envelope (every non-2xx)

```json
{ "error": { "code": "validation_error",
             "message": "height must be between 16 and 4096",
             "details": [ { "field": "height", "rule": "range" } ] } }
```

Canonical codes: `validation_error` (400), `input_not_found` (404), `job_not_found` (404), `not_ready` (409, artifact/job state), `job_terminal` (409, cancel/delete on a finished job), `too_many_jobs` (429), `internal` (500, message summarised — full error goes to the server log, not the client).

### 5.3 SSE events (`/v1/jobs/{id}/events`)

`text/event-stream`, named events, resumable via `Last-Event-ID` = frame index:

| Event | Payload | Rate |
|---|---|---|
| `phase` | `{ "phase": "detect" }` | per phase change |
| `progress` | `{ "done": 1840, "total": 5558, "fps_effective": 23.1 }` | ≤ 4/s |
| `frames` | `{ "records": [FrameRecord, ...] }` | batched (≤ 24 records / 250 ms) |
| `warning` | `{ "message": "..." }` | as emitted |
| `completed` | full `Result` | once |
| `failed` | error envelope | once |
| `cancelled` | `{ "cancelled_at_frame": 1840 }` | once |

A connecting UI gets current state from `GET /jobs/{id}` first, then opens SSE from `Last-Event-ID`; mid-render joins see the tail, not the whole history (pages come from `/frames`).

### 5.4 Settings schema endpoint

Serves the form contract so the UI renders controls from data, not hardcoded markup:

```json
{ "parameters": [
  { "name": "width",  "type": "integer", "default": 240, "min": 16, "max": 4096,
    "description": "Output width in pixels (rounded up to even)" },
  { "name": "analysis_height", "type": "integer", "default": 480, "min": 0,
    "description": "0 = auto (480, never below height)" },
  { "name": "zoom_margin", "type": "number", "default": 1.12, "min": 1.0, "max": 3.0 },
  { "name": "auto_orient", "type": "boolean", "default": false } ] }
```

Ranges: `width`/`height` 16–4096; `fps` 1–120; `analysis_height` 0 or ≥ height, ≤ source height (supersampling above source height costs detection time for no framing gain — measured ~2.8× at `-analysis-height 1080` on a 1080p source); `zoom_margin` 1.0–3.0 (the 1.18 landscape/portrait padding floor applies regardless); `max_zoom` 1.0–8.0 with 0 = default 2.0.

## 6. Coordinate mapping (analysis px → normalized → output px)

All framing numbers live in **analysis-frame pixels**: `AH = analysis_height` (even), `AW = AH × srcW / srcH` (even) — the analysis frame preserves the true source aspect ratio. Three coordinate spaces, one conversion each way:

- **Normalized** (`0..1`): `u = x / AW`, `v = y / AH`. This is the stable wire format for the UI: resolution-independent, comparable across renders and settings.
- **Source px**: `x × srcW / AW` (uniform scale; aspect-preserving resize means the x and y factors are equal within < 1 px of even-rounding). Used to draw boxes over a source-frame poster.
- **Output px**: the applied crop rectangle `(cx, cy, cropW, cropH)` is aspect-locked (`cropW = cropH × OutputW/OutputH`) and scales to exactly fill the output frame, so for any analysis-space point `p`:
  `out = ( (p.x - cx + cropW/2) × OutputW/cropW , (p.y - cy + cropH/2) × OutputH/cropH )`
  Stage-padding crops (landscape spilling past frame edges) map the same way and legitimately produce negative or > OutputW coordinates — the UI clamps for display. The grounded floor pin means a grounded crop's bottom edge is exactly the output bottom edge.

`FrameRecord` carries analysis px (CSV-compatible) and the API layer adds `norm` objects computed from `Result.AnalysisW/H` — the package itself does not normalize (single source of truth, no drift).

## 7. Concurrency and performance policy

- **One render at a time** in the server (queue depth 1, FIFO), unlike the CLI's goroutine-per-file fan-out (**S8**). Detection + ffmpeg is CPU-heavy; serial renders also keep framing deterministic run-to-run, which the whole verification workflow depends on.
- **One probe at a time**, allowed to run concurrently with the running render (probes are short: ≤ 128 decoded frames). A probe of the same video a job is rendering is fine; two simultaneous renders are not.
- Queue cap ~20 pending jobs → 429 `too_many_jobs`. Job creation never blocks waiting for a free renderer slot.
- Pipeline memory unchanged (streaming, one frame in flight per stage). The only new memory is the frame-record buffer (numbers only: ~16 fields × total frames is < 1 MB for a 5-min video) and ≤ 48 annotated snapshots per job.
- Temp dirs remain `os.MkdirTemp` per render, removed on completion/cancel; job workspaces are durable and owned by the server.

## 8. Epics, milestones, stories

Estimates: story points (1, 2, 3, 5, 8). Every story is independently deliverable; acceptance criteria are verifiable without the UI.

### Milestone A — embedded API hardening (library only, no HTTP)

| ID | Story | Est | Depends on | Acceptance criteria |
|---|---|---|---|---|
| A1 | Context-aware `Render`: `exec.CommandContext` for both ffmpeg calls, ctx-aware pipeline tick, cancel kills children + cleans temp dirs | 5 | — | kill mid-render leaves no temp dirs and no zombie ffmpeg; `Cancel()` still works |
| A2 | `Settings`/`Validate`/`Resolve`; `Crop` wraps `Render`; zero=default semantics preserved | 2 | — | CLI output byte-for-byte unchanged on the corpus videos |
| A3 | `FrameRecord` + `FrameSink` + batched `OnFrames`; CSV sink reproduces the 16-col format; env var becomes a shim over it | 3 | — | existing Python analysis scripts run unchanged on a shim-written CSV |
| A4 | `Observer` phase/progress wiring; progressbar and stdout prints move to the CLI side when no observer is set | 3 | A2 | library emits zero stdout when an observer is present |
| A5 | `Probe` + `ProbeReport` with per-sample bboxes; `AutoOrient` delegates to `Probe` | 3 | — | probe on the test corpus matches current `OrientationStat` aggregates exactly |
| A6 | `Result` (paths, dims, elapsed, orientation, warnings from captured ffmpeg stderr) | 2 | A1 | `Result.OutputPath` == file actually on disk, for all 4 corpus renders |
| A7 | atomic frame counters; `Progress`/`Done` kept, marked deprecated | 1 | — | `go test -race` clean on framing/walk tests + a new pipeline race test |

### Milestone B — artifacts and preview (library + workspace)

| ID | Story | Est | Depends on | Acceptance criteria |
|---|---|---|---|---|
| B1 | Job workspace layout + `job.json` state file (create/finish/fail/cancel transitions) | 3 | A2, A6 | crash mid-job leaves a readable `job.json` with `failed` state |
| B2 | Annotated snapshots during render + contact sheet (subject box, EMA box, crop rect, legend, walk markers) | 8 | A3 | tbaka 16:9 render yields ≤ 48 snapshots; overlay boxes match framing.jsonl within rounding |
| B3 | `framing.jsonl` + `framing.csv` persistence via sinks in the workspace | 2 | A3, B1 | both files present after a render; CSV diff vs env-var path is aggregate-identical |
| B4 | `pkg/holocropper/analysis` metrics analyzer ported from the Python tooling (coverage %, edge overshoot, slack min, walk range, dropout count) | 5 | B3 | reproduces the measured §14/§14.1 numbers (tbaka overshoot 47, slack ≥ 3.5 %) |

### Milestone C — HTTP API

| ID | Story | Est | Depends on | Acceptance criteria |
|---|---|---|---|---|
| C1 | `pkg/holocropper/server` skeleton: `/v1` mux, `Renderer` interface, error envelope, `/settings/schema`, `/inputs` | 3 | A2 | schema endpoint mirrors CLI flags; path-traversal attempts on `/inputs` rejected |
| C2 | Job manager: queue (depth 1), states, POST/GET/cancel/delete, `job.json` persistence | 5 | A1, B1 | queued job cancels immediately; running job cancels within one frame-tick of the signal |
| C3 | SSE endpoint with batching + `Last-Event-ID` resume | 3 | C2, A3, A4 | reconnect mid-render replays only missed batches |
| C4 | Artifact endpoints: `output` (Range), contact-sheet, snapshot, `/frames` pages, `/metrics`, `/framing.csv` | 3 | B2, B3, B4, C2 | `curl -C -` resumes a partial output download; 409 `not_ready` before artifacts exist |
| C5 | `POST /probe` endpoint | 2 | A5, C1 | probe result matches CLI auto-orient decision text for the same input/settings |
| C6 | Handler tests with a fake `Renderer` + one golden end-to-end render against a tiny fixture video | 5 | C1–C5 | full test suite runs without a corpus video; golden test asserts Result + metrics shape |

### Milestone D — deferred (explicitly out of scope for this plan)

Web UI shell itself; input upload endpoints; auth/remote binding; embedding the API mux into TinyMikuServer's `main.go`; offline `Annotate` fallback mode; job history database.

## 9. Decision points (owner sign-off needed)

- **D1 — server shape.** Recommend: mountable `pkg/holocropper/server` (returns an `http.Handler`) + standalone `cmd/holocropper-server`. TinyMikuServer can mount the same handler later. Alternative: only a standalone binary.
- **D2 — inputs.** v1 = configured library directory (e.g. `vids/`), no uploads. Confirm.
- **D3 — job persistence.** Flat `job.json` per job on disk, no database. Confirm.
- **D4 — concurrency.** 1 render + 1 concurrent probe, queue cap 20. Confirm or raise.
- **D5 — binding/auth.** Bind `127.0.0.1:8080` by default, no auth in v1. Confirm; LAN exposure later.
- **D6 — annotation path.** Overlays generated during the render only (no offline MP4-annotation mode in v1). Confirm.
- **D7 — settings encoding.** Keep `0 = auto default` semantics on the wire (schema endpoint documents ranges and defaults); no explicit-null protocol. Confirm.
- **D8 — legacy surfaces.** Keep `Crop`, `Progress`, `Done`, env-var CSV as compatibility shims during v1 development; remove only in a later `/v2`. Confirm.

## 10. Sequencing

`A1 → A2 → {A3, A4, A5 in parallel} → A6 → A7 → B1 → {B2, B3} → B4 → C1 → C2 → {C3, C4, C5} → C6`. Milestone A alone already makes the package embeddable; nothing in it depends on HTTP. Milestone C's handler tests (C6) deliberately use the fake `Renderer` so the API is testable in CI without ffmpeg or corpus videos. After D1–D8 are confirmed, implementation starts with A1.
