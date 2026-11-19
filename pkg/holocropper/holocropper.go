package holocropper

/**
 * Refactor of https://github.com/Luigi-Pizzolito/1-1-Content-Aware-Video-Crop into a Go package
 *
 * Story C3 removed the fused one-shot pipeline (the channelled read ->
 * grayscale -> binary -> calc -> crop -> save stages, their channels and the
 * probe decoder). The framing machinery they produced lives on here and in
 * framedecider.go; the passes that run it are the embeddable API in
 * analysis.go / plan.go / renders.go:
 *
 *	Analyze -> NewPlan -> Analysis.RenderPreview / Analysis.RenderFinal
 */

// defaultAnalysisHeight is the analysis frame height when none is requested
// (AnalyzeRequest.AnalysisHeight == 0). Plan rejects streams analysed below
// the output height, because crops are only ever scaled DOWN from the
// analysis frame (see ErrStreamIncompatible).
const defaultAnalysisHeight = 480

// Framing parameter defaults shared by the instance accessors, the
// embeddable Settings resolution and the Schema form spec.
const (
	defaultZoomMargin = 1.12 // subject bbox -> crop padding factor
	defaultMaxZoom    = 2.0  // zoom-in policy cap
)

// HoloCropper holds the framing parameters of a crop. After story C3 removed
// the fused pipeline it is no longer a video processor: it is the
// tuned-parameters holder whose framingParams() feeds the geometry functions
// (aspectLockedTargetCalc, cropRectFromFramingCalc, cropToBBoxCalc) and the
// corpus-calibrated framing tests. The API passes resolve the same parameters
// from Settings, so rendering never needs an instance.
type HoloCropper struct {
	// Output height. The zoom floors compare it with the analysis frame and
	// the source height (never upscale above the source).
	Height int

	// Optional framing parameters; zero values select built-in defaults.
	ZoomMargin float64 // subject bbox -> crop padding factor (default 1.12)
	MaxZoom    float64 // zoom-in policy cap (default 2.0)

	// Private: source frame height, set by whoever knows the source video.
	srcH int
}

func NewHoloCropper() *HoloCropper {
	return &HoloCropper{}
}

// zoomMargin returns the bbox -> crop padding factor (default 1.12).
func (hc *HoloCropper) zoomMargin() float64 {
	if hc.ZoomMargin > 0 {
		return hc.ZoomMargin
	}
	return defaultZoomMargin
}

// maxZoom returns the zoom-in policy cap (default 2.0).
func (hc *HoloCropper) maxZoom() float64 {
	if hc.MaxZoom > 0 {
		return hc.MaxZoom
	}
	return defaultMaxZoom
}

// framingParams snapshots the framing-relevant settings of this instance for
// the instance-free geometry functions and the replay engine.
func (hc *HoloCropper) framingParams() framingParams {
	return framingParams{
		Margin:  hc.zoomMargin(),
		MaxZoom: hc.maxZoom(),
		OutH:    hc.Height,
		SrcH:    hc.srcH,
	}
}
