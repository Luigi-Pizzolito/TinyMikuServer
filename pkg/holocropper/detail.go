package holocropper

import (
	"errors"
	"fmt"
)

/* Lazy per-frame subject detail (HOLOCROPPER-API-PLAN.md Milestone G3,
embedding-app item 761).

The packed stream stays lean - 16 bytes a frame - while the overlay layer wants
the layers the detector produced on its way to those bytes: the merged subject
pools, the raw pre-merge islands, and each island's outline. Those are re-derived
on demand from the cached analysis video, one frame at a time, with the
threshold the detect pass recorded for that frame, so the boxes a user sees are
the boxes that framed the video - not a fresh approximation of them.

Frame numbers here are 0-based, like DetectionStream.Frame(i). FrameRecord.Frame
is 1-based (it matches the debug CSV); a caller that has a record subtracts one. */

// ErrDetailUnavailable reports that a frame's subject detail cannot be
// re-measured: the stream has no cached analysis video to read (a synthetic or
// fit-mode analysis), or its samples predate threshold recording (a cache an
// older build wrote - re-analyze it). errors.Is-compatible.
var ErrDetailUnavailable = errors.New("holocropper: subject detail unavailable")

// SubjectBox is one subject box of a frame, analysis px, in either layer the
// subject filter produces: IsPool boxes are the merged pools the framing
// controller consumed (the app draws them cyan), the rest are the raw pre-merge
// DFS islands (Accepted ones are the app's "big" magenta, rejected ones its
// "small" violet). Pools come first, in merge order, then every island largest
// bbox first.
type SubjectBox struct {
	Rect     Rect  `json:"rect"`
	MaskPx   int   `json:"mask_px"`  // mask pixels inside Rect
	Centroid Point `json:"centroid"` // centre of mass of those pixels
	IsPool   bool  `json:"is_pool"`
	Accepted bool  `json:"accepted"` // raw islands only: it reached the >= 20%-of-largest filter
	Seeds    []int `json:"seeds,omitempty"`
}

// SubjectBoxes returns one frame's subject boxes in both layers. The subject
// count it implies (the number of pools) is what DetectionFrame.Subjects
// recorded, because the measurement is the same function the detect pass ran.
func (s DetectionStream) SubjectBoxes(frame int) ([]SubjectBox, error) {
	d, err := s.measureFrame(frame)
	if err != nil {
		return nil, err
	}
	boxes := make([]SubjectBox, 0, len(d.Pools)+len(d.Components))
	for _, p := range d.Pools {
		boxes = append(boxes, SubjectBox{
			Rect:     rectOf(p.Rect),
			MaskPx:   p.MaskPx,
			Centroid: pointOf(p.Centroid),
			IsPool:   true,
			Accepted: true,
			Seeds:    p.Seeds,
		})
	}
	for _, c := range d.Components {
		boxes = append(boxes, SubjectBox{
			Rect:     rectOf(c.Rect),
			MaskPx:   c.MaskPx,
			Centroid: pointOf(c.Centroid),
			Accepted: c.Accepted,
		})
	}
	return boxes, nil
}

// FrameIslands returns one frame's subject islands with their outlines, largest
// island first: the DFS islands the merge filter joined into pools, each with
// its own pixel outline. The embedding app draws this layer behind a toggle,
// off by default (the contact-sheet review found it legible but busier than the
// rest).
func (s DetectionStream) FrameIslands(frame int) ([]SubjectIsland, error) {
	d, err := s.measureFrame(frame)
	if err != nil {
		return nil, err
	}
	return subjectIslands(d), nil
}

// measureFrame re-runs the full subject measurement of one frame from the
// cached analysis video. Pixels come from the same file the detect pass decoded
// (frame-exact, deterministic), the binarization reuses the threshold the detect
// pass applied, and subjectDetailFromBinary is the same filter - so the answer
// reproduces the recorded master bbox and subject count instead of approximating
// them.
func (s DetectionStream) measureFrame(frame int) (subjectDetail, error) {
	if s.data == nil {
		return subjectDetail{}, fmt.Errorf("%w: this stream holds no frames", ErrDetailUnavailable)
	}
	s.data.mu.RLock()
	idx := frame
	inRange := idx >= 0 && idx < len(s.data.samples)
	sm := streamSample{}
	if inRange {
		sm = s.data.samples[idx]
	}
	path, w, h, fps := s.data.videoPath, s.data.analysisW, s.data.analysisH, s.data.fps
	s.data.mu.RUnlock()

	if !inRange {
		return subjectDetail{}, fmt.Errorf("holocropper: frame %d is outside this stream's %d frames", idx+1, len(s.data.samples))
	}
	if path == "" {
		return subjectDetail{}, fmt.Errorf("%w: frame %d: this stream has no cached analysis video to re-measure (a synthetic or fit-mode analysis)", ErrDetailUnavailable, idx+1)
	}
	if sm.thr == thrUnrecorded {
		return subjectDetail{}, fmt.Errorf("%w: frame %d: this cache predates threshold recording, re-analyze to see subject detail", ErrDetailUnavailable, idx+1)
	}
	if w <= 0 || h <= 0 {
		return subjectDetail{}, fmt.Errorf("%w: frame %d: the stream records no analysis geometry", ErrDetailUnavailable, idx+1)
	}

	frameRGBA, err := seekAnalysisFrame(path, idx, w, h, fps)
	if err != nil {
		return subjectDetail{}, err
	}
	gray := convertToMonochrome(frameRGBA)
	binaryImg := applyThreshold(gray, uint8(sm.thr))
	return subjectDetailFromBinary(binaryImg), nil
}
