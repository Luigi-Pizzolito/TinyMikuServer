package holocropper

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"math"
	"strconv"
)

/* Plan — the real-time replay pass (HOLOCROPPER-API-PLAN.md story A5).

Plan(stream, settings) replays the framingDecider over a cached (or partial)
DetectionStream. It is pure, synchronous and side-effect free: the UI can
call it on every slider tick and get fresh records, orientation, resolved
geometry, walk telemetry and dropout spans in milliseconds. Preview, final
and CLI renders all execute the same replay, so their framing is identical
by construction.

Fit mode (Settings.FitWithBars / NewFitPlan, §6.10) is the non-hologram
variant: no framing controller at all - every frame keeps the full analysis
frame, and the render fit-resizes it into the output on black bars. */

// Plan is an immutable framing replay of one detection stream under one
// settings set. Safe for concurrent use; never mutated after creation.
type Plan struct {
	settings     Settings
	records      []FrameRecord
	cropRects    []image.Rectangle // integer crop rects the render passes cut with
	resolved     Resolved
	orientation  OrientationStat
	walk         WalkSummary
	dropouts     []IntRange
	settingsHash string

	// videoPath is the cached analysis video the source stream was detected
	// from ("" for synthetic streams); Snapshot/ContactSheet composite from it.
	videoPath string
}

// Records returns the full per-frame replay, in stream order. Frame 1 is
// records[0]; every record carries analysis-space boxes plus precomputed
// Norm (0..1) and Out (output px) spaces. The slice is owned by the Plan
// and must be treated as read-only.
func (p *Plan) Records() []FrameRecord { return p.records }

// Record returns the record for stream index i (0-based), or a zero record
// when out of range.
func (p *Plan) Record(i int) FrameRecord {
	if i < 0 || i >= len(p.records) {
		return FrameRecord{}
	}
	return p.records[i]
}

// cropRectAt returns the integer crop rect stream frame i cuts with - the
// replay engine's cropRectFromFraming result in analysis px (it may spill
// horizontally by the landscape stage padding, §12.8). The render passes use
// it directly so a preview, a final render and the CLI pipeline cut the same
// rect for the same frame.
func (p *Plan) cropRectAt(i int) image.Rectangle {
	if i < 0 || i >= len(p.cropRects) {
		return image.ZR
	}
	return p.cropRects[i]
}

// Resolved reports the geometry actually used: defaults applied, dims
// even-rounded, auto-orient decision included.
func (p *Plan) Resolved() Resolved { return p.resolved }

// Orientation reports the auto-orient aggregates and decision computed
// from the stream itself (no separate probe decode). Zero stat for square
// outputs and when auto-orient is off - same reporting semantics as the
// CLI probe.
func (p *Plan) Orientation() OrientationStat { return p.orientation }

// Walk reports the walk-mode telemetry of this replay (§14).
func (p *Plan) Walk() WalkSummary { return p.walk }

// DropoutFrames returns the inclusive 1-based frame spans where the subject
// was lost (matches the timeline shading a scrubber needs).
func (p *Plan) DropoutFrames() []IntRange { return p.dropouts }

// SettingsHash is the stable fingerprint of the settings that produced this
// Plan. Previews record it, so a UI can ask "is the newest preview already
// correct for these settings?" and skip a redundant render.
func (p *Plan) SettingsHash() string { return p.settingsHash }

// Settings returns the (as-supplied) settings behind this Plan.
func (p *Plan) Settings() Settings { return p.settings }

// FitWithBars reports whether this Plan is a fit-mode replay (§6.10): the
// full frame scaled into the output on black bars, no content-aware framing.
// Render passes and stills execute it that way, and the SettingsHash keeps
// fit and crop previews of the same dimensions apart.
func (p *Plan) FitWithBars() bool { return p.settings.FitWithBars }

// NewFitPlan is the explicit fit-mode entry point (HOLOCROPPER-API-PLAN.md
// §6.10), the mode for non-hologram videos: the whole source frame is
// scaled to fit inside the output resolution, centered on black bars - no
// cropping, no zoom, no subject-driven framing. It is exactly NewPlan with
// Settings.FitWithBars set; every other rule (validation, auto-orient,
// ErrStreamIncompatible, previews, final render) applies unchanged.
func NewFitPlan(stream DetectionStream, s Settings) (*Plan, error) {
	s.FitWithBars = true
	return NewPlan(stream, s)
}

// NewPlan replays stream under s. Errors: a *SettingsError when s is invalid,
// or ErrStreamIncompatible when the resolved output height exceeds the
// analyzed height (the crop could only be scaled UP from the analysis
// video - re-Analyze with a taller analysis_height).
//
// A stream whose detect pass is still running plans over what has been
// detected so far; a stream with no geometry yet (pass not started) yields
// an empty Plan.
func NewPlan(stream DetectionStream, s Settings) (*Plan, error) {
	if errs := s.Validate(); len(errs) > 0 {
		return nil, &SettingsError{Errors: errs}
	}
	if stream.data == nil {
		return nil, errors.New("holocropper: Plan requires a detection stream")
	}

	samples, aw, ah, srcW, srcH, _, _ := stream.data.snapshot()
	if aw == 0 || ah == 0 {
		// Analysis geometry not probed yet - a valid empty replay.
		return &Plan{settings: s, settingsHash: planSettingsHash(s, false), videoPath: stream.AnalysisVideoPath()}, nil
	}
	if srcW <= 0 || srcH <= 0 {
		srcW, srcH = aw, ah // meta without source dims: fall back to the analysis frame
	}

	// Resolve the output dims exactly like Crop() does: defaults + even.
	w, h := s.Width, s.Height
	if w == 0 {
		w = 240
	}
	if h == 0 {
		h = 240
	}
	if w%2 != 0 {
		w++
	}
	if h%2 != 0 {
		h++
	}

	// Auto-orient. Crop mode: the flip decision from the stream's own
	// subject aspect ratios - the stream holds every frame's master bbox, so
	// the aggregates cover the whole video (strictly more evidence than the
	// sampled probe decode story C3 removed, same decision machinery
	// (orientationStats) and the same guard against frames whose detection
	// degenerates to the whole frame (degenerateMasterBBox).
	// Fit mode: no subjects matter - the measure is the black-bar area, so
	// the source frame's own aspect is compared against WxH vs HxW
	// (fitOrientationStats) and it decides identically on subjectless clips.
	flip := false
	orientation := OrientationStat{}
	if s.AutoOrient && w != h {
		if s.FitWithBars {
			withSubjects := 0
			for _, sm := range samples {
				if sm.subjects > 0 {
					withSubjects++
				}
			}
			var ow, oh int
			ow, oh, orientation = fitOrientationStats(float64(aw)/float64(ah), w, h, len(samples), withSubjects)
			flip = orientation.Flip
			w, h = ow, oh
		} else {
			ars := make([]float64, 0, len(samples))
			for _, sm := range samples {
				bb := sm.bbox()
				if sm.subjects > 0 && !bb.Empty() && !degenerateMasterBBox(bb, aw, ah) {
					ars = append(ars, float64(bb.Dx())/float64(bb.Dy()))
				}
			}
			var ow, oh int
			ow, oh, orientation = orientationStats(ars, w, h)
			orientation.FramesExamined = len(samples) // the stream IS the probe
			flip = orientation.Flip
			w, h = ow, oh
		}
	}

	if h > ah {
		return nil, fmt.Errorf("%w: output height %d exceeds the analyzed height %d - re-analyze with analysis_height >= %d",
			ErrStreamIncompatible, h, ah, h)
	}

	fps := s.FPS
	if fps == 0 {
		fps = defaultFPS
	}

	// HOLOCROPPER_DEBUG_BOXES shim, kept from the fused CLI path (story
	// C3): when the env var names a file, the replay emits the same CSV
	// the corpus framing tooling reads - one line per frame, same columns.
	// The file reflects the most recent plan built in this process. In fit
	// mode the lines report what the fit path actually applies: the full
	// frame as the crop, EMA = the raw detection box, no leash.
	dbg := newBoxDebugger()
	defer dbg.close()

	if s.FitWithBars {
		records, cropRects, dropouts := fitReplay(samples, aw, ah, w, h, dbg)
		return &Plan{
			settings:  s,
			records:   records,
			cropRects: cropRects,
			resolved: Resolved{
				Settings:  s,
				SourceW:   srcW,
				SourceH:   srcH,
				AnalysisW: aw,
				AnalysisH: ah,
				OutputW:   w,
				OutputH:   h,
			},
			orientation:  orientation,
			walk:         WalkSummary{},
			dropouts:     dropouts,
			settingsHash: planSettingsHash(s, flip),
			videoPath:    stream.AnalysisVideoPath(),
		}, nil
	}

	params := framingParams{
		Margin:  effectiveZoomMargin(s),
		MaxZoom: effectiveMaxZoom(s),
		OutH:    h,
		SrcH:    srcH,
	}
	dec := newFramingDecider(params, float64(w)/float64(h), 1.0/fps, srcW, srcH)

	records := make([]FrameRecord, 0, len(samples))
	cropRects := make([]image.Rectangle, 0, len(samples))
	var dropouts []IntRange
	lostStart := 0
	var walkMin, walkMax float64
	haveWalk := false
	fallback := 0

	bounds := image.Rect(0, 0, aw, ah)
	faw, fah := float64(aw), float64(ah)

	for i, sm := range samples {
		bbox := sm.bbox()
		out := dec.step(i+1, bbox, int(sm.subjects), bounds)

		crop := Rect{X: out.CropCx - out.CropW/2, Y: out.CropCy - out.CropH/2, W: out.CropW, H: out.CropH}
		bb := rectOf(bbox)
		em := rectOf(out.EMA)
		dbg.line(i+1, int(sm.subjects), bbox, out.EMA, out.CropCx, out.CropCy, out.CropW, out.CropH, out.TargetH, out.WalkTgt)

		records = append(records, FrameRecord{
			Frame:       i + 1,
			Subjects:    int(sm.subjects),
			BBox:        bb,
			EMA:         em,
			Crop:        crop,
			TargetH:     out.TargetH,
			WalkTgt:     out.WalkTgt,
			WalkActive:  out.WalkTgt != 0,
			SubjectLost: out.SubjectLost,
			SceneCut:    out.SceneCut,
			Grounded:    out.Grounded,
			Norm: SpaceRects{
				BBox: normalizeRect(bb, faw, fah),
				EMA:  normalizeRect(em, faw, fah),
				Crop: normalizeRect(crop, faw, fah),
			},
			Out: SpaceRects{
				BBox: mapRectToOutput(bb, crop, w, h),
				EMA:  mapRectToOutput(em, crop, w, h),
				Crop: mapRectToOutput(crop, crop, w, h),
			},
		})
		cropRects = append(cropRects, out.CropRect)

		if out.SubjectLost {
			if lostStart == 0 {
				lostStart = i + 1
			}
		} else if lostStart > 0 {
			dropouts = append(dropouts, IntRange{Start: lostStart, End: i})
			lostStart = 0
		}

		if out.WalkTgt != 0 {
			if !haveWalk {
				walkMin, walkMax, haveWalk = out.CropCx, out.CropCx, true
			} else {
				if out.CropCx < walkMin {
					walkMin = out.CropCx
				}
				if out.CropCx > walkMax {
					walkMax = out.CropCx
				}
			}
		} else if dec.WalkGate() && !out.SubjectLost {
			fallback++
		}
	}
	if lostStart > 0 {
		dropouts = append(dropouts, IntRange{Start: lostStart, End: len(samples)})
	}

	walk := WalkSummary{Active: dec.WalkGate(), FallbackFrames: fallback}
	if haveWalk {
		walk.RangePx = walkMax - walkMin
	}

	return &Plan{
		settings:  s,
		records:   records,
		cropRects: cropRects,
		resolved: Resolved{
			Settings:  s,
			SourceW:   srcW,
			SourceH:   srcH,
			AnalysisW: aw,
			AnalysisH: ah,
			OutputW:   w,
			OutputH:   h,
		},
		orientation:  orientation,
		walk:         walk,
		dropouts:     dropouts,
		settingsHash: planSettingsHash(s, flip),
		videoPath:    stream.AnalysisVideoPath(),
	}, nil
}

// fitReplay builds the fit-mode replay (§6.10): the framingDecider never
// runs. Every frame's "crop" is the full analysis frame, and the render
// pass fit-resizes it into the output - a constant transform (scale to fit,
// center, black bars) that also maps the record's Out-space rects, so
// overlays land on the fitted image inside the bars. EMA mirrors the
// detection box: fit mode applies no smoothing, and the record does not
// pretend otherwise. Dropouts still list the spans with no subjects, so a
// timeline can keep shading subject absence even though framing ignores it.
func fitReplay(samples []streamSample, aw, ah, w, h int, dbg *boxDebugger) ([]FrameRecord, []image.Rectangle, []IntRange) {
	faw, fah := float64(aw), float64(ah)
	scale := math.Min(float64(w)/faw, float64(h)/fah)
	sw, sh := faw*scale, fah*scale
	offX, offY := (float64(w)-sw)/2, (float64(h)-sh)/2

	full := Rect{X: 0, Y: 0, W: faw, H: fah}
	fitted := Rect{X: offX, Y: offY, W: sw, H: sh}
	wholeNorm := Rect{X: 0, Y: 0, W: 1, H: 1}
	mapFit := func(r Rect) Rect {
		return Rect{X: offX + r.X*scale, Y: offY + r.Y*scale, W: r.W * scale, H: r.H * scale}
	}

	frameCrop := image.Rect(0, 0, aw, ah)
	records := make([]FrameRecord, 0, len(samples))
	cropRects := make([]image.Rectangle, 0, len(samples))
	var dropouts []IntRange
	lostStart := 0

	for i, sm := range samples {
		bbox := sm.bbox()
		bb := rectOf(bbox)
		lost := bb.W <= 0 || sm.subjects == 0
		norm := normalizeRect(bb, faw, fah)

		dbg.line(i+1, int(sm.subjects), bbox, bbox, faw/2, fah/2, faw, fah, fah, 0)

		records = append(records, FrameRecord{
			Frame:       i + 1,
			Subjects:    int(sm.subjects),
			BBox:        bb,
			EMA:         bb,
			Crop:        full,
			TargetH:     fah,
			WalkTgt:     0,
			WalkActive:  false,
			SubjectLost: lost,
			Norm:        SpaceRects{BBox: norm, EMA: norm, Crop: wholeNorm},
			Out:         SpaceRects{BBox: mapFit(bb), EMA: mapFit(bb), Crop: fitted},
		})
		cropRects = append(cropRects, frameCrop)

		if lost {
			if lostStart == 0 {
				lostStart = i + 1
			}
		} else if lostStart > 0 {
			dropouts = append(dropouts, IntRange{Start: lostStart, End: i})
			lostStart = 0
		}
	}
	if lostStart > 0 {
		dropouts = append(dropouts, IntRange{Start: lostStart, End: len(samples)})
	}
	return records, cropRects, dropouts
}

// planSettingsHash fingerprints the settings (plus the auto-orient flip)
// that produced a Plan. Rendered settings are normalised (0 = default) so
// equivalent settings hash identically. Fit mode is part of the identity:
// a bars-fit preview of the same dimensions is a different product than a
// content-aware crop, and must never satisfy the other's SettingsHash.
func planSettingsHash(s Settings, flip bool) string {
	w, h := s.Width, s.Height
	if w == 0 {
		w = 240
	}
	if h == 0 {
		h = 240
	}
	fps := s.FPS
	if fps == 0 {
		fps = defaultFPS
	}
	ah := s.AnalysisHeight
	if ah == 0 {
		ah = defaultAnalysisHeight
	}
	canonical := fmt.Sprintf("w=%d|h=%d|fps=%s|ah=%d|zm=%s|mz=%s|ao=%d|fit=%d|flip=%d",
		w, h,
		strconv.FormatFloat(fps, 'f', -1, 64),
		ah,
		strconv.FormatFloat(effectiveZoomMargin(s), 'f', -1, 64),
		strconv.FormatFloat(effectiveMaxZoom(s), 'f', -1, 64),
		boolInt(s.AutoOrient), boolInt(s.FitWithBars), boolInt(flip))
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])[:16]
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
