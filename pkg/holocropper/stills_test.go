package holocropper

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// Stills and contact sheets (story B3). Layout, sampling, colours and the
// caption font are pure and pinned here; compositing runs against the real
// fixture analysis video when ffmpeg is available.

// rawFrameSeeked decodes one rgb24 frame out of src with the STILL decode's
// input seek (-ss before -i, -frames:v 1), exactly the byte stream
// runSourceRender reads on the seek path.
func rawFrameSeeked(ffmpeg, src string, ss float64, w, h int) ([]byte, error) {
	cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error",
		"-ss", strconv.FormatFloat(ss, 'f', 6, 64),
		"-i", src, "-map", "0:v:0", "-fps_mode", "passthrough",
		"-f", "rawvideo", "-pix_fmt", "rgb24", "-frames:v", "1", "pipe:1")
	return readRawFrame(cmd, w, h)
}

// rawFrameSelected decodes frame idx of src with a decode-from-start and the
// select filter - the frame-exact reference the contact-sheet machinery uses.
func rawFrameSelected(ffmpeg, src string, idx, w, h int) ([]byte, error) {
	cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error",
		"-i", src, "-map", "0:v:0", "-fps_mode", "passthrough",
		"-vf", fmt.Sprintf("select='eq(n\\,%d)'", idx),
		"-f", "rawvideo", "-pix_fmt", "rgb24", "-frames:v", "1", "pipe:1")
	return readRawFrame(cmd, w, h)
}

func readRawFrame(cmd *exec.Cmd, w, h int) ([]byte, error) {
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	if len(out) != w*h*3 {
		return nil, fmt.Errorf("rawvideo frame is %d bytes, want %d", len(out), w*h*3)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Layout and sampling (pure)

func assertStrictlyIncreasing(t *testing.T, name string, idx []int, n int) {
	t.Helper()
	if len(idx) == 0 {
		t.Fatalf("%s: no indices", name)
	}
	if idx[0] != 0 {
		t.Errorf("%s: first sample %d, want frame index 0", name, idx[0])
	}
	if idx[len(idx)-1] != n-1 {
		t.Errorf("%s: last sample %d, want last frame index %d", name, idx[len(idx)-1], n-1)
	}
	for i := 1; i < len(idx); i++ {
		if idx[i] <= idx[i-1] || idx[i] >= n {
			t.Fatalf("%s: indices not strictly increasing in range: %v", name, idx)
		}
	}
}

func TestContactSheetLayoutSampling(t *testing.T) {
	// 96 frames: stride ceil(96/36)=3 -> 32 tiles; the last sample lands
	// on the final frame.
	idx, cols, tileW, tileH, labelH := contactSheetLayout(96, 648, 1152, 0)
	if len(idx) != 32 {
		t.Fatalf("32 tiles expected, got %d", len(idx))
	}
	assertStrictlyIncreasing(t, "96-frame sampling", idx, 96)
	if idx[1] != 3 || idx[30] != 90 {
		t.Errorf("stride sampling broken: idx[1]=%d idx[30]=%d, want 3 and 90", idx[1], idx[30])
	}
	if cols != contactSheetCols {
		t.Errorf("cols = %d, want %d", cols, contactSheetCols)
	}
	// Default 1280-wide sheet: (1280 - 4*7) / 6 = 208 per tile, 9:16.
	if tileW != 208 || tileH != 370 {
		t.Errorf("tile %dx%d, want 208x370", tileW, tileH)
	}
	if labelH != 61 {
		t.Errorf("labelH = %d, want 61", labelH)
	}
}

func TestContactSheetLayoutLongVideoCapped(t *testing.T) {
	// 4167 frames: stride 116 -> exactly the 36-tile cap, first and last.
	idx, _, _, _, _ := contactSheetLayout(4167, 648, 1152, 0)
	if len(idx) != contactSheetMaxTiles {
		t.Fatalf("tiles = %d, want the cap %d", len(idx), contactSheetMaxTiles)
	}
	assertStrictlyIncreasing(t, "4167-frame sampling", idx, 4167)
	if idx[1] != 116 {
		t.Errorf("stride = %d, want 116", idx[1]-idx[0])
	}
}

func TestContactSheetLayoutSmallVideos(t *testing.T) {
	idx1, cols1, _, _, _ := contactSheetLayout(1, 648, 1152, 0)
	if len(idx1) != 1 || idx1[0] != 0 || cols1 != 1 {
		t.Errorf("single frame: %v cols %d, want [0] cols 1", idx1, cols1)
	}

	// 37 frames with stride 2 already ends on the last frame; no forcing.
	idx37, _, _, _, _ := contactSheetLayout(37, 648, 1152, 0)
	assertStrictlyIncreasing(t, "37-frame sampling", idx37, 37)
	if idx37[18] != 36 || len(idx37) != 19 {
		t.Errorf("37 frames: %d tiles, last %d, want 19 tiles last index 36", len(idx37), idx37[len(idx37)-1])
	}

	idx40, _, _, _, _ := contactSheetLayout(40, 648, 1152, 0)
	assertStrictlyIncreasing(t, "40-frame sampling", idx40, 40)
	if len(idx40) != 20 || idx40[18] != 36 || idx40[19] != 39 {
		t.Errorf("40 frames: %v, want stride 2 with last sample forced to 39", idx40)
	}
}

func TestContactSheetLayoutTileClamp(t *testing.T) {
	// A cap below cols*minTileW clamps tiles to contactSheetMinTileW; the
	// sheet then exceeds the cap - documented floor behaviour.
	_, _, tileW, tileH, _ := contactSheetLayout(36, 648, 1152, 200)
	if tileW != contactSheetMinTileW || tileH != 142 {
		t.Errorf("clamped tile %dx%d, want 80x142", tileW, tileH)
	}
}

func TestStillsCapSize(t *testing.T) {
	cases := []struct {
		w, h, max, wantW, wantH int
	}{
		{648, 1152, 0, 648, 1152},   // no cap
		{648, 1152, 648, 648, 1152}, // cap == width
		{648, 1152, 900, 648, 1152}, // cap above width
		{648, 1152, 324, 324, 576},  // half scale, aspect kept
		{1000, 400, 500, 500, 200},  // landscape
		{648, 1152, 1, 2, 4},        // floor 2 px
	}
	for _, c := range cases {
		gw, gh := stillsCapSize(c.w, c.h, c.max)
		if gw != c.wantW || gh != c.wantH {
			t.Errorf("stillsCapSize(%d,%d,%d) = %dx%d, want %dx%d", c.w, c.h, c.max, gw, gh, c.wantW, c.wantH)
		}
	}
}

func TestScaleRecordToFrame(t *testing.T) {
	rec := FrameRecord{Out: SpaceRects{
		BBox: Rect{X: 120, Y: 240, W: 100, H: 80},
		EMA:  Rect{X: 130, Y: 250, W: 90, H: 70},
	}}
	scaled := scaleRecordToFrame(rec, 480, 640, 240, 320)
	want := Rect{X: 60, Y: 120, W: 50, H: 40}
	if scaled.Out.BBox != want {
		t.Errorf("scaled bbox %+v, want %+v", scaled.Out.BBox, want)
	}
	// Identity sizes must not touch the record, and the original is not mutated.
	if same := scaleRecordToFrame(rec, 480, 640, 480, 640); same.Out != rec.Out {
		t.Errorf("identity scale changed rects: %+v", same.Out)
	}
	if rec.Out.BBox.X != 120 {
		t.Error("scaleRecordToFrame mutated the input record")
	}
}

// ---------------------------------------------------------------------------
// Status bar and caption font (pure)

func TestContactSheetBarPrecedence(t *testing.T) {
	cases := []struct {
		name string
		rec  FrameRecord
		want color.RGBA
	}{
		{"dropout beats all", FrameRecord{SubjectLost: true, SceneCut: true, WalkActive: true}, overlayDropout},
		{"scene cut beats walk", FrameRecord{SceneCut: true, WalkActive: true}, overlaySceneCut},
		{"walk", FrameRecord{WalkActive: true}, overlayWalkOn},
		{"neutral", FrameRecord{}, overlayNeutral},
	}
	for _, c := range cases {
		if got := contactSheetBarColor(c.rec); got != c.want {
			t.Errorf("%s: bar %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestDigitGlyphs(t *testing.T) {
	want1 := [digitGlyphH]byte{0b010, 0b110, 0b010, 0b010, 0b111}
	want4 := [digitGlyphH]byte{0b101, 0b101, 0b111, 0b001, 0b001}
	if digitGlyphs[1] != want1 {
		t.Errorf("glyph '1' = %03b..., want %03b...", digitGlyphs[1], want1)
	}
	if digitGlyphs[4] != want4 {
		t.Errorf("glyph '4' wrong: %v", digitGlyphs[4])
	}
	for d := 0; d < 10; d++ {
		if digitGlyphs[d][0]&0b111 == 0 {
			t.Errorf("glyph %d has an empty top row", d)
		}
	}
}

func TestDrawDigits(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 40, 8))
	white := color.RGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}

	end := drawDigits(img, 1, 1, 12, 2, white) // "12" at scale 2
	// '1': only its middle column is lit in the top row -> pixel at x+2.
	if !sameColor(img.At(3, 1), white) {
		t.Error("'1' top-row pixel missing")
	}
	if sameColor(img.At(1, 1), white) {
		t.Error("'1' top-left pixel should be empty")
	}
	// '2' starts after one digit advance (4 cells * scale 2).
	if !sameColor(img.At(9, 1), white) {
		t.Error("'2' top-row first pixel missing")
	}
	if want := 1 + 2*(digitGlyphW+1)*2; end != want {
		t.Errorf("drawDigits advanced to %d, want %d", end, want)
	}

	// scale < 1 clamps to 1.
	img2 := image.NewRGBA(image.Rect(0, 0, 8, 8))
	drawDigits(img2, 0, 0, 8, 0, white)
	for x := 0; x < 3; x++ {
		if !sameColor(img2.At(x, 0), white) { // '8' top row is full
			t.Errorf("'8' top row pixel %d missing at scale 0", x)
		}
	}
}

func sameColor(a, b color.Color) bool {
	ar, ag, ab, aa := a.RGBA()
	br, bg, bb, ba := b.RGBA()
	return ar == br && ag == bg && ab == bb && aa == ba
}

// ---------------------------------------------------------------------------
// Pixel classification for the ffmpeg-gated tests (JPEG tolerance)

func classify(c color.Color) (white, amber, cyan, magenta bool) {
	r, g, b, _ := c.RGBA()
	r8, g8, b8 := uint8(r>>8), uint8(g>>8), uint8(b>>8)
	white = r8 > 190 && g8 > 190 && b8 > 190
	amber = r8 > 160 && g8 > 90 && b8 < 110 && int(r8)-int(b8) > 60
	cyan = b8 > 150 && int(b8)-int(r8) > 60 && g8 > 90
	magenta = r8 > 150 && int(r8)-int(b8) > 40 && g8 < 110 && b8 > 50
	return
}

func countClassified(img image.Image, r image.Rectangle, want func(color.Color) bool) int {
	n := 0
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			if want(img.At(x, y)) {
				n++
			}
		}
	}
	return n
}

func decodeSheet(t *testing.T, jpegBytes []byte) image.Image {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(jpegBytes))
	if err != nil {
		t.Fatalf("decode sheet JPEG: %v", err)
	}
	return img
}

// ---------------------------------------------------------------------------
// Snapshot against the real fixture (ffmpeg-gated)

// The snapshot IS the shared render pipeline for one frame (MJPEG sink at
// mjpegQ, full output resolution). It carries no baked annotation boxes -
// the UI draws its own overlays - so the fixture test pins the delivery
// shape and the subject content, not box strokes.
func TestSnapshotFixture(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	_, p := setupRenderFixture(t, ffmpeg, 240, 240)

	// Frame 48 (1-based): EMA has long converged, the box is mid-walk.
	snap, err := p.Snapshot(48, 0)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Frame != 48 {
		t.Errorf("Snapshot.Frame = %d, want 48", snap.Frame)
	}
	if snap.Width != 240 || snap.Height != 240 {
		t.Fatalf("snapshot %dx%d, want full output 240x240", snap.Width, snap.Height)
	}

	img := decodeSheet(t, snap.JPEG)
	if b := img.Bounds(); b.Dx() != 240 || b.Dy() != 240 {
		t.Fatalf("encoded JPEG is %v, want 240x240", b)
	}

	white := countClassified(img, img.Bounds(), func(c color.Color) bool { w, _, _, _ := classify(c); return w })
	if white < 1000 {
		t.Errorf("white subject pixels = %d, want > 1000 (the crop must contain the white box scaled to output size)", white)
	}
}

// The live frame strip (Horidashimi item 743) polls Snapshot while a pass is
// running. Snapshot seeks the SOURCE; the package's frame-seeking law
// (seekAnalysisFrame, the half-early -ss) must land on the exact frame the
// select-filter pass over the ANALYSIS video returns - boundaries included.
// Shared law, so the analysis-video parity stands in for the source decode
// here; TestStillsSourceSeekFrameExact pins the source side directly.
func TestSnapshotSeekDecodeMatchesSelect(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	_, p := setupRenderFixture(t, ffmpeg, 240, 240)
	if p.analysisFPS <= 0 {
		t.Fatalf("fixture plan carries no analysis fps")
	}
	n := len(p.records)
	for _, idx := range []int{0, 1, 2, n / 3, n / 2, n - 2, n - 1} {
		seek, err := seekAnalysisFrame(p.videoPath, idx, p.resolved.AnalysisW, p.resolved.AnalysisH, p.analysisFPS)
		if err != nil {
			t.Fatalf("seek decode index %d: %v", idx, err)
		}
		sel, err := p.decodeAnalysisFrames([]int{idx})
		if err != nil {
			t.Fatalf("select decode index %d: %v", idx, err)
		}
		if !bytes.Equal(seek.Pix, sel[0].Pix) {
			t.Errorf("index %d: seek decode is not the same frame as the select decode", idx)
		}
	}
}

// The -ss the still decode relies on must be frame-exact on the SOURCE file
// itself: the raw rgb24 bytes of the seeked decode must equal frame idx of a
// decode-from-start (the select-filter reference the contact sheet uses).
func TestStillsSourceSeekFrameExact(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	_, p := setupRenderFixture(t, ffmpeg, 240, 240)
	src, err := p.sourceInputPath()
	if err != nil {
		t.Fatal(err)
	}
	sw, sh := p.resolved.SourceW, p.resolved.SourceH
	fps := p.analysisFPS
	if fps <= 0 {
		t.Fatalf("fixture plan carries no analysis fps")
	}
	n := len(p.records)
	for _, idx := range []int{0, 1, n / 2, n - 2, n - 1} {
		seek, err := rawFrameSeeked(ffmpeg, src, stillSeekSeconds(idx, fps), sw, sh)
		if err != nil {
			t.Fatalf("seeked source decode index %d: %v", idx, err)
		}
		ref, err := rawFrameSelected(ffmpeg, src, idx, sw, sh)
		if err != nil {
			t.Fatalf("select source decode index %d: %v", idx, err)
		}
		if !bytes.Equal(seek, ref) {
			t.Errorf("index %d: the -ss decode is not the same source frame as the decode-from-start", idx)
		}
	}
}

func TestSnapshotMaxWidthCap(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	_, p := setupRenderFixture(t, ffmpeg, 240, 240)

	cases := []struct {
		max, want int
	}{{120, 120}, {239, 239}, {300, 240}} // 0-cap and oversized caps keep full res
	for _, c := range cases {
		snap, err := p.Snapshot(1, c.max)
		if err != nil {
			t.Fatal(err)
		}
		if snap.Width != c.want || snap.Height != c.want {
			t.Errorf("cap %d -> %dx%d, want %dx%d", c.max, snap.Width, snap.Height, c.want, c.want)
		}
	}
}

func TestSnapshotOutOfRange(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	_, p := setupRenderFixture(t, ffmpeg, 240, 240)

	for _, frame := range []int{0, -5, len(p.records) + 1} {
		if _, err := p.Snapshot(frame, 0); err == nil || !strings.Contains(err.Error(), "out of range") {
			t.Errorf("Snapshot(%d) error = %v, want an out-of-range error", frame, err)
		}
	}
}

func TestStillsRequireCachedVideo(t *testing.T) {
	// A Plan replayed from a synthetic stream has no analysis video.
	s := newTestStream(walkingSamples(30, 100, 4, 120, 200), 428, 240, fixtureW, fixtureH)
	p, err := NewPlan(s, Settings{Width: 240, Height: 240, FPS: fixtureFPS})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Snapshot(1, 0); !errors.Is(err, errStillsNoVideo) {
		t.Errorf("Snapshot error = %v, want errStillsNoVideo", err)
	}
	if _, err := p.ContactSheet(0); !errors.Is(err, errStillsNoVideo) {
		t.Errorf("ContactSheet error = %v, want errStillsNoVideo", err)
	}
}

// ---------------------------------------------------------------------------
// ContactSheet against the real fixture (ffmpeg-gated)

func TestContactSheetFixture(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	_, p := setupRenderFixture(t, ffmpeg, 240, 240)

	sheet, err := p.ContactSheet(640)
	if err != nil {
		t.Fatal(err)
	}
	if sheet.Columns != 6 || sheet.TileWidth != 102 || sheet.TileHeight != 102 {
		t.Fatalf("grid %d cols %dx%d tiles, want 6 cols 102x102", sheet.Columns, sheet.TileWidth, sheet.TileHeight)
	}
	if sheet.Width != 640 || sheet.Height != 742 {
		t.Errorf("sheet %dx%d, want 640x742", sheet.Width, sheet.Height)
	}
	if len(sheet.TileFrames) != 32 {
		t.Fatalf("%d tiles, want 32", len(sheet.TileFrames))
	}
	if sheet.TileFrames[0] != 1 || sheet.TileFrames[1] != 4 || sheet.TileFrames[30] != 91 || sheet.TileFrames[31] != 96 {
		t.Errorf("TileFrames = %v, want stride 3 from frame 1 ending on 96", sheet.TileFrames)
	}
	if len(sheet.JPEG) < 10<<10 || len(sheet.JPEG) >= contactSheetSheetBytes {
		t.Errorf("sheet JPEG %d bytes, want 10 KB..4 MB", len(sheet.JPEG))
	}

	img := decodeSheet(t, sheet.JPEG)
	if b := img.Bounds(); b.Dx() != sheet.Width || b.Dy() != sheet.Height {
		t.Fatalf("encoded sheet is %v, metadata says %dx%d", b, sheet.Width, sheet.Height)
	}

	labelH := 17 // (102/6), as pinned by the layout test above
	for i, frame := range sheet.TileFrames {
		col, row := i%sheet.Columns, i/sheet.Columns
		x := contactSheetGap + col*(sheet.TileWidth+contactSheetGap)
		y := contactSheetGap + row*(sheet.TileHeight+labelH+contactSheetGap)

		// Status bar: the fixture has no dropouts, scene cuts or walk
		// frames (portrait output), so every bar is the neutral gray.
		r, g, b, _ := img.At(x+sheet.TileWidth/2, y+1).RGBA()
		r8, g8, b8 := uint8(r>>8), uint8(g>>8), uint8(b>>8)
		if r8 < 35 || r8 > 125 || g8 < 35 || g8 > 125 || b8 < 35 || b8 > 125 {
			t.Errorf("tile %d (frame %d) bar RGB %d,%d,%d, want near-neutral 0x48", i, frame, r8, g8, b8)
		}

		// Content: a tile whose crop actually contains the subject must
		// show the white box. Frame 1's crop is the EMA warm-up position
		// (CLI-parity) with the subject clipped, so coverage gates the
		// assertion. The fixture's tiny subject (53 analysis px tall)
		// zoom-caps the crop at ~400 px: in a 102 px tile the box is
		// ~26x13 - ~250 white px pre-JPEG.
		rec := p.records[frame-1]
		crop := p.cropRectAt(frame - 1)
		box := image.Rect(int(rec.BBox.X), int(rec.BBox.Y),
			int(rec.BBox.X+rec.BBox.W), int(rec.BBox.Y+rec.BBox.H))
		coverage := 0.0
		if area := box.Dx() * box.Dy(); area > 0 {
			coverage = float64(crop.Intersect(box).Dx()*crop.Intersect(box).Dy()) / float64(area)
		}
		if coverage < 0.75 {
			continue
		}
		white := countClassified(img,
			image.Rect(x, y+contactSheetMarkerH, x+sheet.TileWidth, y+sheet.TileHeight),
			func(c color.Color) bool { w, _, _, _ := classify(c); return w })
		if white < 100 {
			t.Errorf("tile %d (frame %d, crop coverage %.0f%%): white subject pixels = %d, want > 100", i, frame, coverage*100, white)
		}
	}
}

func TestContactSheetCapsAndFloor(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	_, p := setupRenderFixture(t, ffmpeg, 240, 240)

	full, err := p.ContactSheet(0) // default 1280
	if err != nil {
		t.Fatal(err)
	}
	if full.TileWidth != 208 || full.Width != 1276 {
		t.Errorf("default sheet: tile %d width %d, want 208 / 1276", full.TileWidth, full.Width)
	}
	if full.Width > contactSheetDefaultW {
		t.Errorf("default sheet width %d exceeds the %d default", full.Width, contactSheetDefaultW)
	}

	narrow, err := p.ContactSheet(200)
	if err != nil {
		t.Fatal(err)
	}
	if narrow.TileWidth != contactSheetMinTileW {
		t.Errorf("capped tile width %d, want the %d floor", narrow.TileWidth, contactSheetMinTileW)
	}
	if len(narrow.JPEG) >= contactSheetSheetBytes {
		t.Errorf("capped sheet %d bytes exceeds budget", len(narrow.JPEG))
	}
}
