package holocropper

/* §6.10 amendment pins (Horidashimi item 825): the fit mode measures the
frame's black container bars and cuts them BEFORE fitting. The unit test
proves the scanner and the per-side median law on synthetic pixels (no
ffmpeg); the fixture tests prove the end-to-end law on ffmpeg-authored
letterboxed and pillarboxed clips: the content rect lands on the stream, the
fit plan crops to it, the auto-orient scores the CONTENT aspect (not the
root container's), and the rendered preview pixels show the content filling
the output instead of shrinking under its own bars. Synthetic streams (no
analysis video) keep the shipped full-frame transform unchanged. */

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"math"
	"os/exec"
	"path/filepath"
	"testing"

	vidio "github.com/AlexEidt/Vidio"
)

// ---------------------------------------------------------------------------
// The scanner + medians on synthetic frames (no ffmpeg, no fixtures)

func solidFrame(w, h int) *image.RGBA {
	return image.NewRGBA(image.Rect(0, 0, w, h))
}

func fillRows(img *image.RGBA, y0, y1 int, c color.RGBA) {
	for y := y0; y < y1; y++ {
		for x := img.Bounds().Min.X; x < img.Bounds().Dx(); x++ {
			img.Set(x, y, c)
		}
	}
}

func letterFrame(w, h, topInset, botInset int) *image.RGBA {
	img := solidFrame(w, h)
	fillRows(img, topInset, h-botInset, color.RGBA{255, 255, 255, 255})
	return img
}

func TestBarInsetsAndMedians(t *testing.T) {
	// Letterboxed 100x80: rows 10..69 content, top/bottom 10 px bars.
	l, tr, r, b, ok := frameBarInsets(letterFrame(100, 80, 10, 10), 100, 80)
	if !ok || l != 0 || tr != 10 || r != 0 || b != 10 {
		t.Fatalf("letterbox insets = (%d,%d,%d,%d) ok=%v, want (0,10,0,10) true", l, tr, r, b, ok)
	}

	// A fully black frame is UNMEASURABLE (fade/leader), never "all bars".
	if _, _, _, _, ok := frameBarInsets(solidFrame(100, 80), 100, 80); ok {
		t.Fatal("an all-black frame measured as a content rect")
	}

	// Medians absorb a minority of bright-bar frames: 12 true letters + 1
	// frame with a light pixel riding in the top bar (measures 0 there).
	acc := newBarAccumulator(100, 80)
	for i := 0; i < 12; i++ {
		acc.add(letterFrame(100, 80, 10, 10))
	}
	stray := letterFrame(100, 80, 10, 10)
	stray.Set(50, 5, color.RGBA{200, 200, 200, 255})
	acc.add(stray)
	acc.add(solidFrame(100, 80)) // joins no sample
	cr, ok := acc.rect()
	if !ok {
		t.Fatal("13 measurable frames answered unmeasurable")
	}
	if cr != image.Rect(0, 10, 100, 70) {
		t.Errorf("median content rect = %v, want (0,10)-(100,70)", cr)
	}

	// Under barMinFrames measurable frames, no aggregate is trusted.
	thin := newBarAccumulator(100, 80)
	for i := 0; i < barMinFrames-1; i++ {
		thin.add(letterFrame(100, 80, 10, 10))
	}
	if _, ok := thin.rect(); ok {
		t.Error("a clip under the minimum sample count answered a content rect")
	}

	// No bars at all: the aggregate answers the full frame.
	clean := newBarAccumulator(100, 80)
	for i := 0; i < 12; i++ {
		clean.add(letterFrame(100, 80, 0, 0))
	}
	if cr, ok := clean.rect(); !ok || cr != image.Rect(0, 0, 100, 80) {
		t.Errorf("bar-free aggregate = %v ok=%v, want the full frame", cr, ok)
	}
}

// A synthetic stream (no analysis video to measure) keeps the shipped
// full-frame fit transform unchanged.
func TestFitPlanSyntheticStreamKeepsFullFrame(t *testing.T) {
	samples := walkingSamples(8, 100, 4, 120, 60)
	stream := newTestStream(samples, 854, 480, 1280, 720)

	p, err := NewFitPlan(stream, Settings{Width: 320, Height: 240, FPS: 20})
	if err != nil {
		t.Fatal(err)
	}
	r := p.Resolved()
	if r.ContentW != r.AnalysisW || r.ContentH != r.AnalysisH {
		t.Errorf("synthetic fit resolved content = %dx%d, want the full %dx%d frame",
			r.ContentW, r.ContentH, r.AnalysisW, r.AnalysisH)
	}
	if cr := p.cropRectAt(0); cr != image.Rect(0, 0, 854, 480) {
		t.Errorf("synthetic fit crop = %v, want the full analysis frame", cr)
	}
	// The shipped transform scale (computed on the whole frame) survives.
	scale := math.Min(320.0/854.0, 240.0/480.0)
	if got := p.Records()[0].Out.Crop; math.Abs(got.W-854*scale) > 1 || math.Abs(got.H-480*scale) > 1 {
		t.Errorf("synthetic fitted rect = %v, want the whole frame scaled by %.4f", got, scale)
	}
}

// ---------------------------------------------------------------------------
// Fixture law: ffmpeg-authored bar clips

// buildBarClipAt renders a bitexact lavfi clip of CONTENT WxH centered on
// black bars inside a 1280x720 root container - the shape the owner's test
// MV carries (the 825 law is about bars the CONTAINER adds, not about the
// hologram subject).
func buildBarClipAt(t *testing.T, ffmpegPath, dir string, contentW, contentH int) string {
	t.Helper()
	out := filepath.Join(dir, "bars.mp4")
	padX := (1280 - contentW) / 2
	padY := (720 - contentH) / 2
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("color=c=red:s=%dx%d:d=4:r=%d", contentW, contentH, fixtureFPS),
		"-vf", fmt.Sprintf("pad=1280:720:%d:%d:color=black,format=yuv420p", padX, padY),
		"-x264-params", "cpu-independent=1",
		"-sws_flags", "accurate_rnd+bitexact",
		"-c:v", "libx264", "-preset", "ultrafast",
		out,
	}
	cmd := exec.Command(ffmpegPath, args...)
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bar fixture build failed: %v\n%s", err, combined)
	}
	return out
}

// TestFitAnalysisCutsLetterboxBarsBeforeFitting is the 825 acceptance run: a
// 1280x544 red field letterboxed into 1280x720 (88 px top/bottom bars). The
// fit probe must measure the bars, the plan must crop to the content, and a
// preview at 720x360 must paint the CONTENT band where the fitted content
// sits - not the narrower band the shipped whole-frame transform produced.
func TestFitAnalysisCutsLetterboxBarsBeforeFitting(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	inputDir, tempDir := t.TempDir(), t.TempDir()
	fixture := buildBarClipAt(t, ffmpeg, inputDir, 1280, 544)

	a := analyzeWithMode(t, ffmpeg, fixture, tempDir, AnalyzeFit)
	st := a.Stream()

	p, err := NewFitPlan(st, Settings{Width: 720, Height: 360, FPS: 20})
	if err != nil {
		t.Fatal(err)
	}
	res := p.Resolved()

	// The content rect: full width, the 1280x544 aspect scaled to the
	// analysis height (544/720*480 = 362.7 -> a couple of px of scaler
	// tolerance).
	if res.ContentW != res.AnalysisW {
		t.Errorf("content width = %d, want the full analysis width %d", res.ContentW, res.AnalysisW)
	}
	wantCH := 544.0 / 720.0 * float64(res.AnalysisH)
	if math.Abs(float64(res.ContentH)-wantCH) > 3 {
		t.Errorf("content height = %d, want %.0f (the 1280x544 aspect at analysis height)", res.ContentH, wantCH)
	}
	if res.ContentH >= res.AnalysisH {
		t.Errorf("no bars cut: content height %d >= analysis height %d", res.ContentH, res.AnalysisH)
	}
	if cr := p.cropRectAt(0); cr.Dx() != res.AnalysisW || math.Abs(float64(cr.Dy())-wantCH) > 3 {
		t.Errorf("plan crop rect = %v, want the content rect (%dx%.0f)", cr, res.AnalysisW, wantCH)
	}
	rec := p.Records()[0]
	if math.Abs(rec.Crop.W-float64(res.ContentW)) > 1 || math.Abs(rec.Crop.H-float64(res.ContentH)) > 1 {
		t.Errorf("record Crop = %v, want the content rect %dx%d", rec.Crop, res.ContentW, res.ContentH)
	}

	// The rendered product: a crop-kind preview (no overlay chrome), decoded
	// through the same vidio reader the render loop uses. At 720x360 the new
	// law scales min(720/854, 360/363) = 0.8431 -> content fills the width
	// and lands at rows ~27..333. The shipped whole-frame law would paint
	// red only across ~44..316.
	task, err := a.RenderPreview(context.Background(), p, PreviewRequest{
		FFmpegPath: ffmpeg, Options: PreviewOptions{Kind: PreviewCrop},
	})
	if err != nil {
		t.Fatal(err)
	}
	res2, err := task.Wait()
	if err != nil {
		t.Fatal(err)
	}
	v, err := vidio.NewVideo(res2.CropVideoPath)
	if err != nil {
		t.Fatalf("open preview: %v", err)
	}
	defer v.Close()
	w, h := v.Width(), v.Height()
	buf := image.NewRGBA(image.Rect(0, 0, w, h))
	if err := v.SetFrameBuffer(buf.Pix); err != nil {
		t.Fatal(err)
	}
	if !v.Read() {
		t.Fatal("preview decoded zero frames")
	}
	isRed := func(x, y int) bool {
		px := buf.Pix[buf.PixOffset(x, y):]
		return px[0] > 120 && px[1] < 100 && px[2] < 100
	}
	firstRed, lastRed := -1, -1
	for y := 0; y < h; y++ {
		if isRed(w/2, y) {
			if firstRed < 0 {
				firstRed = y
			}
			lastRed = y
		}
	}
	if firstRed < 0 {
		t.Fatal("the rendered preview holds no red content at all")
	}
	// New law: content starts at the fit offset (~27) and reaches ~333.
	// The shipped whole-frame transform would measure 44..316 instead.
	if firstRed < 20 || firstRed > 40 {
		t.Errorf("content band starts at row %d, want ~27 (content-fit offset; the whole-frame law would start at ~44)", firstRed)
	}
	if lastRed < 320 || lastRed > 340 {
		t.Errorf("content band ends at row %d, want ~333 (the whole-frame law would end at ~316)", lastRed)
	}
}

// TestFitAutoOrientScoresContentAspectNotFrame pins the orientation half of
// the law: a portrait 576x720 field pillarboxed into 1280x720. Auto-orient
// at 480x512 must score the CONTENT aspect (~0.8) and KEEP 480x512 - the
// root container's 16:9 aspect would have flipped to 512x480 instead.
func TestFitAutoOrientScoresContentAspectNotFrame(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	inputDir, tempDir := t.TempDir(), t.TempDir()
	fixture := buildBarClipAt(t, ffmpeg, inputDir, 576, 720)

	task, err := Analyze(context.Background(), AnalyzeRequest{
		Input: fixture, TempDir: tempDir, FFmpegPath: ffmpeg,
		Mode: AnalyzeFit, AnalysisHeight: 512,
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := task.Wait()
	if err != nil {
		t.Fatal(err)
	}

	p, err := NewFitPlan(a.Stream(), Settings{Width: 480, Height: 512, FPS: 20, AutoOrient: true})
	if err != nil {
		t.Fatal(err)
	}
	res := p.Resolved()
	if res.OutputW != 480 || res.OutputH != 512 {
		t.Errorf("auto-orient resolved %dx%d, want the requested 480x512 kept (the 16:9 CONTAINER would flip it)",
			res.OutputW, res.OutputH)
	}
	if p.Orientation().Flip {
		t.Error("auto-orient flipped on the container aspect, want the content aspect decision")
	}
	if res.ContentH != res.AnalysisH {
		t.Errorf("content height = %d, want the full analysis height %d (pillarbox: no vertical bars)",
			res.ContentH, res.AnalysisH)
	}
	wantCW := 576.0 / 1280.0 * float64(res.AnalysisW)
	if math.Abs(float64(res.ContentW)-wantCW) > 4 {
		t.Errorf("content width = %d, want %.0f (the 576x720 aspect at analysis width)", res.ContentW, wantCW)
	}
	stat := p.Orientation()
	// Content AR ~0.8 against 480x512 (0.9375): keep = min(0.85, 1.17) ~ 0.85,
	// flip ~ 0.75. The frame-AR law would have scored 0.53/0.64 instead.
	if stat.KeepScore < 0.82 || stat.KeepScore > 0.89 || stat.FlipScore > stat.KeepScore {
		t.Errorf("orientation scores = keep %.3f / flip %.3f, want the content-AR decision (~0.85 keep)",
			stat.KeepScore, stat.FlipScore)
	}
}
