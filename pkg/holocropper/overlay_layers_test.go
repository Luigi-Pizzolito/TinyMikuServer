package holocropper

import (
	"context"
	"image"
	"image/color"
	"testing"
)

/* Milestone G4 pins: the baked overlay renderer ports the judged 761 layer set.
Every pin draws on a BLACK 240x240 frame so the semi-transparent layers produce
their exact integer-blend bytes: blended(c over black) = c.channel x c.alpha / 255.
The law under test: the default layers stay exactly as they were (every new
layer defaults OFF), each new layer paints its judged colour, boxes that reach
outside the frame draw CLIPPED, and a frame the stream cannot measure draws no
detail layer rather than an approximation. */

func g4Black(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	black := color.RGBA{0, 0, 0, 255}
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = black.R, black.G, black.B, black.A
	}
	return img
}

func countColor(img *image.RGBA, want color.RGBA) int {
	n := 0
	for i := 0; i < len(img.Pix); i += 4 {
		if img.Pix[i] == want.R && img.Pix[i+1] == want.G && img.Pix[i+2] == want.B && img.Pix[i+3] == want.A {
			n++
		}
	}
	return n
}

// blendedOverBlack is blendPixel's exact output over a black frame.
func blendedOverBlack(c color.RGBA) color.RGBA {
	a := uint32(c.A)
	return color.RGBA{
		R: uint8(uint32(c.R) * a / 255),
		G: uint8(uint32(c.G) * a / 255),
		B: uint8(uint32(c.B) * a / 255),
		A: 255,
	}
}

// g4Record: one settled crop-mode frame in a 240x240 output, every telemetry
// field populated (a 480x360 analysis crop mapped 1:2 into the output).
func g4Record() FrameRecord {
	return FrameRecord{
		Frame: 7, Subjects: 2,
		BBox:     Rect{X: 100, Y: 100, W: 200, H: 300},
		EMA:      Rect{X: 90, Y: 90, W: 220, H: 315},
		Crop:     Rect{X: 60, Y: 60, W: 480, H: 360},
		Centroid: Point{X: 200, Y: 280}, CentroidKnown: true,
		Out: SpaceRects{
			BBox: Rect{X: 20, Y: 26, W: 100, H: 200},
			EMA:  Rect{X: 15, Y: 20, W: 110, H: 210},
			Crop: Rect{X: 0, Y: 0, W: 240, H: 240},
			// Commanded crop reaching off the LEFT frame edge: the clamp gap
			// the ghost exists to show.
			CropTarget: Rect{X: -40, Y: 10, W: 100, H: 220},
			Centroid:   NormPoint{X: 50, Y: 122},
			PanTarget:  NormPoint{X: 100, Y: 120},
		},
		TargetH: 223, WalkTgt: 100, WalkActive: true, SceneCut: true,
		ZoomCurrent: 1.61, ZoomTarget: 1.60,
	}
}

func TestOverlayLayersDefaultLawDrawsOnlyTheOldLayers(t *testing.T) {
	st := defaultOverlayStyle()
	img := g4Black(240, 240)
	drawOverlayBoxes(img, g4Record(), PreviewOptions{DrawSubject: true, DrawCrop: true}, st)

	for _, layer := range []color.RGBA{st.Centroid, st.TargetCrop, st.AxisTarget, st.Pool,
		st.ComponentBig, st.ComponentSmall, st.ChipFG, st.LeashFG, st.CutFG} {
		if n := countColor(img, layer); n != 0 {
			t.Errorf("a G4 layer colour (%v) appears with the old default opts: %d px", layer, n)
		}
	}
	if n := countColor(img, blendedOverBlack(st.CentroidLine)); n != 0 {
		t.Errorf("centroid line drawn without DrawCentroid: %d px", n)
	}
	if n := countColor(img, blendedOverBlack(st.ChipBG)); n != 0 {
		t.Errorf("a chip plate drawn without DrawReadouts: %d px", n)
	}
	// The pre-G4 layers still land: amber subject, cyan EMA, magenta crop.
	for _, layer := range []color.RGBA{st.Subject, st.EMA, st.Crop} {
		if n := countColor(img, layer); n == 0 {
			t.Errorf("default layer %v missing", layer)
		}
	}
}

func TestOverlayTargetCropGhostDrawsClipped(t *testing.T) {
	st := defaultOverlayStyle()
	img := g4Black(240, 240)
	rec := g4Record()
	drawOverlayBoxes(img, rec, PreviewOptions{DrawCropTarget: true}, st)

	ghost := blendedOverBlack(st.TargetCrop)
	if n := countColor(img, ghost); n == 0 {
		t.Fatal("the commanded crop drew no ghost")
	}
	// The commanded box starts at out x=-40: containment clips it to the frame,
	// so the ghost must paint the LEFT frame edge - the off-frame command the
	// app canvas would mark |<.
	edge := 0
	for y := 0; y < 240; y++ {
		if px := img.RGBAAt(0, y); px == ghost {
			edge++
		}
	}
	if edge == 0 {
		t.Fatal("the off-frame commanded crop ghost never touches the frame edge")
	}
	// A fit record (no controller, all-zero telemetry) draws no ghost.
	img2 := g4Black(240, 240)
	drawOverlayBoxes(img2, FrameRecord{Frame: 1, Out: SpaceRects{Crop: Rect{X: 0, Y: 0, W: 240, H: 240}}}, PreviewOptions{DrawCropTarget: true}, st)
	if n := countColor(img2, ghost); n != 0 {
		t.Errorf("a fit record with no commanded crop drew %d ghost px", n)
	}
}

func TestOverlayPanTargetAxesAreTheCommandedOnes(t *testing.T) {
	st := defaultOverlayStyle()
	img := g4Black(240, 240)
	drawOverlayBoxes(img, g4Record(), PreviewOptions{DrawAxes: true}, st)

	axis := blendedOverBlack(st.AxisTarget)
	vAt, hAt := 0, 0
	for i := 0; i < 240; i++ {
		if img.RGBAAt(100, i) == axis {
			vAt++
		}
		if img.RGBAAt(i, 120) == axis {
			hAt++
		}
	}
	if vAt == 0 || hAt == 0 {
		t.Fatalf("no axis through the commanded pan target (vertical %d px, horizontal %d px)", vAt, hAt)
	}
	if vAt == 240 || hAt == 240 {
		t.Errorf("the target axis drew solid; the sheet laws it dashed (vertical %d, horizontal %d)", vAt, hAt)
	}
	// Zero pan target = no reading (fit law): nothing is drawn.
	img2 := g4Black(240, 240)
	drawOverlayBoxes(img2, FrameRecord{Frame: 1}, PreviewOptions{DrawAxes: true}, st)
	if n := countColor(img2, axis); n != 0 {
		t.Errorf("a record with no pan reading drew %d axis px", n)
	}
}

func TestOverlayCentroidMarker(t *testing.T) {
	st := defaultOverlayStyle()
	img := g4Black(240, 240)
	drawOverlayBoxes(img, g4Record(), PreviewOptions{DrawCentroid: true}, st)

	if n := countColor(img, st.Centroid); n == 0 {
		t.Fatal("no centroid dot")
	}
	if n := countColor(img, overlayDotRing); n == 0 {
		t.Error("centroid dot has no white ring")
	}
	// The full-height blended line at the centroid's output x.
	line := blendedOverBlack(st.CentroidLine)
	n := 0
	for y := 0; y < 240; y++ {
		if img.RGBAAt(50, y) == line {
			n++
		}
	}
	if n < 200 {
		t.Errorf("centroid line covers only %d rows of 240", n)
	}

	// CentroidKnown false (v1 cache, lost frame): NO phantom at the origin.
	rec := g4Record()
	rec.CentroidKnown = false
	img2 := g4Black(240, 240)
	drawOverlayBoxes(img2, rec, PreviewOptions{DrawCentroid: true}, st)
	if n := countColor(img2, st.Centroid); n != 0 {
		t.Errorf("an unknown centroid drew %d marker px anyway", n)
	}
	if n := countColor(img2, line); n != 0 {
		t.Errorf("an unknown centroid drew %d line px anyway", n)
	}
}

func TestOverlayReadoutChips(t *testing.T) {
	st := defaultOverlayStyle()
	img := g4Black(240, 240)
	drawOverlayBoxes(img, g4Record(), PreviewOptions{DrawReadouts: true}, st)

	if n := countColor(img, st.ChipFG); n == 0 {
		t.Error("no zoom-readout text pixels")
	}
	if n := countColor(img, st.LeashFG); n == 0 {
		t.Error("no walk-leash chip text")
	}
	if n := countColor(img, st.CutFG); n == 0 {
		t.Error("no scene-cut chip text")
	}
	if n := countColor(img, blendedOverBlack(st.ChipBG)); n == 0 {
		t.Error("no chip plate")
	}

	// Zoom 0 = "no reading" (fit law): no zoom chip, but the state chips stay.
	rec := g4Record()
	rec.ZoomCurrent, rec.ZoomTarget = 0, 0
	img2 := g4Black(240, 240)
	drawOverlayBoxes(img2, rec, PreviewOptions{DrawReadouts: true}, st)
	// ChipFG only ever spells the zoom readout here; a clean zero means no text.
	if n := countColor(img2, st.ChipFG); n != 0 {
		t.Errorf("a zoom-less record drew %d zoom-text px", n)
	}
	if n := countColor(img2, st.LeashFG); n == 0 {
		t.Error("the leash chip vanished with the zoom chip")
	}
}

// g4DetailMask: two accepted dancers 14 px apart (inside the tol = 360/20 = 18
// merge window, so pool 1 absorbs both while pool 2 keeps B's own box - the
// nesting pools) and one rejected speckle.
func g4DetailMask() *image.Gray {
	return solidMask(640, 360,
		image.Rect(100, 100, 180, 300),
		image.Rect(194, 120, 274, 300),
		image.Rect(500, 50, 508, 58),
	)
}

func TestSubjectLayersColourCodeBothLayers(t *testing.T) {
	st := defaultOverlayStyle()
	det := subjectDetailFromBinary(g4DetailMask())
	if len(det.Pools) != 2 || len(det.Components) != 3 {
		t.Fatalf("fixture detail: %d pools / %d components, want 2/3", len(det.Pools), len(det.Components))
	}
	rec := g4Record()
	rec.Crop = Rect{X: 0, Y: 0, W: 640, H: 360} // 1:2 identity mapping into 240x240

	img := g4Black(240, 240)
	drawSubjectLayers(img, rec, PreviewOptions{DrawSubjectBoxes: true}, st, det)

	if n := countColor(img, st.ComponentBig); n == 0 {
		t.Error("accepted raw components drew no magenta")
	}
	if n := countColor(img, st.ComponentSmall); n == 0 {
		t.Error("rejected raw components drew no violet")
	}
	if n := countColor(img, st.Pool); n == 0 {
		t.Error("merged pools drew no cyan")
	}
	// Island haze belongs to its own toggle (the judged sheet's off-by-default).
	if n := countColor(img, blendedOverBlack(st.IslandBig)); n != 0 {
		t.Errorf("island outlines drew %d px without DrawIslands", n)
	}

	img2 := g4Black(240, 240)
	drawSubjectLayers(img2, rec, PreviewOptions{DrawSubjectBoxes: true, DrawIslands: true}, st, det)
	if n := countColor(img2, blendedOverBlack(st.IslandBig)); n == 0 {
		t.Error("accepted islands drew no magenta outline haze")
	}
	if n := countColor(img2, blendedOverBlack(st.IslandSmall)); n == 0 {
		t.Error("rejected islands drew no violet outline haze")
	}

	// Neither toggle: a frame the caller never asked about paints nothing.
	img3 := g4Black(240, 240)
	drawSubjectLayers(img3, rec, PreviewOptions{}, st, det)
	for _, c := range []color.RGBA{st.Pool, st.ComponentBig, st.ComponentSmall} {
		if n := countColor(img3, c); n != 0 {
			t.Errorf("undetailed opts still drew %d px of %v", n, c)
		}
	}
}

func TestSubjectLayersMapThroughTheAppliedCropAndClip(t *testing.T) {
	st := defaultOverlayStyle()
	det := subjectDetailFromBinary(g4DetailMask())
	rec := g4Record()
	// The applied crop cuts the left dancer in half: analysis x 160..640 maps
	// 1:2 into the frame, so the component reaching left of the crop must draw
	// CLIPPED against the frame edge - the same law as every Out-space box.
	rec.Crop = Rect{X: 160, Y: 0, W: 480, H: 360}
	img := g4Black(240, 240)
	drawSubjectLayers(img, rec, PreviewOptions{DrawSubjectBoxes: true}, st, det)

	atEdge := 0
	for y := 0; y < 240; y++ {
		for x := 0; x < 10; x++ { // the clamped strip left of the crop's own edge
			if img.RGBAAt(x, y) == st.ComponentBig {
				atEdge++
			}
		}
	}
	if atEdge == 0 {
		t.Fatal("a component hanging off the applied crop left no trace at the frame edge")
	}
}

func TestOverlayDrawingIsDeterministic(t *testing.T) {
	st := defaultOverlayStyle()
	det := subjectDetailFromBinary(g4DetailMask())
	rec := g4Record()
	rec.Crop = Rect{X: 0, Y: 0, W: 640, H: 360}
	opts := PreviewOptions{
		DrawSubject: true, DrawCrop: true, DrawCentroid: true, DrawCropTarget: true,
		DrawAxes: true, DrawReadouts: true, DrawSubjectBoxes: true, DrawIslands: true,
	}
	a, b := g4Black(240, 240), g4Black(240, 240)
	for _, img := range []*image.RGBA{a, b} {
		drawSubjectLayers(img, rec, opts, st, det)
		drawOverlayBoxes(img, rec, opts, st)
	}
	for i := range a.Pix {
		if a.Pix[i] != b.Pix[i] {
			t.Fatalf("byte %d differs between two identical draws", i)
		}
	}
}

func TestMeasureDecodedFrameIsExactOnRealFrames(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	inputDir, tempDir := t.TempDir(), t.TempDir()
	fixture := buildFixture(t, ffmpeg, inputDir)
	task, err := Analyze(context.Background(), AnalyzeRequest{Input: fixture, TempDir: tempDir, FFmpegPath: ffmpeg})
	if err != nil {
		t.Fatal(err)
	}
	a, err := task.Wait()
	if err != nil {
		t.Fatal(err)
	}
	stream := a.Stream()

	measured := 0
	for i := 0; i < stream.Len(); i += 7 {
		f := stream.Frame(i)
		if f.Subjects == 0 {
			continue
		}
		frame, err := seekAnalysisFrame(a.AnalysisVideoPath(), i, stream.AnalysisWidth(), stream.AnalysisHeight(), stream.FPS())
		if err != nil {
			t.Fatal(err)
		}
		det, ok := stream.measureDecodedFrame(i, frame)
		if !ok {
			t.Fatalf("frame %d: a threshold-stamped detect frame must measure", i+1)
		}
		if len(det.Pools) != f.Subjects {
			t.Fatalf("frame %d: no-seek measurement found %d pools, the stream recorded %d subjects", i+1, len(det.Pools), f.Subjects)
		}
		union := image.ZR
		for _, p := range det.Pools {
			union = union.Union(p.Rect)
		}
		if !union.Eq(f.BBox.toImage()) {
			t.Fatalf("frame %d: pool union %v != the recorded bbox %v (the no-seek path must agree with the seek path)", i+1, union, f.BBox)
		}
		measured++
	}
	if measured == 0 {
		t.Fatal("the fixture produced no measurable frames")
	}

	// Honest failure, never an approximation: an unmeasurable stream or frame.
	if _, ok := stream.measureDecodedFrame(stream.Len(), image.NewRGBA(image.Rect(0, 0, 8, 8))); ok {
		t.Error("an out-of-range sample measured successfully")
	}
	synthetic := newTestStream([]streamSample{newStreamSample(image.Rect(10, 10, 40, 60), 1, image.Point{})}, 480, 270, 960, 540)
	if _, ok := synthetic.measureDecodedFrame(0, image.NewRGBA(image.Rect(0, 0, 480, 270))); ok {
		t.Error("a threshold-less synthetic stream claimed to measure")
	}
}

func TestPreviewCarriesTheG4OverlayLayers(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	inputDir, tempDir := t.TempDir(), t.TempDir()
	fixture := buildFixture(t, ffmpeg, inputDir)
	task, err := Analyze(context.Background(), AnalyzeRequest{Input: fixture, TempDir: tempDir, FFmpegPath: ffmpeg})
	if err != nil {
		t.Fatal(err)
	}
	a, err := task.Wait()
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewPlan(a.Stream(), Settings{Width: 240, Height: 240, FPS: fixtureFPS})
	if err != nil {
		t.Fatal(err)
	}
	pvTask, err := a.RenderPreview(context.Background(), plan, PreviewRequest{
		FFmpegPath: ffmpeg,
		Options: PreviewOptions{
			Kind: PreviewOverlay, DrawSubject: true, DrawCrop: true,
			DrawCentroid: true, DrawCropTarget: true, DrawAxes: true,
			DrawReadouts: true, DrawSubjectBoxes: true, DrawIslands: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	pv, err := pvTask.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if pv.OverlayVideoPath == "" || pv.Frames == 0 {
		t.Fatalf("the layer-rich overlay pass shipped no frames: %+v", pv)
	}
}
