package holocropper

import (
	"image"
	"math"
	"testing"
)

/* framingDecider tests (HOLOCROPPER-API-PLAN.md story A3).

The decider must be (a) deterministic — the same bbox stream always replays
to the same framing, and (b) behaviourally identical to the old fused
calcVideo loop: plant updates, reset semantics and the walk leash must behave
as the framing tests already pin down for the primitives. Full end-to-end
parity (decider records vs CLI debug CSV on a real video) is covered by the
fixture parity test in the render stories. */

type bboxStep struct {
	bbox     image.Rectangle
	subjects int
}

func steadyStream(n int, r image.Rectangle) []bboxStep {
	s := make([]bboxStep, n)
	for i := range s {
		s[i] = bboxStep{r, 1}
	}
	return s
}

func runDecider(d *framingDecider, stream []bboxStep) []framingOutput {
	bounds := image.Rect(0, 0, 854, 480)
	out := make([]framingOutput, 0, len(stream))
	for i, s := range stream {
		out = append(out, d.step(i+1, s.bbox, s.subjects, bounds))
	}
	return out
}

func defaultSquareDecider() *framingDecider {
	return newFramingDecider(
		framingParams{Margin: 1.12, MaxZoom: 2.0, OutH: 240, SrcH: 720},
		1.0, 1.0/20, 1280, 720)
}

// The replay engine must produce bit-identical framing across runs: two
// fresh deciders over the same bbox stream agree exactly (this is the
// frame-determinism property the corpus verification workflow relies on).
func TestDeciderDeterminism(t *testing.T) {
	// Walking subject with two dropout spans and one scene cut.
	var stream []bboxStep
	for i := 0; i < 120; i++ {
		x := 200 + 3*i
		r := image.Rect(x, 90, x+140, 430)
		subj := 1
		if i >= 40 && i < 46 {
			r, subj = image.ZR, 0 // dropout
		}
		if i == 80 {
			r = image.Rect(650, 60, 800, 420) // scene cut (big jump)
		}
		stream = append(stream, bboxStep{r, subj})
	}

	a := runDecider(defaultSquareDecider(), stream)
	b := runDecider(defaultSquareDecider(), stream)
	if len(a) != len(b) {
		t.Fatalf("length mismatch %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("frame %d diverged:\n  a=%+v\n  b=%+v", i+1, a[i], b[i])
		}
	}
}

// A steady subject must pull the crop onto it: after settling the crop
// center sits on the subject and crop height reaches the floored zoom
// target (bbox 280 px tall * 1.12 margin = 313.6, maxZoom floor 480/2 = 240
// does not bind, no-upscale floor 240*480/720 = 160 does not bind).
func TestDeciderConvergesOnSubject(t *testing.T) {
	d := defaultSquareDecider()
	subject := image.Rect(350, 100, 490, 380)
	out := runDecider(d, steadyStream(500, subject))
	last := out[len(out)-1]

	wantCx := float64(subject.Min.X+subject.Max.X) / 2
	wantH := float64(subject.Dy()) * 1.12
	if math.Abs(last.CropCx-wantCx) > 2 {
		t.Errorf("crop cx %.2f not on subject cx %.2f", last.CropCx, wantCx)
	}
	if math.Abs(last.CropCy-float64(subject.Min.Y+subject.Max.Y)/2) > 2 {
		t.Errorf("crop cy %.2f not on subject cy", last.CropCy)
	}
	if math.Abs(last.CropH-wantH) > 5 {
		// The zoom plant settles inside a small offset band: the slow zoom-in
		// cap (0.25 px/frame) with PI windup can leave a few px of steady-state
		// offset, same tolerance regime as the framing tests.
		t.Errorf("cropH %.2f not at target %.2f", last.CropH, wantH)
	}
	for i, o := range out {
		if o.SubjectLost || o.SceneCut {
			t.Fatalf("frame %d: unexpected lost=%v cut=%v", i+1, o.SubjectLost, o.SceneCut)
		}
	}
}

// Tracking flags: the first subject is an acquisition (no cut), a bbox jump
// past cutJumpFraction is a scene cut with a tracking reset, empty bboxes
// are dropouts, and dropouts open the crop back toward the frame.
func TestDeciderCutLostFlags(t *testing.T) {
	d := defaultSquareDecider()
	static := image.Rect(300, 100, 400, 380)
	jumped := image.Rect(700, 100, 800, 380) // dx=400 > 0.25*854
	var stream []bboxStep
	stream = append(stream, steadyStream(40, static)...)
	stream = append(stream, bboxStep{image.ZR, 0}, bboxStep{image.ZR, 0})
	stream = append(stream, steadyStream(30, static)...)
	stream = append(stream, bboxStep{jumped, 1})
	out := runDecider(d, stream)

	if out[0].SceneCut {
		t.Errorf("frame 1: first subject must not report a scene cut")
	}
	for i := 40; i < 42; i++ {
		if !out[i].SubjectLost {
			t.Errorf("frame %d: dropout not flagged", i+1)
		}
	}
	if out[42].SceneCut {
		t.Errorf("frame 43: reacquisition after dropout must not report a cut")
	}
	if out[72].SubjectLost {
		t.Errorf("frame 73: steady reacquisition flagged lost")
	}
	if !out[72].SceneCut {
		t.Fatalf("frame 73: bbox jump must flag a scene cut")
	}

	// Crop must be converging to the (small) subject before the dropout, and
	// opening toward the frame cap after it (dropout forces tgtH = frameH).
	settled := out[39]
	lostTarget := out[41]
	if !(settled.TargetH < lostTarget.TargetH) {
		t.Errorf("dropout did not open the zoom target: settled %.1f -> lost %.1f",
			settled.TargetH, lostTarget.TargetH)
	}
}

// Walk mode (landscape -> landscape): with slack the leash target equals the
// crop center, the pan PID sees zero error and the crop must not budge while
// the subject walks across the output.
func TestDeciderWalkLeashFreezesCrop(t *testing.T) {
	d := newFramingDecider(
		framingParams{Margin: 1.12, MaxZoom: 2.0, OutH: 200, SrcH: 720},
		648.0/200, 1.0/20, 1280, 720)

	var stream []bboxStep
	for i := 0; i < 80; i++ {
		x := 300 + 2*i // walking subject, narrow relative to the wide crop
		stream = append(stream, bboxStep{image.Rect(x, 140, x+120, 460), 1})
	}
	out := runDecider(d, stream)

	// The gate is derived from the first analysis frame, so it is known after
	// the first step.
	if !d.WalkGate() {
		t.Fatalf("walk gate must be active for 3.24 output from a 16:9 source")
	}

	frozen := 0
	for i := 40; i < 70; i++ {
		o := out[i]
		if o.WalkTgt == 0 {
			t.Fatalf("frame %d: leash inactive while the subject has slack", i+1)
		}
		if o.CropCx == out[i-1].CropCx {
			frozen++
		}
	}
	if frozen < 25 {
		t.Errorf("crop moved during leashed walk: only %d/30 frames frozen", frozen)
	}
}

// Structural invariants across every frame: the crop keeps its aspect lock,
// vertical stays inside the frame, and for portrait/square outputs the crop
// rect never spills horizontally.
func TestDeciderInvariants(t *testing.T) {
	bounds := image.Rect(0, 0, 854, 480)
	cases := []struct {
		name   string
		aspect float64
		params framingParams
	}{
		{"square 1:1", 1.0, framingParams{Margin: 1.12, MaxZoom: 2.0, OutH: 240, SrcH: 720}},
		{"portrait 200x648", 200.0 / 648, framingParams{Margin: 1.12, MaxZoom: 2.0, OutH: 648, SrcH: 720}},
		{"landscape 648x200", 648.0 / 200, framingParams{Margin: 1.12, MaxZoom: 2.0, OutH: 200, SrcH: 720}},
	}
	for _, c := range cases {
		d := newFramingDecider(c.params, c.aspect, 1.0/20, 1280, 720)
		var stream []bboxStep
		for i := 0; i < 200; i++ {
			x := 100 + 3*i%600
			stream = append(stream, bboxStep{image.Rect(x, 60, x+180, 470), 1})
		}
		out := runDecider(d, stream)
		for i, o := range out {
			if math.Abs(o.CropW-o.CropH*c.aspect) > 1e-6 {
				t.Fatalf("%s frame %d: aspect lock broken: w=%.4f h=%.4f ar=%.6f",
					c.name, i+1, o.CropW, o.CropH, c.aspect)
			}
			if o.CropRect.Min.Y < 0 || o.CropRect.Max.Y > bounds.Dy() {
				t.Fatalf("%s frame %d: crop spilled vertically: %v", c.name, i+1, o.CropRect)
			}
			if c.aspect < 2 && (o.CropRect.Min.X < 0 || o.CropRect.Max.X > bounds.Dx()) {
				t.Fatalf("%s frame %d: non-landscape crop spilled horizontally: %v",
					c.name, i+1, o.CropRect)
			}
			if o.CropRect.Empty() {
				t.Fatalf("%s frame %d: degenerate crop rect", c.name, i+1)
			}
		}
	}
}
