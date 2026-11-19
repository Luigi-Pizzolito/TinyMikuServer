package holocropper

import "sort"

/* Framing metrics (HOLOCROPPER-API-PLAN.md story A6).

These are the numbers the corpus-analysis tooling measured from the debug
CSVs (HIGH-ASPECT-RATIO-FIXES.md §14/§14.1 review tables), computed here
from FrameRecords so they recompute in milliseconds on every re-plan:

- coverage: subject height as a share of the crop height (the §14.1
  "median subject-height coverage" - 88.5% -> 84.0% on tbaka when the
  padding floor landed).
- edge overshoot: frames where the raw master bbox crosses a crop side
  edge, i.e. the subject is clipped at the output's left/right edge (the
  §14 "raw bbox-outside-crop" column: tbaka 16:9 1056 -> 47 frames).
- per-side slack: the empty stage band between the subject bbox and each
  crop edge, as a percentage of the crop width - the §14.1 fit guarantee
  (>= 3.5% empty per side on tbaka 3.24, >= 7.6% on 16:9 group fits).
- walk travel: how far the subject's center actually moves across the
  output in pixels (the §14 "subject travel on output" column).

Frames where the subject was lost are excluded from coverage/overshoot/
slack/travel (an empty bbox is not a framing measurement) and counted in
DropoutFrames instead. */

// Metrics is the live framing report a UI shows as badges next to the
// timeline (§9.1 bottom bar). Every field is derived from a Plan's records.
type Metrics struct {
	Frames int `json:"frames"` // records considered (all frames in the Plan)

	CoverageMeanPct   float64 `json:"coverage_mean_pct"`   // subject height / crop height, mean %
	CoverageMedianPct float64 `json:"coverage_median_pct"` // same, median %

	EdgeOvershootFrames int     `json:"edge_overshoot_frames"` // frames clipped at a crop side edge
	WorstOvershootPx    float64 `json:"worst_overshoot_px"`    // most negative slack over all frames (<= 0, analysis px)

	MinSlackPctPerSide float64 `json:"min_slack_pct_per_side"` // tightest empty band per side, % of crop width

	WalkRangePx float64 `json:"walk_range_px"` // subject-center travel across the output (output px)

	DropoutFrames int `json:"dropout_frames"` // frames with the subject lost
	SceneCuts     int `json:"scene_cuts"`     // scene-cut resets
}

// ComputeMetrics rolls a record stream up into the §14 framing report.
// (Plan §6.8 sketched this with a Geometry argument; FrameRecords already
// carry analysis-, normalized- and output-space rects per frame, so no
// extra geometry is needed.)
func ComputeMetrics(records []FrameRecord) *Metrics {
	m := &Metrics{Frames: len(records)}

	coverages := make([]float64, 0, len(records))
	var walkMin, walkMax float64
	haveWalk := false
	worstSlack := 0.0
	haveSlack := false
	minSlackPct := 0.0
	haveSlackPct := false

	for i := range records {
		rec := &records[i]
		if rec.SceneCut {
			m.SceneCuts++
		}
		// An empty bbox is a dropout, not a framing sample.
		if rec.SubjectLost || rec.BBox.W <= 0 || rec.Crop.W <= 0 || rec.Crop.H <= 0 {
			if rec.SubjectLost {
				m.DropoutFrames++
			}
			continue
		}

		coverages = append(coverages, 100*rec.BBox.H/rec.Crop.H)

		// X slack per side: bbox edge minus crop edge (negative = clipped).
		slackLeft := rec.BBox.X - rec.Crop.X
		slackRight := rec.Crop.X + rec.Crop.W - (rec.BBox.X + rec.BBox.W)
		frameWorst := slackLeft
		if slackRight < frameWorst {
			frameWorst = slackRight
		}
		if !haveSlack || frameWorst < worstSlack {
			worstSlack, haveSlack = frameWorst, true
		}
		if slackLeft < 0 || slackRight < 0 {
			m.EdgeOvershootFrames++
		}
		pct := 100 * frameWorst / rec.Crop.W
		if !haveSlackPct || pct < minSlackPct {
			minSlackPct, haveSlackPct = pct, true
		}

		// Subject travel on the output: center X in output px.
		cx := rec.Out.BBox.X + rec.Out.BBox.W/2
		if !haveWalk {
			walkMin, walkMax, haveWalk = cx, cx, true
		} else {
			if cx < walkMin {
				walkMin = cx
			}
			if cx > walkMax {
				walkMax = cx
			}
		}
	}

	if len(coverages) > 0 {
		sum := 0.0
		for _, c := range coverages {
			sum += c
		}
		m.CoverageMeanPct = sum / float64(len(coverages))
		sort.Float64s(coverages)
		mid := len(coverages) / 2
		if len(coverages)%2 == 1 {
			m.CoverageMedianPct = coverages[mid]
		} else {
			m.CoverageMedianPct = (coverages[mid-1] + coverages[mid]) / 2
		}
	}
	if haveSlack && worstSlack < 0 {
		m.WorstOvershootPx = worstSlack
	}
	if haveSlackPct {
		m.MinSlackPctPerSide = minSlackPct
	}
	if haveWalk {
		m.WalkRangePx = walkMax - walkMin
	}
	return m
}

// Metrics computes the framing report for this Plan. Cheap enough to call
// after every re-plan (a single linear pass over the records).
func (p *Plan) Metrics() *Metrics { return ComputeMetrics(p.records) }
