package holocropper

import (
	"fmt"
	"image"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	vidio "github.com/AlexEidt/Vidio"
)

// Auto-orientation (HIGH-ASPECT-RATIO-FIXES.md §13): a preprocessing probe
// that estimates the average aspect ratio of the master subject bounding
// box (the box covering ALL detected subjects, per frame) across sampled
// frames, then picks the output orientation - the requested WxH or its
// transpose HxW - that keeps the subjects best framed at that resolution.
// The video is never rotated: the crop just becomes tall or wide, the
// source stays upright. 1:1 outputs bypass the probe entirely.

const probeMaxSamples = 128

// OrientationStat describes one auto-orient decision for reporting.
type OrientationStat struct {
	FramesExamined     int     // probe frames decoded
	FramesWithSubjects int     // probe frames with at least one subject
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

// probeDetect mirrors the pipeline's subject detection (grayscale -> Otsu
// with the same +/-10 hysteresis -> binary -> connected components -> the
// 20%-of-largest filter with combineBounds -> union master bbox) so the
// probe estimates exactly what the framing controller will see.
func (hc *HoloCropper) probeDetect(img *image.RGBA, lastThr *float64) (image.Rectangle, int) {
	gray := convertToMonochrome(img)
	thr := float64(otsuThreshold(gray))
	if *lastThr < 0 {
		*lastThr = thr
	} else {
		*lastThr = clampF(thr, *lastThr-10, *lastThr+10)
	}
	binaryImg := applyThreshold(gray, uint8(*lastThr))

	// Degenerate-mask guard: on fully dark frames Otsu degenerates to
	// threshold 0 and the whole frame thresholds as "subject", faking a
	// full-frame bbox with the frame's own AR. Such frames carry no
	// subject information and are skipped.
	white, area := 0, binaryImg.Bounds().Dx()*binaryImg.Bounds().Dy()
	for y := binaryImg.Bounds().Min.Y; y < binaryImg.Bounds().Max.Y; y++ {
		for x := binaryImg.Bounds().Min.X; x < binaryImg.Bounds().Max.X; x++ {
			if binaryImg.GrayAt(x, y).Y > 0 {
				white++
			}
		}
	}
	if float64(white) >= 0.92*float64(area) {
		return image.ZR, 0
	}

	// The shared subject-extraction stage (identical filter the pipeline
	// uses) behind the degenerate-mask guard above.
	return subjectsFromBinary(binaryImg)
}

// probeSubjectARs decodes up to probeMaxSamples evenly spread frames of the
// input at a small analysis size and returns the master-bbox aspect ratio
// (w/h) of every sampled frame that contains subjects. The probe writes a
// throwaway tiny video (ffmpeg select + scale, audio dropped) and runs the
// real detection machinery on it; frames without subjects are skipped.
func (hc *HoloCropper) probeSubjectARs(input string) (ars []float64, examined int, err error) {
	src, err := vidio.NewVideo(input)
	if err != nil {
		return nil, 0, err
	}
	srcW, srcH, totalFrames := src.Width(), src.Height(), src.Frames()
	src.Close()
	if srcW <= 0 || srcH <= 0 || totalFrames <= 0 {
		return nil, 0, fmt.Errorf("auto-orient probe: unreadable input video %s", input)
	}

	// Analysis size: same rule as the pipeline but WITHOUT the output-height
	// floor - the probe measures subject aspect ratios, which are
	// scale-invariant, and must not depend on the orientation being tested.
	ah := hc.AnalysisHeight
	if ah <= 0 {
		ah = defaultAnalysisHeight
	}
	aw := ah * srcW / srcH
	if aw < 16 {
		aw = 16
	}
	if aw%2 != 0 {
		aw++
	}
	if ah%2 != 0 {
		ah++
	}

	stride := totalFrames / probeMaxSamples
	if stride < 1 {
		stride = 1
	}

	tmpDir, err := os.MkdirTemp("", "tinyprobe")
	if err != nil {
		return nil, 0, err
	}
	defer os.RemoveAll(tmpDir)
	probeVideo := filepath.Join(tmpDir, "probe.mp4")

	cmd := exec.Command("ffmpeg", "-v", "error",
		"-i", input,
		"-vf", fmt.Sprintf("select='not(mod(n\\,%d))',scale=%d:%d", stride, aw, ah),
		"-fps_mode", "vfr",
		"-an",
		probeVideo, "-y",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, 0, fmt.Errorf("auto-orient probe encode failed: %v: %s", err, out)
	}

	video, err := vidio.NewVideo(probeVideo)
	if err != nil {
		return nil, 0, err
	}
	defer video.Close()

	buf := image.NewRGBA(image.Rect(0, 0, video.Width(), video.Height()))
	video.SetFrameBuffer(buf.Pix)

	lastThr := -1.0
	for video.Read() {
		examined++
		bbox, subj := hc.probeDetect(buf, &lastThr)
		if subj > 0 && bbox.Dx() > 0 && bbox.Dy() > 0 {
			ars = append(ars, float64(bbox.Dx())/float64(bbox.Dy()))
		}
	}
	return ars, examined, nil
}

// orientationStats computes the full OrientationStat (AR aggregates + the
// keep/flip containment-score decision) from a set of master-bbox aspect
// ratios. Shared by the CLI probe path (AutoOrient) and the embeddable Plan
// orientation aggregate, which feeds it the whole detection stream instead
// of a sampled probe decode - so the decision and its reporting are
// identical across paths.
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

// AutoOrient probes the input and returns the output dimensions that best
// fit its subjects at the requested resolution: (w, h) as requested, or
// (h, w) when the transposed aspect scores better. Square outputs bypass
// the probe (1:1 has no portrait/landscape choice and the video is always
// kept upright). A failed or subjectless probe falls back to the requested
// orientation with the error (if any) attached.
func (hc *HoloCropper) AutoOrient(input string, w, h int) (outW, outH int, stat OrientationStat, err error) {
	// Match Crop()'s even-dimension normalisation so the decision aspect is
	// the aspect actually rendered.
	if w%2 != 0 {
		w++
	}
	if h%2 != 0 {
		h++
	}
	if w == h {
		return w, h, stat, nil
	}

	ars, examined, err := hc.probeSubjectARs(input)
	if err != nil {
		return w, h, OrientationStat{FramesExamined: examined}, err
	}
	outW, outH, stat = orientationStats(ars, w, h)
	stat.FramesExamined = examined
	return outW, outH, stat, nil
}
