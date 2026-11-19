package holocropper

import (
	"image"
	"image/color"
	"math"
	"testing"
)

// Corpus-calibrated orientation decisions (sample ARs mirror the measured
// master-bbox ARs of the four test videos, HIGH-ASPECT-RATIO-FIXES.md §13):
// three single-subject dance videos are portrait-optimal, tbaka's
// side-by-side group is landscape-optimal.
var tallSubjectARs = []float64{0.42, 0.56, 0.61, 0.48, 0.72, 0.55, 0.44, 0.84, 0.50, 0.65}
var groupSubjectARs = []float64{1.50, 2.10, 1.90, 2.60, 1.10, 1.70, 1.95, 2.40, 1.80, 2.00}

func TestPickOrientationTallSubjectsFlipToPortrait(t *testing.T) {
	flip, keep, flipped := pickOrientation(648, 200, tallSubjectARs)
	if !flip {
		t.Fatalf("tall subjects requested 648x200 must flip to 200x648 (keep %.3f vs flip %.3f)", keep, flipped)
	}
	if flipped <= keep {
		t.Fatalf("portrait score %.3f must exceed landscape %.3f", flipped, keep)
	}
	// Requesting portrait directly must keep it.
	if flip2, _, _ := pickOrientation(200, 648, tallSubjectARs); flip2 {
		t.Fatal("portrait request with tall subjects must not flip")
	}
}

func TestPickOrientationGroupSubjectsFlipToLandscape(t *testing.T) {
	flip, keep, flipped := pickOrientation(200, 648, groupSubjectARs)
	if !flip {
		t.Fatalf("side-by-side group requested 200x648 must flip to 648x200 (keep %.3f vs flip %.3f)", keep, flipped)
	}
	if flipped <= keep {
		t.Fatalf("landscape score %.3f must exceed portrait %.3f", flipped, keep)
	}
	if flip2, _, _ := pickOrientation(648, 200, groupSubjectARs); flip2 {
		t.Fatal("landscape request with a wide group must not flip")
	}
}

func TestPickOrientationSquareNeverFlips(t *testing.T) {
	// 1:1 mode bypasses orientation choice entirely.
	for _, ars := range [][]float64{tallSubjectARs, groupSubjectARs} {
		if flip, _, _ := pickOrientation(320, 320, ars); flip {
			t.Fatal("1:1 output must never flip")
		}
	}
}

func TestPickOrientationTiesAndNoSubjectsKeepRequested(t *testing.T) {
	if flip, keep, flipped := pickOrientation(648, 200, []float64{1, 1, 1}); flip {
		t.Fatalf("symmetric tie (keep %.6f flip %.6f) must keep the requested orientation", keep, flipped)
	}
	if flip, _, _ := pickOrientation(648, 200, nil); flip {
		t.Fatal("probe without subjects must keep the requested orientation")
	}
}

func TestContainmentScore(t *testing.T) {
	if got := containmentScore(2, []float64{2}); got != 1 {
		t.Fatalf("subject aspect matching the crop must score 1, got %.3f", got)
	}
	if got := containmentScore(3.24, []float64{1}); math.Abs(got-1/3.24) > 1e-9 {
		t.Fatalf("mismatched aspect score %.3f not 1/3.24", got)
	}
	// min(s/a, a/s) is symmetric under inverting both aspects.
	a, s := 3.24, 0.4
	if x, y := containmentScore(a, []float64{s}), containmentScore(1/a, []float64{1 / s}); math.Abs(x-y) > 1e-9 {
		t.Fatalf("score(a,s)=%.4f != score(1/a,1/s)=%.4f", x, y)
	}
}

// syntheticFrame paints subject rectangles on a dark stage. Subjects carry
// a continuous tone band (like real frames); the stage is near-black with
// sparse isolated noise pixels, modelling the dark-stage speckle the real
// pipeline filters by component area.
func syntheticFrame(w, h int, rects []image.Rectangle) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := uint8(0)
			if x%97 == 7 && y%89 == 11 {
				v = 5
			}
			img.Set(x, y, color.RGBA{v, v, v, 255})
		}
	}
	for _, r := range rects {
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for x := r.Min.X; x < r.Max.X; x++ {
				c := uint8(180 + (x+y)%76)
				img.Set(x, y, color.RGBA{c, c, c, 255})
			}
		}
	}
	return img
}

// TestDetectFrameSyntheticSubjects pins the shared single-frame detector the
// detect pass streams frames through (video_dsp.go detectFrame): grayscale ->
// Otsu hysteresis -> binary -> subject filter -> union master bbox. These are
// the AR measurements auto-orientation decides on, so the master bbox must
// match the drawn subject layout.
func TestDetectFrameSyntheticSubjects(t *testing.T) {
	// One tall solo dancer: master bbox == its own box, AR ~ 0.375.
	lastThr := -1.0
	bbox, subj := detectFrame(syntheticFrame(854, 480,
		[]image.Rectangle{image.Rect(300, 90, 420, 410)}), &lastThr)
	if subj != 1 {
		t.Fatalf("solo subject count %d, want 1", subj)
	}
	if ar := float64(bbox.Dx()) / float64(bbox.Dy()); math.Abs(ar-0.375) > 0.1 {
		t.Fatalf("solo master AR %.3f not ~0.375 (bbox %v)", ar, bbox)
	}

	// Three side-by-side dancers: union master bbox is wide, AR ~ 2.29.
	lastThr = -1.0
	bbox, subj = detectFrame(syntheticFrame(854, 480, []image.Rectangle{
		image.Rect(100, 100, 180, 380), image.Rect(380, 100, 460, 380), image.Rect(660, 100, 740, 380),
	}), &lastThr)
	if subj != 3 {
		t.Fatalf("group subject count %d, want 3", subj)
	}
	if ar := float64(bbox.Dx()) / float64(bbox.Dy()); ar < 2.0 || ar > 2.5 {
		t.Fatalf("group master AR %.3f not ~2.29 (bbox %v)", ar, bbox)
	}
}

// degenerateMasterBBox is the AR-level guard the deleted CLI probe carried:
// a detection that stands for the whole frame carries the frame's own aspect
// ratio, not a subject's, and must never enter the AR averages.
func TestDegenerateMasterBBoxGuard(t *testing.T) {
	if !degenerateMasterBBox(image.Rect(0, 0, 854, 480), 854, 480) {
		t.Error("a full-frame detection must be flagged degenerate")
	}
	if !degenerateMasterBBox(image.Rect(4, 4, 851, 477), 854, 480) {
		t.Error("a near-full-frame detection must be flagged degenerate")
	}
	if degenerateMasterBBox(image.Rect(300, 90, 420, 410), 854, 480) {
		t.Error("a real subject bbox flagged degenerate")
	}
	if degenerateMasterBBox(image.ZR, 854, 480) {
		t.Error("an empty bbox is not a degenerate detection, it is no detection")
	}
	if !degenerateMasterBBox(image.Rect(0, 0, 10, 10), 0, 480) {
		t.Error("a zero-area frame has no subject evidence either way")
	}
}

// A square output has no portrait/landscape choice: orientationStats passes
// the requested dims through and never flips, whatever the subjects look like.
func TestOrientationStatsSquareOutputNeverFlips(t *testing.T) {
	for _, ar := range [][]float64{tallSubjectARs, groupSubjectARs, nil} {
		outW, outH, stat := orientationStats(ar, 320, 320)
		if outW != 320 || outH != 320 || stat.Flip {
			t.Fatalf("320x320 with ARs %v must pass through, got %dx%d flip=%v", ar, outW, outH, stat.Flip)
		}
	}
}
