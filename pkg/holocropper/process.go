package holocropper

import (
	"image"
	"math"
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
	// Otsu threshold hysteresis: raw per-frame Otsu flickers with scene
	// brightness and feeds that flicker straight into the framing controller.
	// The effective threshold is clamped to +/-10 gray levels per frame
	// (HIGH-ASPECT-RATIO-FIXES.md Phase 3).
	lastThr := -1.0

	for {
		select {
		case mask, ok := <-hc.procFrameQueue2:
			if !ok {
				// Channel is closed
				return
			}

			// Apply Otsu threshold with hysteresis to the received image
			newThr := float64(hc.otsuThreshold(mask.Mask))
			if lastThr < 0 {
				lastThr = newThr
			} else {
				lastThr = clampF(newThr, lastThr-10, lastThr+10)
			}
			binaryImg := hc.applyThreshold(mask.Mask, uint8(lastThr))

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
					// Append to main subjects list
					subjects = append(subjects, bbox)
				}
			}

			subjects_bbox := hc.totalBoundingBox(subjects)

			// Send to next pipe
			if hc.procFrameQueue3 != nil {
				hc.procFrameQueue3 <- &ImageGrayBoundMask{Mask: binaryImg, Image: mask.Image, Bbox: subjects_bbox, Subj: len(subjects)}
			}
		}
	}
}

/* STEP 4. CALCULATE VIDEO CROP */

// Framing controller gains and limits. All position/size values are in
// analysis-frame pixels; dt is frame time (1/FPS). Tuned via the simulation
// in HIGH-ASPECT-RATIO-FIXES.md §11; see tests in framing_test.go.
const (
	framingPanKp, framingPanKi, framingPanKd = 0.12, 0.0005, 0.02
	framingZoomKp, framingZoomKi, framingZoomKd = 0.25, 0.001, 0.05
	framingPanIntegral  = 250.0
	framingZoomIntegral = 100.0

	panRateLimitX  = 2.0  // px/frame, analysis frame px
	panRateLimitY  = 1.5
	zoomRateOutMax = 4.0  // zoom-out (crop grows) is fast: safety
	zoomRateInMax  = 0.5  // zoom-in (crop shrinks) is slow: prevents breathing

	framingDeadband     = 1.5  // px, noise below this never moves the crop (real-corpus tuning: 1px-out hunting)
	targetEveryFrames   = 6    // zoom/pan target recomputed from EMA every N frames
	targetSmoothAlpha   = 0.2  // EMA factor applied to pan targets (bbox noise -> target noise attenuation)
	emaBBoxAlpha        = 0.2
	cutJumpFraction     = 0.25 // bbox jump > this fraction of frame => scene cut
	retargetJumpFrac    = 0.08 // error > this fraction => allow fast retarget
	retargetGain        = 0.35 // fraction of the gap covered per frame when retargeting fast
	subjectLostAreaFrac = 0.001
)

func (hc *HoloCropper) calcVideo() {
	panX := NewPIDController(framingPanKp, framingPanKi, framingPanKd, framingPanIntegral)
	panY := NewPIDController(framingPanKp, framingPanKi, framingPanKd, framingPanIntegral)
	pidZ := NewPIDController(framingZoomKp, framingZoomKi, framingZoomKd, framingZoomIntegral)

	dt := 1.0 / hc.FPS
	aspect := float64(hc.Width) / float64(hc.Height)

	var emaF [4]float64 // ema bbox: x0,y0,x1,y1 (float to avoid a smoothing dead zone)
	haveEMA := false
	lastSubj := 0

	// plant state (analysis px)
	var cropCx, cropCy, cropH float64
	// target state
	var tgtCx, tgtCy, tgtH, quantH float64
	haveTarget := false
	snapTarget := true // first target after start/cut applies directly, not via EMA

	var frameW, frameH, cropFloor float64
	initialized := false
	frame := 0

	dbg := newBoxDebugger()
	defer dbg.close()

	for {
		select {
		case mask, ok := <-hc.procFrameQueue3:
			if !ok {
				// Channel is closed
				return
			}
			frame++

			bounds := mask.Image.Bounds()
			if !initialized {
				frameW, frameH = float64(bounds.Dx()), float64(bounds.Dy())
				cropCx, cropCy, cropH = frameW/2, frameH/2, frameH
				tgtCx, tgtCy, tgtH = cropCx, cropCy, cropH
				quantH = cropH
				// Zoom-in floor: never upscale above source resolution, and
				// respect the policy maxZoom (HIGH-ASPECT-RATIO-FIXES.md Phase 1).
				floor := frameH / hc.maxZoom()
				if hc.srcH > 0 {
					floor = math.Max(floor, float64(hc.Height)*frameH/float64(hc.srcH))
				}
				cropFloor = math.Min(floor, frameH)
				initialized = true
			}

			bbox := mask.Bbox
			frameArea := float64(bounds.Dx() * bounds.Dy())
			lost := bbox.Empty() || mask.Subj == 0 ||
				float64(bbox.Dx()*bbox.Dy())/frameArea < subjectLostAreaFrac

			if !lost {
				isCut := haveEMA && (math.Abs(emaF[0]-float64(bbox.Min.X)) > cutJumpFraction*frameW ||
					math.Abs(emaF[1]-float64(bbox.Min.Y)) > cutJumpFraction*frameH ||
					mask.Subj-lastSubj >= 3 || lastSubj-mask.Subj >= 3)
				if !haveEMA || isCut {
					emaF = [4]float64{float64(bbox.Min.X), float64(bbox.Min.Y), float64(bbox.Max.X), float64(bbox.Max.Y)}
					panX.Reset()
					panY.Reset()
					pidZ.Reset()
					snapTarget = true
				} else {
					emaF[0] += emaBBoxAlpha * (float64(bbox.Min.X) - emaF[0])
					emaF[1] += emaBBoxAlpha * (float64(bbox.Min.Y) - emaF[1])
					emaF[2] += emaBBoxAlpha * (float64(bbox.Max.X) - emaF[2])
					emaF[3] += emaBBoxAlpha * (float64(bbox.Max.Y) - emaF[3])
				}
				haveEMA = true
				lastSubj = mask.Subj
			}

			ema := image.Rect(int(emaF[0]), int(emaF[1]), int(emaF[2]), int(emaF[3]))

			// Target update (every N frames, quantised with hysteresis).
			// Pan targets pass through an EMA (targetSmoothAlpha) so bbox
			// noise does not translate directly into crop motion; cuts snap.
			if haveEMA && frame%targetEveryFrames == 1 {
				_, h, cx, cy := hc.aspectLockedTarget(ema, frameW, frameH, aspect)
				qStep := 0.01 * frameH
				qh := math.Round(h/qStep) * qStep
				if math.Abs(qh-quantH) < qStep {
					qh = quantH
				}
				// Never let hysteresis keep the crop tighter than the raw fit.
				if qh < h {
					qh = h
				}
				quantH = qh
				tgtH = qh
				if snapTarget || !haveTarget {
					tgtCx, tgtCy = cx, cy
					snapTarget = false
					haveTarget = true
				} else {
					tgtCx += targetSmoothAlpha * (cx - tgtCx)
					tgtCy += targetSmoothAlpha * (cy - tgtCy)
				}
			}
			if lost {
				// Hold the last crop center, open toward full frame at rate
				// limit. Replaces the old hard panAdj=0 snap (F5/gate reset).
				tgtH = frameH
			}

			// Plant updates.
			cropCx = hc.applyAxisPID(panX, cropCx, tgtCx, dt, panRateLimitX, frameW)
			cropCy = hc.applyAxisPID(panY, cropCy, tgtCy, dt, panRateLimitY, frameH)
			cropH = hc.applyZoomPID(pidZ, cropH, tgtH, dt, frameH, cropFloor)

			// Clamp plant to frame so the crop never leaves the frame
			// (no out-of-frame transparent padding, HIGH-ASPECT-RATIO-FIXES.md F3/F4).
			cropW := cropH * aspect
			if cropW > frameW {
				cropW = frameW
			}
			cropCx = clampF(cropCx, cropW/2, frameW-cropW/2)
			cropCy = clampF(cropCy, cropH/2, frameH-cropH/2)

			dbg.line(frame, mask.Subj, bbox, ema, cropCx, cropCy, cropW, cropH, tgtH)

			// Send to next pipe
			if hc.procFrameQueue4 != nil {
				hc.procFrameQueue4 <- &ImageCropMask{CBox: hc.cropRectFromFraming(cropCx, cropCy, cropW, cropH, bounds), Image: mask.Image, zoom: hc.zoom}
			}
		}
	}
}

// applyAxisPID moves pos toward target with a rate limit; large errors (scene
// cuts, framing loss) get a fast retarget allowance so the crop recovers in a
// few frames instead of several seconds.
func (hc *HoloCropper) applyAxisPID(p *PIDController, pos, target, dt, rateLimit, frameSpan float64) float64 {
	error := target - pos
	if math.Abs(error) < framingDeadband {
		return pos
	}
	u := p.Step(pos, target, dt)
	limit := rateLimit
	if math.Abs(error) > retargetJumpFrac*frameSpan {
		limit = math.Max(rateLimit, retargetGain*math.Abs(error))
	}
	return pos + clampF(u, -limit, limit)
}

// applyZoomPID drives crop height toward the target with asymmetric rate
// limits: zoom-out fast (never lose the subject), zoom-in slow (no breathing).
func (hc *HoloCropper) applyZoomPID(p *PIDController, cropH, target, dt, frameH, floor float64) float64 {
	error := target - cropH
	if math.Abs(error) < framingDeadband {
		return cropH
	}
	u := p.Step(cropH, target, dt)
	lo, hi := -zoomRateInMax, zoomRateOutMax
	if math.Abs(error) > retargetJumpFrac*frameH {
		lo = -math.Max(zoomRateInMax, retargetGain*math.Abs(error))
		hi = math.Max(zoomRateOutMax, retargetGain*math.Abs(error))
	}
	return clampF(cropH+clampF(u, lo, hi), floor, frameH)
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

			// Crop to bounding box (always clamped inside the frame)
			croppedImg := hc.cropToBBox(mask.Image, mask.CBox)

			// Scale the framed region to fill the exact output size
			if hc.zoom {
				croppedImg = hc.fillResizeRGBA(croppedImg)
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
