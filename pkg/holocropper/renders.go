package holocropper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	vidio "github.com/AlexEidt/Vidio"
)

/* Render passes (HOLOCROPPER-API-PLAN.md stories B1/B2): output-resolution
previews and the final production render, both executed from a cached
Analysis video plus a Plan - no detection, no re-decode of the source.

Both passes share one frame loop - cropToBBoxCalc + fillResizeTo for the
content-aware modes, fitResizeTo (whole frame scaled onto black bars) for
fit mode (§6.10) - against the cached analysis video,
writing a JPEG sequence into Task-ID-isolated scratch under the caller's
TempDir, then ffmpeg-muxing it with progress. Previews mux video only; the
final render copies the source audio track through, exactly as the CLI does.

Preview files are versioned and never overwritten (requirement 11):
<basename>-<WxH>-v<N>.mp4 and <basename>-<WxH>-overlay-v<N>.mp4 inside the
analysis cache dir's previews/ folder, each with a .meta.json sidecar that
backs Previews() and the retention sweep. */

// renderJPEGQuality is the still quality the render sequences are written at.
// The CLI pipeline uses the encoder default (75); renders go higher because
// these frames are the user's framing judgement surface.
const renderJPEGQuality = 90

// previewRetention is how many versions per kind the sweep keeps (§12 tunable).
const previewRetention = 3

// The vp9-webm preview mux: constant quality with a real-time-leaning encode.
// A preview is a framing check, so the encode spends its time on keeping the
// pass quick rather than on squeezing the last bit of detail out of a JPEG
// sequence that was already lossy.
const (
	previewVP9CRF     = 30
	previewVP9CPUUsed = 4
	// previewGOPMaxFrames is the frame-count ceiling the measured Chromium
	// play zone established for vp9-webm previews (see muxFrameSequence).
	previewGOPMaxFrames = 24
)

// previewGOPFrames is the keyframe interval the vp9-webm preview mux hands
// ffmpeg: at most one second of frames, never more than previewGOPMaxFrames.
// Chromium's VP9 decoder refuses previews whose keyframes sit 48+ frames
// apart - or whose whole clip is one such GOP - on the QA rig's content
// (see muxFrameSequence); 24-frame intervals play. The cap keeps every
// preview inside the measured zone no matter the plan's fps. Never below 2 -
// ffmpeg rejects -g 1 as a GOP size.
func previewGOPFrames(fps float64) int {
	g := int(math.Round(fps))
	if g > previewGOPMaxFrames {
		g = previewGOPMaxFrames
	}
	if g < 2 {
		g = 2
	}
	return g
}

const previewsSubdir = "previews"

// ---------------------------------------------------------------------------
// Render Tasks

// PreviewTask is the live handle of a preview render (Task API via taskBase).
type PreviewTask struct {
	*taskBase

	mu      sync.Mutex
	result  *PreviewResult
	scratch string // Task-isolated JPEG scratch
	keep    bool
}

// Wait blocks until the Task is terminal and returns the finished preview or
// the pass error (*SupersededError, ErrCancelled, render errors).
func (t *PreviewTask) Wait() (*PreviewResult, error) {
	if err := t.waitTerminal(); err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.result, nil
}

// RenderTask is the live handle of the final production render.
type RenderTask struct {
	*taskBase

	mu      sync.Mutex
	result  *Result
	scratch string
	keep    bool
}

// Wait blocks until the Task is terminal and returns the production Result.
func (t *RenderTask) Wait() (*Result, error) {
	if err := t.waitTerminal(); err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.result, nil
}

// ---------------------------------------------------------------------------
// Plan / Analysis compatibility

// planFits reports whether p was replayed from this Analysis's stream: same
// source and analysis geometry, same frame count. Rendering a plan against
// the wrong analysis video would cut rects derived from another geometry.
func (a *Analysis) planFits(p *Plan) error {
	if p == nil {
		return errors.New("holocropper: render requires a Plan")
	}
	r := p.resolved
	if r.AnalysisW != a.meta.AnalysisW || r.AnalysisH != a.meta.AnalysisH {
		return fmt.Errorf("%w: plan analysis frame %dx%d, this analysis is %dx%d",
			ErrStreamIncompatible, r.AnalysisW, r.AnalysisH, a.meta.AnalysisW, a.meta.AnalysisH)
	}
	if r.SourceW != a.meta.SourceW || r.SourceH != a.meta.SourceH {
		return fmt.Errorf("%w: plan source %dx%d, this analysis input is %dx%d",
			ErrStreamIncompatible, r.SourceW, r.SourceH, a.meta.SourceW, a.meta.SourceH)
	}
	if n := a.stream.len(); n > 0 && len(p.records) != n {
		return fmt.Errorf("%w: plan replays %d frames, this analysis has %d - re-plan against the finished stream",
			ErrStreamIncompatible, len(p.records), n)
	}
	if len(p.records) == 0 {
		return fmt.Errorf("%w: plan has no frames to render", ErrStreamIncompatible)
	}
	if r.OutputW < 2 || r.OutputH < 2 {
		return fmt.Errorf("%w: plan output resolution %dx%d is not renderable", ErrStreamIncompatible, r.OutputW, r.OutputH)
	}
	return nil
}

// PreviewsDir is where this Analysis keeps its versioned preview videos; the
// app may serve files straight from it.
func (a *Analysis) PreviewsDir() string { return filepath.Join(a.dir, previewsSubdir) }

// renderScratchDir is the Task-isolated JPEG scratch under a caller TempDir.
func renderScratchDir(tempDir string, taskID int) string {
	return filepath.Join(mustAbs(tempDir), fmt.Sprintf("holocropper-render-%d", taskID))
}

// planFPS is the frame rate a render muxes at: the Settings FPS the Plan was
// replayed with (the same value that set its controller dt).
func planFPS(p *Plan) float64 {
	if p.settings.FPS > 0 {
		return p.settings.FPS
	}
	return defaultFPS
}

// frameSchedule is the output frame list one render pass writes for a Plan.
// The cached analysis holds the SOURCE's frame clock (prepare re-encodes with
// -fps_mode passthrough) and the Plan replays at Settings.FPS, so a product
// must hold seconds x renderFPS frames - not one frame per analysis frame.
// Without this mapping a 12 fps source rendered at a 24 fps preset wrote half
// the seconds its own audio track ran.
type frameSchedule struct {
	Frames int   // output frames the pass writes
	SrcFor []int // SrcFor[i] is the analysis frame, and Plan record, output frame i replays

	AnalysisFPS float64 // the clock the analysis was measured at; 0 when the cache does not say
	RenderFPS   float64 // the clock the product muxes at
	Resampled   bool    // the clocks differ, so analysis frames were repeated or dropped
}

// frameScheduleFor maps a Plan's records from the analysis clock to the render
// clock: output frame i replays analysis frame floor(i x analysisFPS /
// renderFPS). A faster render clock repeats frames, a slower one drops them,
// and the order never changes. Nothing is blended - a repeated frame is the
// same pixels, which is what a fixed frame budget expects. Unknown clocks (a
// cache written before the analysis fps was recorded) keep the old
// one-record-per-frame behaviour instead of guessing.
func frameScheduleFor(records int, analysisFPS, renderFPS float64) frameSchedule {
	if records <= 0 {
		return frameSchedule{AnalysisFPS: analysisFPS, RenderFPS: renderFPS}
	}
	resample := analysisFPS > 0 && renderFPS > 0 && analysisFPS != renderFPS
	frames := records
	if resample {
		frames = int(float64(records)*renderFPS/analysisFPS + 0.5)
		if frames < 1 {
			frames = 1
		}
	}
	s := frameSchedule{
		Frames:      frames,
		SrcFor:      make([]int, frames),
		AnalysisFPS: analysisFPS,
		RenderFPS:   renderFPS,
		Resampled:   resample,
	}
	for i := 0; i < frames; i++ {
		j := i
		if resample {
			j = int(math.Floor(float64(i) * analysisFPS / renderFPS))
			if j >= records {
				j = records - 1
			}
		}
		s.SrcFor[i] = j
	}
	return s
}

// frameSchedule is this Analysis's output frame list for p: the analysis
// cache's recorded fps is the source's clock, planFPS(p) is the render clock.
// Every output pass asks the same question - the frame loop, and the MikV
// audio track that must have one chunk per output frame.
func (a *Analysis) frameSchedule(p *Plan) frameSchedule {
	return frameScheduleFor(len(p.records), a.meta.FPS, planFPS(p))
}

// ---------------------------------------------------------------------------
// RenderPreview (story B1)

// RenderPreview starts an output-resolution preview render for p: the crop
// video, the bbox-overlay video, or both (Options.Kind, default crop_overlay).
// A preview is the final framing at the final size, written as NEW versioned
// files in this Analysis's previews dir.
//
// Last call wins per Analysis (render slot, shared with RenderFinal): a
// running preview or final render on this Analysis is superseded and its Wait
// returns *SupersededError with this Task's ID.
func (a *Analysis) RenderPreview(ctx context.Context, p *Plan, req PreviewRequest) (*PreviewTask, error) {
	if errs := req.Validate(); len(errs) > 0 {
		return nil, &SettingsError{Errors: errs}
	}
	if err := a.planFits(p); err != nil {
		return nil, err
	}
	kind := req.Options.Kind
	if kind == "" {
		kind = PreviewCropOverlay
	}
	opts := req.Options
	opts.Kind = kind
	// Default the draw flags to "everything": an overlay video with no boxes
	// drawn would just be the crop video twice.
	if !opts.DrawSubject && !opts.DrawCrop {
		opts.DrawSubject, opts.DrawCrop = true, true
	}

	if err := os.MkdirAll(a.PreviewsDir(), 0o755); err != nil {
		return nil, fmt.Errorf("holocropper: previews dir: %w", err)
	}

	task := &PreviewTask{
		taskBase: newTaskBase(ctx, TaskPreview, opts.Observer),
		keep:     req.KeepWorkDir,
	}
	task.scratch = renderScratchDir(a.tempDir, task.ID())
	a.renderSlot.install(task.taskBase)
	go runPreview(a, task, p, opts, req.FFmpegPath)
	return task, nil
}

func runPreview(a *Analysis, task *PreviewTask, p *Plan, opts PreviewOptions, ffmpegPath string) {
	tb := task.taskBase
	defer func() {
		if r := recover(); r != nil {
			tb.finish(fmt.Errorf("holocropper: preview pass panicked: %v", r))
		}
	}()
	tb.begin()

	res, err := previewWork(task.ctx, a, task, p, opts, ffmpegPath)
	if err != nil {
		task.cleanupScratch()
		tb.finish(err)
		return
	}
	if !task.keep {
		task.cleanupScratch()
	}
	task.mu.Lock()
	task.result = res
	task.mu.Unlock()
	tb.finish(nil)
}

func (t *PreviewTask) cleanupScratch() {
	if t.scratch != "" {
		os.RemoveAll(t.scratch)
	}
}

func previewWork(ctx context.Context, a *Analysis, task *PreviewTask, p *Plan, opts PreviewOptions, ffmpegPath string) (*PreviewResult, error) {
	tb := task.taskBase
	r := p.resolved
	basename := getBasenameWithoutExt(a.meta.Input)

	wantCrop := opts.Kind == PreviewCrop || opts.Kind == PreviewCropOverlay
	wantOverlay := opts.Kind == PreviewOverlay || opts.Kind == PreviewCropOverlay
	if !wantCrop && !wantOverlay {
		return nil, fmt.Errorf("holocropper: unknown preview kind %q", opts.Kind)
	}

	cropDir, overlayDir := "", ""
	if wantCrop {
		cropDir = filepath.Join(task.scratch, "crop")
		if err := os.MkdirAll(cropDir, 0o755); err != nil {
			return nil, err
		}
	}
	if wantOverlay {
		overlayDir = filepath.Join(task.scratch, "overlay")
		if err := os.MkdirAll(overlayDir, 0o755); err != nil {
			return nil, err
		}
	}

	start := time.Now()

	// The mux decides what the preview files hold, and for vp9-webm whether
	// the analyzed input's audio comes along as Opus.
	mux := opts.MuxVideo
	if mux == "" {
		mux = PreviewMuxMJPEG
	}
	audioSource := ""
	if mux == PreviewMuxVP9WebM {
		hasAudio, err := sourceHasAudio(ctx, ffmpegPath, a.meta.Input)
		if err != nil {
			return nil, err
		}
		if hasAudio {
			audioSource = a.meta.Input
		} else {
			tb.warn("source has no audio track: the VP9 previews are muxed without sound")
		}
	}

	frames, err := runCropLoop(ctx, tb, a, p, cropDir, overlayDir, opts, PhasePreviewFrames)
	if err != nil {
		return nil, err
	}
	// A cancel that landed on the last decoded frame still wins: never publish
	// a preview for a pass that no longer owns the render slot.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	out := &PreviewResult{
		SettingsHash: p.SettingsHash(),
		MuxVideo:     mux,
		Width:        r.OutputW,
		Height:       r.OutputH,
		Frames:       frames,
		FPS:          planFPS(p),
	}

	// Mux each sequence into a NEW versioned file (requirement 11: never
	// overwrite a file a player may be streaming). The mux lands in scratch and
	// is moved in only once complete, so a cancelled mux never publishes a
	// half file.
	if wantCrop {
		v := nextPreviewVersion(a.PreviewsDir(), basename, r.OutputW, r.OutputH, PreviewCrop)
		final := filepath.Join(a.PreviewsDir(), previewFileName(basename, r.OutputW, r.OutputH, PreviewCrop, v, mux))
		tmp := filepath.Join(task.scratch, "crop"+previewFileExt(mux))
		if err := muxFrameSequence(ctx, ffmpegPath, cropDir, audioSource, out.FPS, tmp, mux, PhasePreviewMux, tb, frames); err != nil {
			return nil, err
		}
		if err := moveFile(tmp, final); err != nil {
			return nil, err
		}
		out.Version = v
		out.CropVideoPath = final
		if err := writePreviewSidecar(PreviewRef{
			Version: v, Kind: PreviewCrop, Path: final, MuxVideo: mux, SettingsHash: out.SettingsHash,
			CreatedAt: time.Now().UTC(), Width: r.OutputW, Height: r.OutputH, FPS: out.FPS, Frames: frames,
		}); err != nil {
			return nil, err
		}
	}
	if wantOverlay {
		v := nextPreviewVersion(a.PreviewsDir(), basename, r.OutputW, r.OutputH, PreviewOverlay)
		final := filepath.Join(a.PreviewsDir(), previewFileName(basename, r.OutputW, r.OutputH, PreviewOverlay, v, mux))
		tmp := filepath.Join(task.scratch, "overlay"+previewFileExt(mux))
		if err := muxFrameSequence(ctx, ffmpegPath, overlayDir, audioSource, out.FPS, tmp, mux, PhasePreviewMux, tb, frames); err != nil {
			return nil, err
		}
		if err := moveFile(tmp, final); err != nil {
			return nil, err
		}
		if v > out.Version {
			out.Version = v
		}
		out.OverlayVideoPath = final
		if err := writePreviewSidecar(PreviewRef{
			Version: v, Kind: PreviewOverlay, Path: final, MuxVideo: mux, SettingsHash: out.SettingsHash,
			CreatedAt: time.Now().UTC(), Width: r.OutputW, Height: r.OutputH, FPS: out.FPS, Frames: frames,
		}); err != nil {
			return nil, err
		}
	}

	removed, err := sweepPreviews(a.PreviewsDir())
	if err != nil {
		return nil, err
	}
	out.Removed = removed
	out.Warnings = tb.Warnings()
	out.ElapsedSeconds = time.Since(start).Seconds()
	return out, nil
}

// ---------------------------------------------------------------------------
// The crop loop (shared by previews and the final render)

// runCropLoop decodes the cached analysis video and writes the planned crops.
// cropDir receives the plain crop sequence; overlayDir receives the same
// frames with the framing boxes drawn. The sequences are the SCHEDULED frames:
// file names are 1-based output frame numbers, so the image2 demuxer plays
// them at the render clock.
func runCropLoop(ctx context.Context, tb *taskBase, a *Analysis, p *Plan, cropDir, overlayDir string, opts PreviewOptions, phase Phase) (int, error) {
	r := p.resolved
	style := defaultOverlayStyle()
	return plannedFrames(ctx, tb, a, p, a.frameSchedule(p), phase, func(outIdx, srcIdx int, out *image.RGBA) error {
		if cropDir != "" {
			if err := writeJPEG(cropDir, outIdx+1, out); err != nil {
				return err
			}
		}
		if overlayDir != "" {
			overlay := image.NewRGBA(image.Rect(0, 0, r.OutputW, r.OutputH))
			copyRGBA(overlay, out)
			drawOverlayBoxes(overlay, p.records[srcIdx], opts, style)
			if err := writeJPEG(overlayDir, outIdx+1, overlay); err != nil {
				return err
			}
		}
		return nil
	})
}

// plannedFrames is the frame loop every output pass shares: it decodes the
// cached analysis video, applies the Plan's framing (content-aware crop or
// fit-with-bars) and hands each finished output-resolution frame to fn in
// order. One Plan record per decoded frame: a decode/frame-count mismatch is
// an error, never a silent truncation. The Plan's frames are played out on the
// schedule, so an output pass writes renderFPS seconds-worth of frames even
// when the analysis clock is different. fn owns what the frame becomes -
// a JPEG file (runCropLoop) or a MikV frame payload (RenderMikV) - and is told
// both the output frame it is writing and which analysis frame it replays.
//
// fn receives the same image for every output slot that repeats one analysis
// frame: it must consume it (encode or copy), never keep it.
func plannedFrames(ctx context.Context, tb *taskBase, a *Analysis, p *Plan, sched frameSchedule, phase Phase, fn func(outIdx, srcIdx int, out *image.RGBA) error) (int, error) {
	video, err := vidio.NewVideo(a.AnalysisVideoPath())
	if err != nil {
		return 0, fmt.Errorf("holocropper: open analysis video %s: %w", a.AnalysisVideoPath(), err)
	}
	defer video.Close()

	w, h := video.Width(), video.Height()
	if w != a.meta.AnalysisW || h != a.meta.AnalysisH {
		return 0, fmt.Errorf("holocropper: analysis video is %dx%d, cache metadata says %dx%d",
			w, h, a.meta.AnalysisW, a.meta.AnalysisH)
	}
	buf := image.NewRGBA(image.Rect(0, 0, w, h))
	if err := video.SetFrameBuffer(buf.Pix); err != nil {
		return 0, err
	}
	// The schedule is built from the cache metadata's fps; the file's own frame
	// rate has to agree, or the resample would be steering by the wrong clock.
	if vFPS := video.FPS(); sched.AnalysisFPS > 0 && vFPS > 0 && vFPS != sched.AnalysisFPS {
		tb.warn(fmt.Sprintf("analysis video reports %g fps, its cache metadata says %g: the frame schedule follows the metadata",
			vFPS, sched.AnalysisFPS))
	}
	if sched.Resampled {
		tb.warn(fmt.Sprintf("the analysis runs at %g fps and the plan replays at %g: %d analysis frames become %d output frames",
			sched.AnalysisFPS, sched.RenderFPS, len(p.records), sched.Frames))
	}

	r := p.resolved
	tb.setPhase(phase, sched.Frames)
	out := 0
	decoded := 0
	for src := 0; video.Read(); src++ {
		decoded++
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if src >= len(p.records) {
			return out, fmt.Errorf("holocropper: analysis video has more frames than the plan (%d > %d)",
				src+1, len(p.records))
		}
		// A frame the schedule never plays is still decoded - the decode count
		// is what proves the cache is the one the Plan was built from.
		if out >= sched.Frames || sched.SrcFor[out] != src {
			continue
		}
		var frame *image.RGBA
		if p.settings.FitWithBars {
			// Fit mode (§6.10): no crop - the whole analysis frame is
			// scaled aspect-preserving into the output on black bars.
			frame = fitResizeTo(buf, r.OutputW, r.OutputH)
		} else {
			cropped := cropToBBoxCalc(buf, p.cropRectAt(src))
			frame = fillResizeTo(cropped, r.OutputW, r.OutputH)
		}
		for out < sched.Frames && sched.SrcFor[out] == src {
			if err := fn(out, src, frame); err != nil {
				return out, err
			}
			out++
			tb.setDone(out)
		}
	}
	if decoded != len(p.records) {
		return out, fmt.Errorf("holocropper: analysis video decoded %d frames, plan has %d - cache is stale",
			decoded, len(p.records))
	}
	if out != sched.Frames {
		return out, fmt.Errorf("holocropper: the frame schedule wanted %d output frames, the pass wrote %d",
			sched.Frames, out)
	}
	return out, nil
}

// writeJPEG writes frame N (1-based, the name the ffmpeg image2 demuxer
// expects) into dir.
func writeJPEG(dir string, frameNum int, img image.Image) error {
	f, err := os.Create(filepath.Join(dir, strconv.Itoa(frameNum)+".jpg"))
	if err != nil {
		return err
	}
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: renderJPEGQuality}); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func copyRGBA(dst, src *image.RGBA) {
	copy(dst.Pix, src.Pix)
}

// muxFrameSequence turns a numbered JPEG sequence into a video file. The mux
// picks what the container holds: mjpeg (the engine default, and the PC
// render's own codec - the sequence is put into an MP4 the way the CLI mux does
// it) or vp9-webm (VP9 video in WebM, the pairing every browser decodes).
// audioSource != "" brings that file's audio track along: copied through
// untouched for mjpeg, re-encoded as Opus for vp9-webm, which is the audio
// codec a WebM container - and every browser reading it - accepts.
func muxFrameSequence(ctx context.Context, ffmpegPath, framesDir, audioSource string, fps float64, outPath string, mux PreviewMux, phase Phase, tb *taskBase, total int) error {
	args := []string{
		"-hide_banner",
		"-r", strconv.FormatFloat(fps, 'f', -1, 64),
		"-framerate", strconv.FormatFloat(fps, 'f', -1, 64),
		"-i", filepath.Join(framesDir, "%d.jpg"),
	}
	if audioSource != "" {
		args = append(args, "-i", audioSource)
	}
	args = append(args, "-map", "0:v:0")
	if mux == PreviewMuxVP9WebM {
		// Constant-quality VP9 with a deadline the preview pass can actually
		// afford: a preview is a framing check, not an archive master.
		//
		// -g is not cosmetics. Chromium/blink's VP9 decoder refuses some
		// previews outright (MEDIA_ERR_DECODE straight after
		// loadedmetadata, while every other decoder - libvpx itself
		// included - reads the same bytes fine), and the refusal follows
		// the keyframe interval, not the encoder settings: measured on the
		// same 96-frame JPEG preview content at 24 fps, a GOP of 24 frames
		// or less plays, a GOP of 48 or more does not, and clips of 48
		// frames play at any interval. The encode therefore caps its key
		// interval at ONE second of frames - half of libvpx's default 128 -
		// and the preview stays playable by construction instead of by
		// luck.
		args = append(args, "-c:v", "libvpx-vp9", "-pix_fmt", "yuv420p",
			"-crf", strconv.Itoa(previewVP9CRF), "-b:v", "0",
			"-deadline", "good", "-cpu-used", strconv.Itoa(previewVP9CPUUsed),
			"-g", strconv.Itoa(previewGOPFrames(fps)))
	} else {
		args = append(args, "-c:v", "mjpeg", "-q:v", "6")
	}
	if audioSource != "" {
		args = append(args, "-map", "1:a:0")
		if mux == PreviewMuxVP9WebM {
			args = append(args, "-c:a", "libopus", "-b:a", "128k")
		} else {
			args = append(args, "-c:a", "copy")
		}
	}
	args = append(args, outPath, "-y")

	tb.setPhase(phase, total)
	return runFFmpegProgress(ctx, ffmpegPath, args, phase, tb)
}

// ---------------------------------------------------------------------------
// Preview naming, bookkeeping, sweep

// previewFileExt is the container extension a mux writes. ffmpeg picks the
// muxer from the output file's extension, so the name is not cosmetic: it is
// what decides the container.
func previewFileExt(mux PreviewMux) string {
	if mux == PreviewMuxVP9WebM {
		return ".webm"
	}
	return ".mp4"
}

// previewFileName is the versioned servable name. Crop keeps the bare
// -v<N> suffix, the overlay video carries -overlay-v<N>; both are dimension-
// tagged so different output targets never collide, and both carry the mux's
// extension so the file on disk says what it holds.
func previewFileName(basename string, w, h int, kind PreviewKind, version int, mux PreviewMux) string {
	ext := previewFileExt(mux)
	if kind == PreviewOverlay {
		return fmt.Sprintf("%s-%dx%d-overlay-v%d%s", basename, w, h, version, ext)
	}
	return fmt.Sprintf("%s-%dx%d-v%d%s", basename, w, h, version, ext)
}

func previewSidecarPath(videoPath string) string { return videoPath + ".meta.json" }

// nextPreviewVersion is the monotonic per-kind version: one above the highest
// version on disk for that kind and target size, so numbering survives
// restarts.
func nextPreviewVersion(dir, basename string, w, h int, kind PreviewKind) int {
	refs, err := readPreviewRefs(dir)
	if err != nil {
		return 1
	}
	prefix := basename + fmt.Sprintf("-%dx%d", w, h)
	best := 0
	for _, ref := range refs {
		if ref.Kind == kind && strings.HasPrefix(filepath.Base(ref.Path), prefix) && ref.Version > best {
			best = ref.Version
		}
	}
	return best + 1
}

func writePreviewSidecar(ref PreviewRef) error {
	data, err := json.MarshalIndent(ref, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(previewSidecarPath(ref.Path), data, 0o644)
}

// readPreviewRefs decodes every sidecar in a previews dir. Sidecars that do
// not parse, or that describe a deleted video, are dropped with their file.
func readPreviewRefs(dir string) ([]PreviewRef, error) {
	// A sidecar is named after the video it describes, so it carries that
	// mux's extension: <name>.mp4.meta.json, or <name>.webm.meta.json for the
	// vp9-webm mux.
	var matches []string
	for _, pattern := range []string{"*.mp4.meta.json", "*.webm.meta.json"} {
		got, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil {
			return nil, err
		}
		matches = append(matches, got...)
	}
	var out []PreviewRef
	for _, sidecar := range matches {
		video := strings.TrimSuffix(sidecar, ".meta.json")
		if _, err := os.Stat(video); err != nil {
			os.Remove(sidecar)
			continue
		}
		data, err := os.ReadFile(sidecar)
		if err != nil {
			continue
		}
		var ref PreviewRef
		if err := json.Unmarshal(data, &ref); err != nil {
			os.Remove(sidecar)
			continue
		}
		ref.Path = video // the sidecar's own location is the authority
		out = append(out, ref)
	}
	return out, nil
}

// Previews lists this Analysis's retained preview videos, newest first. A UI
// compares the top entry's SettingsHash with its own Plan to skip a redundant
// render (requirement 11's skip test).
func (a *Analysis) Previews() []PreviewRef {
	refs, err := readPreviewRefs(a.PreviewsDir())
	if err != nil {
		return nil
	}
	sortPreviewRefs(refs)
	return refs
}

func sortPreviewRefs(refs []PreviewRef) {
	sort.Slice(refs, func(i, j int) bool {
		if !refs[i].CreatedAt.Equal(refs[j].CreatedAt) {
			return refs[i].CreatedAt.After(refs[j].CreatedAt)
		}
		if refs[i].Version != refs[j].Version {
			return refs[i].Version > refs[j].Version
		}
		return refs[i].Kind < refs[j].Kind
	})
}

// sweepPreviews keeps the newest previewRetention versions per kind and
// returns the video paths it deleted.
func sweepPreviews(dir string) ([]string, error) {
	refs, err := readPreviewRefs(dir)
	if err != nil {
		return nil, err
	}
	byKind := map[PreviewKind][]PreviewRef{}
	for _, ref := range refs {
		byKind[ref.Kind] = append(byKind[ref.Kind], ref)
	}
	var removed []string
	for _, group := range byKind {
		sortPreviewRefs(group) // newest first
		if len(group) <= previewRetention {
			continue
		}
		for _, ref := range group[previewRetention:] {
			if err := os.Remove(ref.Path); err != nil && !os.IsNotExist(err) {
				return removed, fmt.Errorf("holocropper: preview sweep %s: %w", ref.Path, err)
			}
			os.Remove(previewSidecarPath(ref.Path))
			removed = append(removed, ref.Path)
		}
	}
	sort.Strings(removed)
	return removed, nil
}

// moveFile relocates a finished artifact into its published location, falling
// back to copy+remove when scratch and destination are on different devices.
// renderOutputName picks the final production file name: the API default
// tags it with the output dimensions; req.OutputName (validated bare
// .mp4) overrides with a fixed name, which is how the CLI keeps its legacy
// <basename>.mp4 naming - renders onto the same name overwrite, as before.
func renderOutputName(basename string, w, h int, override string) string {
	if override != "" {
		return override
	}
	return fmt.Sprintf("%s-%dx%d.mp4", basename, w, h)
}

func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return err
	}
	return os.Remove(src)
}

// ---------------------------------------------------------------------------
// RenderFinal (story B2)

// RenderFinal starts the production render for p: the planned crops at the
// RenderFinal starts the final render of p at the Plan's output resolution,
// muxed with the source audio track, written to req.OutputDir. The final
// file is <basename>-<WxH>.mp4 (dimension-tagged, so re-rendering at
// another target never clobbers a previous production file) unless
// req.OutputName overrides it with a fixed name - the CLI keeps its legacy
// <basename>.mp4 naming that way, overwrite semantics included.
//
// It shares the Analysis render slot with RenderPreview: whichever starts
// later supersedes the other.
func (a *Analysis) RenderFinal(ctx context.Context, p *Plan, req RenderRequest) (*RenderTask, error) {
	if errs := req.Validate(); len(errs) > 0 {
		return nil, &SettingsError{Errors: errs}
	}
	if err := a.planFits(p); err != nil {
		return nil, err
	}
	outDir, err := convertToAbsolutePath(req.OutputDir)
	if err != nil {
		return nil, fmt.Errorf("holocropper: render output dir: %w", err)
	}
	if st, err := os.Stat(outDir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("holocropper: render output dir %s is not a directory", outDir)
	}

	task := &RenderTask{
		taskBase: newTaskBase(ctx, TaskRenderFinal, req.Observer),
		keep:     req.KeepWorkDir,
	}
	task.scratch = renderScratchDir(req.TempDir, task.ID())
	a.renderSlot.install(task.taskBase)
	go runRenderFinal(a, task, p, req, outDir)
	return task, nil
}

func runRenderFinal(a *Analysis, task *RenderTask, p *Plan, req RenderRequest, outDir string) {
	tb := task.taskBase
	defer func() {
		if r := recover(); r != nil {
			tb.finish(fmt.Errorf("holocropper: render pass panicked: %v", r))
		}
	}()
	tb.begin()

	res, err := renderFinalWork(task.ctx, a, task, p, req, outDir)
	if err != nil {
		task.cleanupScratch()
		tb.finish(err)
		return
	}
	if task.keep {
		res.WorkDir = task.scratch
	} else {
		task.cleanupScratch()
		res.WorkDir = ""
	}
	task.mu.Lock()
	task.result = res
	task.mu.Unlock()
	tb.finish(nil)
}

func (t *RenderTask) cleanupScratch() {
	if t.scratch != "" {
		os.RemoveAll(t.scratch)
	}
}

func renderFinalWork(ctx context.Context, a *Analysis, task *RenderTask, p *Plan, req RenderRequest, outDir string) (*Result, error) {
	tb := task.taskBase
	r := p.resolved
	start := time.Now()

	framesDir := filepath.Join(task.scratch, "frames")
	if err := os.MkdirAll(framesDir, 0o755); err != nil {
		return nil, err
	}

	frames, err := runCropLoop(ctx, tb, a, p, framesDir, "", PreviewOptions{Kind: PreviewCrop}, PhaseRenderFrames)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	basename := getBasenameWithoutExt(a.meta.Input)
	outPath := filepath.Join(outDir, renderOutputName(basename, r.OutputW, r.OutputH, req.OutputName))
	tmp := filepath.Join(task.scratch, "final.mp4")
	// The PC product keeps the CLI's mux: mjpeg video with the source audio
	// copied through. That pairing IS the format the UMP PC player takes.
	if err := muxFrameSequence(ctx, req.FFmpegPath, framesDir, a.meta.Input, planFPS(p), tmp, PreviewMuxMJPEG, PhaseRenderMux, tb, frames); err != nil {
		return nil, err
	}
	if err := moveFile(tmp, outPath); err != nil {
		return nil, err
	}

	res := &Result{
		InputPath:       a.meta.Input,
		OutputPath:      outPath,
		WorkDir:         task.scratch,
		SourceW:         r.SourceW,
		SourceH:         r.SourceH,
		AnalysisW:       r.AnalysisW,
		AnalysisH:       r.AnalysisH,
		OutputW:         r.OutputW,
		OutputH:         r.OutputH,
		AppliedSettings: r,
		Frames:          frames,
		FPS:             planFPS(p),
		ElapsedSeconds:  time.Since(start).Seconds(),
	}
	if stat := p.Orientation(); stat.FramesExamined > 0 || stat.Flip {
		res.Orientation = &stat
	}
	tb.mu.Lock()
	res.Warnings = append(res.Warnings, tb.warnings...)
	tb.mu.Unlock()
	return res, nil
}

// ---------------------------------------------------------------------------
// RenderWhole (one-shot convenience)

// RenderWhole runs the whole chain - Analyze (cache-aware), NewPlan,
// RenderFinal - and blocks until the production file exists. This is the
// non-interactive path: no previews, no partial streams, no spinner. The
// analysis cache lives under req.TempDir, the render scratch under
// renderReq.TempDir.
func RenderWhole(ctx context.Context, req AnalyzeRequest, settings Settings, renderReq RenderRequest) (*Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if errs := req.Validate(); len(errs) > 0 {
		return nil, &SettingsError{Errors: errs}
	}
	if errs := settings.Validate(); len(errs) > 0 {
		return nil, &SettingsError{Errors: errs}
	}
	if errs := renderReq.Validate(); len(errs) > 0 {
		return nil, &SettingsError{Errors: errs}
	}
	// Fit settings never touch the image, so they need no detection: a
	// one-shot fit render picks the fit analysis mode unless the caller named
	// one explicitly.
	if req.Mode == "" && settings.FitWithBars {
		req.Mode = AnalyzeFit
	}

	task, err := Analyze(ctx, req)
	if err != nil {
		return nil, err
	}
	a, err := task.Wait()
	if err != nil {
		return nil, err
	}
	plan, err := NewPlan(a.Stream(), settings)
	if err != nil {
		return nil, err
	}
	render, err := a.RenderFinal(ctx, plan, renderReq)
	if err != nil {
		return nil, err
	}
	return render.Wait()
}
