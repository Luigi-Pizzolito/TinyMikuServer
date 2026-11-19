package holocropper

import (
	"image"
	"image/color"
)

/* Framing overlay drawing (HOLOCROPPER-API-PLAN.md §6.5): the same boxes the
UI canvas draws - raw subject bbox, smoothed EMA box and the crop window -
composited onto output-resolution frames for preview videos and stills.

Box geometry arrives in OUTPUT pixels (FrameRecord.Out), so what these
previews show is exactly what the crop contains: a subject box clipped by the
crop edge is visibly clipped, and the crop window is the frame border. Boxes
that fall partly outside the frame (landscape stage padding, §12.8) are drawn
clipped to the frame rather than dropped - a subject hanging off the crop edge
is the framing diagnostic a preview exists to reveal. */

// overlayStyle fixes the box colors and stroke weights. A dark underline
// under each colored stroke keeps the boxes legible over any content.
type overlayStyle struct {
	Subject color.RGBA
	EMA     color.RGBA
	Crop    color.RGBA
	// StrokeFrac is the stroke width as a fraction of the frame's shorter
	// edge, floored at 1 px: 240 px previews get 1 px lines, 1080 px
	// outputs get ~4 px.
	StrokeFrac float64
	// DashPeriod/DashOn describe the EMA box's dash pattern in stroke widths.
	DashPeriod, DashOn int
}

func defaultOverlayStyle() overlayStyle {
	return overlayStyle{
		Subject:    color.RGBA{R: 0xFF, G: 0xD4, B: 0x00, A: 0xFF}, // amber
		EMA:        color.RGBA{R: 0x00, G: 0xE5, B: 0xFF, A: 0xFF}, // cyan
		Crop:       color.RGBA{R: 0xFF, G: 0x2D, B: 0x78, A: 0xFF}, // magenta
		StrokeFrac: 0.004,
		DashPeriod: 4,
		DashOn:     3,
	}
}

var overlayUnderline = color.RGBA{R: 0x10, G: 0x10, B: 0x10, A: 0xFF}

// drawOverlayBoxes composites this record's framing boxes onto dst, an
// output-resolution frame. opts selects which boxes are drawn; the subject
// and EMA boxes are skipped when the subject was lost (their Out rects are
// empty), so a dropout frame shows only the crop window holding position.
func drawOverlayBoxes(dst *image.RGBA, rec FrameRecord, opts PreviewOptions, st overlayStyle) {
	b := dst.Bounds()
	w, h := b.Dx(), b.Dy()
	if w < 2 || h < 2 {
		return
	}
	stroke := int(st.StrokeFrac*float64(min(w, h)) + 0.5)
	if stroke < 1 {
		stroke = 1
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

func setPixel(dst *image.RGBA, x, y int, b image.Rectangle, c color.Color) {
	if x < b.Min.X || x >= b.Max.X || y < b.Min.Y || y >= b.Max.Y {
		return
	}
	dst.Set(x, y, c)
}
