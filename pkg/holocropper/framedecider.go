package holocropper

import (
	"image"
	"math"
)

/* framingDecider — the framing controller extracted as a replayable state
machine (HOLOCROPPER-API-PLAN.md story A3, the replay engine).

The Plan pass replays it over a cached DetectionStream. step() transcribes
the fused calcVideo loop body verbatim - the loop story C3 deleted - same
order, same arithmetic, same state, so a replay produces exactly the framing
the real corpus was calibrated against; TestReplayParityVsDebugCSV pins that
against the pipeline's own golden decision CSV. Pure computation only: no
channels, no images, no ffmpeg, no wall-clock.

The controller constants, PI gains and rate caps live in process.go: they were
tuned against the loop body step() transcribes (HIGH-ASPECT-RATIO-FIXES.md
§12.7).
*/

// framingParams are the framing-relevant render settings, free of any
// HoloCropper instance (the embeddable API paths have no instance).
type framingParams struct {
	Margin  float64 // bbox -> crop padding factor (zoomMargin, default 1.12)
	MaxZoom float64 // zoom-in policy cap (maxZoom, default 2.0)
	OutH    int     // output height: the no-upscale floor reference (0 = unknown)
	SrcH    int     // true source video height, 0 = unknown
}

// framingOutput is one frame of framing state in analysis pixels. The plant
// and tracking values mirror the debug CSV columns exactly (ema_*, crop_*,
// target_h, walk_tgt); CropRect is the integer crop rect the render pass
// cuts with — for landscape containment it may spill past the frame edges
// by LandscapePad (§12.8), composed as black stage padding by cropToBBoxCalc.
type framingOutput struct {
	EMA         image.Rectangle // rounded smoothed bbox (zero before the first subject)
	CropCx      float64
	CropCy      float64
	CropW       float64
	CropH       float64
	TargetH     float64 // zoom target applied this frame (CSV target_h)
	WalkTgt     float64 // walk leash target this frame; 0 = inactive (§14)
	SubjectLost bool    // no usable bbox this frame
	SceneCut    bool    // bbox jump / subject-count break reset the tracking
	Grounded    bool    // landscape floor pin applied this frame (§12.10)
	CropRect    image.Rectangle

	// Overlay telemetry (plan Milestone G2): the framing the controllers
	// COMMANDED this frame, which containment then may not grant.
	// Cmd* is the box before the padded-width fold and the frame clamps, so
	// the gap between it and Crop* is exactly what clamping took away.
	// PanX/PanY are the setpoints the axis PIDs were actually fed (a grounded
	// frame has no vertical setpoint: the floor pin replaced it, so PanY is
	// that pin). Zoom* are frame-height-relative zoom factors: 1 = full frame.
	CmdCx, CmdCy, CmdW, CmdH float64
	PanX, PanY               float64
	ZoomCurrent, ZoomTarget  float64
}

// framingDecider is the whole controller plant + tracking state. It is not
// safe for concurrent use (one stream in, one decision sequence out); each
// render/replay gets a fresh instance.
type framingDecider struct {
	params framingParams
	aspect float64 // output aspect (W/H)
	dt     float64 // controller time step: frame time (1/FPS), never wall-clock
	srcW   float64 // true source dims for the walk gate (0 = use analysis frame)
	srcH   float64

	// per-render geometry derived from the analysis frame on the first step
	initialized  bool
	frameW       float64
	frameH       float64
	cropFloor    float64 // zoom-in floor: crop height never below this
	maxCropW     float64 // crop width cap (frame width + landscape stage padding)
	landscapePad float64 // per-side cap on out-of-frame stage padding (§12.8)
	walkActive   bool    // render-level walk gate (§14)

	// PI controllers + their low-passed actuator commands (reset on cuts)
	panX, panY, pidZ *PIDController
	cmdX, cmdY, cmdZ float64

	// subject tracking
	emaF       [4]float64 // ema bbox: x0,y0,x1,y1 (float to avoid a smoothing dead zone)
	haveEMA    bool
	lastSubj   int
	haveTarget bool
	snapTarget bool // first target after start/cut applies directly, not via EMA
	tgtCx      float64
	tgtCy      float64
	tgtH       float64
	vx, vy     float64 // smoothed subject velocity (px/frame) for the pan lead
	prevEcx    float64
	prevEcy    float64
	hasPrevEc  bool

	// plant state (analysis px)
	cropCx float64
	cropCy float64
	cropH  float64
}

// newFramingDecider builds a fresh controller for one render/replay.
// aspect/dt come from the resolved output settings; srcW/srcH are the true
// source dimensions (0 falls back to the analysis frame for the walk gate).
func newFramingDecider(params framingParams, aspect, dt float64, srcW, srcH int) *framingDecider {
	return &framingDecider{
		params:     params,
		aspect:     aspect,
		dt:         dt,
		srcW:       float64(srcW),
		srcH:       float64(srcH),
		panX:       NewPIDController(framingPanKp, framingPanKi, 0, framingPanIntegral),
		panY:       NewPIDController(framingPanKp, framingPanKi, 0, framingPanIntegral),
		pidZ:       NewPIDController(framingZoomKp, framingZoomKi, 0, framingZoomIntegral),
		snapTarget: true,
	}
}

// initialize derives the per-render geometry from the first analysis frame,
// exactly as calcVideo did on its first queued mask.
func (d *framingDecider) initialize(bounds image.Rectangle) {
	d.frameW, d.frameH = float64(bounds.Dx()), float64(bounds.Dy())
	d.cropCx, d.cropCy, d.cropH = d.frameW/2, d.frameH/2, d.frameH
	d.tgtCx, d.tgtCy, d.tgtH = d.cropCx, d.cropCy, d.cropH

	// Zoom-in floor: never upscale above source resolution, and respect the
	// policy maxZoom (HIGH-ASPECT-RATIO-FIXES.md Phase 1).
	floor := d.frameH / d.params.MaxZoom
	if d.params.SrcH > 0 {
		floor = math.Max(floor, float64(d.params.OutH)*d.frameH/float64(d.params.SrcH))
	}
	d.cropFloor = math.Min(floor, d.frameH)

	d.maxCropW, d.landscapePad = d.frameW, 0
	if d.aspect >= 2 {
		// Landscape containment fit (§12.8/§12.9): the maxZoom policy cap
		// yields to the full-subject fit, only the no-upscale floor stays
		// absolute, and the crop may grow past the frame sides up to
		// landscapeMaxCropW (full-frame-height fit) as black stage padding -
		// vertical subject clipping is structurally impossible.
		d.maxCropW = landscapeMaxCropW(d.frameW, d.frameH, d.aspect)
		d.landscapePad = (d.maxCropW - d.frameW) / 2
		d.cropFloor = 0
		if d.params.SrcH > 0 {
			d.cropFloor = math.Min(float64(d.params.OutH)*d.frameH/float64(d.params.SrcH), d.frameH)
		}
	}

	// Walk mode (§14): landscape -> landscape pan leash, always on. Gate on
	// the true source aspect (the analysis frame preserves it but rounds to
	// even pixels).
	gateW, gateH := d.frameW, d.frameH
	if d.srcW > 0 && d.srcH > 0 {
		gateW, gateH = d.srcW, d.srcH
	}
	d.walkActive = walkModeActive(d.aspect, gateW, gateH)
	d.initialized = true
}

// step consumes one frame's detection output (master subject bbox + subject
// count) and advances the plant. The body mirrors the fused calcVideo loop
// iteration verbatim.
func (d *framingDecider) step(frame int, bbox image.Rectangle, subjects int, bounds image.Rectangle) framingOutput {
	if !d.initialized {
		d.initialize(bounds)
	}

	frameArea := float64(bounds.Dx() * bounds.Dy())
	lost := bbox.Empty() || subjects == 0 ||
		float64(bbox.Dx()*bbox.Dy())/frameArea < subjectLostAreaFrac

	// Subject EMA: reset on scene cuts (bbox jump or subject-count break),
	// low-pass otherwise. sceneCut reports an actual cut, not the first
	// subject acquisition.
	sceneCut := false
	if !lost {
		isCut := d.haveEMA && (math.Abs(d.emaF[0]-float64(bbox.Min.X)) > cutJumpFraction*d.frameW ||
			math.Abs(d.emaF[1]-float64(bbox.Min.Y)) > cutJumpFraction*d.frameH ||
			subjects-d.lastSubj >= 3 || d.lastSubj-subjects >= 3)
		if !d.haveEMA || isCut {
			sceneCut = isCut
			d.emaF = [4]float64{float64(bbox.Min.X), float64(bbox.Min.Y), float64(bbox.Max.X), float64(bbox.Max.Y)}
			d.panX.Reset()
			d.panY.Reset()
			d.pidZ.Reset()
			d.snapTarget = true
			d.cmdX, d.cmdY, d.cmdZ = 0, 0, 0
			d.vx, d.vy = 0, 0
			d.hasPrevEc = false
		} else {
			d.emaF[0] += emaBBoxAlpha * (float64(bbox.Min.X) - d.emaF[0])
			d.emaF[1] += emaBBoxAlpha * (float64(bbox.Min.Y) - d.emaF[1])
			d.emaF[2] += emaBBoxAlpha * (float64(bbox.Max.X) - d.emaF[2])
			d.emaF[3] += emaBBoxAlpha * (float64(bbox.Max.Y) - d.emaF[3])
		}
		d.haveEMA = true
		d.lastSubj = subjects
	}

	ema := image.Rect(int(d.emaF[0]), int(d.emaF[1]), int(d.emaF[2]), int(d.emaF[3]))

	// Subject velocity and continuous targets. The EMA bbox center's smoothed
	// velocity feeds a lead onto the pan targets: the target EMA always
	// trails a moving subject by ~(1-alpha)/alpha frames, and the lead
	// cancels that lag without raising the pan gains. Zoom targets stay
	// continuous: a quantised/stale target produced discrete visible zoom
	// steps on the real corpus (§12.7).
	if d.haveEMA {
		ecx, ecy := (d.emaF[0]+d.emaF[2])/2, (d.emaF[1]+d.emaF[3])/2
		if d.hasPrevEc {
			d.vx += velAlpha * ((ecx - d.prevEcx) - d.vx)
			d.vy += velAlpha * ((ecy - d.prevEcy) - d.vy)
		}
		d.prevEcx, d.prevEcy, d.hasPrevEc = ecx, ecy, true
		if (frame-1)%targetEveryFrames == 0 {
			_, h, cx, cy := aspectLockedTargetCalc(d.params, ema, d.frameW, d.frameH, d.aspect)
			if d.snapTarget || !d.haveTarget {
				d.tgtH, d.tgtCx, d.tgtCy = h, cx, cy
				d.snapTarget = false
				d.haveTarget = true
			} else {
				d.tgtH += zoomTargetAlpha * (h - d.tgtH)
				d.tgtCx += targetSmoothAlpha * (cx - d.tgtCx)
				d.tgtCy += targetSmoothAlpha * (cy - d.tgtCy)
			}
		}
	}
	if lost {
		// Hold the last crop center, open toward full frame at rate limit.
		// For landscape, "full frame" is the widest aspect-locked crop the
		// padding cap allows (§12.8).
		d.tgtH = d.frameH
		if d.tgtH*d.aspect > d.maxCropW {
			d.tgtH = d.maxCropW / d.aspect
		}
	}

	// Grounded (landscape, §12.10 - and square too since 793, the same floor
	// law for 1:1 outputs): the EMA bbox bottom sits in the floor-proximity
	// band, i.e. the subject stands on the stage floor even if the mask missed
	// the feet. Used for the hard floor pin below.
	grounded := (d.aspect >= 2 || d.aspect == 1) && d.haveEMA &&
		d.frameH-d.emaF[3] <= landscapeGroundSnapFrac*d.frameH

	// Plant updates. Zoom first so pan spans see this frame's crop size;
	// every plant is PI -> low-passed command -> crop-relative rate cap, so
	// all crop motion is a smooth ramp (no bang-bang switching, §12.7). The
	// velocity lead kills the walk lag.
	d.cropH = applyZoomPIDCalc(d.pidZ, &d.cmdZ, d.cropH, d.tgtH, d.dt, d.frameH, d.cropFloor)
	cropW := d.cropH * d.aspect
	cmdW, cmdH := cropW, d.cropH // commanded geometry, before the padded-width fold
	if cropW > d.maxCropW {
		// Keep the aspect lock when the crop hits the padded width cap
		cropW = d.maxCropW
		d.cropH = cropW / d.aspect
	}
	// Walk mode (§14): with a valid leash window the pan target is the
	// deadzone target (freeze while the subject has slack, edge-chase when
	// the walk exhausts it) and carries no velocity lead. Only a subject
	// wider than the crop minus two margins - no room to leash - falls back
	// to the centered target + lead, which keeps her in-bounds exactly as
	// before.
	tgtX := d.tgtCx + velLeadFrames*d.vx
	tgtY := d.tgtCy + velLeadFrames*d.vy
	walkTgt := 0.0
	if d.walkActive && d.haveEMA && !lost {
		if leash, ok := walkLeashTarget(d.cropCx, d.emaF, cropW); ok {
			tgtX, walkTgt = leash, leash
		}
	}
	d.cropCx = applyAxisPIDCalc(d.panX, &d.cmdX, d.cropCx, tgtX, d.dt, cropW, panFloorX)
	if grounded {
		// Hard floor pin: with the ground snap the feet rest on the frame
		// floor, so pin the crop bottom there exactly instead of chasing a
		// floor-derived target through the pan PID. The feet line cannot lag
		// the zoom plant or target EMA.
		d.cropCy = d.frameH - d.cropH/2
		d.cmdY = 0
		tgtY = d.cropCy // no vertical setpoint this frame: the pin is what the plant holds
	} else {
		d.cropCy = applyAxisPIDCalc(d.panY, &d.cmdY, d.cropCy, tgtY, d.dt, d.cropH, panFloorY)
	}
	cmdCx, cmdCy := d.cropCx, d.cropCy // commanded centre, before the containment clamps below

	// Clamp plant to frame: vertical always fully inside; horizontally the
	// crop may extend past the frame edges by up to landscapePad per side
	// (limited black stage padding for extreme landscape outputs, §12.8).
	// Portrait/square keep the fully in-frame clamp (landscapePad = 0).
	d.cropCx = clampF(d.cropCx, cropW/2-d.landscapePad, d.frameW-cropW/2+d.landscapePad)
	d.cropCy = clampF(d.cropCy, d.cropH/2, d.frameH-d.cropH/2)

	return framingOutput{
		EMA:         ema,
		CropCx:      d.cropCx,
		CropCy:      d.cropCy,
		CropW:       cropW,
		CropH:       d.cropH,
		TargetH:     d.tgtH,
		WalkTgt:     walkTgt,
		SubjectLost: lost,
		SceneCut:    sceneCut,
		Grounded:    grounded,
		CropRect:    cropRectFromFramingCalc(d.cropCx, d.cropCy, cropW, d.cropH, d.landscapePad, bounds),

		CmdCx: cmdCx, CmdCy: cmdCy, CmdW: cmdW, CmdH: cmdH,
		PanX: tgtX, PanY: tgtY,
		ZoomCurrent: zoomFactor(d.frameH, d.cropH), ZoomTarget: zoomFactor(d.frameH, d.tgtH),
	}
}

// zoomFactor states the crop size as a zoom factor relative to the full
// analysis frame: 1 = the whole frame, 2 = a half-height crop. Zero for a
// degenerate height, which the overlay layer reads as "no reading" (plan G2).
func zoomFactor(frameH, cropH float64) float64 {
	if cropH <= 0 {
		return 0
	}
	return frameH / cropH
}

// WalkGate reports the render-level walk gate state derived from the first
// analysis frame (the per-frame leash state is framingOutput.WalkTgt).
func (d *framingDecider) WalkGate() bool { return d.walkActive }
