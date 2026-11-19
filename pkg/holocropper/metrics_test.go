package holocropper

import (
	"math"
	"testing"
)

// Framing metrics semantics (story A6), pinned against the arithmetic of
// the §14/§14.1 corpus review tables (HIGH-ASPECT-RATIO-FIXES.md).

func approx(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

// crop1155 is the tbaka 3.24 crop width from the §14.1 slack measurement.
func crop1155() Rect { return Rect{X: 0, Y: 0, W: 1155, H: 1000} }

func TestComputeMetricsSlackMatchesFixDoc(t *testing.T) {
	// §14.1: tbaka 3.24 with the padding floor measures 58.3 / 40.5 px of
	// empty stage on the tightest frame -> min slack 40.5 px of a 1155 px
	// crop = 3.5065 % per side, comfortably past the 3.5 % guarantee.
	records := []FrameRecord{
		{Frame: 1, BBox: Rect{X: 58.3, W: 1038.2, H: 880}, Crop: crop1155(), Subjects: 1},
		{Frame: 2, BBox: Rect{X: 40.5, W: 1030.5, H: 880}, Crop: crop1155(), Subjects: 1},
		{Frame: 3, BBox: Rect{X: 148.8, W: 857.4, H: 880}, Crop: crop1155(), Subjects: 1},
	}
	m := ComputeMetrics(records)

	approx(t, "MinSlackPctPerSide", m.MinSlackPctPerSide, 100*40.5/1155)
	if m.MinSlackPctPerSide < 3.5 {
		t.Errorf("slack %.4f%% below the 3.5%% fit guarantee it was measured to clear", m.MinSlackPctPerSide)
	}
	if m.EdgeOvershootFrames != 0 {
		t.Errorf("overshoot frames = %d, want 0", m.EdgeOvershootFrames)
	}
	if m.WorstOvershootPx != 0 {
		t.Errorf("WorstOvershootPx = %v, want 0 when no frame is clipped", m.WorstOvershootPx)
	}
}

func TestComputeMetricsEdgeOvershoot(t *testing.T) {
	// The §14 pre-fix measurement: raw bbox crossing a crop side (tbaka
	// 16:9 had 1056 such frames before the padding fix; the semantic is
	// negative X slack = subject clipped at the crop edge).
	records := []FrameRecord{
		{Frame: 1, BBox: Rect{X: -46.7, W: 1201.7, H: 900}, Crop: crop1155(), Subjects: 1},
		{Frame: 2, BBox: Rect{X: 10, W: 1150, H: 900}, Crop: crop1155(), Subjects: 1}, // slackRight = -5
		{Frame: 3, BBox: Rect{X: 100, W: 900, H: 900}, Crop: crop1155(), Subjects: 1},
	}
	m := ComputeMetrics(records)

	if m.EdgeOvershootFrames != 2 {
		t.Errorf("overshoot frames = %d, want 2", m.EdgeOvershootFrames)
	}
	approx(t, "WorstOvershootPx", m.WorstOvershootPx, -46.7)
	approx(t, "MinSlackPctPerSide", m.MinSlackPctPerSide, 100*-46.7/1155)
	if m.MinSlackPctPerSide >= 0 {
		t.Error("MinSlackPctPerSide must go negative when frames are clipped")
	}
}

func TestComputeMetricsCoverage(t *testing.T) {
	// §14.1 quotes median subject-height coverage (88.5 % -> 84.0 % on
	// tbaka when the padding floor landed): coverage = bbox height over
	// crop height.
	odd := []FrameRecord{
		{Frame: 1, BBox: Rect{W: 400, H: 880}, Crop: crop1155(), Subjects: 1},
		{Frame: 2, BBox: Rect{W: 400, H: 900}, Crop: crop1155(), Subjects: 1},
		{Frame: 3, BBox: Rect{W: 400, H: 920}, Crop: crop1155(), Subjects: 1},
	}
	m := ComputeMetrics(odd)
	approx(t, "CoverageMeanPct", m.CoverageMeanPct, 90)
	approx(t, "CoverageMedianPct", m.CoverageMedianPct, 90)

	// Even count averages the two middle samples.
	mEven := ComputeMetrics([]FrameRecord{
		{Frame: 1, BBox: Rect{W: 400, H: 880}, Crop: crop1155(), Subjects: 1},
		{Frame: 2, BBox: Rect{W: 400, H: 900}, Crop: crop1155(), Subjects: 1},
		{Frame: 3, BBox: Rect{W: 400, H: 920}, Crop: crop1155(), Subjects: 1},
		{Frame: 4, BBox: Rect{W: 400, H: 940}, Crop: crop1155(), Subjects: 1},
	})
	approx(t, "CoverageMeanPct", mEven.CoverageMeanPct, 91)
	approx(t, "CoverageMedianPct", mEven.CoverageMedianPct, 91)
}

func TestComputeMetricsWalkRange(t *testing.T) {
	// §14: tbaka 3.24 walk mode moved the subject 244 px across the 648 px
	// wide output - measured as the range of the subject center X in
	// output px.
	out := func(x float64) SpaceRects { return SpaceRects{BBox: Rect{X: x, W: 200, H: 300}} }
	records := []FrameRecord{
		{Frame: 1, BBox: Rect{X: 10, W: 300, H: 800}, Crop: crop1155(), Out: out(100), Subjects: 1},
		{Frame: 2, BBox: Rect{X: 300, W: 300, H: 800}, Crop: crop1155(), Out: out(344), Subjects: 1},
		{Frame: 3, BBox: Rect{X: 150, W: 300, H: 800}, Crop: crop1155(), Out: out(220), Subjects: 1},
	}
	m := ComputeMetrics(records)
	approx(t, "WalkRangePx", m.WalkRangePx, 244)
}

func TestComputeMetricsDropoutsExcluded(t *testing.T) {
	// A lost subject is not a framing measurement: dropout frames are
	// counted separately and must not skew coverage, slack or travel
	// (their bboxes are zero).
	records := []FrameRecord{
		{Frame: 1, BBox: Rect{X: 40, W: 400, H: 880}, Crop: crop1155(), Out: SpaceRects{BBox: Rect{X: 100, W: 200}}, Subjects: 1},
		{Frame: 2, SubjectLost: true, SceneCut: true, Crop: crop1155()}, // zero bbox + flag
		{Frame: 3, Crop: crop1155()}, // zero bbox, no flag: excluded, not a dropout
		{Frame: 4, BBox: Rect{X: 400, W: 400, H: 920}, Crop: crop1155(), Out: SpaceRects{BBox: Rect{X: 346, W: 200}}, Subjects: 1},
	}
	m := ComputeMetrics(records)

	if m.Frames != 4 {
		t.Errorf("Frames = %d, want 4", m.Frames)
	}
	if m.DropoutFrames != 1 {
		t.Errorf("DropoutFrames = %d, want 1", m.DropoutFrames)
	}
	if m.SceneCuts != 1 {
		t.Errorf("SceneCuts = %d, want 1", m.SceneCuts)
	}
	approx(t, "CoverageMeanPct", m.CoverageMeanPct, 90) // (880 + 920) / 2 valid frames only
	approx(t, "WalkRangePx", m.WalkRangePx, 246)        // centers 200..446; the dropout's zero bbox must not drag the minimum to 0
	if m.MinSlackPctPerSide < 0 {
		t.Errorf("MinSlackPctPerSide = %v, want >= 0 (zero-bbox frames carry no slack)", m.MinSlackPctPerSide)
	}
}

func TestComputeMetricsEmpty(t *testing.T) {
	m := ComputeMetrics(nil)
	if m.Frames != 0 {
		t.Errorf("Frames = %d, want 0", m.Frames)
	}
	if m.CoverageMeanPct != 0 || m.CoverageMedianPct != 0 || m.WalkRangePx != 0 ||
		m.MinSlackPctPerSide != 0 || m.WorstOvershootPx != 0 || m.EdgeOvershootFrames != 0 {
		t.Errorf("empty report not zeroed: %+v", m)
	}
}

func TestPlanMetricsMatchesRecords(t *testing.T) {
	// A Plan's own report is ComputeMetrics over its records, recomputable
	// at any time (the UI calls it after every re-plan).
	s := newTestStream(walkingSamples(40, 100, 4, 120, 200), 428, 240, fixtureW, fixtureH)
	pPortrait, err := NewPlan(s, Settings{Width: 240, Height: 240, FPS: fixtureFPS})
	if err != nil {
		t.Fatal(err)
	}
	m := pPortrait.Metrics()
	if m.Frames != len(pPortrait.records) {
		t.Fatalf("Metrics covered %d frames, plan has %d", m.Frames, len(pPortrait.records))
	}
	if *m != *ComputeMetrics(pPortrait.records) {
		t.Errorf("Plan.Metrics() diverged from ComputeMetrics(records): %+v vs %+v", m, ComputeMetrics(pPortrait.records))
	}
	if m.DropoutFrames != 0 || m.EdgeOvershootFrames != 0 {
		t.Errorf("clean synthetic stream reported dropouts/overshoot: %+v", m)
	}

	// The §14 story in miniature: portrait framing pins the walking subject
	// near the output centre (tiny travel); a landscape output (capped at
	// the synthetic stream's 240 analysis height) activates the walk leash
	// and the subject's centre measurably travels.
	pWalk, err := NewPlan(s, Settings{Width: 432, Height: 240, FPS: fixtureFPS})
	if err != nil {
		t.Fatal(err)
	}
	mWalk := pWalk.Metrics()
	t.Logf("walk travel: portrait %.1f px, landscape %.1f px", m.WalkRangePx, mWalk.WalkRangePx)
	if mWalk.WalkRangePx <= m.WalkRangePx+20 {
		t.Errorf("walk travel: landscape %.1f px vs portrait %.1f px, want landscape clearly larger", mWalk.WalkRangePx, m.WalkRangePx)
	}
	// Full uncapped travel in the portrait output would be ~163 px; the pan
	// leash pins the subject much closer to centre.
	if m.WalkRangePx > 90 {
		t.Errorf("portrait travel %.1f px, want pinned well under the ~163 px uncapped travel", m.WalkRangePx)
	}
}
