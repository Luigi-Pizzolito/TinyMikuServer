package holocropper

import (
	"image"
	"sort"
)

/* The full subject measurement of one binary mask frame (HOLOCROPPER-API-PLAN.md
Milestone G3, embedding-app item 761).

The detect pass needs two numbers per frame: the master subject bbox and the
subject count. The embedding app's overlay layer draws the layers underneath
them too - the DFS islands, which islands the merge filter kept, the pools the
merger produced, and each island's outline. All of it comes out of one walk of
the mask, and subjectsFromBinary is this measurement's two-number summary, so
a diagnostic drawn for the user cannot disagree with the stream the framing was
planned from. */

// subjectComponent is one connected island of the subject mask (one DFS
// component of connectedComponentLabeling), largest bbox area first.
type subjectComponent struct {
	Label    int             // id in the label matrix
	Rect     image.Rectangle // tight island bbox, analysis px
	Area     int             // bbox area: what the 20% subject filter measures
	MaskPx   int             // mask pixels in the island
	Centroid image.Point     // centre of mass of the island's own mask pixels
	Accepted bool            // survived the >= 20%-of-largest filter, i.e. it became a subject
}

// subjectPool is one merged subject box. There is one pool per ACCEPTED
// island: the merger starts a box at each accepted island and folds in every
// not-yet-claimed island within tolerance, so pools can nest inside each other
// and the subject count the stream records is len(Pools).
type subjectPool struct {
	Rect     image.Rectangle
	MaskPx   int         // mask pixels inside Rect
	Centroid image.Point // centre of mass over those pixels (the maskCentroid law)
	Seeds    []int       // labels folded into this pool, merge order
}

// PixelRun is one row segment of an outline: x0..x1 inclusive on row y,
// analysis px. A UI strokes it as a 1 px line from (x0, y) to (x1+1, y).
type PixelRun struct {
	Y  int `json:"y"`
	X0 int `json:"x0"`
	X1 int `json:"x1"`
}

// SubjectIsland is one island with its outline as horizontal runs. The outline
// is every mask pixel with at least one 8-neighbour outside the island, so an
// island's holes are outlined as well as its outer edge - the pixel-outline
// layer the 761 contact-sheet review judged. Exported because FrameIslands
// hands it to the embedding app.
type SubjectIsland struct {
	Label    int        `json:"label"`
	BBox     Rect       `json:"bbox"`
	MaskPx   int        `json:"mask_px"`
	Accepted bool       `json:"accepted"`
	Outline  []PixelRun `json:"outline"`
}

// subjectDetail is one frame's full subject measurement.
type subjectDetail struct {
	Components []subjectComponent // every island, largest bbox area first
	Pools      []subjectPool      // one per accepted island, in merge order
	Master     image.Rectangle    // union of the pools: the box the stream records
	Subjects   int                // len(Pools): the count the stream records
	Labels     [][]int            // per-pixel island label (0 = background)
	Bounds     image.Rectangle    // the mask's bounds, for label indexing
}

// islandStat accumulates one label's pixels during the single mask walk.
type islandStat struct {
	count int
	sumX  int64
	sumY  int64
	tight image.Rectangle
	seen  bool
}

// subjectDetailFromBinary runs the shared subject-extraction stage in full:
// connected component labelling -> sort by bbox area (largest first, ties
// broken by position so the merge order is deterministic) -> keep the islands
// whose bbox is at least 20% of the largest bbox, each merging the
// not-yet-claimed neighbours within 5% of the frame height -> union master
// bbox. The merge loop mirrors subjectsFromBinary's iteration order exactly:
// every accepted island is tested against what the earlier pools have not yet
// claimed.
func subjectDetailFromBinary(binaryImg *image.Gray) subjectDetail {
	boxes, labels := connectedComponentLabeling(binaryImg)
	bound := binaryImg.Bounds()
	if len(boxes) == 0 {
		return subjectDetail{Bounds: bound}
	}

	// One pass over the label matrix: pixel counts, centres of mass, and the
	// tight bboxes the outline tracer walks.
	stats := make(map[int]*islandStat, len(boxes))
	for y := bound.Min.Y; y < bound.Max.Y; y++ {
		row := labels[y-bound.Min.Y]
		for x := bound.Min.X; x < bound.Max.X; x++ {
			l := row[x-bound.Min.X]
			if l == 0 {
				continue
			}
			st := stats[l]
			if st == nil {
				st = &islandStat{}
				stats[l] = st
			}
			st.count++
			st.sumX += int64(x)
			st.sumY += int64(y)
			p := image.Rect(x, y, x+1, y+1)
			if !st.seen {
				st.tight, st.seen = p, true
			} else {
				st.tight = st.tight.Union(p)
			}
		}
	}

	comps := make([]subjectComponent, 0, len(stats))
	for l, st := range stats {
		if st.count == 0 {
			continue
		}
		comps = append(comps, subjectComponent{
			Label:    l,
			Rect:     st.tight,
			Area:     st.tight.Dx() * st.tight.Dy(),
			MaskPx:   st.count,
			Centroid: image.Point{X: int(st.sumX / int64(st.count)), Y: int(st.sumY / int64(st.count))},
		})
	}
	sort.Slice(comps, func(i, j int) bool {
		if comps[i].Area != comps[j].Area {
			return comps[i].Area > comps[j].Area
		}
		if comps[i].Rect.Min.Y != comps[j].Rect.Min.Y {
			return comps[i].Rect.Min.Y < comps[j].Rect.Min.Y
		}
		return comps[i].Rect.Min.X < comps[j].Rect.Min.X
	})

	largestArea := comps[0].Area
	tol := bound.Dy() / 20
	d := subjectDetail{Components: comps, Bounds: bound, Labels: labels}

	// remaining is what no pool has claimed yet; it starts as every island
	// (the merge test sees the seed itself, exactly as combineBounds did).
	remaining := make([]subjectComponent, len(comps))
	copy(remaining, comps)
	for i := range comps {
		c := comps[i]
		if 100*float64(c.Area)/float64(largestArea) < 20 {
			continue
		}
		c.Accepted = true
		d.Components[i] = c

		combined, seeds, next := c.Rect, []int{c.Label}, make([]subjectComponent, 0, len(remaining))
		for _, r := range remaining {
			if expandRectangle(combined, tol).Overlaps(r.Rect) {
				combined = combined.Union(r.Rect)
				seeds = append(seeds, r.Label)
				continue
			}
			next = append(next, r)
		}
		remaining = next
		pool := subjectPool{Rect: combined, Seeds: seeds}
		pool.MaskPx, pool.Centroid = maskStats(binaryImg, combined)
		d.Pools = append(d.Pools, pool)
	}
	if len(d.Pools) == 0 {
		return d
	}
	rects := make([]image.Rectangle, len(d.Pools))
	for i, p := range d.Pools {
		rects[i] = p.Rect
	}
	d.Master = totalBoundingBox(rects)
	d.Subjects = len(d.Pools)
	return d
}

// maskStats counts the white mask pixels inside r and their centre of mass.
// Zero count pairs with a zero point: "no reading", never a coordinate.
func maskStats(img *image.Gray, r image.Rectangle) (int, image.Point) {
	var totalX, totalY, count int64
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			if img.GrayAt(x, y).Y == 255 {
				totalX += int64(x)
				totalY += int64(y)
				count++
			}
		}
	}
	if count == 0 {
		return 0, image.Point{}
	}
	return int(count), image.Point{X: int(totalX / count), Y: int(totalY / count)}
}

// subjectIslands traces the outline of every island of a measured frame,
// largest island first. Boundary law: a mask pixel of the island with at least
// one 8-neighbour that is not that island (background, frame edge, or another
// island touching it diagonally). Runs are row-major and maximal: consecutive
// boundary pixels on one row are one run.
func subjectIslands(d subjectDetail) []SubjectIsland {
	if len(d.Components) == 0 || len(d.Labels) == 0 {
		return nil
	}
	out := make([]SubjectIsland, 0, len(d.Components))
	for _, c := range d.Components {
		var runs []PixelRun
		for y := c.Rect.Min.Y; y < c.Rect.Max.Y; y++ {
			runStart := -1
			for x := c.Rect.Min.X; x < c.Rect.Max.X; x++ {
				if d.labelAt(x, y) == c.Label && isIslandBoundary(d, x, y, c.Label) {
					if runStart < 0 {
						runStart = x
					}
					continue
				}
				if runStart >= 0 {
					runs = append(runs, PixelRun{Y: y, X0: runStart, X1: x - 1})
					runStart = -1
				}
			}
			if runStart >= 0 {
				runs = append(runs, PixelRun{Y: y, X0: runStart, X1: c.Rect.Max.X - 1})
			}
		}
		out = append(out, SubjectIsland{
			Label:    c.Label,
			BBox:     rectOf(c.Rect),
			MaskPx:   c.MaskPx,
			Accepted: c.Accepted,
			Outline:  runs,
		})
	}
	return out
}

// labelAt reads the label matrix safely (out of frame reads as background).
func (d subjectDetail) labelAt(x, y int) int {
	if y < d.Bounds.Min.Y || y >= d.Bounds.Max.Y || x < d.Bounds.Min.X || x >= d.Bounds.Max.X {
		return 0
	}
	return d.Labels[y-d.Bounds.Min.Y][x-d.Bounds.Min.X]
}

// isIslandBoundary reports whether (x, y) is a mask pixel of island `label`
// with at least one 8-neighbour outside that island.
func isIslandBoundary(d subjectDetail, x, y, label int) bool {
	for dy := -1; dy <= 1; dy++ {
		for dx := -1; dx <= 1; dx++ {
			if dx == 0 && dy == 0 {
				continue
			}
			if d.labelAt(x+dx, y+dy) != label {
				return true
			}
		}
	}
	return false
}
