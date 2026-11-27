package holocropper

import (
	"image"
	"image/draw"
	"math"

	"github.com/bamiaux/rez"
)

// convertToMonochrome converts a frame to grayscale. For opaque frames the
// conversion is the image/color grayModel law written inline - the exact
// 16-bit arithmetic draw.Draw's color model applies
// (y = (19595*r16 + 38470*g16 + 7471*b16 + 1<<15) >> 24 with r16 = r*257
// for 8-bit input) - proven bit-identical to the draw.Draw path and free of
// its per-pixel color-interface calls (TestPerfDetectFormulaProbe,
// PROFILING.md §5). Frames with any transparency keep the generic
// draw.Draw path.
func convertToMonochrome(img *image.RGBA) *image.Gray {
	monoFrame := image.NewGray(img.Bounds())
	writeGray(monoFrame, img)
	return monoFrame
}

// writeGray applies the grayscale law from img into the caller-owned dst
// (same bounds as src).
func writeGray(dst *image.Gray, img *image.RGBA) {
	if !img.Opaque() {
		draw.Draw(dst, dst.Bounds(), img, img.Bounds().Min, draw.Src)
		return
	}
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	for y := 0; y < h; y++ {
		// image.RGBA.Stride is BYTES per row (len(Pix) >= h*Stride).
		src := img.Pix[y*img.Stride : y*img.Stride+w*4]
		dstRow := dst.Pix[y*dst.Stride : y*dst.Stride+w]
		for x := 0; x < w; x++ {
			o := 4 * x
			v := 19595*uint32(src[o])*257 + 38470*uint32(src[o+1])*257 + 7471*uint32(src[o+2])*257
			dstRow[x] = byte((v + 1<<15) >> 24)
		}
	}
}

// otsuThreshold applies Otsu's thresholding algorithm.
func otsuThreshold(img *image.Gray) uint8 {
	// Calculate histogram (row-slice walk over the image's own Pix: same
	// pixels, same counts as the GrayAt walk it replaces).
	histogram := make([]int, 256)
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	totalPixels := float64(w * h)
	for y := 0; y < h; y++ {
		for _, intensity := range img.Pix[y*img.Stride : y*img.Stride+w] {
			histogram[intensity]++
		}
	}

	// Normalize histogram
	var normalizedHistogram [256]float64
	for i := range histogram {
		normalizedHistogram[i] = float64(histogram[i]) / totalPixels
	}

	// Calculate cumulative sum and cumulative mean
	var cumulativeSum [256]float64
	var cumulativeMean [256]float64
	cumulativeSum[0] = normalizedHistogram[0]
	cumulativeMean[0] = 0
	for i := 1; i < 256; i++ {
		cumulativeSum[i] = cumulativeSum[i-1] + normalizedHistogram[i]
		cumulativeMean[i] = cumulativeMean[i-1] + float64(i)*normalizedHistogram[i]
	}

	// Calculate between-class variance and find optimal threshold
	var maxBetweenClassVariance float64
	var optimalThreshold uint8
	for t := 0; t < 256; t++ {
		omega := cumulativeSum[t]
		mu := cumulativeMean[t]
		omega2 := 1 - omega
		mu2 := (cumulativeMean[255] - mu) / omega2
		betweenClassVariance := omega * omega2 * (mu - mu2) * (mu - mu2)
		if betweenClassVariance > maxBetweenClassVariance {
			maxBetweenClassVariance = betweenClassVariance
			optimalThreshold = uint8(t)
		}
	}

	return optimalThreshold
}

// applyThreshold applies a binary threshold to an image: pixels below the
// threshold turn black, pixels at or above it white.
func applyThreshold(img *image.Gray, threshold uint8) *image.Gray {
	binaryImg := image.NewGray(img.Bounds())
	thresholdInto(binaryImg, img, threshold)
	return binaryImg
}

// thresholdInto applies the binary threshold law into the caller-owned dst
// (same bounds as src).
func thresholdInto(dst, src *image.Gray, threshold uint8) {
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	for y := 0; y < h; y++ {
		row := src.Pix[y*src.Stride : y*src.Stride+w]
		dstRow := dst.Pix[y*dst.Stride : y*dst.Stride+w]
		for x, v := range row {
			if v < threshold {
				dstRow[x] = 0
			} else {
				dstRow[x] = 255
			}
		}
	}
}

// expandRectangle expands the given rectangle by x pixels from its center.
func expandRectangle(rect image.Rectangle, x int) image.Rectangle {
	// Expand the rectangle
	newMinX := rect.Min.X - x
	newMinY := rect.Min.Y - x
	newMaxX := rect.Max.X + x
	newMaxY := rect.Max.Y + x

	// Return the expanded rectangle
	return image.Rect(newMinX, newMinY, newMaxX, newMaxY)
}

// detectState carries the per-frame scratch of one streaming detect pass:
// the grayscale and binary buffers and the subject-scan scratch are reused
// frame to frame instead of re-allocated per frame. A pass owns one
// detectState (frames are measured sequentially; it is not safe for
// concurrent use). The measurement stages are the shared laws: grayModel
// grayscale, Otsu with the +/-10 gray-level hysteresis, and the summary
// subjectScan measurement (subject_detail.go) - proven frame-for-frame
// identical to the per-frame-allocation chain they replace (perf branch,
// PROFILING.md §5).
type detectState struct {
	gray    []byte
	bin     []byte
	grayImg image.Gray
	binImg  image.Gray
	sc      subjectScan
}

// run measures one frame: grayscale -> Otsu with the hysteresis (lastThr
// carries the state across frames, start at -1) -> binary -> the subject
// measurement summary and the master-bbox centroid.
func (st *detectState) run(img *image.RGBA, lastThr *float64) (image.Rectangle, int, image.Point) {
	b := img.Bounds()
	n := b.Dx() * b.Dy()
	if cap(st.gray) < n {
		st.gray = make([]byte, n)
	}
	if cap(st.bin) < n {
		st.bin = make([]byte, n)
	}
	st.gray = st.gray[:n]
	st.bin = st.bin[:n]
	st.grayImg = image.Gray{Pix: st.gray, Stride: b.Dx(), Rect: b}
	st.binImg = image.Gray{Pix: st.bin, Stride: b.Dx(), Rect: b}
	writeGray(&st.grayImg, img)
	thr := float64(otsuThreshold(&st.grayImg))
	if *lastThr < 0 {
		*lastThr = thr
	} else {
		*lastThr = clampF(thr, *lastThr-10, *lastThr+10)
	}
	thresholdInto(&st.binImg, &st.grayImg, uint8(*lastThr))
	d := st.sc.measure(&st.binImg, false)
	bbox, subjects := d.Master, d.Subjects
	if bbox.Empty() {
		return bbox, subjects, image.Point{}
	}
	_, cent := maskStats(&st.binImg, bbox)
	return bbox, subjects, cent
}

// detectFrame runs the full single-frame subject detection exactly as the
// fused pipeline's grayscale+binary stages do: grayscale -> Otsu with the
// +/-10 gray-level hysteresis (lastThr carries the state across frames,
// start at -1) -> binary -> subjectsFromBinary. The embeddable detect pass
// streams frames through a reusable detectState; detectFrame is the
// fresh-scratch wrapper one-shot callers (and the law's own tests) use, so
// the DetectionStream carries the same decisions the pipeline's STEP 3
// produces.
//
// It also reports the centroid: the centre of mass of the subject-mask
// pixels inside the master bbox — the same point the original 1-1 project
// drew as its red cross (U3/761 telemetry). Zero when there is no subject.
func detectFrame(img *image.RGBA, lastThr *float64) (image.Rectangle, int, image.Point) {
	return (&detectState{}).run(img, lastThr)
}

// maskCentroid is the integer centre of mass of the white mask pixels inside
// bbox (the 1-1 project's calculateCentroid: sum pixel coords over white
// pixels, divide by the count). Returns zero when bbox contains no white.
func maskCentroid(img *image.Gray, bbox image.Rectangle) image.Point {
	_, cent := maskStats(img, bbox)
	return cent
}

// aspectLockedTargetCalc computes the desired crop from a (smoothed) subject
// bounding box: a rectangle locked to the output aspect ratio, padded by the
// framing margin, with zoom-in floors:
//  1. never upscale above the source resolution (cropH >= outH * frameH / srcH)
//  2. policy maxZoom (cropH >= frameH / maxZoom) - relaxed for extreme
//     landscape outputs, where full-subject containment outranks it (§12.8)
//  3. extreme portrait (aspect < 0.5) and landscape (aspect >= 2) use a
//     containment fit: the crop must hold the whole padded subject. For
//     landscape this can require black padding beyond the frame's left/
//     right edges (capped at landscapeMaxCropW, which guarantees the full
//     frame height always fits, §12.9), and the crop is anchored so the
//     feet sit at the stage floor (frame bottom). Square outputs center —
//     a GROUNDED square takes the same ground snap and feet anchor
//     (squareGrounded below, Horidashimi 793).
//
// Returns (width, height, centerX, centerY) in analysis-frame pixels.
// framingParams carries the zoom margin, the maxZoom policy cap and the
// output/source heights the zoom floors need.
func aspectLockedTargetCalc(params framingParams, bbox image.Rectangle, frameW, frameH, aspect float64) (float64, float64, float64, float64) {
	margin := params.Margin

	// 793 (Horidashimi #48 report): the ground snap (§12.10) was landscape-only,
	// so a 1:1 output over a floor-standing subject centered its crop on the
	// bbox and the crop bottom could rest ABOVE the frame floor - feet clipped
	// in the previews. A grounded square (bbox bottom in the floor band) now
	// takes the snap and the feet anchor. Airborne squares keep the untouched
	// bbox-centered 1:1 law.
	squareGrounded := aspect == 1 && float64(frameH)-float64(bbox.Max.Y) <= landscapeGroundSnapFrac*float64(frameH)

	var h float64
	if aspect < 0.5 {
		// Tall outputs (e.g. 200:648): containment fit. The crop must contain
		// the full padded subject bbox; containing the subject width can push
		// crop height toward the frame cap. That is the accepted composition
		// trade - the whole subject (feet included) in frame, with small
		// stage padding on the sides, beats maximal subject coverage. The
		// extra padding floor (sweep-tuned, §12.7) keeps headroom and side
		// margin around the subject in the containment regime.
		containMargin := math.Max(margin, portraitContainMargin)
		h = math.Max(float64(bbox.Dy()), float64(bbox.Dx())/aspect) * containMargin
	} else if aspect >= 2 {
		// Wide outputs (e.g. 648:200): containment fit as well (§12.8). An
		// aspect-locked crop inside a 16:9 analysis frame can never be taller
		// than frameW/aspect (~264 px), shorter than a standing subject
		// (~300 px), so keeping the feet in frame forced clipping. The crop
		// now grows past the frame's left/right edges (capped below) with
		// black stage padding - the owner's accepted trade: fitting the full
		// subject outranks maximal subject scale.
		//
		// Ground snap (§12.10): dark footwear on dark stages is often missed
		// by the mask, leaving the bbox bottom (twin-tail tips) 10-70 px
		// above the stage floor while the feet are actually on it. When the
		// bbox bottom is near the frame floor, treat the subject as grounded
		// and extend the bbox to the floor before fitting and anchoring: the
		// crop then reaches the real feet instead of cutting them below the
		// phantom bbox bottom. Over-snapping an airborne subject is harmless
		// (the crop only grows); under-snapping clips feet.
		if float64(frameH)-float64(bbox.Max.Y) <= landscapeGroundSnapFrac*float64(frameH) {
			bbox.Max.Y = int(math.Round(frameH))
		}
		h = math.Max(float64(bbox.Dy()), float64(bbox.Dx())/aspect) * landscapeContainMargin(margin)
	} else {
		// Square outputs: dominant-axis fit. Cover the subject height and
		// at least widthCoverFrac of its width; trailing limbs clip rather
		// than the crop ballooning. Landscape outputs (aspect > 1) carry the
		// width fit at the landscape containment margin instead: subjects
		// must never fill the crop edge-to-edge (§14 review - empty stage
		// padding left/right "like the headroom on top"). An airborne square
		// 1:1 keeps the original look untouched; a grounded square first
		// extends the bbox to the floor (same phantom-feet cure as landscape,
		// §12.10 / 793) so the fit reaches the real feet.
		if squareGrounded {
			bbox.Max.Y = int(math.Round(frameH))
		}
		h = float64(bbox.Dy()) * margin
		hw := float64(bbox.Dx()) * widthCoverFrac(aspect) / aspect
		if aspect > 1 {
			hw = float64(bbox.Dx()) * landscapeContainMargin(margin) / aspect
		}
		if hw > h {
			h = hw
		}
	}
	w := h * aspect

	// Vertical is always inside the frame (no out-of-frame vertical padding).
	// Width is inside the frame except for landscape, where the containment
	// fit may overflow up to landscapeMaxCropW (§12.9).
	if h > frameH {
		h = frameH
		w = h * aspect
	}
	maxW := float64(frameW)
	if aspect >= 2 {
		maxW = landscapeMaxCropW(float64(frameW), float64(frameH), aspect)
	}
	if w > maxW {
		w = maxW
		h = w / aspect
	}

	// Zoom-in floors. The no-upscale floor is always absolute; the policy
	// maxZoom floor is relaxed for landscape, where the containment fit wins.
	srcFloor := 0.0
	if params.SrcH > 0 {
		srcFloor = float64(params.OutH) * frameH / float64(params.SrcH)
	}
	minH := math.Max(frameH/params.MaxZoom, srcFloor)
	if aspect >= 2 {
		minH = math.Max(srcFloor, math.Min(minH, h))
	}
	if h < minH {
		h = minH
		w = h * aspect
		if w > maxW {
			w = maxW
			h = w / aspect
		}
	}

	cx := float64(bbox.Min.X+bbox.Max.X) / 2
	cy := float64(bbox.Min.Y+bbox.Max.Y) / 2
	switch {
	case aspect >= 2, aspect < 0.5:
		// Feet-priority anchoring for both containment regimes: the
		// subject's feet sit a stable pad (feetPadFraction of the crop)
		// above the crop bottom and all remaining slack goes above the head.
		// In these videos the stage floor is the frame bottom, so for
		// grounded subjects the clamp below pins the crop bottom exactly to
		// the frame floor - the feet line is geometrically stable.
		cy = float64(bbox.Max.Y) - h/2 + feetPadFraction*h
		cy = clampF(cy, h/2, frameH-h/2)
	case squareGrounded:
		// 793: the same feet-priority anchor for a grounded 1:1 output - with
		// the snap above the feet rest on the frame floor, and the clamp pins
		// the crop bottom exactly there. An airborne square matches no case
		// and stays bbox-centered.
		cy = float64(bbox.Max.Y) - h/2 + feetPadFraction*h
		cy = clampF(cy, h/2, frameH-h/2)
	}

	return w, h, cx, cy
}

// portraitContainMargin is the padding floor used only by the tall-output
// containment branch of aspectLockedTarget (sweep-tuned on the real corpus,
// HIGH-ASPECT-RATIO-FIXES.md §12.7). Square 1:1 fits keep the user's
// hc.zoomMargin; landscape fits use landscapeContainMargin below.
const portraitContainMargin = 1.18

// landscapeContainMargin is the padding floor for landscape subject fits:
// applied to the containment fit (aspect >= 2) and to the width term for
// moderate landscape ratios (1 < aspect < 2). A subject fit then leaves
// >= 7.6% of the crop width empty on each side - the left/right stage
// padding the owner asked for as "like the headroom on top" (§14 review);
// the moderate-landscape width term previously fit the subject edge-to-edge
// with zero slack. Square 1:1 fits are untouched. A higher user zoomMargin
// always overrides the floor.
func landscapeContainMargin(margin float64) float64 {
	return math.Max(margin, 1.18)
}

// landscapePadCapFrac is the baseline side-padding allowance for the
// landscape containment fit (§12.8/§12.9): 42% of the frame width per side
// is exactly the padding of a full-frame-height crop under a 3.24:1 output
// on a 16:9 analysis frame (1555 px crop in an 854 px frame).
const landscapePadCapFrac = 0.42

// landscapeGroundSnapFrac is the floor-proximity band (§12.10): an EMA bbox
// bottom within this fraction of the frame height of the frame bottom marks
// a grounded subject, whose fit and anchor extend to the frame floor. 0.18
// of 480 is 86 px - comfortably above the corpus' dark-boot mask misses
// (10-70 px), below the airborne gaps at jumps/fades (90-220 px).
const landscapeGroundSnapFrac = 0.18

// landscapeMaxCropW is the widest legal landscape crop: the larger of the
// padding-fraction allowance and the full-frame-height containment fit
// (frameH * aspect). Because a subject bbox can never be taller than the
// frame, this makes vertical subject clipping structurally impossible in
// landscape: the crop can always grow to hold the whole frame height, at
// the cost of black stage padding up to (frameH*aspect - frameW)/2 px per
// side (~41% of frame width for 648x200 output on a 16:9 source; the dark
// stage backgrounds hide it, §12.9).
func landscapeMaxCropW(frameW, frameH, aspect float64) float64 {
	return math.Max(frameW*(1+2*landscapePadCapFrac), frameH*aspect)
}

// widthCoverFrac is the minimum fraction of the subject bbox width the crop
// must cover. Tall output ratios clip trailing limbs/props rather than
// ballooning the crop vertically; square and wide outputs cover the full
// width.
func widthCoverFrac(aspect float64) float64 {
	switch {
	case aspect < 0.5:
		return 0.55
	case aspect < 1:
		return 0.85
	default:
		return 1.0
	}
}

// cropRectFromFramingCalc converts the smoothed float framing state into an
// integer crop rectangle. Vertical extents always stay inside the frame;
// horizontally the rect may extend past the frame's left/right edges by up
// to padAllow pixels per side (landscape containment padding, §12.8), and
// the x clamp window stays valid as long as padAllow >= (w - fw) / 2.
// cropToBBoxCalc composes the out-of-frame regions as black stage padding.
func cropRectFromFramingCalc(cx, cy, w, h, padAllow float64, bounds image.Rectangle) image.Rectangle {
	fw, fh := float64(bounds.Dx()), float64(bounds.Dy())
	cx = clampF(cx, w/2-padAllow, fw-w/2+padAllow)
	cy = clampF(cy, h/2, fh-h/2)
	x0 := int(math.Round(cx - w/2))
	y0 := int(math.Round(cy - h/2))
	x1 := x0 + int(math.Round(w))
	y1 := y0 + int(math.Round(h))
	// The x edges are deliberately not clamped to the frame: shaving them
	// would break the aspect lock. Only a degenerate caller (crop wider than
	// frame + 2*padAllow) gets trimmed to the padded frame edges.
	loX := bounds.Min.X - int(math.Round(padAllow))
	hiX := bounds.Max.X + int(math.Round(padAllow))
	if x1 > hiX {
		x1 = hiX
	}
	if x0 < loX {
		x0 = loX
	}
	// Only y can trim to the frame: cy is always fully in-frame.
	if y1 > bounds.Max.Y {
		y1 = bounds.Max.Y
	}
	if y0 < bounds.Min.Y {
		y0 = bounds.Min.Y
	}
	return image.Rect(x0, y0, x1, y1)
}

// cropToBBoxCalc crops a frame to a bounding box. The rect may extend past
// the frame's left/right edges (landscape containment padding, §12.8): those
// regions are composed as black stage padding. Vertical extents stay inside
// the frame, so out-of-frame vertical padding can never leak through
// (HIGH-ASPECT-RATIO-FIXES.md F3/F4).
func cropToBBoxCalc(src *image.RGBA, cropRect image.Rectangle) *image.RGBA {
	cr := cropRect
	if cr.Empty() {
		// Degenerate guard: never produce an empty image
		mn := src.Bounds().Min
		cr = image.Rect(mn.X, mn.Y, mn.X+1, mn.Y+1)
	}

	// Destination is normalised to Min=(0,0) with the crop's full extent;
	// regions of the crop that lie outside the frame stay zeroed (black).
	dst := image.NewRGBA(image.Rect(0, 0, cr.Dx(), cr.Dy()))
	if inter := cr.Intersect(src.Bounds()); !inter.Empty() {
		draw.Draw(dst, inter.Sub(cr.Min), src, inter.Min, draw.Src)
	}

	return dst
}

// fillResizeTo scales the framed crop to exactly fill outW x outH. The crop
// rect is aspect-locked by aspectLockedTargetCalc, so this is a straight
// scale, not a fit: no letterbox padding is produced.
func fillResizeTo(img *image.RGBA, outW, outH int) *image.RGBA {
	out := image.NewRGBA(image.Rect(0, 0, outW, outH))
	err := rez.Convert(out, img, rez.NewLanczosFilter(2))
	if err != nil {
		panic("Failed to resize img: " + err.Error())
	}
	return out
}

// fitResizeTo scales img ASPECT-PRESERVING to fit inside outW x outH and
// centers it on black: the leftover columns or rows are the bars
// (pillarbox/letterbox). This is the fit-mode resize (§6.10, non-hologram
// videos) - the whole frame survives, nothing is cropped or zoomed. When
// the aspects match it degenerates to an exact resize with no bars.
func fitResizeTo(img *image.RGBA, outW, outH int) *image.RGBA {
	iw, ih := img.Bounds().Dx(), img.Bounds().Dy()
	scale := math.Min(float64(outW)/float64(iw), float64(outH)/float64(ih))
	sw := int(math.Round(float64(iw) * scale))
	sh := int(math.Round(float64(ih) * scale))
	if sw < 1 {
		sw = 1
	}
	if sh < 1 {
		sh = 1
	}

	out := image.NewRGBA(image.Rect(0, 0, outW, outH)) // black bars
	scaled := img
	if sw != iw || sh != ih {
		scaled = fillResizeTo(img, sw, sh)
	}
	offX := (outW - sw) / 2
	offY := (outH - sh) / 2
	draw.Draw(out, image.Rect(offX, offY, offX+sw, offY+sh), scaled, image.Point{}, draw.Over)
	return out
}
