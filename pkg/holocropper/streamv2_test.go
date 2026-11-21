package holocropper

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"image"
	"math"
	"os"
	"path/filepath"
	"testing"
)

/* U3 stream-v2 pins (plan HOLOCROPPER-API-PLAN.md §2/U3, item 761):
the packed stream sample grows 10 B -> 14 B with the subject centroid, the
meta records the layout version, FrameRecords/DetectionFrames carry the
centroid, and a v1-layout file still loads with the centroid simply
unrecorded — an old cache never fails, it just loses the new telemetry. */

func TestStreamV2RoundTripCarriesCentroid(t *testing.T) {
	sd := newStreamData()
	sd.append(newStreamSample(image.Rect(10, 20, 300, 400), 3, image.Point{X: 150, Y: 210}))
	sd.append(newStreamSample(image.Rect(40, 40, 320, 420), 2, image.Point{X: -15, Y: 7})) // negative int16 round-trips
	sd.append(newStreamSample(image.ZR, 0, image.Point{}))                                 // lost frame stays fully zero
	sd.setGeometry(320, 480, 640, 720, 24)

	path := filepath.Join(t.TempDir(), "stream.bin.gz")
	if err := writeStreamFile(path, sd); err != nil {
		t.Fatal(err)
	}
	got, err := readStreamFile(path, streamVersion)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("round trip has %d samples, want 3", len(got))
	}
	want := []image.Point{{X: 150, Y: 210}, {X: -15, Y: 7}, {}}
	for i, s := range got {
		if s.bbox() != sd.samples[i].bbox() {
			t.Errorf("sample %d bbox %v, want %v", i, s.bbox(), sd.samples[i].bbox())
		}
		if s.centroid() != want[i] {
			t.Errorf("sample %d centroid %v, want %v", i, s.centroid(), want[i])
		}
	}
	// A zero-centroid sample reads as unrecorded, matching the v1 semantic.
	if !got[2].centroid().Eq(image.Point{}) {
		t.Error("lost-frame sample carries a phantom centroid")
	}
}

func TestStreamV1FileLoadsDegraded(t *testing.T) {
	// Hand-write the ORIGINAL 10 B layout: bbox + subjects, no centroid.
	var raw bytes.Buffer
	for _, s := range [][5]int16{{10, 20, 300, 400, 3}, {40, 40, 320, 420, 2}} {
		var rec [10]byte
		binary.LittleEndian.PutUint16(rec[0:2], uint16(s[0]))
		binary.LittleEndian.PutUint16(rec[2:4], uint16(s[1]))
		binary.LittleEndian.PutUint16(rec[4:6], uint16(s[2]))
		binary.LittleEndian.PutUint16(rec[6:8], uint16(s[3]))
		binary.LittleEndian.PutUint16(rec[8:10], uint16(s[4]))
		raw.Write(rec[:])
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	zw.Close()
	path := filepath.Join(t.TempDir(), "legacy.bin.gz")
	if err := os.WriteFile(path, gz.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := readStreamFile(path, 0) // meta with no stream_version reads as v1
	if err != nil {
		t.Fatalf("v1 stream load failed: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("v1 stream has %d samples, want 2", len(got))
	}
	if got[0].bbox() != image.Rect(10, 20, 300, 400) || got[0].subjects != 3 {
		t.Errorf("v1 sample 0 = %v / %d, bbox+subjects must survive", got[0].bbox(), got[0].subjects)
	}
	if !got[0].centroid().Eq(image.Point{}) {
		t.Error("v1 sample fabricated a centroid; it must read as unrecorded")
	}
}

func TestAnalyzeCarriesCentroidsEndToEnd(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	inputDir, tempDir := t.TempDir(), t.TempDir()
	fixture := buildFixture(t, ffmpeg, inputDir)

	log := &eventLog{}
	req := AnalyzeRequest{Input: fixture, TempDir: tempDir, FFmpegPath: ffmpeg, Observer: log.observer()}
	task, err := Analyze(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	a1, err := task.Wait()
	if err != nil {
		t.Fatal(err)
	}

	// meta.json records both version bumps: analysis format v3 and stream v2.
	metaBytes, err := os.ReadFile(filepath.Join(a1.Dir(), analysisMetaName))
	if err != nil {
		t.Fatal(err)
	}
	var meta analysisMeta
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.FormatVersion != analysisFormatVersion || analysisFormatVersion != 3 {
		t.Fatalf("meta format_version = %d (const %d), want 3", meta.FormatVersion, analysisFormatVersion)
	}
	if meta.StreamVersion != streamVersion {
		t.Fatalf("meta stream_version = %d, want %d", meta.StreamVersion, streamVersion)
	}

	// DetectionFrames carry a non-zero centroid inside the bbox for every
	// frame with a subject (the fixture: one white box on a black field).
	stream := a1.Stream()
	for i := 0; i < stream.Len(); i++ {
		f := stream.Frame(i)
		if f.Subjects == 0 {
			continue
		}
		if f.Centroid.X == 0 && f.Centroid.Y == 0 {
			t.Fatalf("frame %d: subject with no centroid", i+1)
		}
		if float64(f.Centroid.X) < f.BBox.X || float64(f.Centroid.X) >= f.BBox.X+f.BBox.W ||
			float64(f.Centroid.Y) < f.BBox.Y || float64(f.Centroid.Y) >= f.BBox.Y+f.BBox.H {
			t.Fatalf("frame %d: centroid %v outside bbox %v", i+1, f.Centroid, f.BBox)
		}
	}

	// Plan records mark the centroid known, in all three spaces.
	plan, err := NewPlan(stream, Settings{Width: 240, Height: 240, FPS: fixtureFPS})
	if err != nil {
		t.Fatal(err)
	}
	recs := plan.Records()
	for _, r := range recs {
		if r.Subjects == 0 {
			continue
		}
		if !r.CentroidKnown {
			t.Fatalf("frame %d: CentroidKnown false on a measured frame", r.Frame)
		}
		if r.Norm.Centroid.X <= 0 || r.Norm.Centroid.X >= 1 || r.Norm.Centroid.Y <= 0 || r.Norm.Centroid.Y >= 1 {
			t.Fatalf("frame %d: norm centroid %v not inside 0..1", r.Frame, r.Norm.Centroid)
		}
		// The Out centroid sits inside the mapped bbox: the crop fills the
		// output, so mapPointToOutput of a point inside the crop lands inside
		// the mapped bbox rect.
		if r.Out.Centroid.X < r.Out.BBox.X || r.Out.Centroid.X > r.Out.BBox.X+r.Out.BBox.W ||
			r.Out.Centroid.Y < r.Out.BBox.Y || r.Out.Centroid.Y > r.Out.BBox.Y+r.Out.BBox.H {
			t.Fatalf("frame %d: out centroid %v outside mapped bbox %v", r.Frame, r.Out.Centroid, r.Out.BBox)
		}
	}

	// The live observer batches (detect-time FrameRecords) know the centroid too.
	haveLive := false
	for _, r := range log.frames {
		if r.Subjects > 0 {
			haveLive = true
			if !r.CentroidKnown {
				t.Fatalf("live record frame %d: CentroidKnown false", r.Frame)
			}
		}
	}
	if !haveLive {
		t.Fatal("observer received no frames")
	}

	// Cache-hit reload keeps centroids... (drop the in-memory entry first: a
	// bare Analyze is served from the registry, and this pin is about the
	// bytes on disk.)
	dropAnalysisEntry(a1.Dir())
	task2, err := Analyze(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := task2.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if a2.Stream().Len() != stream.Len() {
		t.Fatalf("cache-hit stream has %d frames, want %d", a2.Stream().Len(), stream.Len())
	}
	for i := 0; i < stream.Len(); i++ {
		if a2.Stream().Frame(i).Centroid != stream.Frame(i).Centroid {
			t.Fatalf("frame %d: cache-hit centroid %v != original %v", i+1, a2.Stream().Frame(i).Centroid, stream.Frame(i).Centroid)
		}
	}

	// ...and a cache written under the OLD 10 B layout re-loads from the same
	// meta path with the centroid simply unrecorded (the badge + re-analyze
	// law's data side: bboxes survive, telemetry degrades, nothing errors).
	legacy := make([]byte, 0, stream.Len()*10)
	for i := 0; i < stream.Len(); i++ {
		f := stream.Frame(i)
		var rec [10]byte
		binary.LittleEndian.PutUint16(rec[0:2], uint16(f.BBox.X))
		binary.LittleEndian.PutUint16(rec[2:4], uint16(f.BBox.Y))
		binary.LittleEndian.PutUint16(rec[4:6], uint16(f.BBox.X+f.BBox.W))
		binary.LittleEndian.PutUint16(rec[6:8], uint16(f.BBox.Y+f.BBox.H))
		binary.LittleEndian.PutUint16(rec[8:10], uint16(f.Subjects))
		legacy = append(legacy, rec[:]...)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(legacy)
	zw.Close()
	if err := os.WriteFile(filepath.Join(a1.Dir(), analysisStreamName), gz.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	meta.StreamVersion = 0 // an old meta had no stream_version field at all
	metaBytes, _ = json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(filepath.Join(a1.Dir(), analysisMetaName), metaBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	dropAnalysisEntry(a1.Dir())
	task3, err := Analyze(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	a3, err := task3.Wait()
	if err != nil {
		t.Fatalf("v1-layout cache reload failed: %v", err)
	}
	if a3.Stream().Len() != stream.Len() {
		t.Fatalf("degraded reload has %d frames, want %d", a3.Stream().Len(), stream.Len())
	}
	if a3.Stream().Frame(0).Centroid != (Point{}) {
		t.Fatal("degraded reload fabricated a centroid")
	}
	plan3, err := NewPlan(a3.Stream(), Settings{Width: 240, Height: 240, FPS: fixtureFPS})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range plan3.Records() {
		if r.Subjects > 0 && r.CentroidKnown {
			t.Fatalf("frame %d: degraded stream claims a known centroid", r.Frame)
		}
		if r.BBox.W <= 0 && r.Subjects > 0 {
			t.Fatalf("frame %d: degraded stream lost the bbox", r.Frame)
		}
	}
}

func TestDetectFrameCentroidBruteForceCOM(t *testing.T) {
	// Two white boxes close enough to merge into one pool: a small one at the
	// top-left and a big one down-right. The centroid is the centre of mass of
	// the subject-mask pixels inside the master bbox — so the expected point is
	// the area-weighted mean of the drawn subject boxes' pixel grids, computed
	// here from the geometry alone (the 1-1 project's calculateCentroid) with
	// the same integer truncation, independent of the grayscale/Otsu/binarize
	// stages.
	rects := []image.Rectangle{
		image.Rect(100, 100, 160, 160),
		image.Rect(170, 120, 330, 260),
	}
	frame := syntheticFrame(480, 480, rects)
	lastThr := -1.0
	bbox, subj, cent := detectFrame(frame, &lastThr)
	if subj != 1 {
		t.Fatalf("merged pool count %d, want 1", subj)
	}
	// The master bbox must cover both boxes (the engine may add its own subject
	// margin around them — see plan_test's feet-on-the-floor pins).
	if bbox.Min.X > 100 || bbox.Min.Y > 100 || bbox.Max.X <= 329 || bbox.Max.Y <= 259 {
		t.Fatalf("master bbox %v does not cover the drawn boxes", bbox)
	}
	var totalX, totalY, count int64
	for _, r := range rects {
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for x := r.Min.X; x < r.Max.X; x++ {
				totalX += int64(x)
				totalY += int64(y)
				count++
			}
		}
	}
	want := image.Point{X: int(totalX / count), Y: int(totalY / count)}
	if math.Abs(float64(cent.X-want.X)) > 1 || math.Abs(float64(cent.Y-want.Y)) > 1 {
		t.Fatalf("centroid %v, geometric COM %v", cent, want)
	}
	// The centroid must be asymmetric toward the big box — proving it is a
	// mask COM and not the bbox center.
	bcX := (bbox.Min.X + bbox.Max.X) / 2
	bcY := (bbox.Min.Y + bbox.Max.Y) / 2
	if math.Abs(float64(cent.X-bcX)) < 5 && math.Abs(float64(cent.Y-bcY)) < 5 {
		t.Fatalf("centroid %v sits on the bbox center %v,%v — looks like a center, not a COM", cent, bcX, bcY)
	}

	// A bbox with no mask pixels reports no centroid rather than a fabricated
	// one (the lost-subject / v1-sample path maskCentroid feeds detectFrame).
	blank := image.NewGray(image.Rect(0, 0, 64, 64))
	if got := maskCentroid(blank, blank.Bounds()); !got.Eq(image.Point{}) {
		t.Fatalf("all-black mask gave centroid %v, want zero", got)
	}
	// And the centroid always lands inside the master bbox the pass recorded.
	if cent.X < bbox.Min.X || cent.X >= bbox.Max.X || cent.Y < bbox.Min.Y || cent.Y >= bbox.Max.Y {
		t.Fatalf("centroid %v outside master bbox %v", cent, bbox)
	}
}
