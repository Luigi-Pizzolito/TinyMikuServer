package holocropper

import (
	"math"
)

// Framing controller machinery: the gains, the axis/zoom PID actuators and
// the walk-mode leash. framedecider.go drives them once per frame, and after
// story C3 removed the fused pipeline this is the only home of the framing
// behaviour the real corpus was tuned against (HIGH-ASPECT-RATIO-FIXES.md
// §12.7, §14).

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
