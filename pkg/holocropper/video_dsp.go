package holocropper

import (
	"image"
	"image/color"
	"image/draw"

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

// Function to calculate centroid (geometric center) of image subjects
func (hc *HoloCropper) calculateCentroid(img *image.Gray, bbox image.Rectangle) image.Point {
	totalX, totalY, count := 0, 0, 0
	for y := bbox.Min.Y; y < bbox.Max.Y; y++ {
		for x := bbox.Min.X; x < bbox.Max.X; x++ {
			if img.GrayAt(x, y).Y == 255 { // white pixel
				totalX += x
				totalY += y
				count++
			}
		}
	}

	if count > 0 {
		return image.Point{totalX / count, totalY / count}
	}

	return image.Point{0, 0}
}

// Function to calculate pan adjustment
func (hc *HoloCropper) calculatePanAdjustment(centroid image.Point, frameWidth int, pid *PIDController) int {
	// Calculate how much you need to pan to keep the centroid in the center
	panAdj := centroid.X - frameWidth/2

	// smoothedPanAdj := ma.Add(panAdj)
	pid.SetSetpoint(float64(panAdj))
	smoothedPanAdj := pid.Update()
	pid.SetDelta(pid.delta + smoothedPanAdj)

	return int(pid.delta)
}

// Clamp math util function
func clamp(value, min, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func (hc *HoloCropper) calculateCropRect(centroid image.Point, frameWidth, cropWidth, panAdj int) image.Rectangle {
	frameCtr := frameWidth / 2
	min := (cropWidth / 2) - frameCtr
	max := -min
	panAdj = clamp(panAdj, min, max)
	cropHeight := cropWidth

	if hc.Width != 0 {
		// offset x
		// frameCtr += (squareSize - widthSize) / 2
		// set new width
		cropWidth = hc.Width
	}

	return image.Rect((frameCtr+panAdj)-cropWidth/2, 0, (frameCtr+panAdj)+cropWidth/2, cropHeight)
}

// Function to calculate the minimum horizontal distance between the bounding box and the crop box
func (hc *HoloCropper) minBoxHDist(bbox, cropbox image.Rectangle) int {
	leftD := (bbox.Min.X - cropbox.Min.X)
	rightD := (-(bbox.Max.X - cropbox.Max.X))
	if leftD < rightD {
		return -leftD
	}
	return -rightD
}

// Function to crop a frame to a bounding box
func (hc *HoloCropper) cropToBBox(src *image.RGBA, cropRect image.Rectangle) *image.RGBA {
	// Create a new RGBA image for the cropped portion
	dst := image.NewRGBA(cropRect)

	// Fill background with black
	// draw.Draw(dst, dst.Bounds(), &image.Uniform{color.RGBA{0, 0, 0, 255}}, image.ZP, draw.Src)

	// Copy the cropped portion from the source image to the destination image
	draw.Draw(dst, cropRect, src, cropRect.Min, draw.Src)

	return dst
}

// Function to resize an image to fit within the specified dimensions
func (hc HoloCropper) resizeRGBA(img *image.RGBA, maxWidth, maxHeight int) *image.RGBA {
	// Calculate the scaling factor for width and height
	scaleWidth := float64(maxWidth) / float64(img.Bounds().Dx())
	scaleHeight := float64(maxHeight) / float64(img.Bounds().Dy())

	// Use the smaller scaling factor to ensure the entire image fits within the specified dimensions
	scale := scaleWidth
	if scaleHeight < scaleWidth {
		scale = scaleHeight
	}

	// Resize the image using the calculated scaling factor
	resizedImg := image.NewRGBA(image.Rect(0, 0, int(float64(img.Bounds().Dx())*scale), int(float64(img.Bounds().Dy())*scale)))
	err := rez.Convert(resizedImg, img, rez.NewLanczosFilter(2))
	if err != nil {
		panic("Failed to resize img: " + err.Error())
	}

	// Create a new RGBA image with the resized dimensions
	resizedRGBA := image.NewRGBA(image.Rect(0, 0, int(resizedImg.Bounds().Dx()), int(resizedImg.Bounds().Dy())))
	// Fill with black
	draw.Draw(resizedRGBA, resizedRGBA.Bounds(), &image.Uniform{color.Black}, image.ZP, draw.Src)

	// Copy the resized image onto the new RGBA image, with alpha blending
	draw.Draw(resizedRGBA, resizedRGBA.Bounds(), resizedImg, resizedImg.Bounds().Min, draw.Over)

	return resizedRGBA
}
