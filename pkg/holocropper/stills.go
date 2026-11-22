package holocropper

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"io"
	"math"
	"os/exec"
	"strconv"
	"strings"
)

/* Stills and contact sheets (HOLOCROPPER-API-PLAN.md story B3).

No Task, no scratch files: both composite in memory straight from the
cached analysis video plus the Plan's records - the same crop rect and
fill-resize the render passes execute (cropToBBoxCalc + fillResizeTo),
with the same output-space overlay boxes the preview videos draw. A
Snapshot is the annotated single frame a scrubber popover opens; a
ContactSheet is the evenly-sampled tile strip under the timeline.

One ffmpeg pipe pass (select filter, rawvideo) fetches every wanted frame,
so a contact sheet costs one decode pass over the analysis video, not one
per tile. */

// contactSheet* fix the tile-strip shape: at most this many tiles, up to
// contactSheetCols per row, tiles with a caption strip for the frame
// number and a status bar coloured by the record (dropout / scene cut /
// walk active).
const (
	contactSheetMaxTiles   = 36
	contactSheetCols       = 6
	contactSheetDefaultW   = 1280 // sheet width when maxWidth is 0
	contactSheetMinTileW   = 80
	contactSheetGap        = 4  // px between tiles
	contactSheetMarkerH    = 3  // status bar height on top of each tile
	contactSheetJPEGQual   = 85 // sheets are a scrubber aid, not print quality
	snapshotJPEGQuality    = 90
	contactSheetSheetBytes = 4 << 20 // the plan's ~4 MB sheet budget (B3)

	// contactSheetCaptionMaxDigits sizes the caption scale for the longest
	// frame number a sheet is designed to caption (videos up to 99 999 frames).
	contactSheetCaptionMaxDigits = 5
)

// ---------------------------------------------------------------------------
// Snapshot

// Snapshot is one annotated output-resolution frame, in memory: the frame
// exactly as the render would show it, with this record's subject, EMA and
// crop boxes composited from its Out geometry. JPEG bytes cross the
// bindings straight to an <img>.
type Snapshot struct {
	Frame  int    `json:"frame"` // 1-based, the same numbering as FrameRecord.Frame
	Width  int    `json:"width"`
	Height int    `json:"height"`
	JPEG   []byte `json:"jpeg"`
}

// Snapshot composites output-space frame `frame` (1-based). maxWidth > 0
// caps the returned width (height follows the output aspect); 0 returns
// the full output resolution. The annotation is always drawn at full
// output resolution first, then scaled, so a capped snapshot's boxes are
// exactly the full-res framing shrunk, never re-derived.
func (p *Plan) Snapshot(frame, maxWidth int) (*Snapshot, error) {
	if frame < 1 || frame > len(p.records) {
		return nil, fmt.Errorf("holocropper: snapshot frame %d out of range 1..%d", frame, len(p.records))
	}
	if p.videoPath == "" {
		return nil, errStillsNoVideo
	}

	frameRGBA, err := p.decodeAnalysisFrame(frame - 1)
	if err != nil {
		return nil, err
	}

	full := p.composeFrame(frame-1, frameRGBA, p.resolved.OutputW, p.resolved.OutputH)
	w, h := full.Bounds().Dx(), full.Bounds().Dy()
	if tw, th := stillsCapSize(w, h, maxWidth); tw != w || th != h {
		full = fillResizeTo(full, tw, th)
		w, h = tw, th
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, full, &jpeg.Options{Quality: snapshotJPEGQuality}); err != nil {
		return nil, err
	}
	return &Snapshot{Frame: frame, Width: w, Height: h, JPEG: buf.Bytes()}, nil
}

// ---------------------------------------------------------------------------
// ContactSheet

// ContactSheet is the scrubber strip: evenly sampled annotated tiles,
// frame numbers captioned under each, and a status bar per tile coloured
// by what happened in that frame (dropout red, scene cut amber, walk
// active green). TileFrames names exactly which frames are shown, so a UI
// can map a tile click to a player seek.
type ContactSheet struct {
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	Columns    int    `json:"columns"`
	TileWidth  int    `json:"tile_width"`
	TileHeight int    `json:"tile_height"`
	TileFrames []int  `json:"tile_frames"` // 1-based, ascending
	JPEG       []byte `json:"jpeg"`
}

// ContactSheet builds the tile strip. maxWidth > 0 caps the total sheet
// width (0 = contactSheetDefaultW); tiles shrink to fit, floor
// contactSheetMinTileW. At most contactSheetMaxTiles tiles, always
// including the first and last frame of the Plan.
func (p *Plan) ContactSheet(maxWidth int) (*ContactSheet, error) {
	if len(p.records) == 0 {
		return nil, fmt.Errorf("holocropper: contact sheet needs a plan with frames")
	}
	if p.videoPath == "" {
		return nil, errStillsNoVideo
	}

	indices, cols, tileW, tileH, labelH := contactSheetLayout(len(p.records), p.resolved.OutputW, p.resolved.OutputH, maxWidth)

	frames, err := p.decodeAnalysisFrames(indices)
	if err != nil {
		return nil, err
	}
	if len(frames) != len(indices) {
		return nil, fmt.Errorf("holocropper: analysis video returned %d frames, sheet asked for %d", len(frames), len(indices))
	}

	rows := (len(indices) + cols - 1) / cols
	sheetW := cols*tileW + contactSheetGap*(cols+1)
	sheetH := rows*(tileH+labelH) + contactSheetGap*(rows+1)
	sheet := image.NewRGBA(image.Rect(0, 0, sheetW, sheetH))
	draw.Draw(sheet, sheet.Bounds(), &image.Uniform{color.RGBA{R: 0x18, G: 0x18, B: 0x18, A: 0xFF}}, image.ZP, draw.Src)

	tileFrames := make([]int, 0, len(indices))
	for i, idx := range indices {
		rec := p.records[idx]
		tileFrames = append(tileFrames, idx+1)

		col := i % cols
		row := i / cols
		x := contactSheetGap + col*(tileW+contactSheetGap)
		y := contactSheetGap + row*(tileH+labelH+contactSheetGap)

		tile := p.composeFrame(idx, frames[i], tileW, tileH)
		draw.Draw(sheet, image.Rect(x, y, x+tileW, y+tileH), tile, tile.Bounds().Min, draw.Src)

		// Status bar: what the timeline needs to show at a glance.
		bar := contactSheetBarColor(rec)
		fillRect(sheet, image.Rect(x, y, x+tileW, y+contactSheetMarkerH), bar)

		// Frame number caption (dropout frames captioned in the dropout red).
		label := color.RGBA{R: 0xE8, G: 0xE8, B: 0xE8, A: 0xFF}
		if rec.SubjectLost {
			label = overlayDropout
		}
		// Caption digits: scaled to the label strip height, capped so a
		// max-width frame number still fits inside the tile.
		scale := max(1, (labelH-contactSheetMarkerH-2)/digitGlyphH)
		if fit := tileW / (contactSheetCaptionMaxDigits * (digitGlyphW + 1)); fit > 0 && scale > fit {
			scale = fit
		}
		drawDigits(sheet, x+2, y+tileH+2, idx+1, scale, label)
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, sheet, &jpeg.Options{Quality: contactSheetJPEGQual}); err != nil {
		return nil, err
	}
	return &ContactSheet{
		Width: sheetW, Height: sheetH, Columns: cols,
		TileWidth: tileW, TileHeight: tileH,
		TileFrames: tileFrames, JPEG: buf.Bytes(),
	}, nil
}

// ---------------------------------------------------------------------------
// Compositing internals

var errStillsNoVideo = errors.New("holocropper: this Plan is not tied to a cached analysis video (replayed from a synthetic stream): re-plan from an Analyze result for stills")

// composeFrame cuts idx's crop rect out of an analysis frame, fill-resizes
// it to tw x th and draws that record's overlay boxes - identical geometry
// to the render passes, at the requested size. Fit-mode plans instead
// fit-resize their content crop (bars cut, §6.10 amendment) onto bars,
// matching their render passes.
func (p *Plan) composeFrame(idx int, analysisFrame *image.RGBA, tw, th int) *image.RGBA {
	rec := p.records[idx]
	var out *image.RGBA
	if p.settings.FitWithBars {
		if cr := p.cropRectAt(idx); cr != analysisFrame.Bounds() {
			out = fitResizeTo(cropToBBoxCalc(analysisFrame, cr), tw, th)
		} else {
			out = fitResizeTo(analysisFrame, tw, th)
		}
	} else {
		cropped := cropToBBoxCalc(analysisFrame, p.cropRectAt(idx))
		out = fillResizeTo(cropped, tw, th)
	}
	drawOverlayBoxes(out, scaleRecordToFrame(rec, p.resolved.OutputW, p.resolved.OutputH, tw, th),
		PreviewOptions{DrawSubject: true, DrawCrop: true}, defaultOverlayStyle())
	return out
}

// scaleRecordToFrame maps a record's output-space boxes to another frame
// size (the contact-sheet tiles). Positions and extents are scaled from the
// full-resolution framing, never re-derived, so a tile shows exactly the
// geometry its full-resolution snapshot would, shrunk.
func scaleRecordToFrame(rec FrameRecord, w, h, tw, th int) FrameRecord {
	if w == tw && h == th || w <= 0 || h <= 0 {
		return rec
	}
	fx := float64(tw) / float64(w)
	fy := float64(th) / float64(h)
	rec.Out.BBox = scaleRect(rec.Out.BBox, fx, fy)
	rec.Out.EMA = scaleRect(rec.Out.EMA, fx, fy)
	return rec
}

func scaleRect(r Rect, fx, fy float64) Rect {
	return Rect{X: r.X * fx, Y: r.Y * fy, W: r.W * fx, H: r.H * fy}
}

// decodeAnalysisFrame fetches exactly one analysis frame with the same
// one-pass select machinery the sheet uses.
// decodeAnalysisFrame fetches one analysis frame. A Plan whose stream locked
// an fps gets the time-seeked decode (decodeAnalysisFrameSeek); a Plan from
// a stream that never locked one keeps the select-filter fetch.
func (p *Plan) decodeAnalysisFrame(idx int) (*image.RGBA, error) {
	if idx < 0 || idx >= len(p.records) {
		return nil, fmt.Errorf("holocropper: analysis video has %d frames, still asks for index %d", len(p.records), idx)
	}
	if p.analysisFPS > 0 {
		return p.decodeAnalysisFrameSeek(idx)
	}
	frames, err := p.decodeAnalysisFrames([]int{idx})
	if err != nil {
		return nil, err
	}
	return frames[0], nil
}

// decodeAnalysisFrameSeek fetches frame idx with an ffmpeg INPUT seek instead
// of a select filter. Accurate input seek decodes from the keyframe before
// the target and discards frames whose pts lands before the seek point, so
// the pipe work is bounded to roughly one GOP ahead of the target instead of
// a full linear pass over the whole analysis video - the cost a live
// frame-strip poll pays several times a second must not scale with video
// length.
//
// The seek sits half a frame EARLY: ffmpeg hands out the first frame with
// pts >= ss, so half a frame below the target's own timestamp makes the
// target frame the first survivor under float rounding (the analysis encode
// is frame-passthrough, so frame idx sits at idx/analysisFPS — the engine's
// frame-grid law, the same CFR assumption the framing decider steps on).
func (p *Plan) decodeAnalysisFrameSeek(idx int) (*image.RGBA, error) {
	if p.resolved.AnalysisW <= 0 || p.resolved.AnalysisH <= 0 {
		return nil, fmt.Errorf("holocropper: plan has no analysis geometry for stills")
	}
	return seekAnalysisFrame(p.videoPath, idx, p.resolved.AnalysisW, p.resolved.AnalysisH, p.analysisFPS)
}

// seekAnalysisFrame fetches analysis frame idx (0-based) out of `path` with an
// ffmpeg INPUT seek. This is the frame-seeking law the whole package shares -
// stills, contact sheets and the lazy subject-detail verbs all read one frame
// through it.
func seekAnalysisFrame(path string, idx, w, h int, fps float64) (*image.RGBA, error) {
	if idx < 0 {
		return nil, fmt.Errorf("holocropper: analysis frame index %d is negative", idx)
	}
	ss := (float64(idx) - 0.5) / fps
	if fps <= 0 || ss < 0 {
		ss = 0
	}

	cmd := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-ss", strconv.FormatFloat(ss, 'f', 6, 64),
		"-i", path,
		"-map", "0:v:0",
		"-frames:v", "1",
		"-f", "image2pipe",
		"-pix_fmt", "rgba",
		"-vcodec", "rawvideo",
		"-fps_mode", "passthrough",
		"-",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("holocropper: seek analysis frame: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("holocropper: seek analysis frame: %w", err)
	}

	frame := image.NewRGBA(image.Rect(0, 0, w, h))
	if _, err := io.ReadFull(stdout, frame.Pix); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return nil, fmt.Errorf("holocropper: seek analysis frame %d: ffmpeg returned no frame: %v: %s",
			idx+1, err, strings.TrimSpace(stderr.String()))
	}
	if err := stdout.Close(); err != nil {
		return nil, fmt.Errorf("holocropper: seek analysis frame: close pipe: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("holocropper: seek analysis frame: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return frame, nil
}

func (p *Plan) decodeAnalysisFrames(indices []int) ([]*image.RGBA, error) {
	if p.resolved.AnalysisW <= 0 || p.resolved.AnalysisH <= 0 {
		return nil, fmt.Errorf("holocropper: plan has no analysis geometry for stills")
	}
	for _, idx := range indices {
		if idx < 0 || idx >= len(p.records) {
			return nil, fmt.Errorf("holocropper: analysis video has %d frames, still asks for index %d", len(p.records), idx)
		}
	}

	// One ffmpeg pass selects every wanted frame (n counts decoded frames,
	// matching our frame numbering because the analysis encode is frame-
	// passthrough). -fps_mode passthrough on the output pipe keeps ffmpeg
	// from dropping or duplicating the selected frames (it replaced the
	// removed -vsync option in ffmpeg 8+).
	expr := strings.Builder{}
	expr.WriteString("select='")
	for i, idx := range indices {
		if i > 0 {
			expr.WriteRune('+')
		}
		fmt.Fprintf(&expr, "eq(n\\,%d)", idx)
	}
	expr.WriteRune('\'')

	cmd := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-i", p.videoPath,
		"-f", "image2pipe",
		"-pix_fmt", "rgba",
		"-vcodec", "rawvideo",
		"-map", "0:v:0",
		"-vf", expr.String(),
		"-fps_mode", "passthrough",
		"-",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("holocropper: seek analysis frames: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("holocropper: seek analysis frames: %w", err)
	}

	w, h := p.resolved.AnalysisW, p.resolved.AnalysisH
	frames := make([]*image.RGBA, len(indices))
	for i := range frames {
		frames[i] = image.NewRGBA(image.Rect(0, 0, w, h))
		if _, err := io.ReadFull(stdout, frames[i].Pix); err != nil {
			cmd.Process.Kill()
			cmd.Wait()
			return nil, fmt.Errorf("holocropper: seek analysis frames: ffmpeg returned fewer than %d frames: %v: %s",
				len(indices), err, strings.TrimSpace(stderr.String()))
		}
	}
	if err := stdout.Close(); err != nil {
		return nil, fmt.Errorf("holocropper: seek analysis frames: close pipe: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("holocropper: seek analysis frames: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return frames, nil
}

// stillsCapSize applies the maxWidth cap: 0 (or a cap at/above the natural
// size) keeps the full resolution; a smaller cap scales down keeping the
// output aspect, even dimensions, floor 2.
func stillsCapSize(w, h, maxWidth int) (int, int) {
	if maxWidth <= 0 || maxWidth >= w {
		return w, h
	}
	if maxWidth < 2 {
		maxWidth = 2
	}
	th := int(math.Round(float64(maxWidth) * float64(h) / float64(w)))
	if th < 2 {
		th = 2
	}
	return maxWidth, th
}

// contactSheetLayout decides the tile sampling and geometry. Frames are
// sampled with a constant stride (first frame always included, the last
// sample lands on the last frame), so TileFrames is a regular scrubber.
func contactSheetLayout(n, outW, outH, maxWidth int) (indices []int, cols, tileW, tileH, labelH int) {
	step := (n + contactSheetMaxTiles - 1) / contactSheetMaxTiles
	count := (n + step - 1) / step
	indices = make([]int, count)
	for i := range indices {
		indices[i] = i * step
	}
	if indices[count-1] != n-1 {
		indices[count-1] = n - 1
	}

	cols = min(contactSheetCols, count)
	used := maxWidth
	if used <= 0 {
		used = contactSheetDefaultW
	}
	usable := used - contactSheetGap*(cols+1)
	tileW = usable / cols
	if tileW < contactSheetMinTileW {
		tileW = contactSheetMinTileW
	}
	tileH = int(math.Round(float64(tileW) * float64(outH) / float64(outW)))
	if tileH < 1 {
		tileH = 1
	}
	labelH = max(12, tileH/6)
	return indices, cols, tileW, tileH, labelH
}

// Status bar colours: dropout dominates (a red tile anywhere in the strip
// is the scrubber's most important signal), then scene cuts, then frames
// where walk mode is actively leashing the subject.
var (
	overlayDropout  = color.RGBA{R: 0xFF, G: 0x3B, B: 0x30, A: 0xFF}
	overlaySceneCut = color.RGBA{R: 0xFF, G: 0x95, B: 0x00, A: 0xFF}
	overlayWalkOn   = color.RGBA{R: 0x34, G: 0xC7, B: 0x59, A: 0xFF}
	overlayNeutral  = color.RGBA{R: 0x48, G: 0x48, B: 0x48, A: 0xFF}
)

func contactSheetBarColor(rec FrameRecord) color.RGBA {
	switch {
	case rec.SubjectLost:
		return overlayDropout
	case rec.SceneCut:
		return overlaySceneCut
	case rec.WalkActive:
		return overlayWalkOn
	default:
		return overlayNeutral
	}
}

// ---------------------------------------------------------------------------
// Caption font: a fixed 3x5 pixel digit face, scaled by integer factors.
// No text layout beyond digits is needed (frame numbers only), and a bitmap
// font keeps stills deterministic - identical bytes for identical records.

const digitGlyphW, digitGlyphH = 3, 5

var digitGlyphs = [10][digitGlyphH]byte{
	{0b111, 0b101, 0b101, 0b101, 0b111}, // 0
	{0b010, 0b110, 0b010, 0b010, 0b111}, // 1
	{0b111, 0b001, 0b111, 0b100, 0b111}, // 2
	{0b111, 0b001, 0b111, 0b001, 0b111}, // 3
	{0b101, 0b101, 0b111, 0b001, 0b001}, // 4
	{0b111, 0b100, 0b111, 0b001, 0b111}, // 5
	{0b111, 0b100, 0b111, 0b101, 0b111}, // 6
	{0b111, 0b001, 0b001, 0b001, 0b001}, // 7
	{0b111, 0b101, 0b111, 0b101, 0b111}, // 8
	{0b111, 0b101, 0b111, 0b001, 0b111}, // 9
}

// drawDigits writes value at (x, y) with each glyph cell scaled by `scale`
// pixels. Returns the x just past the drawn number.
func drawDigits(dst *image.RGBA, x, y, value, scale int, c color.Color) int {
	if scale < 1 {
		scale = 1
	}
	for _, ch := range strconv.Itoa(value) {
		glyph := digitGlyphs[ch-'0']
		for row := 0; row < digitGlyphH; row++ {
			for col := 0; col < digitGlyphW; col++ {
				if glyph[row]&(1<<(digitGlyphW-1-col)) == 0 {
					continue
				}
				for dy := 0; dy < scale; dy++ {
					for dx := 0; dx < scale; dx++ {
						setPixel(dst, x+col*scale+dx, y+row*scale+dy, dst.Bounds(), c)
					}
				}
			}
		}
		x += (digitGlyphW + 1) * scale
	}
	return x
}

// fillRect paints a solid rectangle (status bars, captions backgrounds).
func fillRect(dst *image.RGBA, r image.Rectangle, c color.Color) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			dst.Set(x, y, c)
		}
	}
}
