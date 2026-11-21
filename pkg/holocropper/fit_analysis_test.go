package holocropper

// Mode-aware analysis (plan §2/U1): a fit-mode Analyze runs prepare + the
// geometry probe with NO subject detection; its synthetic stream still drives
// plan replay, previews and every render product; a crop-mode Analyze over a
// fit cache resumes with the detect phase only, and switching back to fit
// needs no work at all. Fixture law: the standard deterministic walking-box
// clip, ffmpeg required.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func analyzeWithMode(t *testing.T, ffmpegPath, input, tempDir string, mode AnalyzeMode) *Analysis {
	t.Helper()
	task, err := Analyze(context.Background(), AnalyzeRequest{
		Input: input, TempDir: tempDir, FFmpegPath: ffmpegPath, Mode: mode,
	})
	if err != nil {
		t.Fatalf("Analyze(mode=%q): %v", mode, err)
	}
	a, err := task.Wait()
	if err != nil {
		t.Fatalf("Analyze(mode=%q) wait: %v", mode, err)
	}
	return a
}

func readAnalysisMeta(t *testing.T, a *Analysis) analysisMeta {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(a.Dir(), analysisMetaName))
	if err != nil {
		t.Fatalf("read meta.json: %v", err)
	}
	var meta analysisMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("parse meta.json: %v", err)
	}
	return meta
}

// TestFitAnalysisSkipsDetection pins the fit pass end to end: geometry-only
// synthetic stream, meta.detected=false, and the analysis video still the
// full frame-aligned encode.
func TestFitAnalysisSkipsDetection(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	inputDir, tempDir := t.TempDir(), t.TempDir()
	fixture := buildFixture(t, ffmpeg, inputDir)

	a := analyzeWithMode(t, ffmpeg, fixture, tempDir, AnalyzeFit)
	if a.Detected() {
		t.Fatal("fit analysis reports Detected() true - the detect pass must not run")
	}

	s := a.Stream()
	if !s.Complete() {
		t.Fatal("fit stream is not complete")
	}
	want := fixtureSeconds * fixtureFPS
	if s.Len() != want {
		t.Fatalf("fit stream has %d samples, want one per frame (%d)", s.Len(), want)
	}
	if s.AnalysisHeight() != defaultAnalysisHeight || s.AnalysisWidth() != 854 {
		t.Errorf("analysis frame = %dx%d, want 854x480", s.AnalysisWidth(), s.AnalysisHeight())
	}
	if s.SourceWidth() != fixtureW || s.SourceHeight() != fixtureH {
		t.Errorf("source frame = %dx%d, want %dx%d", s.SourceWidth(), s.SourceHeight(), fixtureW, fixtureH)
	}
	if s.FPS() != fixtureFPS {
		t.Errorf("analysis fps = %v, want %v", s.FPS(), fixtureFPS)
	}
	if s.AnalysisVideoPath() != a.AnalysisVideoPath() {
		t.Error("fit stream must know its analysis video")
	}
	for i := 0; i < s.Len(); i++ {
		f := s.Frame(i)
		if f.Subjects != 0 || f.BBox.W > 0 {
			t.Fatalf("frame %d: subjects=%d bbox=%+v - synthetic samples must be subject-less", i+1, f.Subjects, f.BBox)
		}
	}

	meta := readAnalysisMeta(t, a)
	if meta.Detected {
		t.Error("meta.json detected=true for a fit analysis")
	}
	if meta.FormatVersion != analysisFormatVersion {
		t.Errorf("meta.json format_version = %d, want %d", meta.FormatVersion, analysisFormatVersion)
	}
	if meta.Frames != want {
		t.Errorf("meta.json frames = %d, want %d", meta.Frames, want)
	}
	if st, err := os.Stat(a.AnalysisVideoPath()); err != nil || st.Size() == 0 {
		t.Errorf("fit pass must still produce the full analysis video: %v", err)
	}
}

// TestAnalyzeModeValidation pins the mode field's validation and default.
func TestAnalyzeModeValidation(t *testing.T) {
	if got := (AnalyzeRequest{}).normalizedMode(); got != AnalyzeCrop {
		t.Errorf(`"" normalizes to %q, want %q`, got, AnalyzeCrop)
	}
	if got := (AnalyzeRequest{Mode: AnalyzeCrop}).normalizedMode(); got != AnalyzeCrop {
		t.Errorf("crop normalizes to %q", got)
	}
	if got := (AnalyzeRequest{Mode: AnalyzeFit}).normalizedMode(); got != AnalyzeFit {
		t.Errorf("fit normalizes to %q", got)
	}

	_, err := Analyze(context.Background(), AnalyzeRequest{
		Input: "in.mp4", TempDir: "tmp", Mode: "banana",
	})
	var serr *SettingsError
	if !errors.As(err, &serr) {
		t.Fatalf("bogus mode error = %v, want *SettingsError", err)
	}
	found := false
	for _, e := range serr.Errors {
		if e.Field == "mode" {
			found = true
		}
	}
	if !found {
		t.Errorf("SettingsError errors = %+v, want one on field \"mode\"", serr.Errors)
	}
}

// TestCropAnalyzeResumesFromFitCache pins the resume law: fit first, then
// crop over the SAME cache dir re-runs only the detect phase over the cached
// analysis video (prepare is skipped - the video's mtime proves it), and the
// third fit Analyze is then an immediate hit over the detected cache.
func TestCropAnalyzeResumesFromFitCache(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	inputDir, tempDir := t.TempDir(), t.TempDir()
	fixture := buildFixture(t, ffmpeg, inputDir)

	fit := analyzeWithMode(t, ffmpeg, fixture, tempDir, AnalyzeFit)
	if fit.Detected() {
		t.Fatal("first pass must be the fit analysis")
	}
	vp := fit.AnalysisVideoPath()
	stBefore, err := os.Stat(vp)
	if err != nil {
		t.Fatal(err)
	}

	crop := analyzeWithMode(t, ffmpeg, fixture, tempDir, AnalyzeCrop)
	if !crop.Detected() {
		t.Fatal("crop Analyze over a fit cache must produce a detected analysis")
	}
	if crop.Dir() != fit.Dir() {
		t.Fatalf("resume used a different cache dir (%s vs %s)", crop.Dir(), fit.Dir())
	}
	stAfter, err := os.Stat(vp)
	if err != nil {
		t.Fatal(err)
	}
	if !stAfter.ModTime().Equal(stBefore.ModTime()) {
		t.Error("the analysis video was re-encoded - resume must skip the prepare phase")
	}

	s := crop.Stream()
	if s.Len() != fixtureSeconds*fixtureFPS {
		t.Fatalf("resumed stream has %d frames, want %d", s.Len(), fixtureSeconds*fixtureFPS)
	}
	withSubjects := 0
	for i := 0; i < s.Len(); i++ {
		if s.Frame(i).Subjects > 0 {
			withSubjects++
		}
	}
	if withSubjects == 0 {
		t.Error("the resumed detect pass found no subject on the walking-box fixture")
	}
	if meta := readAnalysisMeta(t, crop); !meta.Detected {
		t.Error("meta.json still says detected=false after the resume")
	}

	// Fit over a detected cache is an immediate hit: the detected stream is
	// adopted as-is (fit replay ignores the boxes).
	again := analyzeWithMode(t, ffmpeg, fixture, tempDir, AnalyzeFit)
	if !again.Detected() {
		t.Error("fit Analyze over a detected cache must reuse it, not re-run")
	}
	if again.Stream().Len() != s.Len() {
		t.Error("the fit re-Analyze adopted a different stream")
	}
}

// TestFitPlanParityWithDetectedPlan pins the core U1 promise: a fit plan
// replayed over the synthetic stream is the framing product today's fit plan
// produces over a detected stream - same settings hash, same geometry, same
// crop rects and output mapping, same orientation decision. Only the subject
// readouts differ (the synthetic pass never measured them).
func TestFitPlanParityWithDetectedPlan(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	settings := Settings{Width: 320, Height: 480, FPS: fixtureFPS, AutoOrient: true}

	detInput, detTemp := t.TempDir(), t.TempDir()
	det := analyzeWithMode(t, ffmpeg, buildFixture(t, ffmpeg, detInput), detTemp, AnalyzeCrop)
	fitInput, fitTemp := t.TempDir(), t.TempDir()
	fit := analyzeWithMode(t, ffmpeg, buildFixture(t, ffmpeg, fitInput), fitTemp, AnalyzeFit)

	dp, err := NewFitPlan(det.Stream(), settings)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := NewFitPlan(fit.Stream(), settings)
	if err != nil {
		t.Fatal(err)
	}

	if dp.SettingsHash() != fp.SettingsHash() {
		t.Errorf("settings hash: detected %s vs synthetic %s - must be identical",
			dp.SettingsHash(), fp.SettingsHash())
	}
	dr, fr := dp.Resolved(), fp.Resolved()
	if dr.AnalysisW != fr.AnalysisW || dr.AnalysisH != fr.AnalysisH ||
		dr.SourceW != fr.SourceW || dr.SourceH != fr.SourceH ||
		dr.OutputW != fr.OutputW || dr.OutputH != fr.OutputH {
		t.Fatalf("resolved geometry differs: %+v vs %+v", dr, fr)
	}
	if dp.Orientation().Flip != fp.Orientation().Flip {
		t.Errorf("auto-orient flip: detected=%v synthetic=%v - fit orientation must decide from the source frame alone",
			dp.Orientation().Flip, fp.Orientation().Flip)
	}
	if !fp.Orientation().Flip {
		t.Error("16:9 source into a 320x480 fit output must auto-flip to 480x320")
	}

	if len(dp.Records()) != len(fp.Records()) {
		t.Fatalf("record counts differ: %d vs %d", len(dp.Records()), len(fp.Records()))
	}
	for i := range dp.Records() {
		d, f := dp.Record(i), fp.Record(i)
		if d.Crop != f.Crop || d.Norm.Crop != f.Norm.Crop || d.Out.Crop != f.Out.Crop {
			t.Fatalf("frame %d: framing differs - detected crop %+v/%+v/%+v vs synthetic %+v/%+v/%+v",
				i+1, d.Crop, d.Norm.Crop, d.Out.Crop, f.Crop, f.Norm.Crop, f.Out.Crop)
		}
		if d.TargetH != f.TargetH {
			t.Fatalf("frame %d: TargetH %v vs %v", i+1, d.TargetH, f.TargetH)
		}
	}

	// The synthetic pass read no subjects: every frame reports "lost" and the
	// whole video shades as one dropout span. Honest, and the app hides the
	// shading in fit mode.
	if got := fp.DropoutFrames(); len(got) != 1 || got[0].Start != 1 || got[0].End != len(fp.Records()) {
		t.Errorf("synthetic dropouts = %+v, want one whole-video span 1..%d", got, len(fp.Records()))
	}
}

// TestFitRenderChainAllProducts pins that preview, PC final and MikV all
// execute against a fit analysis - the frame-count invariants they check
// internally (records == stream == decoded) hold for the synthetic stream.
func TestFitRenderChainAllProducts(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	inputDir, tempDir := t.TempDir(), t.TempDir()
	fixture := buildFixture(t, ffmpeg, inputDir)

	a := analyzeWithMode(t, ffmpeg, fixture, tempDir, AnalyzeFit)
	p, err := NewFitPlan(a.Stream(), Settings{Width: 160, Height: 120, FPS: fixtureFPS})
	if err != nil {
		t.Fatal(err)
	}

	// Preview (story B1) on the fit plan.
	pvTask, err := a.RenderPreview(context.Background(), p, PreviewRequest{
		FFmpegPath: ffmpeg,
		Options:    PreviewOptions{Kind: PreviewCrop},
	})
	if err != nil {
		t.Fatalf("RenderPreview: %v", err)
	}
	pv, err := pvTask.Wait()
	if err != nil {
		t.Fatalf("preview wait: %v", err)
	}
	if pv.CropVideoPath == "" {
		t.Fatal("fit preview produced no crop video")
	}
	if pv.Frames != fixtureSeconds*fixtureFPS {
		t.Errorf("preview frames = %d, want %d", pv.Frames, fixtureSeconds*fixtureFPS)
	}
	if st, err := os.Stat(pv.CropVideoPath); err != nil || st.Size() == 0 {
		t.Fatalf("fit preview file missing/empty: %v", err)
	}

	// PC final (story B2) on the same fit plan.
	outDir := t.TempDir()
	rdTask, err := a.RenderFinal(context.Background(), p, RenderRequest{
		OutputDir: outDir, TempDir: tempDir, FFmpegPath: ffmpeg,
	})
	if err != nil {
		t.Fatalf("RenderFinal: %v", err)
	}
	res, err := rdTask.Wait()
	if err != nil {
		t.Fatalf("final wait: %v", err)
	}
	frames, w, h, _ := frameDigest(t, res.OutputPath)
	if frames != fixtureSeconds*fixtureFPS {
		t.Errorf("PC fit render decoded %d frames, want %d", frames, fixtureSeconds*fixtureFPS)
	}
	if w != 160 || h != 120 {
		t.Errorf("PC fit render frame = %dx%d, want 160x120", w, h)
	}

	// MikV production on the same fit plan. Opus audio: the plan runs at the
	// source's 24 fps, and raw PCM would need a sample rate that divides
	// evenly over it - OGG-Opus is the plan's other legal pairing.
	mkTask, err := a.RenderMikV(context.Background(), p, MikVRequest{
		OutputDir: outDir, TempDir: tempDir, FFmpegPath: ffmpeg,
		Options: MikVOptions{AudioOpus: true},
	})
	if err != nil {
		t.Fatalf("RenderMikV: %v", err)
	}
	mikv, err := mkTask.Wait()
	if err != nil {
		t.Fatalf("mikv wait: %v", err)
	}
	if st, err := os.Stat(mikv.OutputPath); err != nil || st.Size() == 0 {
		t.Fatalf("MikV fit production missing/empty: %v", err)
	}
}

// TestRenderWholeFitSkipsDetection pins the one-shot convenience law: fit
// settings pick the fit analysis mode automatically.
func TestRenderWholeFitSkipsDetection(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	inputDir, tempDir, outDir := t.TempDir(), t.TempDir(), t.TempDir()
	fixture := buildFixture(t, ffmpeg, inputDir)

	res, err := RenderWhole(context.Background(),
		AnalyzeRequest{Input: fixture, TempDir: tempDir, FFmpegPath: ffmpeg},
		Settings{Width: 160, Height: 120, FPS: fixtureFPS, FitWithBars: true},
		RenderRequest{OutputDir: outDir, TempDir: tempDir, FFmpegPath: ffmpeg})
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(res.OutputPath); err != nil || st.Size() == 0 {
		t.Fatalf("RenderWhole fit output missing: %v", err)
	}

	// The cache the one-shot wrote must be the fit-skipped kind.
	cached, err := CachedAnalysis(fixture, tempDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if cached.Detected() {
		t.Error("RenderWhole with fit settings must analyze in fit mode (no detection)")
	}
}
