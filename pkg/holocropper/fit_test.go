package holocropper

import (
	"context"
	"errors"
	"image"
	"image/color"
	"math"
	"os"
	"path/filepath"
	"testing"

	vidio "github.com/AlexEidt/Vidio"
)

/* Fit mode (§6.10): the non-hologram path. The whole frame is scaled to fit
the output on black bars - no framing controller, no zoom, no crop. These
tests pin the replay records, the geometry-only orientation decision, the
fit resize itself, and a real render through the API chain. */

func TestFitPlanRecords(t *testing.T) {
	samples := walkingSamples(12, 100, 4, 120, 300)
	// Two subject-less frames mid-run -> one dropout span, framing unaffected.
	samples[2] = newStreamSample(image.ZR, 0, image.Point{})
	samples[3] = newStreamSample(image.ZR, 0, image.Point{})
	stream := newTestStream(samples, 854, 480, 1280, 720)

	p, err := NewFitPlan(stream, Settings{Width: 320, Height: 240, FPS: 20})
	if err != nil {
		t.Fatal(err)
	}
	if !p.FitWithBars() {
		t.Fatal("NewFitPlan plan does not report fit mode")
	}
	recs := p.Records()
	if len(recs) != 12 {
		t.Fatalf("records: %d want 12", len(recs))
	}

	// The fit transform: scale to fit, center, black bars.
	scale := math.Min(320.0/854.0, 240.0/480.0)
	sw, sh := 854.0*scale, 480.0*scale
	offX, offY := (320.0-sw)/2, (240.0-sh)/2
	full := Rect{X: 0, Y: 0, W: 854, H: 480}
	fitted := Rect{X: offX, Y: offY, W: sw, H: sh}

	for i, rec := range recs {
		if rec.Crop != full {
			t.Fatalf("frame %d: Crop %+v, want the full analysis frame %+v", rec.Frame, rec.Crop, full)
		}
		if rec.Norm.Crop != (Rect{X: 0, Y: 0, W: 1, H: 1}) {
			t.Fatalf("frame %d: Norm.Crop %+v, want the whole unit square", rec.Frame, rec.Norm.Crop)
		}
		if rec.Out.Crop != fitted {
			t.Fatalf("frame %d: Out.Crop %+v, want the fitted image rect %+v", rec.Frame, rec.Out.Crop, fitted)
		}
		if rec.EMA != rec.BBox {
			t.Errorf("frame %d: EMA %+v must mirror the detection box (no smoothing in fit mode)", rec.Frame, rec.EMA)
		}
		if rec.TargetH != 480 || rec.WalkActive || rec.WalkTgt != 0 {
			t.Errorf("frame %d: TargetH %.1f / walk %+v not the fit-mode idle values", rec.Frame, rec.TargetH, rec.WalkTgt)
		}
		// Subject boxes map through the same fit transform.
		want := Rect{
			X: offX + rec.BBox.X*scale, Y: offY + rec.BBox.Y*scale,
			W: rec.BBox.W * scale, H: rec.BBox.H * scale,
		}
		if rec.Out.BBox != want {
			t.Errorf("frame %d: Out.BBox %+v, want %+v", rec.Frame, rec.Out.BBox, want)
		}
		lost := rec.BBox.W == 0
		if rec.SubjectLost != lost {
			t.Errorf("frame %d: SubjectLost %v, want %v", rec.Frame, rec.SubjectLost, lost)
		}
		if got := p.cropRectAt(i); got != image.Rect(0, 0, 854, 480) {
			t.Fatalf("cropRectAt(%d) = %v, want the full analysis frame", i, got)
		}
	}

	drop := p.DropoutFrames()
	if len(drop) != 1 || drop[0] != (IntRange{Start: 3, End: 4}) {
		t.Errorf("dropouts %v, want the one subject-less span {3,4}", drop)
	}
	if p.Walk().Active {
		t.Error("fit mode must never report an active walk leash")
	}
}

// Fit mode keeps the requested height when the analysis frame is shorter
// than it - the same upscale guard as crop mode, because the render scales
// from the analysis video.
func TestFitStreamIncompatibleGuard(t *testing.T) {
	stream := newTestStream(walkingSamples(4, 100, 4, 120, 150), 426, 240, 640, 360)
	_, err := NewFitPlan(stream, Settings{Width: 240, Height: 420})
	if !errors.Is(err, ErrStreamIncompatible) {
		t.Fatalf("err = %v, want ErrStreamIncompatible", err)
	}
}

// Auto-orient in fit mode is pure geometry: the orientation whose aspect is
// closest to the source's gets the least bars - and it decides identically
// on a subjectless clip, where crop-mode auto-orient keeps the request.
func TestFitAutoOrientPicksLeastBars(t *testing.T) {
	subjects := walkingSamples(8, 100, 4, 120, 300)
	srcclose := 854.0 / 480.0

	p, err := NewFitPlan(newTestStream(subjects, 854, 480, 1280, 720),
		Settings{Width: 240, Height: 420, AutoOrient: true})
	if err != nil {
		t.Fatal(err)
	}
	r := p.Resolved()
	if r.OutputW != 420 || r.OutputH != 240 {
		t.Fatalf("16:9 clip asked for 240x420 fit: got %dx%d, want the flip to 420x240", r.OutputW, r.OutputH)
	}
	st := p.Orientation()
	if !st.Flip || st.FlipScore <= st.KeepScore {
		t.Fatalf("orientation %+v must flip toward the barless orientation", st)
	}
	if absF(st.FlipScore-min(1.0, srcclose/(420.0/240.0))) > 0.01 && absF(st.FlipScore-min(srcclose/(420.0/240.0), (420.0/240.0)/srcclose)) > 0.01 {
		t.Errorf("flip score %.4f should equal the bar coverage at 420x240", st.FlipScore)
	}
	if st.MeanAR != srcclose || st.FramesExamined != 8 {
		t.Errorf("fit stat aggregates %+v, want source aspect %.4f over 8 frames", st, srcclose)
	}

	// Subjectless clip: geometry decision anyway...
	empty := make([]streamSample, 8)
	for i := range empty {
		empty[i] = newStreamSample(image.ZR, 0, image.Point{})
	}
	pFit, err := NewFitPlan(newTestStream(empty, 854, 480, 1280, 720),
		Settings{Width: 240, Height: 420, AutoOrient: true})
	if err != nil {
		t.Fatal(err)
	}
	if r := pFit.Resolved(); r.OutputW != 420 || r.OutputH != 240 {
		t.Fatalf("subjectless fit clip: got %dx%d, want 420x240 - the bar decision ignores subjects", r.OutputW, r.OutputH)
	}
	// ...while crop-mode auto-orient on the SAME subjectless stream keeps
	// the request (no subject evidence, no flip).
	pCrop, err := NewPlan(newTestStream(empty, 854, 480, 1280, 720),
		Settings{Width: 240, Height: 420, AutoOrient: true})
	if err != nil {
		t.Fatal(err)
	}
	if r := pCrop.Resolved(); r.OutputW != 240 || r.OutputH != 420 {
		t.Fatalf("crop-mode subjectless plan: got %dx%d, want the request kept", r.OutputW, r.OutputH)
	}

	// Already-optimal and square requests keep their orientation.
	p2, err := NewFitPlan(newTestStream(subjects, 854, 480, 1280, 720),
		Settings{Width: 420, Height: 240, AutoOrient: true})
	if err != nil {
		t.Fatal(err)
	}
	if p2.Orientation().Flip {
		t.Error("420x240 fit on a 16:9 clip must not flip")
	}
	p3, err := NewFitPlan(newTestStream(subjects, 854, 480, 1280, 720),
		Settings{Width: 320, Height: 320, AutoOrient: true})
	if err != nil {
		t.Fatal(err)
	}
	if p3.Orientation().Flip || p3.Resolved().OutputW != 320 {
		t.Error("square fit request must pass through unchanged")
	}
}

func TestFitResizeTo(t *testing.T) {
	gray := func(w, h int) *image.RGBA {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		c := color.RGBA{128, 128, 128, 255}
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				img.Set(x, y, c)
			}
		}
		return img
	}
	black := func(c color.RGBA) bool { return c.R < 16 && c.G < 16 && c.B < 16 }
	isGray := func(c color.RGBA) bool { return c.R > 96 && c.R < 160 && c.G > 96 && c.B > 96 }

	// 16:9 -> square: letterboxed strip, black bars top and bottom.
	out := fitResizeTo(gray(960, 540), 240, 240)
	if out.Bounds().Dx() != 240 || out.Bounds().Dy() != 240 {
		t.Fatalf("dims %v, want 240x240", out.Bounds())
	}
	if c, _ := out.At(120, 10).(color.RGBA); !black(c) {
		t.Errorf("(120,10) = %v, want a black bar", c)
	}
	if c, _ := out.At(120, 120).(color.RGBA); !isGray(c) {
		t.Errorf("(120,120) = %v, want the gray image interior", c)
	}

	// 4:3 -> 16:9: pillarbox bars left and right.
	out = fitResizeTo(gray(640, 480), 160, 90)
	if out.Bounds().Dx() != 160 || out.Bounds().Dy() != 90 {
		t.Fatalf("dims %v, want 160x90", out.Bounds())
	}
	if c, _ := out.At(5, 45).(color.RGBA); !black(c) {
		t.Errorf("(5,45) = %v, want a black side bar", c)
	}
	if c, _ := out.At(80, 45).(color.RGBA); !isGray(c) {
		t.Errorf("(80,45) = %v, want the gray image interior", c)
	}

	// Matching aspects: exact scale, no bars anywhere.
	out = fitResizeTo(gray(480, 270), 240, 135)
	for _, xy := range [][2]int{{1, 1}, {238, 133}} {
		if c, _ := out.At(xy[0], xy[1]).(color.RGBA); !isGray(c) {
			t.Errorf("(%d,%d) = %v, want gray - equal aspects must produce no bars", xy[0], xy[1], c)
		}
	}
}

// End-to-end fit render through the shipped chain: Analyze -> NewFitPlan ->
// RenderFinal. A gray 960x540 source into 240x240 must land gray content in
// a black-bar output, frame count intact.
func TestRenderFinalFitBars(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	outDir := t.TempDir()

	fixture := sharedGrayFixture(t, ffmpeg)
	a, _ := attachWarmAnalysis(t, ffmpeg, fixture, AnalyzeCrop)

	p, err := NewFitPlan(a.Stream(), Settings{Width: 240, Height: 240, FPS: fixtureFPS})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := a.RenderFinal(context.Background(), p, RenderRequest{
		OutputDir: outDir, TempDir: outDir, FFmpegPath: ffmpeg,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := rt.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if res.OutputW != 240 || res.OutputH != 240 {
		t.Fatalf("output %dx%d, want 240x240", res.OutputW, res.OutputH)
	}
	if res.Frames != fixtureSeconds*fixtureFPS {
		t.Fatalf("frames %d, want %d", res.Frames, fixtureSeconds*fixtureFPS)
	}
	if _, err := os.Stat(res.OutputPath); err != nil {
		t.Fatalf("output file: %v", err)
	}

	// Decode the first output frame: analysis frame is 854x480, fitted into
	// 240x240 -> a ~135-row band (y 52..187) over black bars.
	v, err := vidio.NewVideo(res.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	buf := image.NewRGBA(image.Rect(0, 0, 240, 240))
	if err := v.SetFrameBuffer(buf.Pix); err != nil {
		t.Fatal(err)
	}
	if !v.Read() {
		t.Fatal("output video decoded no frames")
	}
	px := func(x, y int) color.RGBA {
		o := 4 * (y*240 + x)
		return color.RGBA{buf.Pix[o], buf.Pix[o+1], buf.Pix[o+2], 255}
	}
	if c := px(120, 10); c.R > 40 || c.G > 40 || c.B > 40 {
		t.Errorf("bar pixel (120,10) = %v, want black", c)
	}
	if c := px(120, 120); c.R < 80 || c.R > 180 {
		t.Errorf("image pixel (120,120) = %v, want the gray source", c)
	}
}

// The fit/crop modes must never share a preview cache identity.
func TestFitSettingsHashSeparatesModes(t *testing.T) {
	stream := newTestStream(walkingSamples(4, 100, 4, 120, 300), 854, 480, 1280, 720)
	crop, err := NewPlan(stream, Settings{Width: 240, Height: 240})
	if err != nil {
		t.Fatal(err)
	}
	fit, err := NewFitPlan(stream, Settings{Width: 240, Height: 240})
	if err != nil {
		t.Fatal(err)
	}
	fit2, err := NewPlan(stream, Settings{Width: 240, Height: 240, FitWithBars: true})
	if err != nil {
		t.Fatal(err)
	}
	if crop.SettingsHash() == fit.SettingsHash() {
		t.Fatal("fit and crop plans share a SettingsHash - previews would cross-serve")
	}
	if fit.SettingsHash() != fit2.SettingsHash() {
		t.Fatal("NewFitPlan and Settings{FitWithBars:true} must hash identically")
	}
}

// The HOLOCROPPER_DEBUG_BOXES shim in fit mode reports what the fit path
// actually applies: full-frame crop, EMA = detection box, no leash.
func TestFitDebugCSVShim(t *testing.T) {
	csvPath := filepath.Join(t.TempDir(), "fit.csv")
	t.Setenv("HOLOCROPPER_DEBUG_BOXES", csvPath)

	samples := walkingSamples(5, 100, 4, 120, 300)
	if _, err := NewFitPlan(newTestStream(samples, 854, 480, 1280, 720), Settings{Width: 320, Height: 240}); err != nil {
		t.Fatal(err)
	}
	rows := readDebugCSV(t, csvPath)
	if len(rows) != 5 {
		t.Fatalf("CSV rows %d, want 5", len(rows))
	}
	for i, r := range rows {
		bb := samples[i].bbox()
		if r.bboxX0 != bb.Min.X || r.bboxY0 != bb.Min.Y || r.bboxW != bb.Dx() || r.bboxH != bb.Dy() {
			t.Fatalf("row %d: bbox %v vs detection %v", r.frame, [4]int{r.bboxX0, r.bboxY0, r.bboxW, r.bboxH}, bb)
		}
		if r.emaX0 != r.bboxX0 || r.emaY0 != r.bboxY0 || r.emaW != r.bboxW || r.emaH != r.bboxH {
			t.Errorf("row %d: EMA columns %v do not mirror the bbox", r.frame, [4]int{r.emaX0, r.emaY0, r.emaW, r.emaH})
		}
		if r.cropW != 854 || r.cropH != 480 {
			t.Errorf("row %d: crop %.2fx%.2f, want the full 854x480 analysis frame", r.frame, r.cropW, r.cropH)
		}
		if r.targetH != 480 || r.walkTgt != 0 {
			t.Errorf("row %d: target_h %.2f / walk_tgt %.2f not fit-mode idle values", r.frame, r.targetH, r.walkTgt)
		}
	}
}
