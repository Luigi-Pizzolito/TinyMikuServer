package holocropper

import (
	"math"
	"testing"
)

// Walk-mode gate (HIGH-ASPECT-RATIO-FIXES.md §14): landscape -> landscape
// only, and only when the output aspect covers the source aspect (the output
// X axis can show the source's full walk range), with leniency just below the
// source ratio.
func TestWalkModeActiveGate(t *testing.T) {
	cases := []struct {
		name               string
		aspect, srcW, srcH float64
		want               bool
	}{
		{"16:9 out from 16:9 source", 16.0 / 9.0, 854, 480, true},
		{"1152x648 out from 16:9 source", 1152.0 / 648.0, 1280, 720, true},
		{"3.24 out from 16:9 source", 648.0 / 200.0, 1280, 720, true},
		{"16:10 out from 16:9 source (leniency)", 1.6, 1280, 720, true},
		{"3:2 out from 16:9 source (below leniency)", 1.5, 1280, 720, false},
		{"4:3 out from 16:9 source: no room for the full walk range", 4.0 / 3.0, 1280, 720, false},
		{"4:3 out from 4:3 source", 4.0 / 3.0, 640, 480, true},
		{"slightly below 4:3 out from 4:3 source (leniency)", 1.22, 640, 480, true},
		{"square out from 16:9 source", 1.0, 1280, 720, false},
		{"portrait out from 16:9 source", 200.0 / 648.0, 1280, 720, false},
		{"3.24 out from portrait source (not landscape->landscape)", 648.0 / 200.0, 480, 854, false},
		{"4:3 out from square source", 4.0 / 3.0, 480, 480, false},
	}
	for _, c := range cases {
		if got := walkModeActive(c.aspect, c.srcW, c.srcH); got != c.want {
			t.Errorf("%s: walkModeActive(%0.4f, %0.0fx%0.0f) = %v, want %v",
				c.name, c.aspect, c.srcW, c.srcH, got, c.want)
		}
	}
}

func TestWalkLeashFreezesWhileSubjectHasSlack(t *testing.T) {
	// 800-wide crop, 80 px leash margin (walkLeashMarginFrac 0.10):
	// window [x1+80-400, x0-80+400] = [40, 620]; crop at 400 sits well inside.
	ema := [4]float64{300, 100, 360, 470}
	target, leashed := walkLeashTarget(400, ema, 800)
	if !leashed {
		t.Fatal("subject narrower than the crop must leash")
	}
	if target != 400 {
		t.Fatalf("slack means freeze at the current crop x: target %.2f, want 400", target)
	}
}

func TestWalkLeashFollowsTheWalkAtWindowEdge(t *testing.T) {
	// Subject walked to the crop's right side: window lo binds -> target pans
	// right, keeping the leash margin between subject and crop edge.
	ema := [4]float64{700, 100, 760, 470}
	target, leashed := walkLeashTarget(400, ema, 800)
	if !leashed {
		t.Fatal("subject narrower than the crop must leash")
	}
	if want := 760.0 + 80 - 400; target != want {
		t.Fatalf("walk-right target %.2f, want lo edge %.2f", target, want)
	}
	// Subject walked to the left edge: hi binds -> pan left.
	ema = [4]float64{60, 100, 120, 470}
	target, leashed = walkLeashTarget(400, ema, 800)
	if !leashed {
		t.Fatal("subject narrower than the crop must leash")
	}
	if want := 60.0 - 80 + 400; target != want {
		t.Fatalf("walk-left target %.2f, want hi edge %.2f", target, want)
	}
}

func TestWalkLeashTargetIsContinuous(t *testing.T) {
	// A subject walking left->right across the full frame must never produce a
	// target jump: the frozen phase changes by 0, the edge-chase phase moves
	// exactly with the subject.
	const step = 4.0
	x0 := 60.0
	cropCx := 427.0
	prev := 0.0
	for i := 0; i < 200; i++ {
		ema := [4]float64{x0, 100, x0 + 60, 470}
		target, leashed := walkLeashTarget(cropCx, ema, 800)
		if !leashed {
			t.Fatalf("walk at x0=%.0f lost the leash (window invalid)", x0)
		}
		if i > 0 {
			jump := math.Abs(target - prev)
			if jump > step+1e-9 {
				t.Fatalf("target jump %.2f px at x0=%.0f (subject moves %.1f px/frame)", jump, x0, step)
			}
		}
		prev = target
		x0 += step
	}
}

func TestWalkLeashInvalidWindowFallsBackToCentering(t *testing.T) {
	// Subject 750 px wide + 2x80 margins exceeds the 800 px crop: no legal
	// window, so the caller keeps the centered target + velocity lead.
	ema := [4]float64{50, 100, 800, 470}
	target, leashed := walkLeashTarget(427, ema, 800)
	if leashed {
		t.Fatal("subject wider than crop-2m must not leash")
	}
	if target != 427 {
		t.Fatalf("fallback must return the caller position untouched, got %.2f", target)
	}
}

func TestWalkLeashNeverLetsSubjectCrossCropEdge(t *testing.T) {
	// Whatever the subject and crop x, a leashed target always keeps the
	// subject bbox (plus margin) inside the crop it targets.
	ema := [4]float64{300, 100, 380, 470}
	cropW := 700.0
	m := walkLeashMarginFrac * cropW
	for _, cropCx := range []float64{-50, 100, 400, 600, 1200} {
		target, leashed := walkLeashTarget(cropCx, ema, cropW)
		if !leashed {
			continue
		}
		if ema[2]+m > target+cropW/2 || ema[0]-m < target-cropW/2 {
			t.Fatalf("target %.2f does not hold subject [%0.0f,%0.0f]+margin inside crop [%0.0f,%0.0f]",
				target, ema[0], ema[2], target-cropW/2, target+cropW/2)
		}
	}
}
