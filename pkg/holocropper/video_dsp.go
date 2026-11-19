package holocropper

import (
	"image"
	"image/color"
	"image/draw"
	"math"

	"github.com/bamiaux/rez"
)

// Function to convert frame to grayscale
func (hc *HoloCropper) convertToMonochrome(img *image.RGBA) *image.Gray {
	monoFrame := image.NewGray(img.Bounds())
	draw.Draw(monoFrame, monoFrame.Bounds(), img, img.Bounds().Min, draw.Src)
	return monoFrame
}

// Otsu's thresholding algorithm
func (hc *HoloCropper) otsuThreshold(img *image.Gray) uint8 {
	// Calculate histogram
	histogram := make([]int, 256)
	totalPixels := float64(img.Bounds().Dx() * img.Bounds().Dy())
	for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
		for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
			intensity := img.GrayAt(x, y).Y
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

// Apply threshold to image
func (hc *HoloCropper) applyThreshold(img *image.Gray, threshold uint8) *image.Gray {
	binaryImg := image.NewGray(img.Bounds())
	black := color.Gray{Y: 0}
	white := color.Gray{Y: 255}

	for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
		for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
			if img.GrayAt(x, y).Y < threshold {
				binaryImg.SetGray(x, y, black)
			} else {
				binaryImg.SetGray(x, y, white)
			}
		}
	}
	return binaryImg
}

// Connected component labeling algorithm
func (hc *HoloCropper) connectedComponentLabeling(binaryMap *image.Gray) (map[int]image.Rectangle, [][]int) {
	labelMatrix := make([][]int, binaryMap.Bounds().Dy())
	for i := range labelMatrix {
		labelMatrix[i] = make([]int, binaryMap.Bounds().Dx())
	}

	boundingBoxes := make(map[int]image.Rectangle)

	label := 1
	for y := binaryMap.Bounds().Min.Y; y < binaryMap.Bounds().Max.Y; y++ {
		for x := binaryMap.Bounds().Min.X; x < binaryMap.Bounds().Max.X; x++ {
			if binaryMap.GrayAt(x, y).Y > 0 && labelMatrix[y][x] == 0 {
				boundingBox := hc.dfs(binaryMap, labelMatrix, x, y, label)
				boundingBoxes[label] = boundingBox
				label++
			}
		}
	}

	return boundingBoxes, labelMatrix
}

// Depth-first search to label connected components
func (hc *HoloCropper) dfs(binaryMap *image.Gray, labelMatrix [][]int, x, y, label int) image.Rectangle {
	minX, minY, maxX, maxY := x, y, x, y

	stack := []image.Point{{x, y}}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		cx, cy := current.X, current.Y
		labelMatrix[cy][cx] = label

		if cx < minX {
			minX = cx
		}
		if cx > maxX {
			maxX = cx
		}
		if cy < minY {
			minY = cy
		}
		if cy > maxY {
			maxY = cy
		}

		// Check neighboring pixels
		for dx := -1; dx <= 1; dx++ {
			nx, ny := cx+dx, cy-1
			if nx >= 0 && nx < binaryMap.Bounds().Dx() && ny >= 0 && ny < binaryMap.Bounds().Dy() {
				if binaryMap.GrayAt(nx, ny).Y > 0 && labelMatrix[ny][nx] == 0 {
					stack = append(stack, image.Point{nx, ny})
				}
			}
			ny = cy + 1
			if nx >= 0 && nx < binaryMap.Bounds().Dx() && ny >= 0 && ny < binaryMap.Bounds().Dy() {
				if binaryMap.GrayAt(nx, ny).Y > 0 && labelMatrix[ny][nx] == 0 {
					stack = append(stack, image.Point{nx, ny})
				}
			}
		}
		nx := cx - 1
		ny := cy
		if nx >= 0 && nx < binaryMap.Bounds().Dx() && ny >= 0 && ny < binaryMap.Bounds().Dy() {
			if binaryMap.GrayAt(nx, ny).Y > 0 && labelMatrix[ny][nx] == 0 {
				stack = append(stack, image.Point{nx, ny})
			}
		}
		nx = cx + 1
		if nx >= 0 && nx < binaryMap.Bounds().Dx() && ny >= 0 && ny < binaryMap.Bounds().Dy() {
			if binaryMap.GrayAt(nx, ny).Y > 0 && labelMatrix[ny][nx] == 0 {
				stack = append(stack, image.Point{nx, ny})
			}
		}
	}

	return image.Rect(minX, minY, maxX+1, maxY+1)
}

// ByArea implements the sort.Interface for []image.Rectangle based on area.
type ByArea []image.Rectangle

func (a ByArea) Len() int           { return len(a) }
func (a ByArea) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a ByArea) Less(i, j int) bool { return a[i].Dx()*a[i].Dy() > a[j].Dx()*a[j].Dy() }

// CombineBounds combines the bounds of all rectangles in the slice
// that overlap with the given rectangle and removes them from the slice.
func (hc *HoloCropper) combineBounds(rect image.Rectangle, rects []image.Rectangle, tol int) (combined image.Rectangle, remainingRects []image.Rectangle) {
	// Initialize the combined rectangle with the rectangle passed as an argument
	combined = rect

	// Iterate over the rectangles in the slice
	for _, r := range rects {
		// Check if the current rectangle overlaps with the given rectangle
		if hc.expandRectangle(combined, tol).Overlaps(r) {
			// Combine the bounds of overlapping rectangles
			combined = combined.Union(r)
		} else {
			// Add the non-overlapping rectangle to the remaining rectangles slice
			remainingRects = append(remainingRects, r)
		}
	}

	return combined, remainingRects
}

// ExpandRectangle expands the given rectangle by x pixels from its center.
func (hc *HoloCropper) expandRectangle(rect image.Rectangle, x int) image.Rectangle {
	// Expand the rectangle
	newMinX := rect.Min.X - x
	newMinY := rect.Min.Y - x
	newMaxX := rect.Max.X + x
	newMaxY := rect.Max.Y + x

	// Return the expanded rectangle
	return image.Rect(newMinX, newMinY, newMaxX, newMaxY)
}

// TotalBoundingBox finds the total bounding box given a slice of image.Rectangles.
func (hc *HoloCropper) totalBoundingBox(rectangles []image.Rectangle) image.Rectangle {
	if len(rectangles) == 0 {
		// Return zero rectangle if there are no input rectangles
		return image.ZR
	}

	// Initialize min and max coordinates
	minX, minY := rectangles[0].Min.X, rectangles[0].Min.Y
	maxX, maxY := rectangles[0].Max.X, rectangles[0].Max.Y

	// Iterate over rectangles to find min and max coordinates
	for _, rect := range rectangles {
		if rect.Min.X < minX {
			minX = rect.Min.X
		}
		if rect.Min.Y < minY {
			minY = rect.Min.Y
		}
		if rect.Max.X > maxX {
			maxX = rect.Max.X
		}
		if rect.Max.Y > maxY {
			maxY = rect.Max.Y
		}
	}

	// Construct and return the total bounding box
	return image.Rect(minX, minY, maxX, maxY)
}

// aspectLockedTarget computes the desired crop from a (smoothed) subject
// bounding box: a rectangle locked to the output aspect ratio, padded by
// hc.zoomMargin, with zoom-in floors:
//  1. never upscale above the source resolution (cropH >= outH * frameH / srcH)
//  2. policy maxZoom (cropH >= frameH / maxZoom) - relaxed for extreme
//     landscape outputs, where full-subject containment outranks it (§12.8)
//  3. extreme portrait (aspect < 0.5) and landscape (aspect >= 2) use a
//     containment fit: the crop must hold the whole padded subject. For
//     landscape this can require black padding beyond the frame's left/
//     right edges (capped at landscapeMaxCropW, which guarantees the full
//     frame height always fits, §12.9), and the crop is anchored so the
//     feet sit at the stage floor (frame bottom). Square outputs center.
//
// Returns (width, height, centerX, centerY) in analysis-frame pixels.
func (hc *HoloCropper) aspectLockedTarget(bbox image.Rectangle, frameW, frameH, aspect float64) (float64, float64, float64, float64) {
	margin := hc.zoomMargin()

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
		// padding left/right "like the headroom on top"). Square 1:1 keeps
		// the original look untouched.
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
	if hc.srcH > 0 {
		srcFloor = float64(hc.Height) * frameH / float64(hc.srcH)
	}
	minH := math.Max(frameH/hc.maxZoom(), srcFloor)
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

// cropRectFromFraming converts the smoothed float framing state into an
// integer crop rectangle. Vertical extents always stay inside the frame;
// horizontally the rect may extend past the frame's left/right edges by up
// to padAllow pixels per side (landscape containment padding, §12.8), and
// the x clamp window stays valid as long as padAllow >= (w - fw) / 2.
// cropToBBox composes the out-of-frame regions as black stage padding.
func (hc *HoloCropper) cropRectFromFraming(cx, cy, w, h, padAllow float64, bounds image.Rectangle) image.Rectangle {
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

// Function to crop a frame to a bounding box. The rect may extend past the
// frame's left/right edges (landscape containment padding, §12.8): those
// regions are composed as black stage padding. Vertical extents stay inside
// the frame, so out-of-frame vertical padding can never leak through
// (HIGH-ASPECT-RATIO-FIXES.md F3/F4).
func (hc *HoloCropper) cropToBBox(src *image.RGBA, cropRect image.Rectangle) *image.RGBA {
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

// fillResizeRGBA scales the framed crop to exactly fill the requested output
// size. The crop rect is aspect-locked in calcVideo, so this is a straight
// scale, not a fit: no letterbox padding is produced (replaces the old
// fit-scale + black-letterbox resizeRGBA).
func (hc *HoloCropper) fillResizeRGBA(img *image.RGBA) *image.RGBA {
	out := image.NewRGBA(image.Rect(0, 0, hc.Width, hc.Height))
	err := rez.Convert(out, img, rez.NewLanczosFilter(2))
	if err != nil {
		panic("Failed to resize img: " + err.Error())
	}
	return out
}
