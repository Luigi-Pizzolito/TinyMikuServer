package holocropper

import (
	"image"
	"math"
	"sort"
)

// Auto-orientation (HIGH-ASPECT-RATIO-FIXES.md §13): estimate the aspect
// ratio of the master subject bounding box (the box covering ALL detected
// subjects, per frame) across frames, then pick the output orientation - the
// requested WxH or its transpose HxW - that keeps the subjects best framed at
// that resolution. The video is never rotated: the crop just becomes tall or
// wide, the source stays upright. 1:1 outputs bypass the decision entirely.
//
// The evidence is the DetectionStream: Plan feeds orientationStats the
// master-bbox ARs of every frame it already has. Story C3 removed the old
// separate probe decode (a sampled ffmpeg pass plus its own copy of the
// detection stages) - the stream holds the same measurements for the whole
// video, so the decision machinery is shared by every path.

// degenerateBBoxAreaFrac is the share of the analysis frame a master bbox has
// to cover before it counts as standing in for the whole frame. Frames whose
// mask degenerates (a fully dark frame, where Otsu collapses to threshold 0
// and everything thresholds as "subject") report the FRAME's aspect ratio, not
// a subject's, so they must not enter the AR averages. The CLI probe this
// package used to run guarded against exactly that; the stream path filters
// the same frames out of the aggregates while leaving framing untouched.
const degenerateBBoxAreaFrac = 0.92

// degenerateMasterBBox reports whether bbox covers enough of the frameW x
// frameH analysis frame to be a degenerate full-frame detection.
func degenerateMasterBBox(bbox image.Rectangle, frameW, frameH int) bool {
	area := float64(frameW) * float64(frameH)
	if area <= 0 {
		return true
	}
	return float64(bbox.Dx()*bbox.Dy()) >= degenerateBBoxAreaFrac*area
}

// OrientationStat describes one auto-orient decision for reporting.
type OrientationStat struct {
	FramesExamined     int     // frames the stream covers
	FramesWithSubjects int     // frames of those with at least one subject
	MeanAR             float64 // mean master-bbox aspect ratio (w/h)
	MedianAR           float64
	GeoMeanAR          float64 // geometric mean: ARs are ratios, so logs are the natural average
	KeepScore          float64 // mean containment score at the requested WxH
	FlipScore          float64 // mean containment score at the transposed HxW
	Flip               bool    // true when HxW scores strictly better
}

// containmentScore is the mean fraction of the output a subject of aspect
// ratio s occupies inside an aspect-locked containment crop of aspect a,
// up to the constant zoom margin: min(s/a, a/s). 1.0 means the subject's
// aspect matches the crop exactly (optimal scale); smaller means the
// containment fit wastes output area on padding along one axis. Averaged
// over frames it scores "keeps the subjects mostly within the frame" for
// an orientation.
func containmentScore(a float64, subjectARs []float64) float64 {
	if a <= 0 || len(subjectARs) == 0 {
		return 0
	}
	sum := 0.0
	for _, s := range subjectARs {
		sum += math.Min(s/a, a/s)
	}
	return sum / float64(len(subjectARs))
}

// pickOrientation compares the containment scores of the requested aspect
// and its transpose. Ties (and empty probes) keep the requested
// orientation; square outputs never flip.
func pickOrientation(w, h int, subjectARs []float64) (flip bool, keepScore, flipScore float64) {
	if w == h || w <= 0 || h <= 0 || len(subjectARs) == 0 {
		return false, 0, 0
	}
	a := float64(w) / float64(h)
	keepScore = containmentScore(a, subjectARs)
	flipScore = containmentScore(1/a, subjectARs)
	return flipScore > keepScore, keepScore, flipScore
}

// orientationStats computes the full OrientationStat (AR aggregates + the
// keep/flip containment-score decision) from a set of master-bbox aspect
// ratios: the decision and its reporting are one function, whatever path
// measured the ratios.
func orientationStats(subjectARs []float64, w, h int) (outW, outH int, stat OrientationStat) {
	stat.FramesWithSubjects = len(subjectARs)
	if len(subjectARs) == 0 {
		return w, h, stat
	}

	sum, logSum := 0.0, 0.0
	for _, s := range subjectARs {
		sum += s
		logSum += math.Log(s)
	}
	sorted := make([]float64, len(subjectARs))
	copy(sorted, subjectARs)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	median := sorted[mid]
	if len(sorted)%2 == 0 {
		median = (sorted[mid-1] + sorted[mid]) / 2
	}
	stat.MeanAR = sum / float64(len(subjectARs))
	stat.GeoMeanAR = math.Exp(logSum / float64(len(subjectARs)))
	stat.MedianAR = median

	flip, keep, flipped := pickOrientation(w, h, subjectARs)
	stat.KeepScore = keep
	stat.FlipScore = flipped
	stat.Flip = flip
	if flip {
		return h, w, stat
	}
	return w, h, stat
}

// fitOrientationStats is the auto-orient decision of fit mode (§6.10).
// There is no subject containment to optimize - the quality measure is the
// black-bar area: a containment score evaluated on the SOURCE frame's own
// aspect ratio equals the fraction of the output the fitted image fills
// (1.0 = no bars). So keep/flip compare WxH against HxW purely on output-
// vs-source geometry, and the decision is identical whether or not the
// clip has subjects. The AR aggregates report the source aspect; the frame
// counts are carried for reporting only.
func fitOrientationStats(srcAR float64, w, h, framesExamined, framesWithSubjects int) (outW, outH int, stat OrientationStat) {
	stat.FramesExamined = framesExamined
	stat.FramesWithSubjects = framesWithSubjects
	stat.MeanAR, stat.MedianAR, stat.GeoMeanAR = srcAR, srcAR, srcAR

	flip, keep, flipped := pickOrientation(w, h, []float64{srcAR})
	stat.KeepScore, stat.FlipScore, stat.Flip = keep, flipped, flip
	if flip {
		return h, w, stat
	}
	return w, h, stat
}
