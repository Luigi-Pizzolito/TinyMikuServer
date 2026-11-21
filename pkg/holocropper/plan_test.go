package holocropper

import (
	"errors"
	"fmt"
	"image"
	"math"
	"path/filepath"
	"testing"
	"time"
)

/* Plan (story A5) tests. These drive NewPlan over synthetic detection
streams - no video decoding needed - covering settings resolution,
auto-orient, stream incompatibility, dropout spans, walk telemetry,
coordinate-space precomputation and the benchmark gate (< 100 ms replay of
5 558 frames, the corpus 5-minute video). */

func newTestStream(samples []streamSample, aw, ah, srcW, srcH int) DetectionStream {
	sd := newStreamData()
	sd.samples = samples
	sd.analysisW, sd.analysisH = aw, ah
	sd.srcW, sd.srcH = srcW, srcH
	sd.fps = 24
	sd.complete = true
	return DetectionStream{data: sd}
}

func walkingSamples(n int, x0, step, w, h int) []streamSample {
	s := make([]streamSample, n)
	for i := 0; i < n; i++ {
		x := x0 + step*i
		s[i] = newStreamSample(image.Rect(x, 60, x+w, 60+h), 1, image.Point{})
	}
	return s
}

func TestNewPlanValidationErrors(t *testing.T) {
	stream := newTestStream(walkingSamples(10, 100, 4, 120, 300), 854, 480, 1280, 720)
	_, err := NewPlan(stream, Settings{Width: 8, Height: 0, FPS: 900})
	var se *SettingsError
	if !errors.As(err, &se) {
		t.Fatalf("want *SettingsError, got %v", err)
	}
	fields := map[string]bool{}
	for _, ve := range se.Errors {
		fields[ve.Field] = true
	}
	if !fields["width"] || !fields["fps"] {
		t.Fatalf("expected width+fps field errors, got %+v", se.Errors)
	}
}

func TestNewPlanStreamIncompatible(t *testing.T) {
	stream := newTestStream(walkingSamples(10, 100, 4, 120, 300), 428, 240, 960, 540)
	_, err := NewPlan(stream, Settings{Width: 420, Height: 400, FPS: 24})
	if !errors.Is(err, ErrStreamIncompatible) {
		t.Fatalf("want ErrStreamIncompatible, got %v", err)
	}

	// The incompatible height is the EFFECTIVE one after auto-orient flip.
	portrait := newTestStream(
		[]streamSample{newStreamSample(image.Rect(300, 60, 420, 460), 1, image.Point{})},
		640, 360, 960, 540)
	_, err = NewPlan(portrait, Settings{Width: 648, Height: 200, FPS: 24, AutoOrient: true})
	if !errors.Is(err, ErrStreamIncompatible) {
		t.Fatalf("flipped 200x648 from analysis height 360: want ErrStreamIncompatible, got %v", err)
	}
}

func TestNewPlanAutoOrientFlip(t *testing.T) {
	// Tall subjects (AR 0.3): landscape 648x200 must flip to 200x648.
	samples := walkingSamples(40, 300, 6, 120, 400)
	stream := newTestStream(samples, 1280, 720, 1280, 720)

	p, err := NewPlan(stream, Settings{Width: 648, Height: 200, FPS: 24, AutoOrient: true, AnalysisHeight: 720})
	if err != nil {
		t.Fatal(err)
	}
	r := p.Resolved()
	if r.OutputW != 200 || r.OutputH != 648 {
		t.Errorf("auto-orient flip: got %dx%d, want 200x648", r.OutputW, r.OutputH)
	}
	o := p.Orientation()
	if !o.Flip {
		t.Errorf("orientation Flip = false, want true")
	}
	if o.FlipScore <= o.KeepScore {
		t.Errorf("flip should score better: keep %.3f flip %.3f", o.KeepScore, o.FlipScore)
	}
	if o.FramesWithSubjects != 40 || o.FramesExamined != 40 {
		t.Errorf("orientation frames: %+v", o)
	}

	// Without auto-orient the requested orientation stands, stat untouched.
	p2, err := NewPlan(stream, Settings{Width: 648, Height: 200, FPS: 24, AnalysisHeight: 720})
	if err != nil {
		t.Fatal(err)
	}
	if r2 := p2.Resolved(); r2.OutputW != 648 || r2.OutputH != 200 {
		t.Errorf("auto-orient off: got %dx%d, want 648x200", r2.OutputW, r2.OutputH)
	}
	if o2 := p2.Orientation(); o2.Flip || o2.FramesExamined != 0 {
		t.Errorf("auto-orient off should leave the orientation stat untouched: %+v", o2)
	}
}

func TestNewPlanDropouts(t *testing.T) {
	samples := walkingSamples(60, 100, 4, 120, 300)
	for i := 10; i <= 14; i++ {
		samples[i] = streamSample{} // frames 11..15 (1-based) lost
	}
	for i := 56; i < 60; i++ {
		samples[i] = streamSample{} // frames 57..60 lost to end of stream
	}
	stream := newTestStream(samples, 854, 480, 1280, 720)
	p, err := NewPlan(stream, Settings{Width: 240, Height: 240, FPS: 24})
	if err != nil {
		t.Fatal(err)
	}
	d := p.DropoutFrames()
	if len(d) != 2 {
		t.Fatalf("want 2 dropout spans, got %+v", d)
	}
	if d[0] != (IntRange{Start: 11, End: 15}) {
		t.Errorf("first span: %+v want {11 15}", d[0])
	}
	if d[1] != (IntRange{Start: 57, End: 60}) {
		t.Errorf("second span: %+v want {57 60}", d[1])
	}
	for _, i := range []int{11, 12, 13, 14, 15, 57, 60} {
		if !p.Record(i - 1).SubjectLost {
			t.Errorf("frame %d not flagged lost", i)
		}
	}
}

func TestNewPlanWalkTelemetry(t *testing.T) {
	// Leashed walk: gate active, crop roams once slack exhausts, no fallback.
	stream := newTestStream(walkingSamples(200, 100, 8, 150, 420), 1280, 720, 1280, 720)
	p, err := NewPlan(stream, Settings{Width: 648, Height: 200, FPS: 24})
	if err != nil {
		t.Fatal(err)
	}
	w := p.Walk()
	if !w.Active {
		t.Fatalf("walk gate should be active for 648x200 from a 16:9 source")
	}
	if w.FallbackFrames != 0 {
		t.Errorf("fallback frames: got %d, want 0", w.FallbackFrames)
	}
	if w.RangePx < 50 {
		t.Errorf("walk range too small: %.1f px", w.RangePx)
	}
	if !p.Record(150).WalkActive {
		t.Errorf("frame 151: expected an active leash")
	}

	// Subject too wide to leash once the crop settles: dx > 80% of the crop
	// width. The slow zoom-in cap (0.25 px/frame) means the crop needs ~1000
	// frames to shrink under the 1260 px wide subject, after which every
	// frame falls back to subject centering while the gate stays active.
	wide := newTestStream([]streamSample{}, 1280, 720, 1280, 720)
	for i := 0; i < 1200; i++ {
		wide.data.samples = append(wide.data.samples, newStreamSample(image.Rect(10, 260, 1270, 460), 1, image.Point{}))
	}
	p2, err := NewPlan(wide, Settings{Width: 648, Height: 200, FPS: 24})
	if err != nil {
		t.Fatal(err)
	}
	w2 := p2.Walk()
	if !w2.Active {
		t.Fatalf("walk gate should be active for 648x200 from a 16:9 source")
	}
	if w2.FallbackFrames < 100 {
		t.Errorf("wide-subject fallback frames: got %d, want >= 100 (crop settles at frame ~1045)", w2.FallbackFrames)
	}
	if w2.RangePx != 0 {
		t.Errorf("static wide subject should not roam: range %.1f", w2.RangePx)
	}

	// Square output: gate never active.
	p3, err := NewPlan(newTestStream(walkingSamples(20, 100, 4, 120, 300), 854, 480, 1280, 720),
		Settings{Width: 240, Height: 240, FPS: 24})
	if err != nil {
		t.Fatal(err)
	}
	if p3.Walk().Active {
		t.Errorf("square output must not activate the walk gate")
	}
}

func TestNewPlanRecordSpaces(t *testing.T) {
	stream := newTestStream(walkingSamples(80, 200, 6, 140, 320), 854, 480, 1280, 720)
	p, err := NewPlan(stream, Settings{Width: 240, Height: 240, FPS: 24})
	if err != nil {
		t.Fatal(err)
	}
	recs := p.Records()
	if len(recs) != 80 {
		t.Fatalf("records: %d want 80", len(recs))
	}
	for i, r := range recs {
		if r.Frame != i+1 {
			t.Fatalf("record %d: Frame = %d (want %d)", i, r.Frame, i+1)
		}
		// Norm space = analysis px / analysis frame, exactly.
		if math.Abs(r.Norm.Crop.X*854-r.Crop.X) > 1e-9 || math.Abs(r.Norm.Crop.H*480-r.Crop.H) > 1e-9 {
			t.Fatalf("frame %d: Norm crop not derived from analysis crop", r.Frame)
		}
		// The applied crop fills the output frame exactly.
		out := r.Out.Crop
		if math.Abs(out.X) > 1e-9 || math.Abs(out.Y) > 1e-9 ||
			math.Abs(out.W-240) > 1e-9 || math.Abs(out.H-240) > 1e-9 {
			t.Fatalf("frame %d: Out.Crop = %+v, want {0 0 240 240}", r.Frame, out)
		}
		// The subject maps through the same transform: analysis center ->
		// output center offset from the crop center, scaled by out/crop.
		if r.SubjectLost || r.BBox.W <= 0 {
			continue
		}
		sx := 240 / r.Crop.W
		wantX := (r.BBox.X - r.Crop.X) * sx
		if math.Abs(r.Out.BBox.X-wantX) > 1e-9 {
			t.Fatalf("frame %d: Out.BBox.X = %.4f, want %.4f", r.Frame, r.Out.BBox.X, wantX)
		}
	}
}

// TestNormalizeRectKeepsStagePaddingOutsideUnit: the Norm space is division,
// never clamping. A landscape stage-padding crop (§12.8) sits partly outside
// the analysis frame and its normalized copy must say so - the overlay law a
// consumer has to be able to follow is "draw the crop window where it truly
// is", not "where the frame happens to end". Only draw.clampToFrame clamps,
// and only for burned-in box drawing.
func TestNormalizeRectKeepsStagePaddingOutsideUnit(t *testing.T) {
	// The shape TestPlanNormCropCarriesLandscapeStagePadding's replay answers at
	// a settled frame: a 3.24:1 crop standing on the frame floor, overflowing
	// both x edges.
	crop := Rect{X: -310.1, Y: 25, W: 1474.2, H: 455}
	n := normalizeRect(crop, 854, 480)
	if n.X >= 0 || n.X+n.W <= 1 {
		t.Fatalf("Norm crop %+v was pulled back inside the frame: the padding is not reported", n)
	}
	// Division only, on all four edges.
	if math.Abs(n.X*854-crop.X) > 1e-9 || math.Abs(n.W*854-crop.W) > 1e-9 ||
		math.Abs(n.Y*480-crop.Y) > 1e-9 || math.Abs(n.H*480-crop.H) > 1e-9 {
		t.Fatalf("Norm crop %+v is not the crop divided by the frame", n)
	}
	// Vertical extents are always fully in-frame, so they stay in 0..1.
	if n.Y < 0 || n.Y+n.H > 1 {
		t.Fatalf("Norm crop %+v left the frame vertically", n)
	}
	if got := normalizeRect(crop, 0, 480); got != (Rect{}) {
		t.Fatalf("degenerate frame answered %+v, want the zero rect", got)
	}
}

// TestPlanNormCropCarriesLandscapeStagePadding: the same law read off a real
// replay, because it is the records (not the helper) a UI consumes. A 350x300
// subject standing on the frame floor of a 16:9 analysis frame, rendered
// 648x200, cannot be contained by any in-frame 3.24:1 crop: framing pads the
// stage instead (§12.8/§12.10). Every frame's crop leaves the frame's left AND
// right edges, and the Norm space carries that out-of-range geometry so a UI
// overlaying the ORIGINAL video can draw the window's off-frame edges as gaps
// instead of painting a border at the source edge.
func TestPlanNormCropCarriesLandscapeStagePadding(t *testing.T) {
	samples := make([]streamSample, 80)
	for i := range samples {
		x := 200 + 4*i
		if x+350 > 854 {
			x = 854 - 350
		}
		samples[i] = newStreamSample(image.Rect(x, 180, x+350, 480), 1, image.Point{}) // feet on the frame floor
	}
	p, err := NewPlan(newTestStream(samples, 854, 480, 1280, 720), Settings{Width: 648, Height: 200, FPS: 24})
	if err != nil {
		t.Fatal(err)
	}
	recs := p.Records()
	if len(recs) != 80 {
		t.Fatalf("records: %d want 80", len(recs))
	}
	for _, r := range recs {
		// Norm = analysis px / analysis frame, exactly, out-of-range included.
		if math.Abs(r.Norm.Crop.X*854-r.Crop.X) > 1e-9 || math.Abs(r.Norm.Crop.W*854-r.Crop.W) > 1e-9 ||
			math.Abs(r.Norm.Crop.Y*480-r.Crop.Y) > 1e-9 || math.Abs(r.Norm.Crop.H*480-r.Crop.H) > 1e-9 {
			t.Fatalf("frame %d: Norm crop %+v not the analysis crop divided by the frame", r.Frame, r.Norm.Crop)
		}
		if r.Norm.Crop.X >= 0 || r.Norm.Crop.X+r.Norm.Crop.W <= 1 {
			t.Fatalf("frame %d: Norm crop %+v never leaves the frame horizontally: stage padding was hidden", r.Frame, r.Norm.Crop)
		}
		if r.Norm.Crop.Y < 0 || r.Norm.Crop.Y+r.Norm.Crop.H > 1 {
			t.Fatalf("frame %d: Norm crop %+v escaped the frame vertically", r.Frame, r.Norm.Crop)
		}
		// The overflow is stage padding, not a broken crop: aspect-locked and
		// within the §12.8 per-side cap.
		if math.Abs(r.Crop.W/r.Crop.H-648.0/200) > 0.01 {
			t.Fatalf("frame %d: crop %+v not aspect-locked to 648:200", r.Frame, r.Crop)
		}
		if pad := (r.Crop.W - 854) / 2; pad > landscapePadCapFrac*854+1 {
			t.Fatalf("frame %d: side padding %.1f exceeds the cap %.1f", r.Frame, pad, landscapePadCapFrac*854)
		}
		// The padding is inside the OUTPUT: the crop always fills it exactly.
		if r.Out.Crop != (Rect{X: 0, Y: 0, W: 648, H: 200}) {
			t.Fatalf("frame %d: Out.Crop %+v, want the whole output frame", r.Frame, r.Out.Crop)
		}
	}
}

func TestNewPlanSettingsHash(t *testing.T) {
	stream := newTestStream(walkingSamples(20, 100, 4, 120, 300), 854, 480, 1280, 720)
	other := newTestStream(walkingSamples(30, 400, 2, 120, 300), 854, 480, 1280, 720)

	base := Settings{Width: 240, Height: 240, FPS: 24}
	explicit := Settings{Width: 240, Height: 240, FPS: 24, AnalysisHeight: 480, ZoomMargin: 1.12, MaxZoom: 2.0}
	pa, err := NewPlan(stream, base)
	if err != nil {
		t.Fatal(err)
	}
	pb, err := NewPlan(other, explicit)
	if err != nil {
		t.Fatal(err)
	}
	if pa.SettingsHash() != pb.SettingsHash() {
		t.Errorf("defaults and equivalent explicit settings must hash identically: %s vs %s",
			pa.SettingsHash(), pb.SettingsHash())
	}

	diffs := []Settings{
		{Width: 242, Height: 240, FPS: 24},
		{Width: 240, Height: 240, FPS: 30},
		{Width: 240, Height: 240, FPS: 24, ZoomMargin: 1.3},
		{Width: 240, Height: 240, FPS: 24, MaxZoom: 1.5},
		{Width: 240, Height: 240, FPS: 24, AnalysisHeight: 720},
	}
	seen := map[string]string{pa.SettingsHash(): "base"}
	for _, s := range diffs {
		pd, err := NewPlan(stream, s)
		if err != nil {
			t.Fatal(err)
		}
		h := pd.SettingsHash()
		if h == pa.SettingsHash() {
			t.Errorf("settings %+v hashed as base", s)
		}
		if prev, ok := seen[h]; ok {
			t.Errorf("settings %+v collide with %s", s, prev)
		}
		seen[h] = fmtSettings(s)
	}
}

func fmtSettings(s Settings) string {
	return fmt.Sprintf("%+v", s)
}

func TestNewPlanEmptyAndPartialStreams(t *testing.T) {
	// Geometry not probed yet (analyze barely started): valid empty Plan.
	p, err := NewPlan(newTestStream(nil, 0, 0, 0, 0), Settings{Width: 240, Height: 240, FPS: 24})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Records()) != 0 {
		t.Fatalf("unprobed stream should replay to zero records, got %d", len(p.Records()))
	}
	if p.SettingsHash() == "" {
		t.Error("empty Plan must still carry its settings hash")
	}

	// Partial: samples present but only part of the video detected so far.
	stream := newTestStream(walkingSamples(17, 200, 6, 120, 300), 854, 480, 1280, 720)
	stream.data.mu.Lock()
	stream.data.complete = false
	stream.data.mu.Unlock()
	p2, err := NewPlan(stream, Settings{Width: 240, Height: 240, FPS: 24})
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.Records()) != 17 {
		t.Fatalf("partial replay: got %d records, want 17", len(p2.Records()))
	}
	if stream.Complete() {
		t.Error("partial stream must report Complete() == false")
	}
}

// Benchmark gate (story A5 acceptance): replaying 5 558 frames (the corpus
// 5-minute video at 18.6 fps) must complete in well under 100 ms.
func TestNewPlanBenchmark(t *testing.T) {
	stream := newTestStream(walkingSamples(5558, 100, 3, 150, 420), 854, 480, 1280, 720)
	settings := Settings{Width: 240, Height: 240, FPS: 20}

	start := time.Now()
	p, err := NewPlan(stream, settings)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Records()) != 5558 {
		t.Fatalf("records: %d want 5558", len(p.Records()))
	}
	t.Logf("NewPlan replay of 5558 frames: %v", elapsed)
	if elapsed > 100*time.Millisecond {
		t.Fatalf("benchmark gate: %v exceeds 100 ms", elapsed)
	}

	// With auto-orient enabled (stream-wide AR aggregates + flip replay). The
	// 720-tall analysis frame keeps the flipped 200x648 output compatible.
	tall := newTestStream(walkingSamples(5558, 100, 3, 150, 420), 1280, 720, 1280, 720)
	start = time.Now()
	if _, err := NewPlan(tall, Settings{Width: 648, Height: 200, FPS: 20, AutoOrient: true}); err != nil {
		t.Fatal(err)
	}
	t.Logf("auto-orient replay of 5558 frames: %v", time.Since(start))
}

// The HOLOCROPPER_DEBUG_BOXES shim on the replay path (story C3): with the
// env set, NewPlan writes the same 16-column CSV the fused CLI path wrote -
// one line per frame, values straight from the records it returns.
func TestPlanDebugCSVMatchesRecords(t *testing.T) {
	csvPath := filepath.Join(t.TempDir(), "plan.csv")
	t.Setenv("HOLOCROPPER_DEBUG_BOXES", csvPath)

	s := newTestStream(walkingSamples(12, 100, 4, 120, 200), 428, 240, fixtureW, fixtureH)
	p, err := NewPlan(s, Settings{Width: 240, Height: 240, FPS: fixtureFPS})
	if err != nil {
		t.Fatal(err)
	}

	rows := readDebugCSV(t, csvPath)
	recs := p.Records()
	if len(rows) != len(recs) {
		t.Fatalf("shim wrote %d rows for %d records", len(rows), len(recs))
	}
	const tol = 0.011 // the CSV's own 2-decimal rounding band, plus a hair
	for i, r := range rows {
		rec := recs[i]
		if r.frame != rec.Frame || r.subjects != rec.Subjects {
			t.Errorf("row %d: frame/subjects %d/%d vs record %d/%d", i, r.frame, r.subjects, rec.Frame, rec.Subjects)
		}
		if r.bboxX0 != int(rec.BBox.X) || r.bboxY0 != int(rec.BBox.Y) ||
			r.bboxW != int(rec.BBox.W) || r.bboxH != int(rec.BBox.H) {
			t.Errorf("frame %d: CSV bbox {%d,%d,%d,%d} vs record %+v", r.frame, r.bboxX0, r.bboxY0, r.bboxW, r.bboxH, rec.BBox)
		}
		if r.emaX0 != int(rec.EMA.X) || r.emaY0 != int(rec.EMA.Y) ||
			r.emaW != int(rec.EMA.W) || r.emaH != int(rec.EMA.H) {
			t.Errorf("frame %d: CSV ema {%d,%d,%d,%d} vs record %+v", r.frame, r.emaX0, r.emaY0, r.emaW, r.emaH, rec.EMA)
		}
		cropCx, cropCy := rec.Crop.X+rec.Crop.W/2, rec.Crop.Y+rec.Crop.H/2
		for _, pr := range [][2]float64{
			{r.cropCx, cropCx}, {r.cropCy, cropCy}, {r.cropW, rec.Crop.W}, {r.cropH, rec.Crop.H},
			{r.targetH, rec.TargetH}, {r.walkTgt, rec.WalkTgt},
		} {
			if math.Abs(pr[0]-pr[1]) > tol {
				t.Errorf("frame %d: CSV %.4f vs record %.4f (drift > %.3f)", r.frame, pr[0], pr[1], tol)
			}
		}
	}
}
