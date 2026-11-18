package holocropper

import (
	"image"
	"math"
	"math/rand"
	"testing"
)

/* PID controller tests (HIGH-ASPECT-RATIO-FIXES.md Phase 2 / §8) */

func newFramingZoom() *PIDController {
	return NewPIDController(framingZoomKp, framingZoomKi, framingZoomKd, framingZoomIntegral)
}

// Tests exercise the real plant path (applyZoomPID: asymmetric rate limits +
// deadband), not the bare controller, so limit cycles of the raw controller
// cannot hide behind a test-only symmetric clamp.

func TestPIDConvergence(t *testing.T) {
	hc := &HoloCropper{}
	p := newFramingZoom()
	pos := 0.0
	tol := framingDeadband + 0.5 // plant holds inside the deadband by design
	for i := 0; i < 600; i++ {
		pos = hc.applyZoomPID(p, pos, 100, 1.0/24, 10000, 0)
		if i > 300 && math.Abs(pos-100) > tol {
			t.Fatalf("not converged by frame %d: pos=%.2f", i, pos)
		}
	}
	if math.Abs(pos-100) > tol {
		t.Fatalf("final pos %.2f not within %.2f of setpoint", pos, tol)
	}
}

func TestPIDAntiWindup(t *testing.T) {
	hc := &HoloCropper{}
	p := newFramingZoom()
	pos := 0.0
	// sustained large error for 1000 frames
	for i := 0; i < 1000; i++ {
		pos = hc.applyZoomPID(p, pos, 50, 1.0/24, 10000, -1000)
	}
	if math.Abs(p.integral) > framingZoomIntegral+1 {
		t.Fatalf("integral %.2f escaped anti-windup clamp %.2f", p.integral, framingZoomIntegral)
	}
	// flip target: recovery must be quick because the integrator did not wind up
	recovered := -1
	for i := 0; i < 400; i++ {
		pos = hc.applyZoomPID(p, pos, -50, 1.0/24, 10000, -1000)
		if math.Abs(pos+50) < framingDeadband+0.5 {
			recovered = i
			break
		}
	}
	if recovered < 0 {
		t.Fatalf("no recovery within 400 frames after target flip (pos=%.2f)", pos)
	}
}

func TestPIDNoiseAttenuation(t *testing.T) {
	hc := &HoloCropper{}
	p := newFramingZoom()
	pos := 400.0
	rng := rand.New(rand.NewSource(1))
	var deltas []float64
	prev := pos
	for i := 0; i < 500; i++ {
		target := 400 + rng.NormFloat64()*6 // noisy setpoint sigma=6
		pos = hc.applyZoomPID(p, pos, target, 1.0/24, 100000, 0)
		deltas = append(deltas, pos-prev)
		prev = pos
	}
	// applied motion must stay inside the actuator rate limits
	for _, d := range deltas {
		if d > zoomRateOutMax+1e-3 || d < -zoomRateInMax-1e-3 {
			t.Fatalf("per-frame motion %.2f exceeded rate limits [%v, %v]", d, -zoomRateInMax, zoomRateOutMax)
		}
	}
	// controller output must not amplify the input noise
	var variance float64
	for _, d := range deltas {
		variance += d * d
	}
	variance /= float64(len(deltas))
	if math.Sqrt(variance) > 6.0 {
		t.Fatalf("output jitter std %.2f exceeded input noise std 6", math.Sqrt(variance))
	}
}

func TestPIDDeterminism(t *testing.T) {
	run := func() []float64 {
		p := newFramingZoom()
		pos := 400.0
		out := make([]float64, 0, 300)
		for i := 0; i < 300; i++ {
			target := 300 + 80*math.Sin(float64(i)/17)
			pos += clampF(p.Step(pos, target, 1.0/24), -4, 4)
			out = append(out, pos)
		}
		return out
	}
	a, b := run(), run()
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("non-deterministic output at frame %d: %v vs %v", i, a[i], b[i])
		}
	}
}

/* aspect-locked target geometry tests (Phase 1 / Phase 4) */

func TestAspectLockedTargetSquare1to1(t *testing.T) {
	hc := &HoloCropper{Height: 240, Width: 240, srcH: 1080}
	// subject bbox 111x422 in an 854x480 analysis frame from a 1080-tall source
	bbox := image.Rect(300, 30, 411, 452)
	w, h, _, cy := hc.aspectLockedTarget(bbox, 854, 480, 1)
	if h < 470 || h > 476 {
		t.Fatalf("square crop height %.1f not ~473 (1.12x of 422)", h)
	}
	if w != h {
		t.Fatalf("square output must produce square crop, got %.1fx%.1f", w, h)
	}
	if math.Abs(cy-float64(bbox.Dy())/2-float64(bbox.Min.Y)) > 1 {
		t.Fatalf("square mode should center on bbox center, cy=%.1f", cy)
	}
}

func TestAspectLockedTargetPortrait(t *testing.T) {
	hc := &HoloCropper{Height: 648, Width: 200, srcH: 1080}
	// 200:648 output, 1152x648 analysis frame, subject bbox 150x570
	bbox := image.Rect(500, 39, 650, 609)
	w, h, _, _ := hc.aspectLockedTarget(bbox, 1152, 648, 200.0/648)
	if h < 636 || h > 641 {
		t.Fatalf("portrait crop height %.1f not ~638 (1.12x of 570)", h)
	}
	if w < 195 || w > 199 {
		t.Fatalf("portrait crop width %.1f not ~197 (aspect locked)", w)
	}
}

func TestAspectLockedTargetPortraitSmallSubjectZoomsIn(t *testing.T) {
	hc := &HoloCropper{Height: 648, Width: 200, srcH: 1080}
	// small subject 40x120: must zoom in to the floors, not stay full-frame
	bbox := image.Rect(556, 264, 596, 384)
	w, h, _, _ := hc.aspectLockedTarget(bbox, 1152, 648, 200.0/648)
	// floor = max(outH*frameH/srcH, frameH/maxZoom) = max(388.8, 324) = 388.8
	if h < 388 || h > 390 {
		t.Fatalf("small-subject crop height %.1f not at floor ~389", h)
	}
	if w < 119 || w > 121 {
		t.Fatalf("small-subject crop width %.1f not ~120 (aspect locked at floor)", w)
	}
}

func TestAspectLockedTargetExtremeLandscapePriorityFit(t *testing.T) {
	hc := &HoloCropper{Height: 200, Width: 648, srcH: 1080}
	// 648x200 output: a tall narrow subject (111x422) cannot fit any legal
	// crop; expect frame-width clamp, height >= 0.6*bbox, head-biased center
	bbox := image.Rect(300, 30, 411, 452)
	w, h, cx, cy := hc.aspectLockedTarget(bbox, 854, 480, 648.0/200)
	if w > 854.001 {
		t.Fatalf("landscape crop width %.1f exceeds frame", w)
	}
	if h < 260 || h > 268 {
		t.Fatalf("landscape crop height %.1f not ~264 (frame-width clamp)", h)
	}
	if h < 0.6*float64(bbox.Dy())-1 {
		t.Fatalf("landscape crop height %.1f below 0.6*bbox subject-fit floor", h)
	}
	wantCy := float64(bbox.Min.Y) + 0.5*h // subject taller than crop => exactly top-aligned
	if math.Abs(cy-wantCy) > 1 {
		t.Fatalf("landscape centroid %.1f != top-aligned %.1f", cy, wantCy)
	}
	if math.Abs(cx-float64(bbox.Dx())/2-float64(bbox.Min.X)) > 1 {
		t.Fatalf("landscape center x %.1f not bbox center", cx)
	}
}

func TestAspectLockedTargetLandscapeShortSubjectSlackSplit(t *testing.T) {
	hc := &HoloCropper{Height: 200, Width: 648, srcH: 1080}
	// short wide subject 500x150 in 854x480 frame: crop must cover full width
	// (560*aspect>frame) - slack above subject height splits 25/75 so the top
	// strip stays small.
	bbox := image.Rect(177, 165, 677, 315) // 500x150
	w, h, _, cy := hc.aspectLockedTarget(bbox, 854, 480, 648.0/200)
	if h < 154 {
		t.Fatalf("landscape crop height %.1f below subject+margin", h)
	}
	topGap := float64(bbox.Min.Y) - (cy - h/2) // empty strip above subject top
	botGap := (cy + h/2) - (float64(bbox.Min.Y) + float64(bbox.Dy())) // empty strip below subject bottom
	if topGap > 0.4*(h-float64(bbox.Dy()))+1 || topGap < -1 {
		t.Fatalf("top gap %.1f not ~25%% of slack (bottom gap %.1f, crop %.1fx%.1f)", topGap, botGap, w, h)
	}
	if botGap < topGap-1 {
		t.Fatalf("slack not bottom-biased: top %.1f bottom %.1f", topGap, botGap)
	}
}

func TestAspectLockedTargetPortraitClipsWideSubjectInsteadOfBloating(t *testing.T) {
	hc := &HoloCropper{Height: 648, Width: 200, srcH: 1080}
	// 200x400 subject: full-width containment would balloon the crop to the
	// 648 frame height (32% dead vertical padding). The height-dominant fit
	// must keep crop height at ~1.12x subject height and clip the sides.
	bbox := image.Rect(480, 124, 680, 524) // 200x400
	w, h, _, _ := hc.aspectLockedTarget(bbox, 1152, 648, 200.0/648)
	if h < 445 || h > 452 {
		t.Fatalf("portrait crop height %.1f not ~448 (height-dominant fit)", h)
	}
	if w < 130 || w > 150 {
		t.Fatalf("portrait crop width %.1f not within [0.65, 0.75]*bbox width", w)
	}
}

/* framing state -> crop rect tests */

func TestCropRectAlwaysInsideFrame(t *testing.T) {
	hc := &HoloCropper{Height: 648, Width: 200}
	bounds := image.Rect(0, 0, 1152, 648)
	cases := []struct{ cx, cy, w, h float64 }{
		{10, 10, 200, 648},     // pushed out top-left
		{1140, 640, 200, 648},  // pushed out bottom-right
		{576, 324, 2000, 6480}, // absurd oversize
	}
	for _, c := range cases {
		r := hc.cropRectFromFraming(c.cx, c.cy, c.w, c.h, bounds)
		if !r.In(bounds) {
			t.Fatalf("crop %v escapes frame %v", r, bounds)
		}
		if r.Empty() {
			t.Fatalf("crop %v degenerated to empty", r)
		}
	}
}
