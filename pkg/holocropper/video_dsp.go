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
// bounding box: a rectangle locked to the output aspect ratio, fitted on the
// dominant axis (fitDominantAxis), padded by hc.zoomMargin, with zoom-in
// floors:
//  1. never upscale above the source resolution (cropH >= outH * frameH / srcH)
//  2. policy maxZoom (cropH >= frameH / maxZoom)
//  3. extreme landscape ratios (aspect >= 2): cropH >= 0.6 * subject height
//     so a tall subject is never reduced to a sliver; head/torso priority
//     centering is used there (HIGH-ASPECT-RATIO-FIXES.md Phase 1/4).
// Returns (width, height, centerX, centerY) in analysis-frame pixels.
func (hc *HoloCropper) aspectLockedTarget(bbox image.Rectangle, frameW, frameH, aspect float64) (float64, float64, float64, float64) {
	margin := hc.zoomMargin()

	// Fit the dominant axis: the crop always covers the subject's full height,
	// and at least widthCoverFrac(aspect) of its width. Tall output ratios
	// (e.g. 200:648) clip trailing limbs instead of ballooning the crop to
	// frame size, which is what produced 30-45% dead vertical padding on the
	// real corpus (fit-full-width was width-dominant in ~100% of frames).
	h := float64(bbox.Dy()) * margin
	if hw := float64(bbox.Dx()) * widthCoverFrac(aspect) / aspect; hw > h {
		h = hw
	}
	w := h * aspect

	// Stay inside the analysis frame: no out-of-frame padding, no shrink below
	// source scale. Width constrains landscape outputs.
	if w > frameW {
		w = frameW
		h = w / aspect
	}
	if h > frameH {
		h = frameH
		w = h * aspect
	}

	// Zoom-in floors
	minH := frameH / hc.maxZoom()
	if hc.srcH > 0 {
		minH = math.Max(minH, float64(hc.Height)*frameH/float64(hc.srcH))
	}
	if aspect >= 2 {
		minH = math.Max(minH, 0.6*float64(bbox.Dy()))
	}
	if h < minH {
		h = minH
		w = minH * aspect
	}
	if w > frameW {
		w = frameW
	}

	cx := float64(bbox.Min.X+bbox.Max.X) / 2
	cy := float64(bbox.Min.Y+bbox.Max.Y) / 2
	if aspect >= 2 {
		// Top-aligned framing for extreme landscape: when the subject is
		// taller than the crop its top edge sits at the crop top, so the
		// output never carries an empty strip above the head (legs overflow
		// the bottom instead). When the subject is shorter than the crop, the
		// slack is distributed 25/75 top/bottom so the head still hugs the
		// top edge (documented policy, HIGH-ASPECT-RATIO-FIXES.md Phase 4).
		dy := float64(bbox.Dy())
		cy = float64(bbox.Min.Y) + 0.5*math.Min(h, dy) + 0.25*math.Max(0, h-dy)
	}

	return w, h, cx, cy
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
// integer crop rectangle clamped fully inside the frame.
func (hc *HoloCropper) cropRectFromFraming(cx, cy, w, h float64, bounds image.Rectangle) image.Rectangle {
	fw, fh := float64(bounds.Dx()), float64(bounds.Dy())
	cx = clampF(cx, w/2, fw-w/2)
	cy = clampF(cy, h/2, fh-h/2)
	x0 := int(math.Round(cx - w/2))
	y0 := int(math.Round(cy - h/2))
	x1 := x0 + int(math.Round(w))
	y1 := y0 + int(math.Round(h))
	if x1 > bounds.Max.X {
		x1 = bounds.Max.X
	}
	if y1 > bounds.Max.Y {
		y1 = bounds.Max.Y
	}
	if x0 < bounds.Min.X {
		x0 = bounds.Min.X
	}
	if y0 < bounds.Min.Y {
		y0 = bounds.Min.Y
	}
	return image.Rect(x0, y0, x1, y1)
}

// Function to crop a frame to a bounding box. The crop rect is always
// intersected with the frame: out-of-frame regions can no longer leak through
// as transparent (black) padding (HIGH-ASPECT-RATIO-FIXES.md F3/F4).
func (hc *HoloCropper) cropToBBox(src *image.RGBA, cropRect image.Rectangle) *image.RGBA {
	cr := cropRect.Intersect(src.Bounds())
	if cr.Empty() {
		// Degenerate guard: never produce an empty image
		mn := src.Bounds().Min
		cr = image.Rect(mn.X, mn.Y, mn.X+1, mn.Y+1)
	}

	// Create a new RGBA image for the cropped portion
	dst := image.NewRGBA(cr)

	// Copy the cropped portion from the source image to the destination image
	draw.Draw(dst, cr, src, cr.Min, draw.Src)

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
