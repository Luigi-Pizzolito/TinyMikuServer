package holocropper

import (
	"image"
	"math"
	"testing"
)

/* Milestone G2 pins (plan HOLOCROPPER-API-PLAN.md §10/§11): the framing
controller's COMMANDED geometry reaches the FrameRecords, so the embedding app
can draw what the controllers asked for next to what containment applied.
Fixture-free: synthetic streams through the real controller, values measured
off the controller's own output. */

func edgePinnedStream(n int) []streamSample {
	s := make([]streamSample, n)
	for i := range s { // a 200x600 subject jammed against the left frame edge
		s[i] = newStreamSample(image.Rect(0, 100, 200, 700), 1, image.Point{X: 100, Y: 400})
	}
	return s
}

func TestPlanTelemetryCommandedVsApplied(t *testing.T) {
	const aw, ah = 1200, 800
	stream := newTestStream(edgePinnedStream(140), aw, ah, aw, ah)
	plan, err := NewPlan(stream, Settings{Width: 240, Height: 240, FPS: 24})
	if err != nil {
		t.Fatal(err)
	}
	recs := plan.Records()

	// Frame 1: the plant starts at the full frame, so the commanded zoom is
	// already ahead of the (rate-limited) applied one.
	first := recs[0]
	if first.ZoomCurrent < 1 || first.ZoomCurrent > 1.02 {
		t.Fatalf("frame 1 zoom current %.4f: the plant starts at the full frame", first.ZoomCurrent)
	}
	if first.ZoomTarget <= first.ZoomCurrent {
		t.Fatalf("frame 1: zoom target %.4f not ahead of current %.4f", first.ZoomTarget, first.ZoomCurrent)
	}

	// Settled frames: containment holds the crop flush against the left frame
	// edge, and the controller keeps asking for more than it is granted. That
	// gap - bounded by the pan rate cap - is what the target-crop overlay shows.
	for _, r := range recs[100:] {
		if r.CropTarget.W != r.Crop.W || r.CropTarget.H != r.Crop.H {
			t.Fatalf("frame %d: commanded box %v differs in size from applied %v where no width fold applies",
				r.Frame, r.CropTarget, r.Crop)
		}
		if math.Abs(r.Crop.X) > 1e-9 {
			t.Fatalf("frame %d: applied crop %v is not held flush against the left frame edge", r.Frame, r.Crop)
		}
		gap := r.Crop.X - r.CropTarget.X
		if gap <= 1 {
			t.Fatalf("frame %d: commanded crop %v never asks for more than containment granted %v", r.Frame, r.CropTarget, r.Crop)
		}
		if cap := panCapFrac * r.Crop.W; gap > cap+1e-9 {
			t.Fatalf("frame %d: commanded overshoot %.2f px exceeds the pan rate cap %.2f px", r.Frame, gap, cap)
		}
		// The pan SETPOINT is the subject; the applied centre is the clamp.
		if math.Abs(r.PanTarget.X-100) > 0.01 {
			t.Fatalf("frame %d: pan target x %.4f is not the subject centre 100", r.Frame, r.PanTarget.X)
		}
		if centre := r.Crop.X + r.Crop.W/2; centre-r.PanTarget.X < 200 {
			t.Fatalf("frame %d: applied centre %.2f is not visibly held off the pan target %.2f", r.Frame, centre, r.PanTarget.X)
		}
		if r.WalkActive {
			t.Fatalf("frame %d: walk leash active on a square output", r.Frame)
		}
	}

	// The zoom readouts are the recorded geometry restated: factor = analysis
	// height / crop height, target from target_h, 1 = full frame.
	for _, r := range recs {
		if r.ZoomCurrent != float64(ah)/r.Crop.H {
			t.Fatalf("frame %d: zoom current %.6f != %d/%.6f", r.Frame, r.ZoomCurrent, ah, r.Crop.H)
		}
		if r.ZoomTarget != float64(ah)/r.TargetH {
			t.Fatalf("frame %d: zoom target %.6f != %d/%.6f", r.Frame, r.ZoomTarget, ah, r.TargetH)
		}
		if r.ZoomTarget < 1 {
			t.Fatalf("frame %d: zoom target %.4f below the full frame", r.Frame, r.ZoomTarget)
		}
	}

	// Normalized space: the same numbers over the analysis frame, including the
	// off-frame command (negative X) — the app draws that as its off-frame hint.
	for _, r := range recs[100:110] {
		n := r.Norm
		if math.Abs(n.CropTarget.X-r.CropTarget.X/aw) > 1e-12 || math.Abs(n.CropTarget.Y-r.CropTarget.Y/ah) > 1e-12 {
			t.Fatalf("frame %d: Norm.CropTarget %v is not CropTarget over the analysis frame", r.Frame, n.CropTarget)
		}
		if math.Abs(n.CropTarget.W-r.CropTarget.W/aw) > 1e-12 || math.Abs(n.CropTarget.H-r.CropTarget.H/ah) > 1e-12 {
			t.Fatalf("frame %d: Norm.CropTarget size %v is not normalized", r.Frame, n.CropTarget)
		}
		if math.Abs(n.PanTarget.X-r.PanTarget.X/aw) > 1e-12 || math.Abs(n.PanTarget.Y-r.PanTarget.Y/ah) > 1e-12 {
			t.Fatalf("frame %d: Norm.PanTarget %v is not normalized", r.Frame, n.PanTarget)
		}
		if n.CropTarget.X >= 0 {
			t.Fatalf("frame %d: Norm.CropTarget %v hides that the commanded box sits off the left edge", r.Frame, n.CropTarget)
		}
		if n.PanTarget.X < 0 || n.PanTarget.X > 1 || n.PanTarget.Y < 0 || n.PanTarget.Y > 1 {
			t.Fatalf("frame %d: Norm.PanTarget %v outside the analysis frame", r.Frame, n.PanTarget)
		}
	}

	// Output space maps the commanded box through the APPLIED crop.
	for _, r := range recs[100:110] {
		if math.Abs(r.Out.Crop.X) > 1e-6 || math.Abs(r.Out.Crop.Y) > 1e-6 ||
			math.Abs(r.Out.Crop.W-240) > 1e-6 || math.Abs(r.Out.Crop.H-240) > 1e-6 {
			t.Fatalf("frame %d: Out.Crop %v is not the whole output", r.Frame, r.Out.Crop)
		}
		if want := mapRectToOutput(r.CropTarget, r.Crop, 240, 240); r.Out.CropTarget != want {
			t.Fatalf("frame %d: Out.CropTarget %v != mapped through the applied crop %v", r.Frame, r.Out.CropTarget, want)
		}
		if r.Out.CropTarget.X >= 0 {
			t.Fatalf("frame %d: Out.CropTarget %v hides that the command reaches outside the shipped frame", r.Frame, r.Out.CropTarget)
		}
	}
}

func floorStandingStream(n int) []streamSample {
	s := make([]streamSample, n)
	for i := range s { // feet on the analysis frame floor
		s[i] = newStreamSample(image.Rect(500, 140, 700, 540), 1, image.Point{X: 600, Y: 340})
	}
	return s
}

func TestPlanTelemetryGroundedPinAndWalkLeash(t *testing.T) {
	const aw, ah = 1280, 540
	stream := newTestStream(floorStandingStream(120), aw, ah, aw, ah)
	plan, err := NewPlan(stream, Settings{Width: 648, Height: 200, FPS: 24})
	if err != nil {
		t.Fatal(err)
	}
	recs := plan.Records()

	grounded, leashed := 0, 0
	for _, r := range recs[60:] {
		cropCy := r.Crop.Y + r.Crop.H/2
		if r.Grounded {
			grounded++
			// On a grounded frame the floor pin IS the vertical setpoint, and
			// it applies exactly: commanded == applied vertically.
			if math.Abs(r.PanTarget.Y-cropCy) > 1e-9 {
				t.Fatalf("frame %d: grounded pan target y %.6f != the pinned crop centre %.6f", r.Frame, r.PanTarget.Y, cropCy)
			}
			if r.CropTarget != r.Crop {
				t.Fatalf("frame %d: a grounded frame whose crop containment grants in full shows a divergence: commanded %v applied %v", r.Frame, r.CropTarget, r.Crop)
			}
			if math.Abs(r.Crop.Y+r.Crop.H-ah) > 1e-9 {
				t.Fatalf("frame %d: grounded crop %v does not rest on the frame floor %d", r.Frame, r.Crop, ah)
			}
		}
		if r.WalkActive {
			leashed++
			if r.PanTarget.X != r.WalkTgt {
				t.Fatalf("frame %d: walk-active pan target x %.4f != the leash target %.4f", r.Frame, r.PanTarget.X, r.WalkTgt)
			}
		}
	}
	if grounded == 0 {
		t.Fatal("no settled frame of a floor-standing subject is grounded")
	}
	if leashed == 0 {
		t.Fatal("a landscape output off a landscape source never engaged the walk leash")
	}
}

func TestPlanTelemetryOpensToFullFrameWhileLost(t *testing.T) {
	samples := edgePinnedStream(30)
	for i := 0; i < 15; i++ {
		samples = append(samples, newStreamSample(image.ZR, 0, image.Point{})) // subject gone
	}
	samples = append(samples, edgePinnedStream(15)...)

	stream := newTestStream(samples, 1200, 800, 1200, 800)
	plan, err := NewPlan(stream, Settings{Width: 240, Height: 240, FPS: 24})
	if err != nil {
		t.Fatal(err)
	}
	lostSeen := 0
	for _, r := range plan.Records() {
		if !r.SubjectLost {
			continue
		}
		lostSeen++
		// A lost subject opens the zoom target to the full frame instead of
		// holding a stale subject-derived zoom.
		if r.ZoomTarget < 1 || r.ZoomTarget > 1.000001 {
			t.Fatalf("frame %d: lost-subject zoom target %.4f is not the full frame", r.Frame, r.ZoomTarget)
		}
		if r.CentroidKnown {
			t.Fatalf("frame %d: a lost subject reports a known centroid", r.Frame)
		}
		if r.CropTarget.W <= 0 || r.CropTarget.H <= 0 {
			t.Fatalf("frame %d: no commanded crop on a dropout frame (the plant keeps running)", r.Frame)
		}
	}
	if lostSeen < 10 {
		t.Fatalf("only %d frames report a lost subject across a 15-frame dropout", lostSeen)
	}
}

func TestPlanTelemetryShowsTheZoomPlantRacingItsTarget(t *testing.T) {
	// A centred subject in a square output: containment never refuses anything
	// (commanded == applied every frame), while the rate-limited zoom plant is
	// still closing on its target — the readout that makes the crop's motion
	// legible to the app.
	const aw, ah = 900, 900
	samples := make([]streamSample, 60)
	for i := range samples {
		samples[i] = newStreamSample(image.Rect(380, 200, 520, 700), 1, image.Point{X: 450, Y: 450})
	}
	stream := newTestStream(samples, aw, ah, aw, ah)
	plan, err := NewPlan(stream, Settings{Width: 240, Height: 240, FPS: 24})
	if err != nil {
		t.Fatal(err)
	}
	recs := plan.Records()
	for _, r := range recs[50:] {
		if r.CropTarget.X != r.Crop.X || r.CropTarget.Y != r.Crop.Y {
			t.Fatalf("frame %d: a centred subject's commanded box %v is not what was applied %v", r.Frame, r.CropTarget, r.Crop)
		}
		if r.CropTarget.W != r.Crop.W || r.CropTarget.H != r.Crop.H {
			t.Fatalf("frame %d: commanded size %vx%v != applied %vx%v", r.Frame, r.CropTarget.W, r.CropTarget.H, r.Crop.W, r.Crop.H)
		}
		if !(r.ZoomCurrent > 1 && r.ZoomCurrent < r.ZoomTarget) {
			t.Fatalf("frame %d: zoom readout %.4f -> %.4f does not show the plant closing on its target", r.Frame, r.ZoomCurrent, r.ZoomTarget)
		}
		if math.Abs(r.PanTarget.X-450) > 0.01 || math.Abs(r.PanTarget.Y-450) > 0.01 {
			t.Fatalf("frame %d: pan target (%.2f,%.2f) is not the centred subject (450,450)", r.Frame, r.PanTarget.X, r.PanTarget.Y)
		}
	}
}

func TestFitPlanLeavesFramingTelemetryUnrecorded(t *testing.T) {
	samples := walkingSamples(20, 100, 6, 120, 300)
	for i := range samples {
		samples[i] = newStreamSample(samples[i].bbox(), 1, image.Point{X: 160, Y: 210})
	}
	stream := newTestStream(samples, 854, 480, 1280, 720)
	plan, err := NewFitPlan(stream, Settings{Width: 420, Height: 240, FPS: 24})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range plan.Records() {
		if r.CropTarget != (Rect{}) {
			t.Fatalf("frame %d: fit record carries a commanded crop %v — a bars fit runs no controller", r.Frame, r.CropTarget)
		}
		if r.PanTarget != (FPoint{}) {
			t.Fatalf("frame %d: fit record carries a pan target %v", r.Frame, r.PanTarget)
		}
		if r.ZoomCurrent != 0 || r.ZoomTarget != 0 {
			t.Fatalf("frame %d: fit record carries zoom readouts %.3f/%.3f — a bars fit never zooms", r.Frame, r.ZoomCurrent, r.ZoomTarget)
		}
	}
}
