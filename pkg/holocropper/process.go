package holocropper

import (
	"image"
	"math"
)

/* STEP 1. Read Video */

func (hc *HoloCropper) readVideo() {
	// Set frame buffer
	w, h := hc.video.Width(), hc.video.Height()
	vbuffer := image.NewRGBA(image.Rect(0, 0, w, h))
	hc.video.SetFrameBuffer(vbuffer.Pix)

	// Start reading and piping raw frames. vidio decodes into vbuffer.Pix on
	// every Read, so each frame is copied out before it enters the pipeline:
	// downstream stages then own their frames exclusively, and the next Read
	// cannot mutate a frame already in flight (S4 shared-buffer race, the
	// frame-aliasing bug the original CLI only got away with by luck).
	for hc.video.Read() {
		if hc.procFrameQueue1 != nil {
			frame := image.NewRGBA(image.Rect(0, 0, w, h))
			copy(frame.Pix, vbuffer.Pix)
			hc.procFrameQueue1 <- frame
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
			maskFrame := convertToMonochrome(img)
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
			newThr := float64(otsuThreshold(mask.Mask))
			if lastThr < 0 {
				lastThr = newThr
			} else {
				lastThr = clampF(newThr, lastThr-10, lastThr+10)
			}
			binaryImg := applyThreshold(mask.Mask, uint8(lastThr))

			// Subject extraction: the shared stage (video_dsp.go) used by the
			// embeddable detect pass too, so pipeline and detection stream
			// always agree frame for frame.
			subjects_bbox, subj := subjectsFromBinary(binaryImg)

			// Send to next pipe
			if hc.procFrameQueue3 != nil {
				hc.procFrameQueue3 <- &ImageGrayBoundMask{Mask: binaryImg, Image: mask.Image, Bbox: subjects_bbox, Subj: subj}
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
	// STEP 4 delegates to the framingDecider replay engine (framedecider.go):
	// the live pipeline and the embeddable Plan pass share one controller
	// implementation, so replayed framing is bit-identical to live framing.
	d := newFramingDecider(hc.framingParams(), float64(hc.Width)/float64(hc.Height), 1.0/hc.FPS, hc.srcW, hc.srcH)

	dbg := newBoxDebugger()
	defer dbg.close()

	frame := 0
	for {
		select {
		case mask, ok := <-hc.procFrameQueue3:
			if !ok {
				// Channel is closed
				return
			}
			frame++

			out := d.step(frame, mask.Bbox, mask.Subj, mask.Image.Bounds())

			dbg.line(frame, mask.Subj, mask.Bbox, out.EMA, out.CropCx, out.CropCy, out.CropW, out.CropH, out.TargetH, out.WalkTgt)

			// Send to next pipe
			if hc.procFrameQueue4 != nil {
				hc.procFrameQueue4 <- &ImageCropMask{CBox: out.CropRect, Image: mask.Image, zoom: hc.zoom}
			}
		}
	}
}

// applyAxisPID is the pipeline wrapper of applyAxisPIDCalc (used by the CLI
// path and the framing tests).
func (hc *HoloCropper) applyAxisPID(p *PIDController, cmd *float64, pos, target, dt, span, floorRate float64) float64 {
	return applyAxisPIDCalc(p, cmd, pos, target, dt, span, floorRate)
}

// applyAxisPIDCalc moves pos toward target: PI output -> low-passed actuator
// command (cmd carries the EMA across frames, reset on cuts) -> crop-relative
// rate cap. The low-pass is what makes crop motion a smooth ramp instead of
// frame-to-frame snapping (§12.7). span is the crop's own extent on this axis:
// allowed speed is a fraction of it plus an error-proportional allowance,
// hard-capped at panCapFrac of the crop, so wide crops pan fast in pixels and
// narrow slit crops stay smooth. Target callers may add a velocity lead.
func applyAxisPIDCalc(p *PIDController, cmd *float64, pos, target, dt, span, floorRate float64) float64 {
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

// applyZoomPID is the pipeline wrapper of applyZoomPIDCalc (used by the CLI
// path and the framing tests).
func (hc *HoloCropper) applyZoomPID(p *PIDController, cmd *float64, cropH, target, dt, frameH, floor float64) float64 {
	return applyZoomPIDCalc(p, cmd, cropH, target, dt, frameH, floor)
}

// applyZoomPIDCalc drives crop height toward the target: PI output ->
// low-passed command -> asymmetric caps. Zoom-out (crop grows) is fast:
// safety; zoom-in (crop shrinks) is deliberately slow (zoomInMax) so the crop
// never "breathes". Fast proportional pursuit is gated to retarget gaps
// larger than zoomRetargetJumpFrac of the frame (reacquiring after subject
// loss); ordinary target drift stays inside the gentle cap and never shows up
// as a visible zoom step (§12.7).
func applyZoomPIDCalc(p *PIDController, cmd *float64, cropH, target, dt, frameH, floor float64) float64 {
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
			hc.currentFrame.Add(1)
		}
	}
}
