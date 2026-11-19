package holocropper

import (
	"image"
	"image/color"
	"math"
	"math/rand"
	"testing"
)

/* Framing controller tests (HIGH-ASPECT-RATIO-FIXES.md §12.7).

The shipped controller is PI (Kd deliberately 0: derivative kicks on moving
targets were a measured jitter source on the real corpus) with low-passed
actuator commands and crop-relative rate caps. Tests exercise the real plant
path (applyAxisPID / applyZoomPID), not the bare PIDController. */

func newFramingZoom() *PIDController {
	return NewPIDController(framingZoomKp, framingZoomKi, 0, framingZoomIntegral)
}

func TestPIDConvergence(t *testing.T) {
	hc := &HoloCropper{}
	p := newFramingZoom()
	pos := 0.0
	cmd := 0.0
	tol := framingDeadband + 1.0 // plant holds inside the deadband by design
	for i := 0; i < 600; i++ {
		pos = hc.applyZoomPID(p, &cmd, pos, 100, 1.0/24, 10000, 0)
		if i > 500 && math.Abs(pos-100) > tol {
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
	cmd := 0.0
	// large error for 1000 frames: the integrator must clamp, not wind up
	for i := 0; i < 1000; i++ {
		pos = hc.applyZoomPID(p, &cmd, pos, 50, 1.0/24, 10000, -1000)
	}
	if math.Abs(p.integral) > framingZoomIntegral+1 {
		t.Fatalf("integral %.2f escaped anti-windup clamp %.2f", p.integral, framingZoomIntegral)
	}
	// flip target: recovery is bounded by the rate limit, and quick because
	// the integrator did not wind up beyond its clamp
	recovered := -1
	for i := 0; i < 600; i++ {
		pos = hc.applyZoomPID(p, &cmd, pos, -50, 1.0/24, 10000, -1000)
		if math.Abs(pos+50) < framingDeadband+1.0 {
			recovered = i
			break
		}
	}
	if recovered < 0 {
		t.Fatalf("no recovery within 600 frames after target flip (pos=%.2f)", pos)
	}
}

func TestPIDNoiseAttenuation(t *testing.T) {
	hc := &HoloCropper{}
	p := newFramingZoom()
	pos := 400.0
	cmd := 0.0
	rng := rand.New(rand.NewSource(1))
	var deltas []float64
	prev := pos
	for i := 0; i < 500; i++ {
		target := 400 + rng.NormFloat64()*6 // noisy setpoint sigma=6
		pos = hc.applyZoomPID(p, &cmd, pos, target, 1.0/24, 100000, 0)
		deltas = append(deltas, pos-prev)
		prev = pos
	}
	// applied motion must stay inside the actuator rate limits (no retarget
	// gap here: frameH is huge relative to the noise)
	for _, d := range deltas {
		if d > zoomOutMax+1e-3 || d < -zoomInMax-1e-3 {
			t.Fatalf("per-frame motion %.2f exceeded rate limits [%v, %v]", d, -zoomInMax, zoomOutMax)
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

// TestZoomDriftStaysSmooth: ordinary slow target drift (bbox breathing) must
// never trigger the fast retarget allowance: every applied zoom step stays
// inside the gentle zoomInMax cap.
func TestZoomDriftStaysSmooth(t *testing.T) {
	hc := &HoloCropper{}
	p := newFramingZoom()
	cropH, cmd := 400.0, 0.0
	for i := 0; i < 200; i++ {
		target := 400 + 0.2*float64(i) // 0.2 px/frame drift, well under any retarget gap
		prev := cropH
		cropH = hc.applyZoomPID(p, &cmd, cropH, target, 1.0/24, 10000, 0)
		if d := math.Abs(cropH - prev); d > zoomInMax+1e-9 {
			t.Fatalf("frame %d: drift step %.2f exceeded zoomInMax %.2f (retarget allowance leaked)", i, d, zoomInMax)
		}
	}
	if cropH < 440-10 {
		t.Fatalf("crop %.1f failed to track the drifting target (%.1f)", cropH, 440.0)
	}
}

// TestZoomRetargetAllowanceGated: gaps larger than zoomRetargetJumpFrac of
// the frame (reacquire after subject loss) DO get the fast allowance - faster
// than the base cap, but still bounded by zoomRetOutMax/zoomRetInMax.
func TestZoomRetargetAllowanceGated(t *testing.T) {
	hc := &HoloCropper{}
	// grow: gap 3000 > 0.25*10000
	p := newFramingZoom()
	cropH, cmd := 400.0, 0.0
	start, target := 400.0, 3400.0
	for i := 0; i < 30; i++ {
		prev := cropH
		cropH = hc.applyZoomPID(p, &cmd, cropH, target, 1.0/24, 10000, 0)
		if d := cropH - prev; d > zoomRetOutMax+1e-9 {
			t.Fatalf("frame %d: zoom-out step %.2f exceeded zoomRetOutMax %.2f", i, d, zoomRetOutMax)
		}
	}
	if cropH-start < 150 {
		t.Fatalf("retarget covered %.1f px in 30 frames: fast allowance not applied", cropH-start)
	}
	// shrink: same gap, allowance capped at zoomRetInMax (zoom-in stays gentle)
	p = newFramingZoom()
	cropH, cmd = 400.0, 0.0
	maxD := 0.0
	for i := 0; i < 30; i++ {
		prev := cropH
		cropH = hc.applyZoomPID(p, &cmd, cropH, -2600, 1.0/24, 10000, -6000)
		d := math.Abs(cropH - prev)
		if d > zoomRetInMax+1e-9 {
			t.Fatalf("frame %d: zoom-in step %.2f exceeded zoomRetInMax %.2f", i, d, zoomRetInMax)
		}
		if d > maxD {
			maxD = d
		}
	}
	if maxD <= zoomInMax {
		t.Fatalf("retarget zoom-in never exceeded the base cap %.2f (max %.2f)", zoomInMax, maxD)
	}
}

/* aspect-locked target geometry tests (Phase 1 / Phase 4 / §12.7) */

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
	// 200:648 output, 1152x648 analysis frame, subject bbox 150x570.
	// Containment fit: 1.18 * 570 = 673 clamps to the frame height.
	bbox := image.Rect(500, 39, 650, 609)
	w, h, _, _ := hc.aspectLockedTarget(bbox, 1152, 648, 200.0/648)
	if h < 647 || h > 649 {
		t.Fatalf("portrait crop height %.1f not clamped to frame height 648", h)
	}
	if w < 199 || w > 201 {
		t.Fatalf("portrait crop width %.1f not ~200 (aspect locked)", w)
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

// TestAspectLockedTargetLandscapeContainment: the shipped landscape policy
// (§12.8/§12.10, §14 padding floor). A 350x300 subject standing 2 px off the
// frame floor is ground-snapped to 302, needing a 356-tall crop for full
// containment plus side padding; the crop may overflow the frame's left/right
// edges (bounded padding) and its bottom pins to the frame floor.
func TestAspectLockedTargetLandscapeContainment(t *testing.T) {
	hc := &HoloCropper{Height: 200, Width: 648, srcH: 1080}
	bbox := image.Rect(252, 178, 602, 478) // 350x300, feet on the frame floor
	w, h, cx, cy := hc.aspectLockedTarget(bbox, 854, 480, 648.0/200)
	if h < 355 || h > 357 {
		t.Fatalf("landscape crop height %.1f not ~356 (1.18x containment of 302)", h)
	}
	if h < float64(bbox.Dy())-0.5 {
		t.Fatalf("crop height %.1f shorter than subject %d: feet would clip", h, bbox.Dy())
	}
	if w < 1153 || w > 1156 {
		t.Fatalf("landscape crop width %.1f not ~1155 (aspect locked)", w)
	}
	pad := (w - 854) / 2
	if pad > landscapePadCapFrac*854+1 {
		t.Fatalf("side padding %.1f exceeds cap %.1f", pad, landscapePadCapFrac*854)
	}
	if math.Abs(cy+h/2-480) > 0.5 {
		t.Fatalf("crop bottom %.1f not pinned to the frame floor 480", cy+h/2)
	}
	if math.Abs(cx-float64(bbox.Dx())/2-float64(bbox.Min.X)) > 1 {
		t.Fatalf("landscape center x %.1f not bbox center", cx)
	}
}

// TestAspectLockedTargetLandscapeFeetAnchor: short subject (500x150) in the
// landscape containment regime. The containment fit (182.1) sits below the
// old maxZoom floor (240): for landscape the fit wins, and the feet stay
// feetPadFraction*h above the crop bottom instead of the 25/75 slack split.
func TestAspectLockedTargetLandscapeFeetAnchor(t *testing.T) {
	hc := &HoloCropper{Height: 200, Width: 648, srcH: 1080}
	bbox := image.Rect(177, 165, 677, 315) // 500x150
	w, h, _, cy := hc.aspectLockedTarget(bbox, 854, 480, 648.0/200)
	if h < 181 || h > 183 {
		t.Fatalf("landscape crop height %.1f not ~182 (containment fit 1.18*154.3)", h)
	}
	if h >= 480/2.0 {
		t.Fatalf("crop height %.1f not relaxed below the maxZoom floor 240", h)
	}
	if math.Abs(w-h*(648.0/200)) > 1e-9 {
		t.Fatalf("crop %.1fx%.1f not aspect locked", w, h)
	}
	gap := (cy + h/2) - float64(bbox.Max.Y) // empty strip below the feet
	if want := feetPadFraction * h; math.Abs(gap-want) > 0.5 {
		t.Fatalf("feet gap %.1f != feetPadFraction*h %.1f", gap, want)
	}
	if topGap := float64(bbox.Min.Y) - (cy - h/2); topGap < -1 {
		t.Fatalf("head clipped: top gap %.1f", topGap)
	}
	if cy+h/2 > 480 || cy-h/2 < 0 {
		t.Fatalf("crop leaves the frame vertically: [%.1f, %.1f]", cy-h/2, cy+h/2)
	}
}

// TestAspectLockedTargetLandscapeNoClipGuarantee: the §12.9 cap policy. A
// dissapearance-style tall subject (bh 429 in a 480 frame) may not be cut
// vertically: the crop grows to the full-frame-height fit (1555 px wide,
// 350 px black stage padding per side) under the padded-width cap (1571)
// and pins its bottom to the frame floor.
func TestAspectLockedTargetLandscapeNoClipGuarantee(t *testing.T) {
	hc := &HoloCropper{Height: 200, Width: 648, srcH: 1080}
	bbox := image.Rect(212, 51, 412, 480) // 200x429, feet on the frame floor
	w, h, _, cy := hc.aspectLockedTarget(bbox, 854, 480, 648.0/200)
	if h < float64(bbox.Dy()) {
		t.Fatalf("crop height %.1f shorter than subject %d: head/feet would clip", h, bbox.Dy())
	}
	if h < 479 || h > 481 {
		t.Fatalf("crop height %.1f not the full-frame fit 480", h)
	}
	if w < 1554 || w > 1556 {
		t.Fatalf("crop width %.1f not aspect-locked full-frame fit ~1555", w)
	}
	pad := (w - 854) / 2
	if pad > landscapePadCapFrac*854+1 {
		t.Fatalf("side padding %.1f exceeds cap %.1f", pad, landscapePadCapFrac*854)
	}
	if math.Abs(cy+h/2-480) > 0.5 {
		t.Fatalf("crop bottom %.1f not pinned to the frame floor 480", cy+h/2)
	}
	if top := cy - h/2; top > float64(bbox.Min.Y) {
		t.Fatalf("crop top %.1f below subject top %d: head would clip", top, bbox.Min.Y)
	}
}

// TestAspectLockedTargetLandscapeGroundSnap: dissapearance 02:04 geometry
// (§12.10). The mask misses her dark boots: the bbox (139x329) ends 53 px
// above the frame floor although she stands on it. The snap extends fit and
// anchor to the floor: full-frame-height fit, bottom pinned to 480, head
// clear of the crop top.
func TestAspectLockedTargetLandscapeGroundSnap(t *testing.T) {
	hc := &HoloCropper{Height: 200, Width: 648, srcH: 1080}
	bbox := image.Rect(341, 98, 480, 427) // 139x329, 53 px floor gap
	w, h, _, cy := hc.aspectLockedTarget(bbox, 854, 480, 648.0/200)
	if h < 449 || h > 452 {
		t.Fatalf("crop height %.1f not the snapped fit 1.18*(480-98)~450.8", h)
	}
	if math.Abs(w-h*(648.0/200)) > 0.5 {
		t.Fatalf("crop %.1fx%.1f not aspect locked", w, h)
	}
	if math.Abs(cy+h/2-480) > 0.5 {
		t.Fatalf("crop bottom %.1f not pinned to the frame floor 480", cy+h/2)
	}
	if top := cy - h/2; top > float64(bbox.Min.Y) {
		t.Fatalf("crop top %.1f below bbox top %d: head would clip", top, bbox.Min.Y)
	}
}

// TestAspectLockedTargetLandscapeAirborneNoSnap: a bbox bottom 100 px above
// the frame floor is outside the snap band (jump): no snap, the fit uses the
// actual bbox height and the feet anchor stays at bbox bottom + feet pad.
func TestAspectLockedTargetLandscapeAirborneNoSnap(t *testing.T) {
	hc := &HoloCropper{Height: 200, Width: 648, srcH: 1080}
	bbox := image.Rect(341, 100, 480, 380) // 139x280, 100 px floor gap
	_, h, _, cy := hc.aspectLockedTarget(bbox, 854, 480, 648.0/200)
	if h < 330 || h > 331 {
		t.Fatalf("crop height %.1f not 1.18*280~330.4: snap fired on an airborne subject", h)
	}
	bot := cy + h/2
	if want := float64(bbox.Max.Y) + feetPadFraction*h; math.Abs(bot-want) > 0.5 {
		t.Fatalf("crop bottom %.1f not bbox bottom + feet pad %.1f", bot, want)
	}
}

// TestAspectLockedTargetLandscapeSidePadding169: §14 review - landscape
// outputs must show empty stage padding on the left and right, "like the
// headroom on top". A wide tbaka-style dance group (575 px) at a 16:9 output
// from a 16:9 source used to fit the crop edge-to-edge: the moderate-landscape
// width term was dx/aspect with no margin at all, so the dancers grazed the
// screen edges. The landscape containment margin now sizes the crop
// 1.18*dx/aspect, guaranteeing 7.6% of the crop width empty per side - well
// over the 2% the owner asked for.
func TestAspectLockedTargetLandscapeSidePadding169(t *testing.T) {
	hc := &HoloCropper{Height: 288, Width: 512, srcH: 1080}
	bbox := image.Rect(139, 215, 714, 473) // 575x258 group, feet near the frame floor
	w, h, _, _ := hc.aspectLockedTarget(bbox, 854, 480, 512.0/288)
	if h < 381 || h > 383 {
		t.Fatalf("crop height %.1f not the padded width fit ~381.7 (1.18*575/1.778)", h)
	}
	if w < 677 || w > 680 {
		t.Fatalf("crop width %.1f not ~678 (aspect locked)", w)
	}
	if frac := (w - 575) / 2 / w; frac < 0.075 {
		t.Fatalf("empty side padding is %.1f%% of the crop width: below the 7.5%% floor", frac*100)
	}
}

// TestAspectLockedTargetLandscapeSidePadding324: the same group at the 3.24
// regime. The fit stays height-driven (ground-snapped to 480-215=265 ->
// 1.18*265=312.7) and the padded crop is then far wider than the group:
// every side keeps >= 2% of the crop width as empty stage, the crop bottom
// still pins to the frame floor.
func TestAspectLockedTargetLandscapeSidePadding324(t *testing.T) {
	hc := &HoloCropper{Height: 200, Width: 648, srcH: 1080}
	bbox := image.Rect(139, 215, 714, 473) // snapped: 265 tall, 575 wide
	w, h, _, cy := hc.aspectLockedTarget(bbox, 854, 480, 648.0/200)
	if h < 312 || h > 314 {
		t.Fatalf("crop height %.1f not the snapped fit 1.18*(480-215)~312.7", h)
	}
	if w <= float64(bbox.Dx()) {
		t.Fatalf("crop width %.1f not wider than subject %d: no side padding", w, bbox.Dx())
	}
	if frac := (w - 575) / 2 / w; frac < 0.02 {
		t.Fatalf("side padding is %.1f%% of the crop width: below the 2%% floor", frac*100)
	}
	if math.Abs(cy+h/2-480) > 0.5 {
		t.Fatalf("crop bottom %.1f not pinned to the frame floor 480", cy+h/2)
	}
}

func TestAspectLockedTargetPortraitContainsWideSubject(t *testing.T) {
	hc := &HoloCropper{Height: 648, Width: 200, srcH: 1080}
	// 200x400 subject (twin-tails spread wide): the containment fit must keep
	// the whole subject - feet included - in frame even though the crop then
	// runs to the full analysis-frame height. Feat clipping (the old
	// height-dominant fit) was rejected on vision review (§12.7).
	bbox := image.Rect(480, 124, 680, 524) // 200x400
	w, h, _, _ := hc.aspectLockedTarget(bbox, 1152, 648, 200.0/648)
	if h < 647 || h > 648 {
		t.Fatalf("portrait crop height %.1f not at frame height: subject not contained", h)
	}
	if w < 199 || w > 201 {
		t.Fatalf("portrait crop width %.1f not ~200 (aspect locked)", w)
	}
	if float64(bbox.Dx()) > w {
		t.Fatalf("subject width %d exceeds crop width %.1f: sides clipped", bbox.Dx(), w)
	}
}

func TestAspectLockedTargetPortraitFeetAnchor(t *testing.T) {
	hc := &HoloCropper{Height: 648, Width: 200, srcH: 1080}
	// feet-priority anchoring: subject 150x300 with feet at y=600; the crop
	// bottom must sit feetPadFraction*h below the feet, all slack above the
	// head. bbox width 150 -> width containment drives h: 486*1.18 = 573.5.
	bbox := image.Rect(500, 300, 650, 600)
	_, h, _, cy := hc.aspectLockedTarget(bbox, 1152, 648, 200.0/648)
	if h < 571 || h > 575 {
		t.Fatalf("portrait crop height %.1f not ~573 (containment fit)", h)
	}
	gap := (cy + h/2) - float64(bbox.Max.Y) // empty strip below the feet
	if want := feetPadFraction * h; math.Abs(gap-want) > 0.5 {
		t.Fatalf("feet gap %.1f != feetPadFraction*h %.1f", gap, want)
	}
	if cy+h/2 > 648 {
		t.Fatalf("crop bottom %.1f leaves the frame", cy+h/2)
	}
}

/* plant rate-limit tests: caps scale with the crop's own span (§12.7) */

func TestPanRateLimitsAreCropRelative(t *testing.T) {
	hc := &HoloCropper{}
	for _, c := range []struct {
		span, floor float64
	}{{160, panFloorX}, {473, panFloorX}, {264, panFloorY}, {30, panFloorX}} {
		p := NewPIDController(framingPanKp, framingPanKi, 0, framingPanIntegral)
		cmd, pos, prev := 0.0, 0.0, 0.0
		maxDelta := 0.0
		for i := 0; i < 40; i++ {
			pos = hc.applyAxisPID(p, &cmd, pos, 1000, 1.0/24, c.span, c.floor)
			d := math.Abs(pos - prev)
			prev = pos
			if d > maxDelta {
				maxDelta = d
			}
			if cap := math.Max(panCapFrac*c.span, c.floor); d > cap+1e-9 {
				t.Fatalf("span %.0f: pan step %.2f exceeded cap %.2f", c.span, d, cap)
			}
		}
		// the error allowance must raise the rate above the base crop fraction
		if maxDelta <= panCropFrac*c.span {
			t.Fatalf("span %.0f: allowance never raised the pan rate above base %.2f (max %.2f)",
				c.span, panCropFrac*c.span, maxDelta)
		}
	}
}

func TestPanTracksWalkingSubjectPortrait(t *testing.T) {
	hc := &HoloCropper{}
	p := NewPIDController(framingPanKp, framingPanKi, 0, framingPanIntegral)
	const cropW = 160.0 // 200:648 crop in a 1152-wide analysis frame
	const walk = 8.0    // px/frame steady subject x-speed
	cmd := 0.0
	pos, subject, vx, prevSubject := 0.0, 0.0, 0.0, 0.0
	maxLag := 0.0
	for i := 0; i < 300; i++ {
		if i > 30 {
			subject += walk
		}
		vx += velAlpha * ((subject - prevSubject) - vx)
		prevSubject = subject
		// the shipped loop applies the smoothed velocity lead to the target
		pos = hc.applyAxisPID(p, &cmd, pos, subject+velLeadFrames*vx, 1.0/24, cropW, panFloorX)
		if i > 120 {
			if lag := math.Abs(pos - subject); lag > maxLag {
				maxLag = lag
			}
		}
	}
	if maxLag >= cropW/2 {
		t.Fatalf("steady tracking lag %.0f px >= half crop %.0f px: subject leaves frame", maxLag, cropW/2)
	}
	// the velocity lead keeps the proportional lag well inside the crop
	if maxLag > 0.3*cropW {
		t.Fatalf("tracking lag %.0f px exceeds 30%% of crop width: velocity lead not effective", maxLag)
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
		// portrait/square pass padAllow = 0: fully in-frame clamp (F3/F4)
		r := hc.cropRectFromFraming(c.cx, c.cy, c.w, c.h, 0, bounds)
		if !r.In(bounds) {
			t.Fatalf("crop %v escapes frame %v", r, bounds)
		}
		if r.Empty() {
			t.Fatalf("crop %v degenerated to empty", r)
		}
	}
}

// TestCropRectLandscapePadding: with a landscape padAllow the rect may leave
// the frame's left/right edges, but never past the allowance, never
// vertically, and never at the cost of the aspect lock.
func TestCropRectLandscapePadding(t *testing.T) {
	hc := &HoloCropper{Height: 200, Width: 648}
	bounds := image.Rect(0, 0, 854, 480)
	padAllow := landscapePadCapFrac * 854
	// subject pinned hard against the left edge: cx would sit far out of the
	// in-frame clamp window
	r := hc.cropRectFromFraming(100, 312, 1090, 336, padAllow, bounds)
	if r.Dx() != 1090 {
		t.Fatalf("crop width %d not 1090: aspect lock broken by edge clamp", r.Dx())
	}
	// each side's overflow is bounded by the allowance; a side may have none
	// (the clamp window is wider than the frame when padAllow > 0)
	if left := max(0, -r.Min.X); float64(left) > padAllow+1 {
		t.Fatalf("left overflow %d exceeds allowance %.1f", left, padAllow)
	}
	if left := max(0, -r.Min.X); float64(left) < 0.9*padAllow {
		t.Fatalf("left overflow %d: edge-pinned subject did not get the allowance", left)
	}
	if right := max(0, r.Max.X-854); float64(right) > padAllow+1 {
		t.Fatalf("right overflow %d exceeds allowance %.1f", right, padAllow)
	}
	if r.Min.X >= 0 && r.Max.X <= 854 {
		t.Fatalf("crop %v never used the padding allowance it was given", r)
	}
	if r.Min.Y < 0 || r.Max.Y > 480 {
		t.Fatalf("crop %v escaped the frame vertically", r)
	}
}

// TestCropToBBoxBlackPadding: crop regions outside the frame compose as
// black stage padding; in-frame regions copy the source pixels.
func TestCropToBBoxBlackPadding(t *testing.T) {
	hc := &HoloCropper{Height: 200, Width: 648}
	src := image.NewRGBA(image.Rect(0, 0, 854, 480))
	filled := color.RGBA{R: 20, G: 200, B: 60, A: 255}
	for y := 0; y < 480; y++ {
		for x := 0; x < 854; x++ {
			src.Set(x, y, filled)
		}
	}
	dst := hc.cropToBBox(src, image.Rect(-120, 0, 240, 480))
	if dst.Bounds() != image.Rect(0, 0, 360, 480) {
		t.Fatalf("padded crop bounds %v not the full rect extent", dst.Bounds())
	}
	if got := dst.At(0, 240); got != (color.RGBA{0, 0, 0, 0}) {
		t.Fatalf("out-of-frame padding pixel %v not transparent black", got)
	}
	if got := dst.At(119, 240); got != (color.RGBA{0, 0, 0, 0}) {
		t.Fatalf("out-of-frame padding pixel %v not transparent black", got)
	}
	if got := dst.At(120, 240); got != filled {
		t.Fatalf("in-frame pixel %v not copied from source", got)
	}
	if got := dst.At(359, 479); got != filled {
		t.Fatalf("in-frame pixel %v not copied from source", got)
	}
}
