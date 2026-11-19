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
// analysis-frame pixels; dt is frame time (1/FPS). Tuned programmatically by
// replaying the controller against real-corpus bbox streams
// (HIGH-ASPECT-RATIO-FIXES.md §12.7); see tests in framing_test.go.
const (
	// PI gains. No derivative term: the D kick on moving/updated targets was
	// a measured jitter source on the real corpus (§12.7).
	framingPanKp, framingPanKi   = 0.25, 0.002
	framingZoomKp, framingZoomKi = 0.10, 0.001
	framingPanIntegral           = 250.0
	framingZoomIntegral          = 100.0

	panFloorX   = 2.0 // px/frame floors for pan rate limits
	panFloorY   = 1.5
	panCropFrac = 0.05 // crop-relative pan rate: fraction of the crop's own dimension per frame
	panErrGain  = 0.10 // extra pan rate allowance proportional to tracking error
	panCapFrac  = 0.06 // hard cap: never pan faster than this fraction of the crop's own span
	zoomOutMax  = 4.0  // zoom-out (crop grows) is fast: safety
	zoomInMax   = 0.25 // zoom-in (crop shrinks) is slow: prevents breathing

	framingDeadband   = 1.0  // px, noise below this never moves the crop
	targetEveryFrames = 1    // zoom/pan targets recomputed from the EMA bbox every frame
	targetSmoothAlpha = 0.7  // EMA factor applied to pan targets (bbox noise attenuation)
	zoomTargetAlpha   = 0.15 // EMA factor applied to the zoom target: continuous, never quantised

	// Actuator command low-pass: each axis' applied command is an EMA of the
	// controller output, so the crop accelerates in a smooth ramp instead of
	// snapping frame-to-frame (smoothness the original controller was known
	// for, rebuilt structurally - §12.7).
	cmdSmoothAlpha = 0.5

	// Smoothed velocity-lead feedforward on the pan targets: cancels the
	// target-EMA lag so walking subjects are followed without raising gains.
	velLeadFrames = 3
	velAlpha      = 0.5

	// Feet-priority padding for tall outputs: fraction of the crop held
	// between the subject's feet and the crop bottom (video_dsp.go).
	feetPadFraction = 0.05

	// Zoom retarget allowance, gated to gaps larger than this fraction of the
	// frame (reacquire after subject loss). Ordinary target drift stays inside
	// the gentle zoom-in cap and never shows up as a visible zoom step.
	zoomRetargetJumpFrac = 0.25
	zoomRetargetGain     = 0.25
	zoomRetInMax         = 1.5  // px/frame max zoom-in during retarget
	zoomRetOutMax        = 10.0 // px/frame max zoom-out during retarget
	emaBBoxAlpha         = 0.3
	cutJumpFraction      = 0.25 // bbox jump > this fraction of frame => scene cut
	subjectLostAreaFrac  = 0.001

	// Walk mode (§14, landscape -> landscape): a continuous pan leash instead
	// of subject centering. walkAspectLeniency gates it on the output aspect
	// covering the source aspect (output AR wider or equal to the input's),
	// with 10% leniency so slightly-narrower-than-source ratios still qualify
	// (e.g. 1.20 for a 4:3 source, 1.60 for 16:9).
	// walkLeashMarginFrac is the leash margin: the smoothed subject bbox is
	// kept at least this fraction of the crop width from the crop's left or
	// right edge, absorbing EMA/PID tracking lag so the subject visually
	// never grazes or crosses a screen edge.
	walkAspectLeniency  = 0.90
	walkLeashMarginFrac = 0.10
)

func (hc *HoloCropper) calcVideo() {
	// PI controllers: Kd is deliberately 0 (derivative kicks were the corpus
	// jitter source, §12.7).
	panX := NewPIDController(framingPanKp, framingPanKi, 0, framingPanIntegral)
	panY := NewPIDController(framingPanKp, framingPanKi, 0, framingPanIntegral)
	pidZ := NewPIDController(framingZoomKp, framingZoomKi, 0, framingZoomIntegral)

	dt := 1.0 / hc.FPS
	aspect := float64(hc.Width) / float64(hc.Height)

	var emaF [4]float64 // ema bbox: x0,y0,x1,y1 (float to avoid a smoothing dead zone)
	haveEMA := false
	lastSubj := 0

	// plant state (analysis px)
	var cropCx, cropCy, cropH float64
	// target state
	var tgtCx, tgtCy, tgtH float64
	haveTarget := false
	snapTarget := true // first target after start/cut applies directly, not via EMA
	// low-passed actuator commands (see applyAxisPID/applyZoomPID)
	var cmdX, cmdY, cmdZ float64
	// smoothed subject velocity (analysis px/frame) for the pan lead
	var vx, vy, prevEcx, prevEcy float64
	hasPrevEc := false

	var frameW, frameH, cropFloor float64
	// maxCropW bounds the crop width (frame width, plus the black stage
	// padding allowance per side for landscape outputs); landscapePad is the
	// cx clamp allowance (§12.8, video_dsp.go).
	var maxCropW, landscapePad float64
	// walkActive gates the walk-mode pan leash for this render (§14).
	var walkActive bool
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
				// Zoom-in floor: never upscale above source resolution, and
				// respect the policy maxZoom (HIGH-ASPECT-RATIO-FIXES.md Phase 1).
				floor := frameH / hc.maxZoom()
				if hc.srcH > 0 {
					floor = math.Max(floor, float64(hc.Height)*frameH/float64(hc.srcH))
				}
				cropFloor = math.Min(floor, frameH)
				maxCropW, landscapePad = frameW, 0
				if aspect >= 2 {
					// Landscape containment fit (§12.8/§12.9): the maxZoom policy
					// cap yields to the full-subject fit, only the no-upscale
					// floor stays absolute, and the crop may grow past the frame
					// sides up to landscapeMaxCropW (full-frame-height fit) as
					// black stage padding - vertical subject clipping is
					// structurally impossible.
					maxCropW = landscapeMaxCropW(frameW, frameH, aspect)
					landscapePad = (maxCropW - frameW) / 2
					cropFloor = 0
					if hc.srcH > 0 {
						cropFloor = math.Min(float64(hc.Height)*frameH/float64(hc.srcH), frameH)
					}
				}
				// Walk mode (§14): landscape -> landscape pan leash, always on.
				// Gate on the true source aspect (the analysis frame preserves it
				// but rounds to even pixels).
				gateW, gateH := frameW, frameH
				if hc.srcW > 0 && hc.srcH > 0 {
					gateW, gateH = float64(hc.srcW), float64(hc.srcH)
				}
				walkActive = walkModeActive(aspect, gateW, gateH)
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
					cmdX, cmdY, cmdZ = 0, 0, 0
					vx, vy = 0, 0
					hasPrevEc = false
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

			// Subject velocity and continuous targets. The EMA bbox center's
			// smoothed velocity feeds a lead onto the pan targets: the target
			// EMA always trails a moving subject by ~(1-alpha)/alpha frames,
			// and the lead cancels that lag without raising the pan gains.
			// Zoom targets stay continuous: a quantised/stale target produced
			// discrete visible zoom steps on the real corpus (§12.7).
			if haveEMA {
				ecx, ecy := (emaF[0]+emaF[2])/2, (emaF[1]+emaF[3])/2
				if hasPrevEc {
					vx += velAlpha * ((ecx - prevEcx) - vx)
					vy += velAlpha * ((ecy - prevEcy) - vy)
				}
				prevEcx, prevEcy, hasPrevEc = ecx, ecy, true
				if (frame-1)%targetEveryFrames == 0 {
					_, h, cx, cy := hc.aspectLockedTarget(ema, frameW, frameH, aspect)
					if snapTarget || !haveTarget {
						tgtH, tgtCx, tgtCy = h, cx, cy
						snapTarget = false
						haveTarget = true
					} else {
						tgtH += zoomTargetAlpha * (h - tgtH)
						tgtCx += targetSmoothAlpha * (cx - tgtCx)
						tgtCy += targetSmoothAlpha * (cy - tgtCy)
					}
				}
			}
			if lost {
				// Hold the last crop center, open toward full frame at rate
				// limit. Replaces the old hard panAdj=0 snap (F5/gate reset).
				// For landscape, "full frame" is the widest aspect-locked crop
				// the padding cap allows (§12.8).
				tgtH = frameH
				if tgtH*aspect > maxCropW {
					tgtH = maxCropW / aspect
				}
			}

			// Grounded (landscape, §12.10): the EMA bbox bottom sits in the
			// floor-proximity band, i.e. the subject stands on the stage
			// floor even if the mask missed the feet (see ground snap in
			// aspectLockedTarget). Used for the hard floor pin below.
			grounded := aspect >= 2 && haveEMA &&
				frameH-emaF[3] <= landscapeGroundSnapFrac*frameH

			// Plant updates. Zoom first so pan spans see this frame's crop
			// size; every plant is PI -> low-passed command -> crop-relative
			// rate cap, so all crop motion is a smooth ramp (no bang-bang
			// switching, §12.7). The velocity lead kills the walk lag.
			cropH = hc.applyZoomPID(pidZ, &cmdZ, cropH, tgtH, dt, frameH, cropFloor)
			cropW := cropH * aspect
			if cropW > maxCropW {
				// Keep the aspect lock when the crop hits the padded width cap
				cropW = maxCropW
				cropH = cropW / aspect
			}
			// Walk mode (§14): with a valid leash window the pan target is the
			// deadzone target (freeze while the subject has slack, edge-chase
			// when the walk exhausts it) and carries no velocity lead. Only a
			// subject wider than the crop minus two margins - no room to leash
			// - falls back to the centered target + lead, which keeps her
			// in-bounds exactly as before.
			tgtX := tgtCx + velLeadFrames*vx
			walkTgt := 0.0
			if walkActive && haveEMA && !lost {
				if leash, ok := walkLeashTarget(cropCx, emaF, cropW); ok {
					tgtX, walkTgt = leash, leash
				}
			}
			cropCx = hc.applyAxisPID(panX, &cmdX, cropCx, tgtX, dt, cropW, panFloorX)
			if grounded {
				// Hard floor pin: with the ground snap the feet rest on the
				// frame floor, so pin the crop bottom there exactly instead
				// of chasing a floor-derived target through the pan PID.
				// The feet line cannot lag the zoom plant or target EMA.
				cropCy = frameH - cropH/2
				cmdY = 0
			} else {
				cropCy = hc.applyAxisPID(panY, &cmdY, cropCy, tgtCy+velLeadFrames*vy, dt, cropH, panFloorY)
			}

			// Clamp plant to frame: vertical always fully inside; horizontally
			// the crop may extend past the frame edges by up to landscapePad
			// per side (limited black stage padding for extreme landscape
			// outputs, §12.8). Portrait/square keep the old fully in-frame
			// clamp (landscapePad = 0 there, F3/F4).
			cropCx = clampF(cropCx, cropW/2-landscapePad, frameW-cropW/2+landscapePad)
			cropCy = clampF(cropCy, cropH/2, frameH-cropH/2)

			dbg.line(frame, mask.Subj, bbox, ema, cropCx, cropCy, cropW, cropH, tgtH, walkTgt)

			// Send to next pipe
			if hc.procFrameQueue4 != nil {
				hc.procFrameQueue4 <- &ImageCropMask{CBox: hc.cropRectFromFraming(cropCx, cropCy, cropW, cropH, landscapePad, bounds), Image: mask.Image, zoom: hc.zoom}
			}
		}
	}
}

// applyAxisPID moves pos toward target: PI output -> low-passed actuator
// command (cmd carries the EMA across frames, reset on cuts) -> crop-relative
// rate cap. The low-pass is what makes crop motion a smooth ramp instead of
// frame-to-frame snapping (§12.7). span is the crop's own extent on this axis:
// allowed speed is a fraction of it plus an error-proportional allowance,
// hard-capped at panCapFrac of the crop, so wide crops pan fast in pixels and
// narrow slit crops stay smooth. Target callers may add a velocity lead.
func (hc *HoloCropper) applyAxisPID(p *PIDController, cmd *float64, pos, target, dt, span, floorRate float64) float64 {
	error := target - pos
	if math.Abs(error) < framingDeadband {
		return pos
	}
	u := p.Step(pos, target, dt)
	*cmd += cmdSmoothAlpha * (u - *cmd)
	limit := clampF(panCropFrac*span+panErrGain*math.Abs(error), floorRate, panCapFrac*span)
	*cmd = clampF(*cmd, -limit, limit)
	return pos + *cmd
}

// walkModeActive decides when the walk-mode pan leash applies
// (HIGH-ASPECT-RATIO-FIXES.md §14): landscape -> landscape only - the source
// is landscape, the output is landscape, and the output aspect covers the
// source aspect up to walkAspectLeniency, so the output X axis is wide enough
// to show the source's full horizontal walk range. Portrait and square
// outputs are never touched: their close crops have no slack to walk in anyway.
func walkModeActive(aspect, srcW, srcH float64) bool {
	if aspect <= 1 || srcW <= srcH {
		return false
	}
	return aspect >= (srcW/srcH)*walkAspectLeniency
}

// walkLeashTarget is the continuous deadzone pan target of walk mode. The
// crop may keep its current x as long as the subject bbox plus the leash
// margin fits inside the crop: cropCx inside [lo, hi] yields target == cropCx,
// freezing the crop so the walking subject visibly travels across the output.
// Once the walk exhausts the slack the target becomes the binding window edge,
// which moves continuously with the subject, so the crop follows her while the
// margin keeps her clear of the crop edge. A subject wider than the crop minus
// two margins invalidates the window (leashed=false): the caller then falls
// back to the centered target with velocity lead. Leash targets never carry
// the velocity lead - the window edge already tracks the subject, and a lead
// on top of it would overshoot and drift the crop.
func walkLeashTarget(cropCx float64, ema [4]float64, cropW float64) (target float64, leashed bool) {
	m := walkLeashMarginFrac * cropW
	lo := ema[2] + m - cropW/2 // cx must stay >= this to keep the subject's right edge (+margin) inside
	hi := ema[0] - m + cropW/2 // cx must stay <= this to keep the subject's left edge (-margin) inside
	if lo > hi {
		return cropCx, false
	}
	return clampF(cropCx, lo, hi), true
}

// applyZoomPID drives crop height toward the target: PI output -> low-passed
// command -> asymmetric caps. Zoom-out (crop grows) is fast: safety; zoom-in
// (crop shrinks) is deliberately slow (zoomInMax) so the crop never "breathes".
// Fast proportional pursuit is gated to retarget gaps larger than
// zoomRetargetJumpFrac of the frame (reacquiring after subject loss); ordinary
// target drift stays inside the gentle cap and never shows up as a visible
// zoom step (§12.7).
func (hc *HoloCropper) applyZoomPID(p *PIDController, cmd *float64, cropH, target, dt, frameH, floor float64) float64 {
	error := target - cropH
	if math.Abs(error) < framingDeadband {
		return cropH
	}
	u := p.Step(cropH, target, dt)
	*cmd += cmdSmoothAlpha * (u - *cmd)
	limIn, limOut := zoomInMax, zoomOutMax
	if math.Abs(error) > zoomRetargetJumpFrac*frameH {
		limIn = math.Min(zoomRetInMax, math.Max(zoomInMax, zoomRetargetGain*math.Abs(error)))
		limOut = math.Min(zoomRetOutMax, math.Max(zoomOutMax, zoomRetargetGain*math.Abs(error)))
	}
	*cmd = clampF(*cmd, -limIn, limOut)
	return clampF(cropH+*cmd, floor, frameH)
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

			// Crop to the framed region (out-of-frame sides compose as black
			// stage padding for landscape, §12.8)
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
