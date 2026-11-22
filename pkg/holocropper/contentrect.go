package holocropper

/* Content rect (§6.10 amendment, Horidashimi item 825): many ordinary music
videos carry BLACK CONTAINER BARS - the picture is a smaller aspect (a 4:3
clip, a 2.35:1 clip) letterboxed or pillarboxed inside a 16:9 root container.
The shipped fit transform (§6.10) scales the WHOLE analysis frame, so those
bars ride into the output and the actual content shrinks further. This file
measures the content rectangle - the frame left after the black bars are cut -
so the fit pass can crop the bars out BEFORE fitting: the clip's content then
fills the output exactly the way an unletterboxed clip does.

Law:
  · A scan line is bar material only when the MAXIMUM sampled luma on it is
    at or below barMaxLuma - ffmpeg cropdetect's default black limit. One
    bright pixel (a caption in the bar, noise) makes the line content.
  · Each side is scanned inward from the frame edge while its lines stay bar
    material. A side that eats half the frame makes the FRAME unmeasurable
    (a fade, a black leader, an all-black frame) - it joins no aggregate;
    it is never read as "all bars".
  · The clip's rect is the per-side MEDIAN over the measurable frames:
    container bars are static, and a median survives the handful of frames
    whose edges happen to be dark (or bright) content.
  · Fresh fit analyses measure every frame for free - the fit probe already
    decodes them all. Streams that were probed before this law (old caches,
    detect-resumed crop caches) measure lazily on the first fit plan: the
    first barProbeFrames frames of the cached analysis video, once per
    stream, memoized. Synthetic streams (no analysis video) keep the shipped
    full-frame transform unchanged.
*/

import (
	"image"
	"sort"

	vidio "github.com/AlexEidt/Vidio"
)

// barMaxLuma is ffmpeg cropdetect's default black limit (24): a scan line
// counts as bar material only when its maximum sampled luma is at or below
// this value. Bars are authored black; content never sits under this level
// for a whole line in the clips this law targets.
const barMaxLuma = 24

// barScanStride samples every Nth pixel along a scan line. A bar line is
// authored uniformly black, so a stride reads it exactly while capping the
// probe's cost to a rounding error on the decode that already happens.
const barScanStride = 4

// barProbeFrames caps the LAZY content probe (streams written before this
// law, or crops resumed over a fit cache): the first N frames of the
// analysis video are enough to measure STATIC container bars - the law does
// not chase bars that move.
const barProbeFrames = 120

// barMinFrames is the number of measurable frames a clip must offer before
// the per-side medians are trusted.
const barMinFrames = 8

// lumaOf is Rec.601 luma from an RGBA triple, in integer arithmetic.
func lumaOf(r, g, b uint8) uint8 {
	return uint8((77*uint16(r) + 150*uint16(g) + 29*uint16(b)) >> 8)
}

// rowIsBar reports whether the row y across [x0,x1) is bar material.
func rowIsBar(img *image.RGBA, y, x0, x1 int) bool {
	for x := x0; x < x1; x += barScanStride {
		p := img.PixOffset(x, y)
		if lumaOf(img.Pix[p], img.Pix[p+1], img.Pix[p+2]) > barMaxLuma {
			return false
		}
	}
	return true
}

// colIsBar reports whether the column x across [y0,y1) is bar material.
func colIsBar(img *image.RGBA, x, y0, y1 int) bool {
	for y := y0; y < y1; y += barScanStride {
		p := img.PixOffset(x, y)
		if lumaOf(img.Pix[p], img.Pix[p+1], img.Pix[p+2]) > barMaxLuma {
			return false
		}
	}
	return true
}

// frameBarInsets scans the four sides of a w x h RGBA frame inward. ok is
// false when any side eats half the frame or more: such a frame is
// unmeasurable (fade/leader/all-black), not a content rect.
func frameBarInsets(img *image.RGBA, w, h int) (left, top, right, bottom int, ok bool) {
	limitW, limitH := w/2, h/2
	for top < limitH && rowIsBar(img, top, 0, w) {
		top++
	}
	for bottom < limitH && rowIsBar(img, h-1-bottom, 0, w) {
		bottom++
	}
	for left < limitW && colIsBar(img, left, 0, h) {
		left++
	}
	for right < limitW && colIsBar(img, w-1-right, 0, h) {
		right++
	}
	if top >= limitH || bottom >= limitH || left >= limitW || right >= limitW {
		return 0, 0, 0, 0, false
	}
	return left, top, right, bottom, true
}

// barAccumulator folds per-frame insets into the clip's content rect medians.
type barAccumulator struct {
	w, h  int
	sides [4][]int // left, top, right, bottom depths of measurable frames
}

func newBarAccumulator(w, h int) *barAccumulator {
	return &barAccumulator{w: w, h: h}
}

func (a *barAccumulator) add(img *image.RGBA) {
	l, t, r, b, ok := frameBarInsets(img, a.w, a.h)
	if !ok {
		return
	}
	a.sides[0] = append(a.sides[0], l)
	a.sides[1] = append(a.sides[1], t)
	a.sides[2] = append(a.sides[2], r)
	a.sides[3] = append(a.sides[3], b)
}

func medianInts(v []int) int {
	s := make([]int, len(v))
	copy(s, v)
	sort.Ints(s)
	mid := len(s) / 2
	if len(s)%2 == 0 {
		return (s[mid-1] + s[mid]) / 2
	}
	return s[mid]
}

// rect answers the aggregated content rect (full frame when the clip carries
// no bars) and whether any aggregate could be built at all.
func (a *barAccumulator) rect() (image.Rectangle, bool) {
	if a.w <= 0 || a.h <= 0 || len(a.sides[0]) < barMinFrames {
		return image.ZR, false
	}
	l := medianInts(a.sides[0])
	t := medianInts(a.sides[1])
	r := medianInts(a.sides[2])
	b := medianInts(a.sides[3])
	return image.Rect(l, t, a.w-r, a.h-b), true
}

// contentRectFor is the lazy path: decode the first barProbeFrames frames of
// the analysis video and aggregate. It reads with the same vidio frame buffer
// the probe and render loops use; the decode cost is capped by design.
func contentRectFor(videoPath string, w, h int) (image.Rectangle, bool) {
	video, err := vidio.NewVideo(videoPath)
	if err != nil {
		return image.ZR, false
	}
	defer video.Close()
	vw, vh := video.Width(), video.Height()
	if vw != w || vh != h {
		// The measurement is only valid for the frame grid the stream
		// describes; a mismatched file must not answer one.
		return image.ZR, false
	}
	buf := image.NewRGBA(image.Rect(0, 0, vw, vh))
	if err := video.SetFrameBuffer(buf.Pix); err != nil {
		return image.ZR, false
	}
	acc := newBarAccumulator(vw, vh)
	count := 0
	for video.Read() && count < barProbeFrames {
		count++
		acc.add(buf)
	}
	return acc.rect()
}
