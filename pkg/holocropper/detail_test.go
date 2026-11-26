package holocropper

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

/* Milestone G3 pins: the lazy detail verbs (SubjectBoxes / FrameIslands) and the
measurement they share with the detect pass. The mask-level pins run on hand-built
masks; the end-to-end pin re-measures real fixture frames and requires it to
reproduce the recorded stream exactly. */

var colorWhite = color.Gray{Y: 255}

// rectContains is the containment test this package's Go toolchain lacks as a
// method (no Rectangle.Includes): a holds b when their intersection is b itself.
func rectContains(a, b image.Rectangle) bool {
	return !b.Empty() && a.Intersect(b).Eq(b)
}

func solidMask(w, h int, rects ...image.Rectangle) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, w, h))
	for _, r := range rects {
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for x := r.Min.X; x < r.Max.X; x++ {
				img.SetGray(x, y, colorWhite)
			}
		}
	}
	return img
}

// dancersMask: two standing islands 25 px apart — inside the merge tolerance
// (frameH/20 = 32 px on a 648-tall frame) — and one speckle the 20% filter drops.
func dancersMask() *image.Gray {
	return solidMask(1152, 648,
		image.Rect(300, 120, 380, 520),
		image.Rect(405, 140, 485, 520),
		image.Rect(900, 40, 910, 52),
	)
}

func TestSubjectDetailMeasuresBothLayers(t *testing.T) {
	mask := dancersMask()
	d := subjectDetailFromBinary(mask)

	if len(d.Components) != 3 {
		t.Fatalf("%d islands, want 3 (two dancers + the speckle)", len(d.Components))
	}
	// Largest bbox first, and the tall dancers are the accepted subjects.
	if d.Components[0].Rect.Dx()*d.Components[0].Rect.Dy() < d.Components[1].Rect.Dx()*d.Components[1].Rect.Dy() {
		t.Fatalf("components are not sorted by bbox area: %v then %v", d.Components[0].Rect, d.Components[1].Rect)
	}
	bigAccepted := 0
	for _, c := range d.Components {
		area := c.Rect.Dx() * c.Rect.Dy()
		want := float64(area) >= 0.2*float64(d.Components[0].Rect.Dx()*d.Components[0].Rect.Dy())
		if c.Accepted != want {
			t.Errorf("island %v accepted=%v, 20%% filter says %v", c.Rect, c.Accepted, want)
		}
		if c.Accepted {
			bigAccepted++
		}
		if c.Rect.Dx()*c.Rect.Dy() < c.MaskPx {
			t.Errorf("island %v: bbox area %d below its own pixel count %d", c.Rect, c.Rect.Dx()*c.Rect.Dy(), c.MaskPx)
		}
	}
	if bigAccepted != 2 {
		t.Fatalf("%d accepted islands, want the 2 dancers", bigAccepted)
	}

	// One pool per accepted island (2), the dancers folded together because they
	// sit inside each other's tolerance, so the union covers both.
	if d.Subjects != 2 || len(d.Pools) != 2 {
		t.Fatalf("subjects %d / pools %d, want 2 (one pool per accepted island)", d.Subjects, len(d.Pools))
	}
	merged := 0
	for _, p := range d.Pools {
		if len(p.Seeds) >= 2 {
			merged++
		}
		if !rectContains(d.Master, p.Rect) {
			t.Errorf("pool %v escapes the master bbox %v", p.Rect, d.Master)
		}
		for _, c := range d.Components {
			if c.Accepted && rectContains(p.Rect, c.Rect) && !containsLabel(p.Seeds, c.Label) {
				t.Errorf("pool %v covers accepted island %v without absorbing it", p.Rect, c.Rect)
			}
		}
	}
	if merged == 0 {
		t.Fatal("no pool absorbed more than its seed: the tolerance merge is not running")
	}

	// The master bbox is the union of the pools, and the two-number summary the
	// detect pass uses is exactly this measurement's last two fields.
	master, count := subjectsFromBinary(mask)
	if master != d.Master || count != d.Subjects {
		t.Fatalf("subjectsFromBinary gave %v/%d, the detail measurement gives %v/%d", master, count, d.Master, d.Subjects)
	}

	// Solid islands: their centre of mass is their own bbox centre.
	for _, c := range d.Components {
		want := image.Point{X: (c.Rect.Min.X + c.Rect.Max.X - 1) / 2, Y: (c.Rect.Min.Y + c.Rect.Max.Y - 1) / 2}
		if c.Centroid != want {
			t.Errorf("island %v centroid %v, want its centre %v", c.Rect, c.Centroid, want)
		}
	}
}

func containsLabel(labels []int, want int) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

func TestSubjectIslandsOutlineEveryIslandIncludingHoles(t *testing.T) {
	// A 60x20 solid island and a 6x6 island with a 2x2 hole punched out of it.
	mask := solidMask(200, 120, image.Rect(20, 20, 80, 40))
	for y := 102; y < 108; y++ {
		for x := 102; x < 108; x++ {
			if x >= 104 && x < 106 && y >= 104 && y < 106 {
				continue // the hole
			}
			mask.SetGray(x, y, colorWhite)
		}
	}
	d := subjectDetailFromBinary(mask)
	islands := subjectIslands(d)
	if len(islands) != 2 {
		t.Fatalf("%d islands, want 2", len(islands))
	}

	byArea := func(i, j int) bool {
		return islands[i].MaskPx > islands[j].MaskPx
	}
	if !byArea(0, 1) {
		t.Fatalf("islands are not largest-first: %d px then %d px", islands[0].MaskPx, islands[1].MaskPx)
	}

	// Solid 60x20 island: the outline is its perimeter - 2w + 2h - 4 pixels -
	// drawn as the top run, the bottom run, and one pixel on each side per row.
	solid := islands[0]
	wantPerimeter := 2*60 + 2*20 - 4
	if got := outlinePixels(solid.Outline); got != wantPerimeter {
		t.Fatalf("solid island outline %d px, want the perimeter %d", got, wantPerimeter)
	}
	if len(solid.Outline) != 2+2*18 {
		t.Fatalf("solid island drew %d runs, want 2 full-width runs + 2 side pixels x 18 rows", len(solid.Outline))
	}
	for _, run := range solid.Outline {
		r := image.Rect(run.X0, run.Y, run.X1+1, run.Y+1)
		if !rectContains(solid.BBox.toImage(), r) {
			t.Fatalf("outline run %v escapes island bbox %v", run, solid.BBox)
		}
	}

	// Holey island: outer perimeter (20) plus the closed 12-pixel 8-ring the
	// boundary law draws around the hole - 4 sides, 4 corners - so the hole is
	// an outline of its own.
	holed := islands[1]
	if got := outlinePixels(holed.Outline); got != 20+12 {
		t.Fatalf("holed island outline %d px, want 20 outer + 12 around the hole", got)
	}
	if holed.MaskPx != 36-4 {
		t.Fatalf("holed island mask_px %d, want 32", holed.MaskPx)
	}
}

func outlinePixels(runs []PixelRun) int {
	n := 0
	for _, r := range runs {
		n += r.X1 - r.X0 + 1
	}
	return n
}

func TestSubjectDetailNeedsACachedVideoAndARecordedThreshold(t *testing.T) {
	// A synthetic stream has no analysis video: the verbs say so instead of
	// inventing boxes.
	synthetic := newTestStream([]streamSample{newStreamSample(image.Rect(10, 10, 40, 60), 1, image.Point{X: 25, Y: 35})}, 480, 270, 960, 540)
	if _, err := synthetic.SubjectBoxes(0); !errors.Is(err, ErrDetailUnavailable) {
		t.Errorf("synthetic stream SubjectBoxes err = %v, want ErrDetailUnavailable", err)
	}
	if _, err := synthetic.FrameIslands(0); !errors.Is(err, ErrDetailUnavailable) {
		t.Errorf("synthetic stream FrameIslands err = %v, want ErrDetailUnavailable", err)
	}

	// A cache an older build wrote (10 B samples, no threshold) says the same
	// thing and names re-analysis, which is what the app's badge keys on.
	var raw bytes.Buffer
	var rec [10]byte
	binary.LittleEndian.PutUint16(rec[0:2], uint16(10))
	binary.LittleEndian.PutUint16(rec[2:4], uint16(20))
	binary.LittleEndian.PutUint16(rec[4:6], uint16(300))
	binary.LittleEndian.PutUint16(rec[6:8], uint16(400))
	binary.LittleEndian.PutUint16(rec[8:10], 1)
	raw.Write(rec[:])
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(raw.Bytes())
	zw.Close()
	path := filepath.Join(t.TempDir(), "legacy.bin.gz")
	if err := os.WriteFile(path, gz.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	samples, err := readStreamFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	sd := newStreamData()
	sd.samples = samples
	sd.analysisW, sd.analysisH = 1152, 648
	sd.fps = 24
	sd.complete = true
	sd.videoPath = path // a video IS present; only the threshold is missing
	stale := DetectionStream{data: sd}
	if _, err := stale.SubjectBoxes(0); !errors.Is(err, ErrDetailUnavailable) {
		t.Errorf("v1 cache SubjectBoxes err = %v, want ErrDetailUnavailable", err)
	}
	if _, err := stale.FrameIslands(0); err == nil || !errors.Is(err, ErrDetailUnavailable) {
		t.Errorf("v1 cache FrameIslands err = %v, want ErrDetailUnavailable naming re-analysis", err)
	}
	if _, err := stale.FrameIslands(0); err == nil || !strings.Contains(err.Error(), "re-analyze") {
		t.Errorf("v1 cache FrameIslands err = %v, want it to name re-analysis", err)
	}
	if _, err := stale.SubjectBoxes(9); err == nil || errors.Is(err, ErrDetailUnavailable) {
		t.Errorf("out-of-range frame err = %v, want a plain range error", err)
	}
}

func TestSubjectDetailReproducesTheRecordedStream(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	fixture := sharedBoxFixture(t, ffmpeg)
	a, _ := attachWarmAnalysis(t, ffmpeg, fixture, AnalyzeCrop)
	stream := a.Stream()

	measured := 0
	for i := 0; i < stream.Len(); i += 7 {
		f := stream.Frame(i)
		if f.Subjects == 0 {
			continue
		}
		boxes, err := stream.SubjectBoxes(i)
		if err != nil {
			t.Fatalf("frame %d: %v", i+1, err)
		}
		pools, islands := 0, 0
		for _, b := range boxes {
			if b.IsPool {
				pools++
			} else {
				islands++
			}
		}
		// The verb re-measures the frame the detect pass saw: same count, same
		// master bbox, no fresh approximation.
		if pools != f.Subjects {
			t.Fatalf("frame %d: %d pools, the stream recorded %d subjects", i+1, pools, f.Subjects)
		}
		union := image.ZR
		for _, b := range boxes {
			if b.IsPool {
				union = union.Union(b.Rect.toImage())
			}
		}
		if union != f.BBox.toImage() {
			t.Fatalf("frame %d: re-measured pools union %v, the stream recorded %v", i+1, union, f.BBox)
		}
		if islands < pools {
			t.Fatalf("frame %d: %d raw islands cannot be fewer than the %d pools they formed", i+1, islands, pools)
		}

		// Determinism: the same frame measured twice answers identically, so a
		// UI scrubbing back and forth never sees the layers flicker.
		again, err := stream.SubjectBoxes(i)
		if err != nil {
			t.Fatal(err)
		}
		if len(again) != len(boxes) {
			t.Fatalf("frame %d: second measurement has %d boxes, first had %d", i+1, len(again), len(boxes))
		}
		for k := range boxes {
			if !sameSubjectBox(again[k], boxes[k]) {
				t.Fatalf("frame %d: box %d changed between measurements: %+v vs %+v", i+1, k, boxes[k], again[k])
			}
		}

		isle, err := stream.FrameIslands(i)
		if err != nil {
			t.Fatalf("frame %d: FrameIslands: %v", i+1, err)
		}
		if len(isle) != islands {
			t.Fatalf("frame %d: %d islands, SubjectBoxes reported %d raw islands", i+1, len(isle), islands)
		}
		for _, isl := range isle {
			if isl.MaskPx <= 0 || len(isl.Outline) == 0 {
				t.Fatalf("frame %d: island %+v has no pixels or no outline", i+1, isl)
			}
			if outlinePixels(isl.Outline) > isl.MaskPx {
				t.Fatalf("frame %d: island outline has %d px, more than its %d mask px", i+1, outlinePixels(isl.Outline), isl.MaskPx)
			}
			for _, run := range isl.Outline {
				if !rectContains(isl.BBox.toImage(), image.Rect(run.X0, run.Y, run.X1+1, run.Y+1)) {
					t.Fatalf("frame %d: outline run %v escapes island bbox %v", i+1, run, isl.BBox)
				}
			}
		}
		measured++
	}
	if measured == 0 {
		t.Fatal("the fixture produced no measurable subject frames")
	}

	// Out of range is a plain error, not the unavailable one.
	if _, err := stream.SubjectBoxes(stream.Len()); err == nil || errors.Is(err, ErrDetailUnavailable) {
		t.Errorf("out-of-range SubjectBoxes err = %v, want a range error", err)
	}
}

func sameSubjectBox(a, b SubjectBox) bool {
	return a.Rect == b.Rect && a.MaskPx == b.MaskPx && a.Centroid == b.Centroid &&
		a.IsPool == b.IsPool && a.Accepted == b.Accepted &&
		len(a.Seeds) == len(b.Seeds) && containsAllLabels(a.Seeds, b.Seeds)
}

func containsAllLabels(a, b []int) bool {
	for _, l := range a {
		if !containsLabel(b, l) {
			return false
		}
	}
	return true
}
