package holocropper

import (
	"image"
	"sort"
)

/* STEP 1. Read Video */

func (hc *HoloCropper) readVideo() {
	// Set frame buffer
	vbuffer := image.NewRGBA(image.Rect(0, 0, hc.video.Width(), hc.video.Height()))
	hc.video.SetFrameBuffer(vbuffer.Pix)

	// Start reading and piping raw frames
	for hc.video.Read() {
		if hc.procFrameQueue1 != nil {
			hc.procFrameQueue1 <- vbuffer
		}
	}

	hc.video.Close()
}

/* STEP 2. GRAYSCALE VIDEO */

func (hc *HoloCropper) grayscaleVideo() {
	for {
		select {
		case img, ok := <-hc.procFrameQueue1:
			if !ok {
				// Channel is closed
				return
			}

			// Convert received frame to grayscale
			maskFrame := hc.convertToMonochrome(img)
			// Send to next pipe
			if hc.procFrameQueue2 != nil {
				hc.procFrameQueue2 <- &ImageGrayMask{Mask: maskFrame, Image: img}
			}
		}
	}
}

/* STEP 3. BINARY VIDEO (BITMAP) */

func (hc *HoloCropper) binaryVideo() {
	for {
		select {
		case mask, ok := <-hc.procFrameQueue2:
			if !ok {
				// Channel is closed
				return
			}

			// Apply Otsu threshold to the received image
			threshold := hc.otsuThreshold(mask.Mask)
			binaryImg := hc.applyThreshold(mask.Mask, threshold)

			// Apply connected component labelling algorithm
			boundingBoxes, _ := hc.connectedComponentLabeling(binaryImg)

			// Convert map to slice of rectangles
			var rectangles []image.Rectangle
			for _, rect := range boundingBoxes {
				rectangles = append(rectangles, rect)
			}
			// Sort bounding boxes by area
			sort.Sort(ByArea(rectangles))

			// Combine overlapping bounding boxes
			subjects := make([]image.Rectangle, 0)
			computedOverlapRects := rectangles
			for _, bbox := range rectangles {
				largestArea := rectangles[0].Dx() * rectangles[0].Dy()
				area := bbox.Dx() * bbox.Dy()
				perCovered := 100 * float64(float64(area)/float64(largestArea))
				// Boxes classified as big have at least 20% of the area of the biggest box
				if perCovered >= 20 {
					// Merge bounding boxes
					bbox, computedOverlapRects = hc.combineBounds(bbox, computedOverlapRects, mask.Image.Bounds().Dy()/20)
					// Calculate centroid
					// ctr := hc.calculateCentroid(binaryImg, bbox)
					// Append to main subjects list
					subjects = append(subjects, bbox)
				}
			}

			subjects_bbox := hc.totalBoundingBox(subjects)
			//? extra padding?
			// subjects_bbox = ExpandRectangle(subjects_bbox, mask.Image.Bounds().Dy()/20)

			// Send to next pipe
			if hc.procFrameQueue3 != nil {
				hc.procFrameQueue3 <- &ImageGrayBoundMask{Mask: binaryImg, Image: mask.Image, Bbox: subjects_bbox, Subj: len(subjects)}
			}
		}
	}
}

/* STEP 4. CALCULATE VIDEO CROP */

func (hc *HoloCropper) calcVideo() {
	// PID controllers for horizontal pan and zoom
	// pid = NewPIDController(0.1, 0.000000000005, 0.00000002, 0) // nice oscillation at 0.1, 0.000000000005, 0.00000002
	//                     P    I             D            Initial Setpoint
	pid := NewPIDController(0.1, 0.001*10e-10, 0.002*10e10, 0)
	pidz := NewPIDController(0.2, 0.003*10e-10, 0.002*10e10, 0)

	for {
		select {
		case mask, ok := <-hc.procFrameQueue3:
			if !ok {
				// Channel is closed
				return
			}

			// Calculate centroid of the subject bounding box
			centroid := hc.calculateCentroid(mask.Mask, mask.Bbox)

			// Calculate horizontal pan adjustment
			panAdj := hc.calculatePanAdjustment(centroid, mask.Image.Bounds().Dx(), pid)

			// Check if percentage covered by horizontal pan adjustment makes sense
			bboxArea := mask.Bbox.Dx() * mask.Bbox.Dy()
			frameArea := mask.Image.Bounds().Dx() * mask.Image.Bounds().Dy()
			perCovered := float64(float64(bboxArea)/float64(frameArea)) * 100
			if perCovered < 0.1 {
				// bounding box too small (< .1%), do not pan
				panAdj = 0
			} else if perCovered > 75 {
				// bounding box too big (> 80%) do not pan
				panAdj = 0
			}

			// Calculate crop rectangle
			cropRect := hc.calculateCropRect(centroid, mask.Image.Bounds().Dx(), mask.Image.Bounds().Dy(), panAdj)

			// Zoom
			if hc.zoom {
				// Zoom out if width of bbox > cropRect
				padding := int(cropRect.Dx() / 8)                  // padding width of 1/8th of the crop rectangle width
				difOut := hc.minBoxHDist(mask.Bbox, cropRect)      // minimum horizontal distance between the bounding box and the crop box
				difOut = clamp(difOut+padding, 0, cropRect.Dx()*5) // clamp to 5 times the crop rectangle width
				zoompx := difOut
				pidz.SetSetpoint(float64(zoompx))
				smoothedZoomAdj := pidz.Update()
				pidz.SetDelta(pidz.delta + smoothedZoomAdj)

				// Calculate zoom adjustment
				cropRect = hc.expandRectangle(cropRect, clamp(int(pidz.delta), 0, cropRect.Dx()*5))
			}

			// Send to next pipe
			if hc.procFrameQueue4 != nil {
				hc.procFrameQueue4 <- &ImageCropMask{CBox: cropRect, Image: mask.Image, zoom: hc.zoom}
			}
		}
	}
}

/* STEP 5. CROP VIDEO */

func (hc *HoloCropper) cropVideo() {
	for {
		select {
		case mask, ok := <-hc.procFrameQueue4:
			if !ok {
				// Channel is closed
				return
			}

			// Crop to bounding box
			croppedImg := hc.cropToBBox(mask.Image, mask.CBox)

			// Zoom needs additional post-crop
			if hc.zoom {
				croppedImg = hc.resizeRGBA(croppedImg, hc.Width, hc.Height)
			}

			// Send to next pipe
			if hc.procFrameQueue5 != nil {
				hc.procFrameQueue5 <- croppedImg
			}
		}
	}
}

/* STEP 6. SAVE FINAL FRAMES */
func (hc *HoloCropper) saveFinalFrames() {
	for {
		select {
		case img, ok := <-hc.procFrameQueue5:
			if !ok {
				// Channel is closed
				return
			}

			// Save frame to temp folder
			hc.copyAndWrite(img)
			hc.currentFrame++
		}
	}
}
