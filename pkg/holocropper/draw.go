package holocropper

import (
	"image"
	"image/color"
	"strconv"
)

/* Framing overlay drawing (HOLOCROPPER-API-PLAN.md §6.5, Milestone G4): the
layers the app's canvas draws, baked into output-resolution frames for preview
videos and stills.

The layer law is the 761 contact-sheet review's (the owner judged it in
docs/current/VIDEO_SECTION_V2.md): bottom to top - island outlines, raw
pre-merge components, merged subject pools, the ghosted COMMANDED crop, the
commanded pan-target axis, then the framing boxes the previews always drew
(crop window, subject bbox, EMA), the centroid marker, and the readout chips
on top. Every layer is opt-in through PreviewOptions; DrawSubject/DrawCrop
keep their old default-on behaviour and every new layer defaults OFF, so an
overlay file written before G4 and one written after are byte-identical.

Colours are the judged sheet's (OriginalPane law): amber subject, cyan EMA,
magenta crop window (pre-G4), red centroid, white target-crop ghost, amber
dashed target axis, cyan pools, magenta accepted / violet rejected raw
components, the same colours as a haze for island outlines, dark chips with
light text.

Box geometry for the record layers arrives in OUTPUT pixels (FrameRecord.Out);
the detail layers (pools, components, islands) arrive in ANALYSIS pixels from
the G3 subject measurement and map through the frame's APPLIED crop rect -
the same mapping every Out space uses - so the baked preview shows exactly
what the app canvas shows. Boxes that fall partly outside the frame (landscape
stage padding, §12.8; an off-frame commanded crop, G2) are drawn CLIPPED to
the frame, never dropped: a subject hanging off the crop edge, or a clamp the
controller lost, is precisely the diagnostic an overlay exists to reveal.

Text is the same fixed 3x5 bitmap face the contact sheet captions use (digits
in stills.go, letters added here): deterministic bytes, no font dependency.
Semi-transparent layers integer-blend over the frame - identical bytes for
identical records. */

// overlayStyle fixes the layer colours and stroke weights. A dark underline
// under each solid box keeps the boxes legible over any content; the ghosted
// and hazy layers blend instead, because their point is to sit BACK.
type overlayStyle struct {
	Subject color.RGBA
	EMA     color.RGBA
	Crop    color.RGBA

	// G4 layers (the judged 761 set):
	Centroid       color.RGBA // subject centre-of-mass dot (red)
	CentroidLine   color.RGBA // full-height centroid line, blended
	TargetCrop     color.RGBA // ghosted commanded crop (white ghost)
	AxisTarget     color.RGBA // commanded pan-target axis, dashed, blended
	Pool           color.RGBA // merged subject pools (cyan)
	ComponentBig   color.RGBA // accepted raw components (magenta)
	ComponentSmall color.RGBA // rejected raw components (violet)
	IslandBig      color.RGBA // accepted island outlines (magenta haze)
	IslandSmall    color.RGBA // rejected island outlines (violet haze)
	ChipBG         color.RGBA // readout chip plate
	ChipFG         color.RGBA // zoom-readout text
	LeashFG        color.RGBA // walk-leash chip text
	CutFG          color.RGBA // scene-cut chip text

	// StrokeFrac is the stroke width as a fraction of the frame's shorter
	// edge, floored at 1 px: 240 px previews get 1 px lines, 1080 px
	// outputs get ~4 px.
	StrokeFrac float64
	// DashPeriod/DashOn describe the EMA box's dash pattern in stroke widths.
	DashPeriod, DashOn int
	// AxisDashPeriod/AxisDashOn dash the target axis (the sheet's 10-on/8-off).
	AxisDashPeriod, AxisDashOn int
	// DotFrac sizes the centroid/pool dots; TextFrac sizes the chip face.
	DotFrac, TextFrac float64
}

func defaultOverlayStyle() overlayStyle {
	return overlayStyle{
		Subject: color.RGBA{R: 0xFF, G: 0xD4, B: 0x00, A: 0xFF}, // amber
		EMA:     color.RGBA{R: 0x00, G: 0xE5, B: 0xFF, A: 0xFF}, // cyan
		Crop:    color.RGBA{R: 0xFF, G: 0x2D, B: 0x78, A: 0xFF}, // magenta

		Centroid:       color.RGBA{R: 239, G: 68, B: 68, A: 0xFF},
		CentroidLine:   color.RGBA{R: 239, G: 68, B: 68, A: 0xBE},
		TargetCrop:     color.RGBA{R: 235, G: 240, B: 246, A: 0xD7},
		AxisTarget:     color.RGBA{R: 251, G: 146, B: 60, A: 0xA0},
		Pool:           color.RGBA{R: 34, G: 211, B: 238, A: 0xFF},
		ComponentBig:   color.RGBA{R: 232, G: 64, B: 200, A: 0xFF},
		ComponentSmall: color.RGBA{R: 167, G: 84, B: 226, A: 0xFF},
		IslandBig:      color.RGBA{R: 232, G: 64, B: 200, A: 0x96},
		IslandSmall:    color.RGBA{R: 167, G: 84, B: 226, A: 0x69},
		ChipBG:         color.RGBA{R: 10, G: 12, B: 14, A: 0xD2},
		ChipFG:         color.RGBA{R: 230, G: 235, B: 240, A: 0xFF},
		LeashFG:        color.RGBA{R: 140, G: 230, B: 255, A: 0xFF},
		CutFG:          color.RGBA{R: 255, G: 170, B: 160, A: 0xFF},

		StrokeFrac:     0.004,
		DashPeriod:     4,
		DashOn:         3,
		AxisDashPeriod: 18,
		AxisDashOn:     10,
		DotFrac:        0.011,
		TextFrac:       1.0 / 120.0,
	}
}

var overlayUnderline = color.RGBA{R: 0x10, G: 0x10, B: 0x10, A: 0xFF}
var overlayDotRing = color.RGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}

// overlayStrokeWidth scales the stroke with the frame, floored at 1 px.
func overlayStrokeWidth(w, h int, st overlayStyle) int {
	s := int(st.StrokeFrac*float64(min(w, h)) + 0.5)
	if s < 1 {
		s = 1
	}
	return s
}

// drawOverlayBoxes composites the RECORD-derived layers onto dst, an
// output-resolution frame, bottom to top: the ghosted commanded crop, the
// commanded pan-target axis, the crop window, the subject and EMA boxes, the
// centroid marker, the readout chips. opts selects which layers are drawn;
// the subject and EMA boxes are skipped when the subject was lost (their Out
// rects are empty), so a dropout frame shows only the crop window holding
// position. The detail layers (pools, components, islands) are drawn by
// drawSubjectLayers BEFORE this, under everything.
func drawOverlayBoxes(dst *image.RGBA, rec FrameRecord, opts PreviewOptions, st overlayStyle) {
	b := dst.Bounds()
	w, h := b.Dx(), b.Dy()
	if w < 2 || h < 2 {
		return
	}
	stroke := overlayStrokeWidth(w, h, st)

	// Ghosted commanded crop (G2 telemetry): the box the controllers ASKED
	// for, blended so it reads as a ghost behind the applied framing. Its
	// gap with the frame edge is what containment took away; a commanded box
	// that reaches outside the frame is drawn clipped to it, which is exactly
	// the |< >| off-screen story the app canvas tells.
	if opts.DrawCropTarget && rec.Out.CropTarget != (Rect{}) {
		if r := clampToFrame(rec.Out.CropTarget.toImage(), b); !r.Empty() {
			drawBlendBoxOutline(dst, r, st.TargetCrop, stroke)
		}
	}
	// Commanded pan-target axes (G2): the setpoints the axis PIDs were fed,
	// as an amber dashed cross. The CURRENT crop-centre axes are deliberately
	// not drawn: in output space the applied crop IS the frame, so a
	// crop-centre cross is always the frame centre and says nothing. Only the
	// target - where the controller wanted the centre - carries information.
	if opts.DrawAxes && rec.Out.PanTarget != (NormPoint{}) {
		if x := int(rec.Out.PanTarget.X + 0.5); x >= b.Min.X && x < b.Max.X {
			drawDashLine(dst, x, b.Min.Y, x, b.Max.Y-1, st.AxisTarget, stroke, st.AxisDashPeriod, st.AxisDashOn)
		}
		if y := int(rec.Out.PanTarget.Y + 0.5); y >= b.Min.Y && y < b.Max.Y {
			drawDashLine(dst, b.Min.X, y, b.Max.X-1, y, st.AxisTarget, stroke, st.AxisDashPeriod, st.AxisDashOn)
		}
	}

	if opts.DrawCrop {
		drawBoxOutline(dst, fullFrameBox(b), st.Crop, stroke, 0, 0)
	}
	if opts.DrawSubject && !rec.SubjectLost {
		if r := clampToFrame(rec.Out.BBox.toImage(), b); !r.Empty() {
			drawBoxOutline(dst, r, st.Subject, stroke, 0, 0)
		}
		if r := clampToFrame(rec.Out.EMA.toImage(), b); !r.Empty() {
			drawBoxOutline(dst, r, st.EMA, stroke, st.DashPeriod, st.DashOn)
		}
	}

	// Centroid: the subject mask's centre of mass (G1), a full-height blended
	// line plus a ringed dot - the sheet's red marker.
	if opts.DrawCentroid && rec.CentroidKnown {
		cx := int(rec.Out.Centroid.X + 0.5)
		cy := int(rec.Out.Centroid.Y + 0.5)
		if cx >= b.Min.X && cx < b.Max.X {
			for y := b.Min.Y; y < b.Max.Y; y++ {
				blendPixel(dst, cx, y, b, st.CentroidLine)
			}
		}
		dot := int(st.DotFrac*float64(min(w, h)) + 0.5)
		drawDot(dst, cx, cy, max(2, dot), st.Centroid, overlayDotRing)
	}

	// Readout chips, the UI layer on top: zoom current -> target, plus the
	// state chips the judged sheet carries. Zoom 0 means "no reading" (fit
	// mode), which draws no chip.
	if opts.DrawReadouts {
		scale := int(st.TextFrac*float64(min(w, h)) + 0.5)
		if scale < 1 {
			scale = 1
		}
		pad := 2 * scale
		x := pad
		if rec.ZoomCurrent > 0 && rec.ZoomTarget > 0 {
			text := "zoom " + fixed2(rec.ZoomCurrent) + "x -> " + fixed2(rec.ZoomTarget) + "x"
			x = drawChip(dst, pad, pad, text, st.ChipFG, st.ChipBG, scale) + 2*scale
		}
		if rec.WalkActive {
			drawChip(dst, x, pad, "walk leash", st.LeashFG, st.ChipBG, scale)
		}
		if rec.SceneCut {
			chipH := digitGlyphH*scale + 4*scale
			drawChip(dst, pad, pad+chipH+2*scale, "scene cut · tracking reset", st.CutFG, st.ChipBG, scale)
		}
	}
}

// drawSubjectLayers composites the G3 DETAIL layers onto dst, analysis-space
// boxes mapped through this frame's applied crop: island outlines as the
// bottom layer, raw pre-merge components next (magenta accepted, violet
// rejected), merged subject pools on top (cyan boxes with their cyan centroid
// dots) - the colour-coding the 761 review chose for SubjectBoxes' two layers.
func drawSubjectLayers(dst *image.RGBA, rec FrameRecord, opts PreviewOptions, st overlayStyle, det subjectDetail) {
	b := dst.Bounds()
	w, h := b.Dx(), b.Dy()
	if w < 2 || h < 2 || rec.Crop.W <= 0 || rec.Crop.H <= 0 {
		return
	}
	if len(det.Components) == 0 && len(det.Pools) == 0 {
		return
	}
	if !opts.DrawIslands && !opts.DrawSubjectBoxes {
		return
	}
	stroke := overlayStrokeWidth(w, h, st)

	// Island outlines (bottom, opt-in separately from the boxes): the pixel
	// haze the sheet judged - accepted islands magenta, rejected violet.
	if opts.DrawIslands {
		for _, isl := range subjectIslands(det) {
			c := st.IslandSmall
			if isl.Accepted {
				c = st.IslandBig
			}
			for _, run := range isl.Outline {
				x0 := int((float64(run.X0)-rec.Crop.X)*(float64(w)/rec.Crop.W) + 0.5)
				x1 := int((float64(run.X1+1)-rec.Crop.X)*(float64(w)/rec.Crop.W) + 0.5)
				y := int((float64(run.Y)+0.5-rec.Crop.Y)*(float64(h)/rec.Crop.H) + 0.5)
				for x := x0; x < x1; x++ {
					blendPixel(dst, x, y, b, c)
				}
			}
		}
	}
	// Raw pre-merge component boxes: magenta where the 20% filter accepted
	// the island, violet where it did not. The pools draw AFTER and cover the
	// raw outline wherever the merged box shares an edge with it - the sheet's
	// Z-order, not a bug.
	if opts.DrawSubjectBoxes {
		for _, c := range det.Components {
			col := st.ComponentSmall
			if c.Accepted {
				col = st.ComponentBig
			}
			box := clampToFrame(mapRectToOutput(rectOf(c.Rect), rec.Crop, w, h).toImage(), b)
			if !box.Empty() {
				drawBoxOutline(dst, box, col, stroke, 0, 0)
			}
		}
		// Merged pools: the boxes the framing controller actually consumed.
		dot := max(2, int(st.DotFrac*float64(min(w, h))+0.5))
		for _, p := range det.Pools {
			box := clampToFrame(mapRectToOutput(rectOf(p.Rect), rec.Crop, w, h).toImage(), b)
			if !box.Empty() {
				drawBoxOutline(dst, box, st.Pool, stroke, 0, 0)
			}
			if cp := mapXYToOutput(float64(p.Centroid.X), float64(p.Centroid.Y), rec.Crop, w, h); cp != (NormPoint{}) {
				drawDot(dst, int(cp.X+0.5), int(cp.Y+0.5), dot, st.Pool, color.RGBA{})
			}
		}
	}
}

// fullFrameBox is the crop window in output space: the crop fills the output
// exactly, so its window is the frame border (inset by one stroke so the
// line is actually visible).
func fullFrameBox(b image.Rectangle) image.Rectangle {
	return image.Rect(b.Min.X, b.Min.Y, b.Max.X, b.Max.Y)
}

// clampToFrame clips a box to the drawable frame. A box entirely outside the
// frame comes back empty and is not drawn.
func clampToFrame(r image.Rectangle, b image.Rectangle) image.Rectangle {
	if r.Empty() {
		return image.ZR
	}
	out := image.Rect(
		max(r.Min.X, b.Min.X),
		max(r.Min.Y, b.Min.Y),
		min(r.Max.X, b.Max.X),
		min(r.Max.Y, b.Max.Y),
	)
	if out.Max.X <= out.Min.X || out.Max.Y <= out.Min.Y {
		return image.ZR
	}
	return out
}

// drawBoxOutline strokes a rectangle outline thickness px wide, centred on
// the rect edges. dashPeriod > 0 dashes the horizontal edges (the EMA box:
// visually distinct from the solid subject box without hiding its extent).
// Every stroke is underlined in the dark key colour for contrast.
func drawBoxOutline(dst *image.RGBA, r image.Rectangle, c color.Color, thickness, dashPeriod, dashOn int) {
	if thickness < 1 {
		thickness = 1
	}
	t0 := thickness / 2
	drawBoxLine(dst, r, overlayUnderline, thickness+t0*2, dashPeriod, dashOn)
	drawBoxLine(dst, r, c, thickness, dashPeriod, dashOn)
}

func drawBoxLine(dst *image.RGBA, r image.Rectangle, c color.Color, thickness, dashPeriod, dashOn int) {
	b := dst.Bounds()
	half := thickness / 2
	stroke := thickness

	// Horizontal edges: x runs the full width of the box, y centred on each
	// edge. Dash pattern applies to the horizontal edges only.
	for _, y := range []int{r.Min.Y, r.Max.Y - 1} {
		x := r.Min.X
		seg := 0
		for ; x < r.Max.X; x++ {
			dashed := dashPeriod > 0 && seg%dashPeriod >= dashOn
			if !dashed {
				for dy := -half; dy < -half+stroke; dy++ {
					setPixel(dst, x, y+dy, b, c)
				}
			}
			seg++
		}
	}
	// Vertical edges, drawn solid so the box corners always read as one box.
	for _, x := range []int{r.Min.X, r.Max.X - 1} {
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for dx := -half; dx < -half+stroke; dx++ {
				setPixel(dst, x+dx, y, b, c)
			}
		}
	}
}

// drawBlendBoxOutline strokes a rectangle with a blended (semi-transparent)
// colour, no dark underline: a GHOST, not a solid box.
func drawBlendBoxOutline(dst *image.RGBA, r image.Rectangle, c color.RGBA, thickness int) {
	if thickness < 1 {
		thickness = 1
	}
	b := dst.Bounds()
	half := thickness / 2
	for _, y := range []int{r.Min.Y, r.Max.Y - 1} {
		for x := r.Min.X; x < r.Max.X; x++ {
			for dy := -half; dy < -half+thickness; dy++ {
				blendPixel(dst, x, y+dy, b, c)
			}
		}
	}
	for _, x := range []int{r.Min.X, r.Max.X - 1} {
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for dx := -half; dx < -half+thickness; dx++ {
				blendPixel(dst, x+dx, y, b, c)
			}
		}
	}
}

// drawDashLine strokes a straight axis-aligned segment with the dash pattern
// running along its length.
func drawDashLine(dst *image.RGBA, x0, y0, x1, y1 int, c color.RGBA, thickness, dashPeriod, dashOn int) {
	if thickness < 1 {
		thickness = 1
	}
	b := dst.Bounds()
	half := thickness / 2
	vertical := x0 == x1
	seg := 0
	if vertical {
		for y := y0; y <= y1; y++ {
			if dashPeriod == 0 || seg%dashPeriod < dashOn {
				for dx := -half; dx < -half+thickness; dx++ {
					blendPixel(dst, x0+dx, y, b, c)
				}
			}
			seg++
		}
		return
	}
	for x := x0; x <= x1; x++ {
		if dashPeriod == 0 || seg%dashPeriod < dashOn {
			for dy := -half; dy < -half+thickness; dy++ {
				blendPixel(dst, x, y0+dy, b, c)
			}
		}
		seg++
	}
}

// drawDot fills a circle and rings it in ring when ring is non-transparent.
func drawDot(dst *image.RGBA, cx, cy, r int, fill, ring color.RGBA) {
	b := dst.Bounds()
	for dy := -r; dy <= r; dy++ {
		for dx := -r; dx <= r; dx++ {
			d2 := dx*dx + dy*dy
			if d2 <= r*r {
				setPixel(dst, cx+dx, cy+dy, b, fill)
			} else if ring.A > 0 && d2 <= (r+1)*(r+1) {
				setPixel(dst, cx+dx, cy+dy, b, ring)
			}
		}
	}
}

// drawChip paints a blended plate with text and returns its total width, so
// the caller can lay the next chip beside it.
func drawChip(dst *image.RGBA, x, y int, text string, fg, bg color.RGBA, scale int) int {
	tw := textWidth(text, scale)
	b := dst.Bounds()
	pad := 2 * scale
	x0, y0, x1, y1 := x, y, x+tw+2*pad, y+digitGlyphH*scale+2*pad
	for py := y0; py < y1; py++ {
		for px := x0; px < x1; px++ {
			blendPixel(dst, px, py, b, bg)
		}
	}
	drawString(dst, x0+pad, y0+pad, text, scale, fg)
	return x1 - x0
}

// fixed2 renders a float with two decimals for the zoom readout.
func fixed2(v float64) string {
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// blendPixel composites c over the existing pixel with integer alpha math -
// deterministic, identical bytes for identical inputs.
func blendPixel(dst *image.RGBA, x, y int, b image.Rectangle, c color.RGBA) {
	if x < b.Min.X || x >= b.Max.X || y < b.Min.Y || y >= b.Max.Y {
		return
	}
	if c.A == 0 {
		return
	}
	if c.A == 255 {
		dst.Set(x, y, c)
		return
	}
	old := dst.RGBAAt(x, y)
	a := uint32(c.A)
	inv := 255 - a
	dst.SetRGBA(x, y, color.RGBA{
		R: uint8((uint32(old.R)*inv + uint32(c.R)*a) / 255),
		G: uint8((uint32(old.G)*inv + uint32(c.G)*a) / 255),
		B: uint8((uint32(old.B)*inv + uint32(c.B)*a) / 255),
		A: 255,
	})
}

func setPixel(dst *image.RGBA, x, y int, b image.Rectangle, c color.Color) {
	if x < b.Min.X || x >= b.Max.X || y < b.Min.Y || y >= b.Max.Y {
		return
	}
	dst.Set(x, y, c)
}

// ---------------------------------------------------------------------------
// The 3x5 caption face (shared with stills.go's digit glyphs)

// letterGlyphs extends digitGlyphs with the letters and marks the readout
// chips need: "zoom 1.61x -> 1.60x", "walk leash", "scene cut · tracking
// reset". Same 3x5 cell, same bit order (bit 2 = leftmost column).
var letterGlyphs = map[rune][digitGlyphH]byte{
	' ': {0b000, 0b000, 0b000, 0b000, 0b000},
	'·': {0b000, 0b000, 0b010, 0b000, 0b000},
	'.': {0b000, 0b000, 0b000, 0b000, 0b010},
	'-': {0b000, 0b000, 0b111, 0b000, 0b000},
	'>': {0b100, 0b010, 0b001, 0b010, 0b100},
	'a': {0b010, 0b001, 0b010, 0b101, 0b111},
	'c': {0b011, 0b100, 0b100, 0b100, 0b011},
	'd': {0b001, 0b001, 0b011, 0b101, 0b111},
	'e': {0b010, 0b101, 0b111, 0b100, 0b010},
	'g': {0b101, 0b101, 0b111, 0b001, 0b110},
	'h': {0b101, 0b101, 0b111, 0b101, 0b101},
	'i': {0b010, 0b000, 0b010, 0b010, 0b010},
	'k': {0b101, 0b101, 0b110, 0b101, 0b101},
	'l': {0b100, 0b100, 0b100, 0b100, 0b110},
	'm': {0b101, 0b111, 0b111, 0b101, 0b101},
	'n': {0b110, 0b101, 0b101, 0b101, 0b101},
	'o': {0b010, 0b101, 0b101, 0b101, 0b010},
	'r': {0b110, 0b101, 0b100, 0b100, 0b100},
	's': {0b011, 0b100, 0b010, 0b001, 0b110},
	't': {0b100, 0b110, 0b100, 0b100, 0b010},
	'u': {0b101, 0b101, 0b101, 0b101, 0b111},
	'w': {0b101, 0b101, 0b101, 0b101, 0b010},
	'x': {0b101, 0b101, 0b010, 0b101, 0b101},
	'z': {0b111, 0b001, 0b010, 0b100, 0b111},
}

func tinyGlyph(ch rune) ([digitGlyphH]byte, bool) {
	if ch >= '0' && ch <= '9' {
		return digitGlyphs[ch-'0'], true
	}
	g, ok := letterGlyphs[ch]
	return g, ok
}

// textWidth is the drawn width of s at scale: glyphW+1 advance per glyph.
func textWidth(s string, scale int) int {
	if scale < 1 {
		scale = 1
	}
	if s == "" {
		return 0
	}
	return len(s)*(digitGlyphW+1)*scale - scale
}

// drawString writes s with the shared 3x5 face scaled by scale; unknown
// runes render as spaces. Returns the x just past the drawn text.
func drawString(dst *image.RGBA, x, y int, s string, scale int, c color.Color) int {
	if scale < 1 {
		scale = 1
	}
	for _, ch := range s {
		glyph, known := tinyGlyph(ch)
		if known {
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
		}
		x += (digitGlyphW + 1) * scale
	}
	return x
}
