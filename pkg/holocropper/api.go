package holocropper

// Embeddable API surface (HOLOCROPPER-API-PLAN.md v5).
//
// The package offers two ways in: the classic one-shot Crop() used by the
// CLI (unchanged), and the pass model for embedding apps:
//
//	Analyze (prepare+detect, cached DetectionStream)
//	  -> Plan (in-process replay of the framing controller, milliseconds)
//	  -> Analysis.RenderPreview / Analysis.RenderFinal (output-res videos
//	     executed from the cached analysis video, no re-detection)
//
// Every async pass is a Task (see task.go): cheap non-blocking State()/
// Progress()/Cancel from any goroutine, last-write-wins supersede per slot,
// and a single TaskInfo status struct driving UI spinners. All disk scratch
// lives under caller-supplied TempDir. All exported wire types carry
// snake_case JSON tags so they cross Go->JS bindings unchanged.

import (
	"errors"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Settings (the render parameters) and the form schema

// Settings mirrors every tunable render parameter. Zero values select the
// built-in defaults, exactly like the CLI flags.
type Settings struct {
	Width          int     `json:"width"`           // output width; 0 = 240; rounded up to even
	FPS            float64 `json:"fps"`             // 0 = 20; controller dt + mux rate
	Height         int     `json:"height"`          // output height; 0 = 240; rounded up to even
	AnalysisHeight int     `json:"analysis_height"` // 0 = 480; never below Height for rendering
	ZoomMargin     float64 `json:"zoom_margin"`     // 0 = 1.12
	MaxZoom        float64 `json:"max_zoom"`        // 0 = 2.0
	AutoOrient     bool    `json:"auto_orient"`     // pick WxH vs HxW from subject AR
}

// ValidationError names the offending field so a settings form can highlight
// inputs individually instead of receiving one opaque error string.
type ValidationError struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

func (e ValidationError) Error() string { return e.Field + ": " + e.Reason }

// SettingsError aggregates per-field validation failures (from Settings or
// request Validate) so a settings form can highlight each input, instead of
// receiving one opaque error string.
type SettingsError struct {
	Errors []ValidationError `json:"errors"`
}

func (e *SettingsError) Error() string {
	parts := make([]string, len(e.Errors))
	for i, ve := range e.Errors {
		parts[i] = ve.Error()
	}
	return "holocropper: invalid settings: " + strings.Join(parts, "; ")
}

// Validate reports ALL violations at once (empty slice = valid).
func (s Settings) Validate() []ValidationError {
	var errs []ValidationError
	add := func(field, reason string) {
		errs = append(errs, ValidationError{Field: field, Reason: reason})
	}
	if s.Width != 0 && (s.Width < 16 || s.Width > 4096) {
		add("width", "must be 0 (default 240) or between 16 and 4096")
	}
	if s.Height != 0 && (s.Height < 16 || s.Height > 4096) {
		add("height", "must be 0 (default 240) or between 16 and 4096")
	}
	if s.FPS != 0 && (s.FPS < 1 || s.FPS > 120) {
		add("fps", "must be 0 (default 20) or between 1 and 120")
	}
	if s.AnalysisHeight != 0 && s.AnalysisHeight < 16 {
		add("analysis_height", "must be 0 (default 480) or at least 16")
	}
	if s.ZoomMargin != 0 && (s.ZoomMargin < 1.0 || s.ZoomMargin > 10.0) {
		add("zoom_margin", "must be 0 (default 1.12) or between 1.0 and 10.0")
	}
	if s.MaxZoom != 0 && (s.MaxZoom < 1.0 || s.MaxZoom > 8.0) {
		add("max_zoom", "must be 0 (default 2.0) or between 1.0 and 8.0")
	}
	return errs
}

// defaultFPS is the frame rate used when Settings.FPS is unset: it feeds the
// controller dt and every mux, never detection.
const defaultFPS = 20.0

// effectiveZoomMargin resolves the bbox -> crop padding factor (0 = default).
func effectiveZoomMargin(s Settings) float64 {
	if s.ZoomMargin > 0 {
		return s.ZoomMargin
	}
	return defaultZoomMargin
}

// effectiveMaxZoom resolves the zoom-in policy cap (0 = default).
func effectiveMaxZoom(s Settings) float64 {
	if s.MaxZoom > 0 {
		return s.MaxZoom
	}
	return defaultMaxZoom
}

// ParamSpec describes one Settings field for auto-generated forms.
type ParamSpec struct {
	Name    string `json:"name"`
	Type    string `json:"type"` // "integer" | "float" | "bool"
	Default any    `json:"default"`
	Min     any    `json:"min,omitempty"`
	Max     any    `json:"max,omitempty"`
	Help    string `json:"help"`
}

// Schema covers every Settings field: defaults, ranges and help text.
func Schema() []ParamSpec {
	return []ParamSpec{
		{Name: "width", Type: "integer", Default: 240, Min: 16, Max: 4096, Help: "Output width in pixels (0 = default; rounded up to even)"},
		{Name: "height", Type: "integer", Default: 240, Min: 16, Max: 4096, Help: "Output height in pixels (0 = default; rounded up to even)"},
		{Name: "fps", Type: "float", Default: 20.0, Min: 1.0, Max: 120.0, Help: "Output frames per second (0 = default); controller time step and mux rate"},
		{Name: "analysis_height", Type: "integer", Default: defaultAnalysisHeight, Min: 16, Help: "Internal analysis frame height (0 = default 480); renders require it >= output height. Larger than the source height costs detection time for no framing gain"},
		{Name: "zoom_margin", Type: "float", Default: defaultZoomMargin, Min: 1.0, Max: 10.0, Help: "Subject bbox to crop padding factor (0 = default); landscape and portrait fits enforce a 1.18 floor"},
		{Name: "max_zoom", Type: "float", Default: defaultMaxZoom, Min: 1.0, Max: 8.0, Help: "Maximum zoom-in factor (0 = default); never upscales past source resolution regardless"},
		{Name: "auto_orient", Type: "bool", Default: false, Help: "Pick WxH or HxW from the subjects' aspect ratios; skipped for 1:1 outputs"},
	}
}

// Resolved reports the settings actually applied after defaults,
// even-rounding and the auto-orient decision, plus the frame geometries.
type Resolved struct {
	Settings  `json:"settings"`
	SourceW   int `json:"source_w"`
	SourceH   int `json:"source_h"`
	AnalysisW int `json:"analysis_w"`
	AnalysisH int `json:"analysis_h"`
	OutputW   int `json:"output_w"`
	OutputH   int `json:"output_h"`
}

// ---------------------------------------------------------------------------
// Wire-friendly geometry types

// Rect is a top-left + size rectangle in float pixels. All public framing
// types use it (never image.Rectangle) so JSON payloads stay flat.
type Rect struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

func rectOf(r image.Rectangle) Rect {
	return Rect{X: float64(r.Min.X), Y: float64(r.Min.Y), W: float64(r.Dx()), H: float64(r.Dy())}
}

func (r Rect) toImage() image.Rectangle {
	return image.Rect(int(r.X), int(r.Y), int(r.X+r.W), int(r.Y+r.H))
}

// SpaceRects carries the same three boxes in one derived coordinate space.
type SpaceRects struct {
	BBox Rect `json:"bbox"`
	EMA  Rect `json:"ema"`
	Crop Rect `json:"crop"`
}

// FrameRecord is one frame of the framing replay. Raw boxes are analysis-
// frame pixels (the same values the debug CSV writes); Norm (0..1 over the
// analysis frame) and Out (output-frame pixels, mapped through the applied
// crop rect) are precomputed so a UI draws them anywhere with zero math.
type FrameRecord struct {
	Frame    int `json:"frame"` // 1-based, matches the debug CSV
	Subjects int `json:"subjects"`

	BBox    Rect    `json:"bbox"` // master subject bbox (zero when lost)
	EMA     Rect    `json:"ema"`  // smoothed subject box (zero before first subject)
	Crop    Rect    `json:"crop"` // applied crop rectangle
	TargetH float64 `json:"target_h"`
	WalkTgt float64 `json:"walk_tgt"` // 0 = walk leash inactive
	// WalkActive reports the per-frame walk state: the render-level walk gate
	// is active AND this frame actually has a live leash target (the raw
	// walk_tgt value). A gated render may still fall back to centering on
	// frames where the subject is too wide to leash.
	WalkActive  bool `json:"walk_active"`
	SubjectLost bool `json:"subject_lost"`
	SceneCut    bool `json:"scene_cut"`
	Grounded    bool `json:"grounded"`

	Norm SpaceRects `json:"norm"` // 0..1 over the analysis frame
	Out  SpaceRects `json:"out"`  // output-frame pixels
}

// normalizeRect scales a rect into 0..1 space over a frame of w x h.
func normalizeRect(r Rect, w, h float64) Rect {
	if w <= 0 || h <= 0 {
		return Rect{}
	}
	return Rect{X: r.X / w, Y: r.Y / h, W: r.W / w, H: r.H / h}
}

// mapRectToOutput maps an analysis-space rect into output-frame pixels via
// the applied crop rect: the crop fills the output exactly (aspect-locked),
// so scale = outW/cropW = outH/cropH. Landscape stage-padding crops that
// spill past the frame edges map the same way and legitimately produce
// out-of-bounds values; consumers clamp for display.
func mapRectToOutput(r, crop Rect, outW, outH int) Rect {
	if crop.W <= 0 || crop.H <= 0 {
		return Rect{}
	}
	sx := float64(outW) / crop.W
	sy := float64(outH) / crop.H
	return Rect{X: (r.X - crop.X) * sx, Y: (r.Y - crop.Y) * sy, W: r.W * sx, H: r.H * sy}
}

// IntRange is an inclusive frame-number span (dropout runs etc).
type IntRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// WalkSummary reports the walk-mode telemetry of a Plan (HIGH-ASPECT-RATIO-
// FIXES.md §14): whether the render-level gate is active, how far the crop
// center actually roamed while leashed (px, analysis space), and how many
// frames fell back to subject centering because the subject was too wide to
// leash (or lost).
type WalkSummary struct {
	Active         bool    `json:"active"`
	RangePx        float64 `json:"range_px"`
	FallbackFrames int     `json:"fallback_frames"`
}

// ---------------------------------------------------------------------------
// Task status plane (spinner contract)

type Phase string

const (
	PhasePrepare       Phase = "prepare"        // ffmpeg analysis re-encode
	PhaseDetect        Phase = "detect"         // subject detection pass
	PhasePreviewFrames Phase = "preview_frames" // crop+overlay JPEG pass
	PhasePreviewMux    Phase = "preview_mux"    // ffmpeg preview mux
	PhaseRenderFrames  Phase = "render_frames"  // final crop JPEG pass
	PhaseRenderMux     Phase = "render_mux"     // ffmpeg final mux + audio
)

type TaskKind string

const (
	TaskAnalyze     TaskKind = "analyze"
	TaskPreview     TaskKind = "preview"
	TaskRenderFinal TaskKind = "render_final"
)

type TaskState string

const (
	StateQueued     TaskState = "queued"
	StateRunning    TaskState = "running"
	StateCompleted  TaskState = "completed"
	StateFailed     TaskState = "failed"
	StateCancelled  TaskState = "cancelled"
	StateSuperseded TaskState = "superseded"
)

// TaskInfo is the ONE status struct every Task speaks. Cheap and
// non-blocking; valid before, during and after the pass.
type TaskInfo struct {
	ID             int       `json:"id"`
	Kind           TaskKind  `json:"kind"`
	State          TaskState `json:"state"`
	Phase          Phase     `json:"phase"`
	Done           int       `json:"done"`
	Total          int       `json:"total"`
	ElapsedSeconds float64   `json:"elapsed_seconds"`
	Err            string    `json:"err,omitempty"`
	SupersededTo   int       `json:"superseded_to,omitempty"` // new Task ID when State == superseded
}

// Observer receives live events from Tasks. Callbacks fire from pipeline
// goroutines and MUST NOT BLOCK (the app-side job is an EventsEmit or a
// buffered channel send). All fields optional.
type Observer struct {
	OnTask    func(TaskInfo)      // state/phase changes immediately; progress ≤ 4 Hz
	OnFrames  func([]FrameRecord) // ≤ 24 records or 250 ms per batch
	OnWarning func(string)        // ffmpeg stderr diagnostics
}

// AnalysisStatus is the aggregate snapshot that reconciles every UI spinner
// in one cheap call: the live analyze Task for this Analysis's cache key
// (nil when idle), the live render Task on the shared render slot (nil when
// idle), and whether a complete cached stream exists.
type AnalysisStatus struct {
	Analysis *TaskInfo `json:"analysis,omitempty"`
	Render   *TaskInfo `json:"render,omitempty"`
	Cached   bool      `json:"cached"`
}

// ---------------------------------------------------------------------------
// Requests

// AnalyzeRequest drives the prepare+detect pass.
type AnalyzeRequest struct {
	Input          string    `json:"input"`           // input video file
	TempDir        string    `json:"temp_dir"`        // required: cache + scratch parent
	FFmpegPath     string    `json:"ffmpeg_path"`     // "" = "ffmpeg" from PATH
	AnalysisHeight int       `json:"analysis_height"` // 0 = 480; part of the cache key
	KeepWorkDir    bool      `json:"keep_work_dir"`   // debug: leave scratch on failure/cancel
	Observer       *Observer `json:"-"`
}

// Validate reports ALL field violations at once (filesystem existence is
// checked when the pass resolves the request).
func (r AnalyzeRequest) Validate() []ValidationError {
	var errs []ValidationError
	if r.Input == "" {
		errs = append(errs, ValidationError{Field: "input", Reason: "input video path is required"})
	}
	if r.TempDir == "" {
		errs = append(errs, ValidationError{Field: "temp_dir", Reason: "a scratch/cache parent directory is required"})
	}
	if r.AnalysisHeight != 0 && r.AnalysisHeight < 16 {
		errs = append(errs, ValidationError{Field: "analysis_height", Reason: "must be 0 (default 480) or at least 16"})
	}
	return errs
}

// PreviewRequest drives one output-resolution preview render. It carries no
// TempDir: previews are written inside the Analysis they were started from
// (its cache dir under the TempDir that produced it), and their JPEG scratch
// lives beside that cache.
type PreviewRequest struct {
	FFmpegPath  string         `json:"ffmpeg_path"`   // "" = "ffmpeg" from PATH
	KeepWorkDir bool           `json:"keep_work_dir"` // debug: leave the preview JPEG scratch behind
	Options     PreviewOptions `json:"options"`
}

// PreviewOptions selects which preview videos to render and what the
// overlay video draws.
type PreviewOptions struct {
	Kind        PreviewKind `json:"kind"` // "" = crop_overlay
	DrawSubject bool        `json:"draw_subject"`
	DrawCrop    bool        `json:"draw_crop"`
	Observer    *Observer   `json:"-"`
}

type PreviewKind string

const (
	PreviewCrop        PreviewKind = "crop"
	PreviewOverlay     PreviewKind = "overlay"
	PreviewCropOverlay PreviewKind = "crop_overlay"
)

// Validate reports ALL field violations at once. A preview needs no
// directories: it writes inside the Analysis it was started from.
func (r PreviewRequest) Validate() []ValidationError {
	var errs []ValidationError
	switch r.Options.Kind {
	case "", PreviewCrop, PreviewOverlay, PreviewCropOverlay:
	default:
		errs = append(errs, ValidationError{Field: "options.kind",
			Reason: fmt.Sprintf("must be crop, overlay or crop_overlay (got %q)", r.Options.Kind)})
	}
	return errs
}

// RenderRequest drives the final production render.
type RenderRequest struct {
	OutputDir   string    `json:"output_dir"`  // required, must exist
	TempDir     string    `json:"temp_dir"`    // required scratch parent
	FFmpegPath  string    `json:"ffmpeg_path"` // "" = PATH
	OutputName  string    `json:"output_name"` // final file name inside OutputDir; "" = <basename>-<WxH>.mp4
	KeepWorkDir bool      `json:"keep_work_dir"`
	Observer    *Observer `json:"-"`
}

// Validate reports ALL field violations at once (directory existence is
// checked when the render resolves the request).
func (r RenderRequest) Validate() []ValidationError {
	var errs []ValidationError
	if r.OutputDir == "" {
		errs = append(errs, ValidationError{Field: "output_dir", Reason: "an existing output directory is required"})
	}
	if r.TempDir == "" {
		errs = append(errs, ValidationError{Field: "temp_dir", Reason: "a scratch parent directory is required"})
	}
	if r.OutputName != "" && !validOutputFileName(r.OutputName) {
		errs = append(errs, ValidationError{Field: "output_name", Reason: "must be a plain .mp4 file name (no path separators)"})
	}
	return errs
}

// validOutputFileName accepts only a bare .mp4 file name: OutputName never
// steers the write outside OutputDir.
func validOutputFileName(name string) bool {
	return name != ".mp4" && filepath.Base(name) == name && strings.HasSuffix(name, ".mp4")
}

// ---------------------------------------------------------------------------
// Preview results

// PreviewRef describes one retained preview video file (servable).
type PreviewRef struct {
	Version      int         `json:"version"`
	Kind         PreviewKind `json:"kind"`
	Path         string      `json:"path"`
	SettingsHash string      `json:"settings_hash"`
	CreatedAt    time.Time   `json:"created_at"`
	Width        int         `json:"width"`
	Height       int         `json:"height"`
	FPS          float64     `json:"fps"`
	Frames       int         `json:"frames"`
}

// PreviewResult is what a finished PreviewTask Wait() returns.
type PreviewResult struct {
	Version          int      `json:"version"`
	SettingsHash     string   `json:"settings_hash"`
	CropVideoPath    string   `json:"crop_video_path"`    // "" when kind=overlay
	OverlayVideoPath string   `json:"overlay_video_path"` // "" when kind=crop
	Width            int      `json:"width"`              // OUTPUT target resolution
	Height           int      `json:"height"`
	Frames           int      `json:"frames"`
	FPS              float64  `json:"fps"`
	ElapsedSeconds   float64  `json:"elapsed_seconds"`
	Removed          []string `json:"removed,omitempty"`  // preview files the sweep deleted
	Warnings         []string `json:"warnings,omitempty"` // ffmpeg stderr diagnostics from this pass
}

// Result is what a finished RenderTask Wait() returns: the production file
// plus everything that produced it, so the app can log, verify and show it.
type Result struct {
	InputPath  string `json:"input_path"`
	OutputPath string `json:"output_path"`
	WorkDir    string `json:"work_dir"` // render scratch; "" when it was cleaned up

	SourceW   int `json:"source_w"`
	SourceH   int `json:"source_h"`
	AnalysisW int `json:"analysis_w"`
	AnalysisH int `json:"analysis_h"`
	OutputW   int `json:"output_w"`
	OutputH   int `json:"output_h"`

	AppliedSettings Resolved         `json:"applied_settings"`
	Orientation     *OrientationStat `json:"orientation,omitempty"`

	Frames         int      `json:"frames"`
	FPS            float64  `json:"fps"`
	ElapsedSeconds float64  `json:"elapsed_seconds"`
	Warnings       []string `json:"warnings,omitempty"`
}

// ---------------------------------------------------------------------------
// Inputs

// InputInfo lists one renderable input video.
type InputInfo struct {
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	SizeBytes int64     `json:"size_bytes"`
	ModTime   time.Time `json:"mod_time"`
}

// ListInputs returns the .mp4 files directly inside dir, sorted by name.
func ListInputs(dir string) ([]InputInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("list inputs %s: %w", dir, err)
	}
	var out []InputInfo
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".mp4") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, InputInfo{
			Name:      e.Name(),
			Path:      filepath.Join(dir, e.Name()),
			SizeBytes: info.Size(),
			ModTime:   info.ModTime(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ---------------------------------------------------------------------------
// Errors

var (
	// ErrCancelled is what a cancelled Task's Wait() returns (Cancel() was
	// called, or the pass was replaced - superseded carries its own error).
	ErrCancelled = errors.New("holocropper: task cancelled")
	// ErrAnalysisCacheMiss reports no valid cached analysis for the key
	// (CachedAnalysis); re-run Analyze.
	ErrAnalysisCacheMiss = errors.New("holocropper: no cached analysis for this input and analysis height")
	// ErrStreamIncompatible reports a Plan or render attempted against a
	// DetectionStream that does not match the requested geometry (e.g.
	// output height above the analyzed height - re-analyze higher).
	ErrStreamIncompatible = errors.New("holocropper: detection stream incompatible with requested settings")
)

// FFmpegError wraps a failed ffmpeg pass with a captured stderr tail.
type FFmpegError struct {
	Phase      Phase  `json:"phase"`
	Command    string `json:"command"`
	ExitCode   int    `json:"exit_code"`
	StderrTail string `json:"stderr_tail"`
}

func (e *FFmpegError) Error() string {
	return fmt.Sprintf("ffmpeg %s failed (exit %d): %s", e.Phase, e.ExitCode, e.StderrTail)
}

// SupersededError is what Wait() returns when another pass in the same slot
// replaced this Task. NewTaskID is the spinner to move to.
type SupersededError struct {
	NewTaskID int `json:"new_task_id"`
}

func (e *SupersededError) Error() string {
	return fmt.Sprintf("holocropper: task superseded by task %d", e.NewTaskID)
}
