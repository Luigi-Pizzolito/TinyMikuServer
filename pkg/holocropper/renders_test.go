package holocropper

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	vidio "github.com/AlexEidt/Vidio"
)

/* Render pass tests (stories B1/B2). ffmpeg-gated like the analysis tests.

The chain under test is Analyze -> NewPlan -> RenderPreview / RenderFinal:
versioned preview files with the retention sweep, supersede on the shared
render slot, cancel mid-render leaving nothing published, scratch isolation
under TempDir, and the final render's output naming, audio track and frame
determinism. */

// setupRenderFixture runs the analysis pass once and returns the cached
// Analysis plus a Plan at the requested output size.
func setupRenderFixture(t *testing.T, ffmpegPath string, outW, outH int) (*Analysis, *Plan) {
	t.Helper()
	inputDir, tempDir := t.TempDir(), t.TempDir()
	fixture := buildFixture(t, ffmpegPath, inputDir)
	task, err := Analyze(context.Background(), AnalyzeRequest{Input: fixture, TempDir: tempDir, FFmpegPath: ffmpegPath})
	if err != nil {
		t.Fatal(err)
	}
	a, err := task.Wait()
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewPlan(a.Stream(), Settings{Width: outW, Height: outH, FPS: fixtureFPS})
	if err != nil {
		t.Fatal(err)
	}
	return a, p
}

func globRenderScratch(t *testing.T, tempDir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(mustAbs(tempDir), "holocropper-render-*"))
	if err != nil {
		t.Fatalf("scratch glob: %v", err)
	}
	return matches
}

// frameDigest decodes a video and folds every frame buffer into one hash:
// container-independent frame determinism.
func frameDigest(t *testing.T, path string) (frames, w, h int, digest string) {
	t.Helper()
	v, err := vidio.NewVideo(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer v.Close()
	w, h = v.Width(), v.Height()
	if w <= 0 || h <= 0 {
		t.Fatalf("%s has no video stream", path)
	}
	buf := image.NewRGBA(image.Rect(0, 0, w, h))
	if err := v.SetFrameBuffer(buf.Pix); err != nil {
		t.Fatalf("frame buffer %s: %v", path, err)
	}
	sum := sha256.New()
	for v.Read() {
		frames++
		sum.Write(buf.Pix)
	}
	return frames, w, h, hex.EncodeToString(sum.Sum(nil))
}

// hasAudioStream reports whether ffprobe sees an audio track. It skips the
// assertion when ffprobe is unavailable.
func hasAudioStream(t *testing.T, path string) bool {
	t.Helper()
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Logf("ffprobe unavailable; audio track assertion skipped for %s", path)
		return true
	}
	out, err := exec.Command(ffprobe, "-v", "error", "-select_streams", "a",
		"-show_entries", "stream=codec_type", "-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var parsed struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("ffprobe json for %s: %v (%s)", path, err, out)
	}
	return len(parsed.Streams) > 0
}

// waitForRenderFrame blocks until the render pass is visibly writing frames,
// until it reaches a terminal state (test failure: the pass beat us to it),
// or until the deadline passes.
func waitForRenderFrame(t *testing.T, tb *taskBase) error {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		info := tb.Progress()
		if info.Phase == PhasePreviewFrames && info.Done > 0 {
			return nil
		}
		if info.State == StateCompleted || info.State == StateFailed ||
			info.State == StateCancelled || info.State == StateSuperseded {
			return fmt.Errorf("render reached %s before the test could act on it: %+v", info.State, info)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("render never reached the frame phase: %+v", info)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestRenderPreviewVersionsAndSweep is the story B1 acceptance run: a preview
// pair at output resolution, monotonic versions per kind, never overwritten,
// the newest-3 retention sweep, and Previews() answering the skip question.
func TestRenderPreviewVersionsAndSweep(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	a, p := setupRenderFixture(t, ffmpeg, 240, 240)
	total := a.Stream().Len()

	first, err := a.RenderPreview(context.Background(), p, PreviewRequest{FFmpegPath: ffmpeg})
	if err != nil {
		t.Fatal(err)
	}
	res, err := first.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != 1 {
		t.Errorf("first preview version = %d, want 1", res.Version)
	}
	if res.SettingsHash != p.SettingsHash() {
		t.Errorf("preview SettingsHash %s, plan %s", res.SettingsHash, p.SettingsHash())
	}
	if res.Width != 240 || res.Height != 240 {
		t.Errorf("preview resolution %dx%d, want the output target 240x240", res.Width, res.Height)
	}
	if res.Frames != total {
		t.Errorf("preview frames %d, analysis has %d", res.Frames, total)
	}
	if res.FPS != fixtureFPS {
		t.Errorf("preview fps %v, want %v", res.FPS, fixtureFPS)
	}
	if res.CropVideoPath == "" || res.OverlayVideoPath == "" {
		t.Fatalf("crop_overlay preview must produce both videos: %+v", res)
	}
	if filepath.Dir(res.CropVideoPath) != a.PreviewsDir() || filepath.Dir(res.OverlayVideoPath) != a.PreviewsDir() {
		t.Errorf("preview files must live in %s: %+v", a.PreviewsDir(), res)
	}
	if base := filepath.Base(res.CropVideoPath); base != "fixture-240x240-v1.mp4" {
		t.Errorf("crop preview name %s", base)
	}
	if base := filepath.Base(res.OverlayVideoPath); base != "fixture-240x240-overlay-v1.mp4" {
		t.Errorf("overlay preview name %s", base)
	}
	for _, path := range []string{res.CropVideoPath, res.OverlayVideoPath} {
		frames, w, h, _ := frameDigest(t, path)
		if frames != total || w != 240 || h != 240 {
			t.Errorf("preview %s decoded %d frames at %dx%d, want %d at 240x240", path, frames, w, h, total)
		}
	}
	if left := globRenderScratch(t, a.tempDir); len(left) != 0 {
		t.Errorf("preview scratch not cleaned up: %v", left)
	}

	refs := a.Previews()
	if len(refs) != 2 {
		t.Fatalf("Previews() returned %d refs: %+v", len(refs), refs)
	}
	for _, ref := range refs {
		if ref.Version != 1 || ref.SettingsHash != p.SettingsHash() || ref.Frames != total {
			t.Errorf("preview ref wrong: %+v", ref)
		}
		if _, err := os.Stat(ref.Path); err != nil {
			t.Errorf("preview ref %s: %v", ref.Path, err)
		}
		if _, err := os.Stat(previewSidecarPath(ref.Path)); err != nil {
			t.Errorf("preview sidecar for %s: %v", ref.Path, err)
		}
	}
	if refs[0].Kind == refs[1].Kind {
		t.Errorf("a crop_overlay render must produce one crop and one overlay ref: %+v", refs)
	}

	// Re-render with different settings: new version numbers, old files intact
	// (requirement 11: never overwrite).
	oldCrop, oldOverlay := res.CropVideoPath, res.OverlayVideoPath
	settings := Settings{Width: 240, Height: 240, FPS: fixtureFPS, ZoomMargin: 1.3}
	for i := 0; i < 3; i++ {
		settings.ZoomMargin = 1.3 + float64(i)*0.1
		pAlt, err := NewPlan(a.Stream(), settings)
		if err != nil {
			t.Fatal(err)
		}
		t2, err := a.RenderPreview(context.Background(), pAlt, PreviewRequest{FFmpegPath: ffmpeg})
		if err != nil {
			t.Fatal(err)
		}
		r2, err := t2.Wait()
		if err != nil {
			t.Fatal(err)
		}
		want := i + 2
		if r2.Version != want {
			t.Errorf("render %d version = %d, want %d", i+2, r2.Version, want)
		}
		if r2.SettingsHash != pAlt.SettingsHash() {
			t.Errorf("render %d settings hash mismatch", i+2)
		}
	}

	// Sweep: 4 renders produced v1..v4 per kind; only the newest 3 survive.
	if _, err := os.Stat(oldCrop); !os.IsNotExist(err) {
		t.Errorf("v1 crop preview survived the sweep (%v)", err)
	}
	if _, err := os.Stat(oldOverlay); !os.IsNotExist(err) {
		t.Errorf("v1 overlay preview survived the sweep (%v)", err)
	}
	refs = a.Previews()
	byKind := map[PreviewKind][]PreviewRef{}
	for _, ref := range refs {
		byKind[ref.Kind] = append(byKind[ref.Kind], ref)
	}
	for kind, group := range byKind {
		if len(group) != previewRetention {
			t.Errorf("kind %s kept %d previews, want %d", kind, len(group), previewRetention)
		}
		for i, ref := range group {
			want := 4 - i
			if ref.Version != want {
				t.Errorf("kind %s ref %d version %d, want %d (newest first)", kind, i, ref.Version, want)
			}
			if _, err := os.Stat(ref.Path); err != nil {
				t.Errorf("retained preview %s missing: %v", ref.Path, err)
			}
		}
	}

	// The skip test a UI runs: newest preview already matches its settings.
	newest := a.Previews()[0]
	if newest.SettingsHash == "" {
		t.Error("newest preview carries no settings hash")
	}
	if newest.SettingsHash == p.SettingsHash() {
		t.Error("the newest preview should reflect the latest settings, not the first plan")
	}
}

// TestRenderPreviewKindCropOnly: kind=crop produces only the crop video.
func TestRenderPreviewKindCropOnly(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	a, p := setupRenderFixture(t, ffmpeg, 240, 240)

	task, err := a.RenderPreview(context.Background(), p, PreviewRequest{
		FFmpegPath: ffmpeg,
		Options:    PreviewOptions{Kind: PreviewCrop},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := task.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if res.CropVideoPath == "" {
		t.Error("crop preview missing")
	}
	if res.OverlayVideoPath != "" {
		t.Errorf("kind=crop produced an overlay video: %s", res.OverlayVideoPath)
	}
	refs := a.Previews()
	if len(refs) != 1 || refs[0].Kind != PreviewCrop {
		t.Errorf("Previews() = %+v, want one crop ref", refs)
	}

	// An overlay-only render draws both box families by default.
	task2, err := a.RenderPreview(context.Background(), p, PreviewRequest{
		FFmpegPath: ffmpeg,
		Options:    PreviewOptions{Kind: PreviewOverlay},
	})
	if err != nil {
		t.Fatal(err)
	}
	res2, err := task2.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if res2.OverlayVideoPath == "" || res2.CropVideoPath != "" {
		t.Errorf("kind=overlay produced the wrong pair: %+v", res2)
	}
	if res2.Version != 1 {
		t.Errorf("overlay-only render version = %d, want 1 (crop kind has its own counter)", res2.Version)
	}

	// Invalid kind surfaces as a field error, not a Task.
	if _, err := a.RenderPreview(context.Background(), p, PreviewRequest{
		Options: PreviewOptions{Kind: PreviewKind("sideways")},
	}); err == nil {
		t.Error("invalid preview kind: want a validation error")
	}
}

// TestRenderSlotSupersede: preview and final share one render slot per
// Analysis - the later call supersedes the running pass.
func TestRenderSlotSupersede(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	a, p := setupRenderFixture(t, ffmpeg, 240, 240)

	first, err := a.RenderPreview(context.Background(), p, PreviewRequest{FFmpegPath: ffmpeg})
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := a.RenderPreview(context.Background(), p, PreviewRequest{FFmpegPath: ffmpeg})
	if err != nil {
		t.Fatal(err)
	}

	_, err = first.Wait()
	var se *SupersededError
	if !errors.As(err, &se) {
		t.Fatalf("superseded preview: want *SupersededError, got %v", err)
	}
	if se.NewTaskID != replacement.ID() {
		t.Errorf("SupersededError.NewTaskID = %d, want %d", se.NewTaskID, replacement.ID())
	}
	if first.State() != StateSuperseded {
		t.Errorf("superseded preview state = %s", first.State())
	}
	if a.Status().Render == nil {
		t.Error("Status().Render must report the running replacement")
	}

	res, err := replacement.Wait()
	if err != nil {
		t.Fatalf("replacement preview failed: %v", err)
	}
	if res.Version != 1 {
		t.Errorf("replacement version = %d, want 1 (the superseded pass published nothing)", res.Version)
	}
	if a.Status().Render != nil {
		t.Errorf("Status().Render after completion = %+v, want nil", a.Status().Render)
	}

	// A final render supersedes a running preview on the same Analysis.
	preview, err := a.RenderPreview(context.Background(), p, PreviewRequest{FFmpegPath: ffmpeg})
	if err != nil {
		t.Fatal(err)
	}
	final, err := a.RenderFinal(context.Background(), p, RenderRequest{
		OutputDir: t.TempDir(), TempDir: t.TempDir(), FFmpegPath: ffmpeg,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := preview.Wait(); !errors.As(err, &se) {
		t.Errorf("preview under a final render: want *SupersededError, got %v", err)
	}
	if _, err := final.Wait(); err != nil {
		t.Fatalf("final render failed: %v", err)
	}
}

// TestRenderCancelMidPreview: cancelling mid-crop publishes nothing and leaves
// no scratch, no partial files and no preview refs.
func TestRenderCancelMidPreview(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	// A larger target keeps the frame loop long enough to cancel inside it.
	a, p := setupRenderFixture(t, ffmpeg, 480, 480)

	task, err := a.RenderPreview(context.Background(), p, PreviewRequest{FFmpegPath: ffmpeg})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitForRenderFrame(t, task.taskBase); err != nil {
		t.Fatal(err)
	}
	task.Cancel()

	res, err := task.Wait()
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("want ErrCancelled, got %v", err)
	}
	if res != nil {
		t.Error("cancelled preview returned a result")
	}
	if task.State() != StateCancelled {
		t.Errorf("state = %s, want cancelled", task.State())
	}
	if got := a.Previews(); len(got) != 0 {
		t.Errorf("cancelled render published previews: %+v", got)
	}
	entries, err := os.ReadDir(a.PreviewsDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("previews dir not empty after cancel: %v", names)
	}
	if left := globRenderScratch(t, a.tempDir); len(left) != 0 {
		t.Errorf("cancel left render scratch behind: %v", left)
	}
	// The Analysis is still usable for a fresh render.
	again, err := a.RenderPreview(context.Background(), p, PreviewRequest{FFmpegPath: ffmpeg})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := again.Wait(); err != nil {
		t.Fatalf("render after a cancel failed: %v", err)
	}
	if again.Progress().ID == 0 {
		t.Error("render after cancel has no Task ID")
	}
}

// TestRenderRequestOutputNameValidation: OutputName must be a bare .mp4
// file name - it never steers the write outside OutputDir.
func TestRenderRequestOutputNameValidation(t *testing.T) {
	good := RenderRequest{OutputDir: "o", TempDir: "t", OutputName: "video.mp4"}
	if errs := good.Validate(); len(errs) != 0 {
		t.Errorf("valid OutputName rejected: %v", errs)
	}
	for _, bad := range []string{"video.avi", "../escape.mp4", "sub/dir.mp4", "/abs.mp4", ".mp4"} {
		r := RenderRequest{OutputDir: "o", TempDir: "t", OutputName: bad}
		errs := r.Validate()
		if len(errs) == 0 {
			t.Errorf("OutputName %q accepted, want a validation error", bad)
			continue
		}
		if errs[len(errs)-1].Field != "output_name" {
			t.Errorf("OutputName %q error field = %q, want output_name", bad, errs[len(errs)-1].Field)
		}
	}
}

// TestRenderFinalLegacyOutputName: the CLI-compat naming channel. A fixed
// OutputName lands exactly there and overwrites on the next render, like
// the legacy Crop path; the default stays dimension-tagged.
func TestRenderFinalLegacyOutputName(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	a, p := setupRenderFixture(t, ffmpeg, 240, 240)
	outDir, tempDir := t.TempDir(), t.TempDir()

	task, err := a.RenderFinal(context.Background(), p, RenderRequest{
		OutputDir: outDir, TempDir: tempDir, FFmpegPath: ffmpeg,
		OutputName: "fixture.mp4",
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := task.Wait()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(outDir, "fixture.mp4")
	if res.OutputPath != want {
		t.Fatalf("output path %s, want %s", res.OutputPath, want)
	}
	if st, err := os.Stat(want); err != nil || st.Size() == 0 {
		t.Fatalf("legacy-named output missing/empty: %v", err)
	}

	// Same name again (different target size): overwrite, no second file,
	// exactly like the legacy CLI re-cropping the same input.
	pWide, err := NewPlan(a.Stream(), Settings{Width: 320, Height: 320, FPS: fixtureFPS})
	if err != nil {
		t.Fatal(err)
	}
	task2, err := a.RenderFinal(context.Background(), pWide, RenderRequest{
		OutputDir: outDir, TempDir: tempDir, FFmpegPath: ffmpeg,
		OutputName: "fixture.mp4",
	})
	if err != nil {
		t.Fatal(err)
	}
	res2, err := task2.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if res2.OutputPath != want {
		t.Errorf("re-render path %s, want the same %s", res2.OutputPath, want)
	}
	files, _ := filepath.Glob(filepath.Join(outDir, "*.mp4"))
	if len(files) != 1 {
		t.Errorf("output dir has %v, want only the overwritten legacy file", files)
	}
}

// TestRenderFinalOutputAndDeterminism is the story B2 acceptance run: the
// dimension-tagged file, correct frame count and resolution, the source audio
// track copied through, cleaned scratch, and frame determinism across runs.
func TestRenderFinalOutputAndDeterminism(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	a, p := setupRenderFixture(t, ffmpeg, 240, 240)
	total := a.Stream().Len()
	outDir, tempDir := t.TempDir(), t.TempDir()

	task, err := a.RenderFinal(context.Background(), p, RenderRequest{
		OutputDir: outDir, TempDir: tempDir, FFmpegPath: ffmpeg,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := task.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if res.OutputPath != filepath.Join(outDir, "fixture-240x240.mp4") {
		t.Errorf("output path %s, want <basename>-<WxH>.mp4 in %s", res.OutputPath, outDir)
	}
	if res.WorkDir != "" {
		t.Errorf("WorkDir = %s, want empty after cleanup", res.WorkDir)
	}
	if left := globRenderScratch(t, tempDir); len(left) != 0 {
		t.Errorf("render scratch not cleaned: %v", left)
	}
	if res.Frames != total || res.FPS != fixtureFPS {
		t.Errorf("result frames %d fps %v, want %d / %v", res.Frames, res.FPS, total, fixtureFPS)
	}
	if res.OutputW != 240 || res.OutputH != 240 || res.AnalysisH != defaultAnalysisHeight || res.SourceW != fixtureW {
		t.Errorf("result geometry wrong: %+v", res)
	}
	if res.InputPath == "" {
		t.Error("result lost the input path")
	}

	frames, w, h, digest := frameDigest(t, res.OutputPath)
	if frames != total || w != 240 || h != 240 {
		t.Fatalf("output decoded %d frames at %dx%d, want %d at 240x240", frames, w, h, total)
	}
	if !hasAudioStream(t, res.OutputPath) {
		t.Error("final render dropped the source audio track")
	}

	// A re-render at another target does not clobber the first production file.
	pWide, err := NewPlan(a.Stream(), Settings{Width: 320, Height: 320, FPS: fixtureFPS})
	if err != nil {
		t.Fatal(err)
	}
	wideTask, err := a.RenderFinal(context.Background(), pWide, RenderRequest{
		OutputDir: outDir, TempDir: tempDir, FFmpegPath: ffmpeg,
	})
	if err != nil {
		t.Fatal(err)
	}
	wideRes, err := wideTask.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if wideRes.OutputPath == res.OutputPath {
		t.Fatal("a different target reused the same output file")
	}
	if _, err := os.Stat(res.OutputPath); err != nil {
		t.Errorf("the first production file vanished: %v", err)
	}

	// Frame determinism: re-rendering the same plan produces the same frames.
	sameDir := t.TempDir()
	again, err := a.RenderFinal(context.Background(), p, RenderRequest{
		OutputDir: sameDir, TempDir: tempDir, FFmpegPath: ffmpeg,
	})
	if err != nil {
		t.Fatal(err)
	}
	againRes, err := again.Wait()
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, digest2 := frameDigest(t, againRes.OutputPath)
	if digest != digest2 {
		t.Errorf("renders of the same plan produced different frames: %s vs %s", digest, digest2)
	}

	// KeepWorkDir leaves the scratch and reports it.
	keepDir := t.TempDir()
	keepTask, err := a.RenderFinal(context.Background(), p, RenderRequest{
		OutputDir: keepDir, TempDir: tempDir, FFmpegPath: ffmpeg, KeepWorkDir: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	keepRes, err := keepTask.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if keepRes.WorkDir == "" {
		t.Error("KeepWorkDir: result must report the work dir")
	}
	if st, err := os.Stat(keepRes.WorkDir); err != nil || !st.IsDir() {
		t.Errorf("KeepWorkDir: %s should still exist (%v)", keepRes.WorkDir, err)
	}
	if frames, err := filepath.Glob(filepath.Join(keepRes.WorkDir, "frames", "*.jpg")); err != nil || len(frames) != total {
		t.Errorf("KeepWorkDir: frames scratch has %d jpegs, want %d", len(frames), total)
	}

	// A missing output dir is a request error, not a crashed Task.
	if _, err := a.RenderFinal(context.Background(), p, RenderRequest{
		OutputDir: filepath.Join(outDir, "does-not-exist"), TempDir: tempDir,
	}); err == nil {
		t.Error("missing output dir: want an error")
	}
	if _, err := a.RenderFinal(context.Background(), p, RenderRequest{OutputDir: outDir}); err == nil {
		t.Error("missing temp dir: want a validation error")
	}
}

// TestRenderRejectsMismatchedPlan: a Plan from another analysis geometry (or a
// partial plan) never renders.
func TestRenderRejectsMismatchedPlan(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	a, p := setupRenderFixture(t, ffmpeg, 240, 240)

	// A plan replayed over a synthetic 428x240 analysis stream.
	other := newTestStream(walkingSamples(30, 100, 4, 120, 200), 428, 240, fixtureW, fixtureH)
	pOther, err := NewPlan(other, Settings{Width: 240, Height: 240, FPS: fixtureFPS})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.RenderPreview(context.Background(), pOther, PreviewRequest{}); !errors.Is(err, ErrStreamIncompatible) {
		t.Errorf("mismatched plan preview: want ErrStreamIncompatible, got %v", err)
	}
	// Same geometry, truncated replay: the frame count must match too.
	s := a.Stream()
	partial := newTestStream(walkingSamples(s.Len()/2, 100, 4, 120, 60), s.AnalysisWidth(), s.AnalysisHeight(), fixtureW, fixtureH)
	pPartial, err := NewPlan(partial, Settings{Width: 240, Height: 240, FPS: fixtureFPS})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.RenderPreview(context.Background(), pPartial, PreviewRequest{}); !errors.Is(err, ErrStreamIncompatible) {
		t.Errorf("partial plan render: want ErrStreamIncompatible, got %v", err)
	}
	// The matching plan passes the same gate.
	if err := a.planFits(p); err != nil {
		t.Errorf("planFits on the matching plan: %v", err)
	}
	if _, err := a.RenderFinal(context.Background(), pOther, RenderRequest{OutputDir: t.TempDir(), TempDir: t.TempDir()}); !errors.Is(err, ErrStreamIncompatible) {
		t.Errorf("mismatched plan render: want ErrStreamIncompatible, got %v", err)
	}
	if _, err := a.RenderPreview(context.Background(), nil, PreviewRequest{}); err == nil {
		t.Error("nil plan: want an error")
	}
}

// TestRenderWholeOneShot: Analyze -> NewPlan -> RenderFinal in one call, cache
// included (a second call reuses the analysis).
func TestRenderWholeOneShot(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	inputDir, tempDir, outDir := t.TempDir(), t.TempDir(), t.TempDir()
	fixture := buildFixture(t, ffmpeg, inputDir)

	analyze := AnalyzeRequest{Input: fixture, TempDir: tempDir, FFmpegPath: ffmpeg}
	render := RenderRequest{OutputDir: outDir, TempDir: tempDir, FFmpegPath: ffmpeg}
	settings := Settings{Width: 240, Height: 320, FPS: fixtureFPS}

	res, err := RenderWhole(context.Background(), analyze, settings, render)
	if err != nil {
		t.Fatal(err)
	}
	if res.OutputPath != filepath.Join(outDir, "fixture-240x320.mp4") {
		t.Errorf("output path %s", res.OutputPath)
	}
	frames, w, h, _ := frameDigest(t, res.OutputPath)
	if frames == 0 || w != 240 || h != 320 {
		t.Errorf("output decoded %d frames at %dx%d", frames, w, h)
	}
	if res.OutputW != 240 || res.OutputH != 320 {
		t.Errorf("result geometry %+v", res)
	}
	if left := globRenderScratch(t, tempDir); len(left) != 0 {
		t.Errorf("RenderWhole left scratch behind: %v", left)
	}

	// Second call hits the analysis cache (no re-detect) and re-renders.
	res2, err := RenderWhole(context.Background(), analyze, settings, render)
	if err != nil {
		t.Fatal(err)
	}
	if res2.OutputPath != res.OutputPath {
		t.Errorf("cached run changed the output path: %s vs %s", res2.OutputPath, res.OutputPath)
	}

	// Invalid settings surface before any pass runs.
	if _, err := RenderWhole(context.Background(), analyze, Settings{Width: 4, Height: 240, FPS: 20}, render); err == nil {
		t.Error("invalid settings: want a validation error")
	}
}

// TestDrawOverlayBoxes is the pure unit test behind the overlay previews:
// boxes land where the record says, out-of-frame geometry is clipped rather
// than dropped, and a lost subject draws nothing but the crop window.
func TestDrawOverlayBoxes(t *testing.T) {
	const w, h = 200, 100
	st := defaultOverlayStyle()
	st.StrokeFrac = 0.01 // 2 px strokes on a small test frame

	newFrame := func() *image.RGBA {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				img.Set(x, y, color.RGBA{0x20, 0x20, 0x20, 0xFF})
			}
		}
		return img
	}
	// pixelAt reads one pixel as plain RGBA for colour comparisons.
	pixelAt := func(img *image.RGBA, x, y int) color.RGBA {
		r, g, b, _ := img.At(x, y).RGBA()
		return color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), 0xFF}
	}

	rec := FrameRecord{
		Frame: 1,
		Out: SpaceRects{
			BBox: Rect{X: 20, Y: 20, W: 60, H: 40},
			EMA:  Rect{X: 32, Y: 26, W: 40, H: 28},
			Crop: Rect{X: 0, Y: 0, W: w, H: h},
		},
	}

	img := newFrame()
	drawOverlayBoxes(img, rec, PreviewOptions{DrawSubject: true, DrawCrop: true}, st)
	if got := pixelAt(img, 20, 40); got != st.Subject {
		t.Errorf("subject box left edge at (20,40) = %+v, want %+v", got, st.Subject)
	}
	if got := pixelAt(img, 79, 40); got != st.Subject {
		t.Errorf("subject box right edge at (79,40) = %+v, want %+v", got, st.Subject)
	}
	if got := pixelAt(img, 32, 26); got != st.EMA {
		t.Errorf("EMA box corner at (32,26) = %+v, want %+v", got, st.EMA)
	}
	if got := pixelAt(img, 100, 0); got != st.Crop {
		t.Errorf("crop window top edge at (100,0) = %+v, want %+v", got, st.Crop)
	}

	// Draw flags are honoured independently. The EMA box belongs to the
	// subject visualisation group: DrawSubject draws the raw bbox and the
	// smoothed box; DrawCrop alone draws neither.
	img = newFrame()
	drawOverlayBoxes(img, rec, PreviewOptions{DrawSubject: true}, st)
	if got := pixelAt(img, 100, 0); got == st.Crop {
		t.Error("crop window drawn with DrawCrop false")
	}
	if got := pixelAt(img, 32, 26); got != st.EMA {
		t.Errorf("EMA box missing with DrawSubject true: %+v", got)
	}
	img = newFrame()
	drawOverlayBoxes(img, rec, PreviewOptions{DrawCrop: true}, st)
	if got := pixelAt(img, 100, 0); got != st.Crop {
		t.Errorf("crop window missing with DrawCrop true: %+v", got)
	}
	if got := pixelAt(img, 20, 40); got == st.Subject {
		t.Error("subject box drawn with DrawSubject false")
	}
	if got := pixelAt(img, 32, 26); got == st.EMA {
		t.Error("EMA box drawn with DrawSubject false")
	}

	// A box hanging off the crop edge is drawn clipped, not dropped.
	recClipped := FrameRecord{
		Frame:    2,
		Subjects: 1,
		Out:      SpaceRects{BBox: Rect{X: -40, Y: 20, W: 60, H: 40}, Crop: Rect{X: 0, Y: 0, W: w, H: h}},
	}
	img = newFrame()
	drawOverlayBoxes(img, recClipped, PreviewOptions{DrawSubject: true}, st)
	if got := pixelAt(img, 0, 40); got != st.Subject {
		t.Errorf("clipped subject box not drawn at the frame edge: %+v", got)
	}
	if got := pixelAt(img, 19, 40); got != st.Subject {
		t.Errorf("clipped subject box right edge at (19,40) = %+v", got)
	}

	// Fully outside the frame: nothing drawn.
	recOutside := FrameRecord{Frame: 3, Out: SpaceRects{BBox: Rect{X: -100, Y: -100, W: 30, H: 30}}}
	img = newFrame()
	drawOverlayBoxes(img, recOutside, PreviewOptions{DrawSubject: true}, st)
	if got := pixelAt(img, 0, 0); got == st.Subject {
		t.Error("a fully out-of-frame box was drawn")
	}

	// Lost subject: no subject/EMA strokes, crop window still holds position.
	recLost := FrameRecord{Frame: 4, SubjectLost: true, Out: SpaceRects{Crop: Rect{X: 0, Y: 0, W: w, H: h}}}
	img = newFrame()
	drawOverlayBoxes(img, recLost, PreviewOptions{DrawSubject: true, DrawCrop: true}, st)
	if got := pixelAt(img, 20, 40); got == st.Subject {
		t.Error("lost subject still drew a bbox")
	}
	if got := pixelAt(img, 100, 0); got != st.Crop {
		t.Errorf("lost subject lost the crop window: %+v", got)
	}
}

// TestPreviewFileNameCoversKinds keeps the naming contract the sweep and the
// UI's player swap rely on.
func TestPreviewFileNameCoversKinds(t *testing.T) {
	cases := []struct {
		kind    PreviewKind
		version int
		mux     PreviewMux
		want    string
	}{
		{PreviewCrop, 1, "", "clip-648x200-v1.mp4"},
		{PreviewCropOverlay, 7, PreviewMuxMJPEG, "clip-648x200-v7.mp4"},
		{PreviewOverlay, 2, PreviewMuxMJPEG, "clip-648x200-overlay-v2.mp4"},
		// The extension is the mux's, and it is not cosmetic: it is what
		// tells ffmpeg (and every player) which container the bytes are.
		{PreviewCrop, 3, PreviewMuxVP9WebM, "clip-648x200-v3.webm"},
		{PreviewOverlay, 4, PreviewMuxVP9WebM, "clip-648x200-overlay-v4.webm"},
	}
	for _, c := range cases {
		if got := previewFileName("clip", 648, 200, c.kind, c.version, c.mux); got != c.want {
			t.Errorf("kind %s mux %q: got %s, want %s", c.kind, c.mux, got, c.want)
		}
	}
	// Version numbering is per kind and per target size - and per kind across
	// muxes, so a mux change lands as the next version, never over a file a
	// player may be streaming.
	dir := t.TempDir()
	writePreviewSidecar(PreviewRef{Version: 1, Kind: PreviewCrop, Path: filepath.Join(dir, "clip-240x240-v1.mp4")})
	writePreviewSidecar(PreviewRef{Version: 4, Kind: PreviewOverlay, Path: filepath.Join(dir, "clip-240x240-overlay-v4.mp4")})
	writePreviewSidecar(PreviewRef{Version: 2, Kind: PreviewCrop, MuxVideo: PreviewMuxVP9WebM, Path: filepath.Join(dir, "clip-240x240-v2.webm")})
	os.WriteFile(filepath.Join(dir, "clip-240x240-v1.mp4"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "clip-240x240-overlay-v4.mp4"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "clip-240x240-v2.webm"), []byte("x"), 0o644)
	if v := nextPreviewVersion(dir, "clip", 240, 240, PreviewCrop); v != 3 {
		t.Errorf("crop version numbering: got %d, want 3 (the webm preview counts)", v)
	}
	if v := nextPreviewVersion(dir, "clip", 240, 240, PreviewOverlay); v != 5 {
		t.Errorf("overlay version numbering: got %d, want 5", v)
	}
	if v := nextPreviewVersion(dir, "clip", 648, 200, PreviewCrop); v != 1 {
		t.Errorf("a new target size must start its own sequence, got %d", v)
	}
	// Both sidecar shapes are read, and a sidecar whose video is gone is
	// dropped rather than counted.
	if refs, err := readPreviewRefs(dir); err != nil || len(refs) != 3 {
		t.Fatalf("readPreviewRefs saw %d refs (%v), want all three muxes", len(refs), err)
	}
	os.Remove(filepath.Join(dir, "clip-240x240-v1.mp4"))
	if refs, err := readPreviewRefs(dir); err != nil || len(refs) != 2 {
		t.Errorf("stale sidecar cleanup: %d refs (%v)", len(refs), err)
	}
}

// probeMediaShape reports what ffprobe reads from a media file: the container
// format name, the first video stream's codec, and the first audio stream's
// codec ("" when the file carries no audio at all). The mux tests pin all
// three - a codec in the wrong container is as broken as the wrong codec.
func probeMediaShape(t *testing.T, path string) (format, videoCodec, audioCodec string) {
	t.Helper()
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Fatalf("ffprobe is not on PATH and this test pins what the mux wrote: %v", err)
	}
	out, err := exec.Command(ffprobe, "-v", "error",
		"-show_entries", "format=format_name",
		"-show_entries", "stream=codec_type,codec_name",
		"-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var parsed struct {
		Format struct {
			FormatName string `json:"format_name"`
		} `json:"format"`
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("ffprobe json for %s: %v (%s)", path, err, out)
	}
	for _, s := range parsed.Streams {
		switch s.CodecType {
		case "video":
			if videoCodec == "" {
				videoCodec = s.CodecName
			}
		case "audio":
			if audioCodec == "" {
				audioCodec = s.CodecName
			}
		}
	}
	if videoCodec == "" {
		t.Fatalf("%s has no video stream", path)
	}
	return parsed.Format.FormatName, videoCodec, audioCodec
}

// TestRenderPreviewMuxChoice pins the preview mux plane (Options.MuxVideo):
// the default stays the engine's own mjpeg-in-MP4 video-only file, vp9-webm
// writes VP9 video with the source's audio as Opus in a WebM container - the
// pairing every browser decodes - and every retained preview records which of
// the two it is, because a UI that skips a render on SettingsHash must also
// want the codec and container the retained file holds.
func TestRenderPreviewMuxChoice(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	a, p := setupRenderFixture(t, ffmpeg, 240, 240)

	// vp9-mp4 is NOT a choice: VP9's home in this engine is WebM, the container
	// the codec spike measured as decodable in WebKitGTK and Chromium alike.
	if errs := (PreviewRequest{Options: PreviewOptions{MuxVideo: "vp9-mp4"}}).Validate(); len(errs) != 1 ||
		errs[0].Field != "options.mux_video" {
		t.Errorf("mux_video validation said %+v, want one error on options.mux_video", errs)
	}

	// The default: unchanged for every existing caller - mjpeg in an MP4, no audio.
	defTask, err := a.RenderPreview(context.Background(), p, PreviewRequest{
		FFmpegPath: ffmpeg,
		Options:    PreviewOptions{Kind: PreviewCrop},
	})
	if err != nil {
		t.Fatal(err)
	}
	def, err := defTask.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if def.MuxVideo != PreviewMuxMJPEG {
		t.Errorf("default preview mux = %q, want %q", def.MuxVideo, PreviewMuxMJPEG)
	}
	if ext := filepath.Ext(def.CropVideoPath); ext != ".mp4" {
		t.Errorf("default preview name %s ends %s, want .mp4", def.CropVideoPath, ext)
	}
	if format, codec, audio := probeMediaShape(t, def.CropVideoPath); !strings.Contains(format, "mp4") ||
		codec != "mjpeg" || audio != "" {
		t.Errorf("default preview %s: format %q codec %q audio %q, want an mjpeg-only MP4",
			def.CropVideoPath, format, codec, audio)
	}

	// vp9-webm: a clip every browser plays, carrying the source's sound, as a
	// NEW versioned file next to the default one.
	vp9Task, err := a.RenderPreview(context.Background(), p, PreviewRequest{
		FFmpegPath: ffmpeg,
		Options:    PreviewOptions{Kind: PreviewCrop, MuxVideo: PreviewMuxVP9WebM},
	})
	if err != nil {
		t.Fatal(err)
	}
	vp9, err := vp9Task.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if vp9.MuxVideo != PreviewMuxVP9WebM {
		t.Errorf("vp9 preview mux = %q, want %q", vp9.MuxVideo, PreviewMuxVP9WebM)
	}
	if ext := filepath.Ext(vp9.CropVideoPath); ext != ".webm" {
		t.Errorf("vp9 preview name %s ends %s, want .webm", vp9.CropVideoPath, ext)
	}
	if vp9.Version <= def.Version {
		t.Errorf("vp9 preview landed as version %d after version %d: the mux choice must not overwrite a retained file",
			vp9.Version, def.Version)
	}
	format, codec, audio := probeMediaShape(t, vp9.CropVideoPath)
	if !strings.Contains(format, "webm") {
		t.Errorf("vp9 preview %s: container %q, want WebM", vp9.CropVideoPath, format)
	}
	if codec != "vp9" {
		t.Errorf("vp9 preview %s: video codec %q, want vp9", vp9.CropVideoPath, codec)
	}
	if audio != "opus" {
		t.Errorf("vp9 preview %s: audio codec %q, want the source's track as Opus", vp9.CropVideoPath, audio)
	}

	// The retained list carries the choice per file.
	refs := a.Previews()
	if len(refs) != 2 {
		t.Fatalf("Previews() returned %d refs, want both muxes retained: %+v", len(refs), refs)
	}
	seen := map[PreviewMux]int{}
	for _, ref := range refs {
		seen[ref.MuxVideo]++
	}
	if seen[PreviewMuxMJPEG] != 1 || seen[PreviewMuxVP9WebM] != 1 {
		t.Errorf("Previews() mux labels = %+v, want one mjpeg and one vp9-webm", seen)
	}
}
