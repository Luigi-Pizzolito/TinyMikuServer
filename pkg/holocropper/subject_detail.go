package holocropper

import (
	"image"
	"sort"
)

/* The full subject measurement of one binary mask frame (HOLOCROPPER-API-PLAN.md
Milestone G3, embedding-app item 761).

The detect pass needs two numbers per frame: the master subject bbox and the
subject count. The embedding app's overlay layer draws the layers underneath
them too - the flood islands, which islands the merge filter kept, the pools the
merger produced, and each island's outline. All of it comes out of one walk of
the mask, and subjectsFromBinary is this measurement's two-number summary, so
a diagnostic drawn for the user cannot disagree with the stream the framing was
planned from: both run the same subjectScan core below.

The core walks reusable scratch (flat label matrix, flat flood stack,
per-label stat slices) instead of allocating maps and [][]int matrices per
frame. The LAW is untouched: same (y,x) seed scan, same 8-neighbour flood
order, label-on-pop with duplicate pushes, bbox equal to the per-pixel Union
tight bound, same comparator, same merge claim order. The perf branch proved
the formulation bit-identical frame-for-frame to the original one over every
frame of the corpus analyses (PROFILING.md §5). */

// subjectComponent is one connected island of the subject mask (one flood
// component), largest bbox area first.
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

// subjectScan is the reusable scratch of the measurement core: the flat label
// matrix and flood stack, the per-label statistics, the sorted component
// order and the merge claim flags. One scan serves one measurement, or a
// streaming series of same-shaped frames (the detect pass reuses one scan for
// a whole analysis video); it is not safe for concurrent frames.
type subjectScan struct {
	labels []int32 // label matrix, row-major over the mask bounds; 0 = background
	stack  []int32 // flood stack of label indices

	n     int // stat slots ensured for the last flood (labels are 1..n)
	count []int32
	sumX  []int64
	sumY  []int64
	minX  []int32 // tight island bbox tracked through the flood; maxX/maxY
	minY  []int32 // hold the EXCLUSIVE edge (image.Rect semantics); all
	maxX  []int32 // relative to the mask bounds origin
	maxY  []int32

	order   []int32 // label ids in component (merge) order
	claimed []bool  // per order position: folded into an earlier pool
}

// ensureLabel grows the per-label stat slots through the given label,
// starting every accumulator fresh.
func (sc *subjectScan) ensureLabel(label int32) {
	for sc.n <= int(label) {
		sc.n++
		sc.count = append(sc.count, 0)
		sc.sumX = append(sc.sumX, 0)
		sc.sumY = append(sc.sumY, 0)
		sc.minX = append(sc.minX, 1<<30)
		sc.minY = append(sc.minY, 1<<30)
		sc.maxX = append(sc.maxX, -1)
		sc.maxY = append(sc.maxY, -1)
	}
}

// measure is the single subject-extraction stage: connected component
// labelling -> sort by bbox area (largest first, ties broken by position,
// stable in label order so the merge order is deterministic) -> keep the
// islands whose bbox is at least 20% of the largest bbox, each merging the
// not-yet-claimed neighbours within 5% of the frame height -> union master
// bbox. full mode additionally counts each island's mask pixels and centre
// of mass, marks the accepted islands, and materialises the label matrix the
// outline tracer (subjectIslands) walks; that is the layer set the lazy
// detail verbs (SubjectBoxes/FrameIslands) draw. The summary mode the detect
// pass streams measures exactly the numbers the stream records, and the
// master-bbox centroid the caller takes from it with maskStats.
func (sc *subjectScan) measure(binaryImg *image.Gray, full bool) subjectDetail {
	b := binaryImg.Bounds()
	w, h := b.Dx(), b.Dy()
	if len(sc.labels) != w*h {
		sc.labels = make([]int32, w*h)
	} else {
		for i := range sc.labels {
			sc.labels[i] = 0
		}
	}
	labels := sc.labels
	sc.n = 0

	// Connected component labelling: the original walk - (y,x) seed scan, the
	// flood's 8-neighbour push order (both rows per column offset, then left,
	// then right), label on pop, duplicate pushes allowed (a pixel pushed
	// twice is re-popped and re-labelled with its own component's label -
	// idempotent), and the tight bbox tracked through the flood, which is
	// exactly the per-pixel Union bound the original stats pass computed.
	stack := sc.stack[:0]
	label := int32(1)
	for y := 0; y < h; y++ {
		binRow := binaryImg.Pix[y*binaryImg.Stride : y*binaryImg.Stride+w]
		rowOff := y * w
		for x := 0; x < w; x++ {
			if binRow[x] == 0 || labels[rowOff+x] != 0 {
				continue
			}
			sc.ensureLabel(label)
			sc.count[label], sc.sumX[label], sc.sumY[label] = 0, 0, 0
			minX, minY, maxX, maxY := x, y, x, y
			stack = append(stack, int32(rowOff+x))
			for len(stack) > 0 {
				cur := int(stack[len(stack)-1])
				stack = stack[:len(stack)-1]
				cx, cy := cur%w, cur/w
				labels[cur] = label
				if cx < minX {
					minX = cx
				}
				if cx > maxX {
					maxX = cx
				}
				if cy < minY {
					minY = cy
				}
				if cy > maxY {
					maxY = cy
				}
				for dx := -1; dx <= 1; dx++ {
					nx := cx + dx
					if nx < 0 || nx >= w {
						continue
					}
					if ny := cy - 1; ny >= 0 {
						if binaryImg.Pix[ny*binaryImg.Stride+nx] > 0 && labels[ny*w+nx] == 0 {
							stack = append(stack, int32(ny*w+nx))
						}
					}
					if ny := cy + 1; ny < h {
						if binaryImg.Pix[ny*binaryImg.Stride+nx] > 0 && labels[ny*w+nx] == 0 {
							stack = append(stack, int32(ny*w+nx))
						}
					}
				}
				if cx > 0 {
					if binaryImg.Pix[cy*binaryImg.Stride+cx-1] > 0 && labels[cy*w+cx-1] == 0 {
						stack = append(stack, int32(cy*w+cx-1))
					}
				}
				if cx+1 < w {
					if binaryImg.Pix[cy*binaryImg.Stride+cx+1] > 0 && labels[cy*w+cx+1] == 0 {
						stack = append(stack, int32(cy*w+cx+1))
					}
				}
			}
			sc.minX[label] = int32(minX)
			sc.minY[label] = int32(minY)
			sc.maxX[label] = int32(maxX + 1)
			sc.maxY[label] = int32(maxY + 1)
			label++
		}
	}
	sc.stack = stack
	nComps := int(label) - 1
	if nComps == 0 {
		return subjectDetail{Bounds: b}
	}

	// One pass over the label matrix for the island mask counts and centres
	// of mass - full mode only; the summary numbers never read them.
	if full {
		for y := 0; y < h; y++ {
			rowOff := y * w
			for x := 0; x < w; x++ {
				l := labels[rowOff+x]
				if l == 0 {
					continue
				}
				sc.count[l]++
				sc.sumX[l] += int64(b.Min.X + x)
				sc.sumY[l] += int64(b.Min.Y + y)
			}
		}
	}

	areaOf := func(l int32) int {
		return int(sc.maxX[l]-sc.minX[l]) * int(sc.maxY[l]-sc.minY[l])
	}
	rectOf := func(l int32) image.Rectangle {
		return image.Rect(b.Min.X+int(sc.minX[l]), b.Min.Y+int(sc.minY[l]),
			b.Min.X+int(sc.maxX[l]), b.Min.Y+int(sc.maxY[l]))
	}

	if cap(sc.order) < nComps {
		sc.order = make([]int32, nComps)
	}
	order := sc.order[:nComps]
	for i := range order {
		order[i] = int32(i + 1)
	}
	sort.SliceStable(order, func(i, j int) bool {
		li, lj := order[i], order[j]
		if ai, aj := areaOf(li), areaOf(lj); ai != aj {
			return ai > aj
		}
		if sc.minY[li] != sc.minY[lj] {
			return sc.minY[li] < sc.minY[lj]
		}
		return sc.minX[li] < sc.minX[lj]
	})
	if cap(sc.claimed) < nComps {
		sc.claimed = make([]bool, nComps)
	}
	claimed := sc.claimed[:nComps]
	for i := range claimed {
		claimed[i] = false
	}

	largestArea := areaOf(order[0])
	tol := h / 20
	d := subjectDetail{Bounds: b}

	// The merge loop keeps the original iteration law exactly: accepted
	// islands (a prefix, since the order is area-descending and the filter is
	// monotone in area) each start a pool at their own rect and fold in every
	// not-yet-claimed island from their own position forward - including the
	// seed itself, which is why an unclaimed seed's label appears in Seeds
	// exactly as the original combineBounds-era merge listed it. A pool is
	// started for every accepted island even one an earlier pool already
	// swallowed.
	haveMaster := false
	for i := 0; i < nComps; i++ {
		li := order[i]
		if 100*float64(areaOf(li))/float64(largestArea) < 20 {
			continue
		}
		combined := rectOf(li)
		var seeds []int
		if full {
			seeds = []int{int(li)}
		}
		for j := i; j < nComps; j++ {
			if claimed[j] {
				continue
			}
			lj := order[j]
			rj := rectOf(lj)
			if expandRectangle(combined, tol).Overlaps(rj) {
				combined = combined.Union(rj)
				claimed[j] = true
				if full {
					seeds = append(seeds, int(lj))
				}
			}
		}
		if !haveMaster {
			d.Master, haveMaster = combined, true
		} else {
			d.Master = d.Master.Union(combined)
		}
		d.Subjects++
		if full {
			pool := subjectPool{Rect: combined, Seeds: seeds}
			pool.MaskPx, pool.Centroid = maskStats(binaryImg, combined)
			d.Pools = append(d.Pools, pool)
		}
	}
	if !full {
		return d
	}

	comps := make([]subjectComponent, nComps)
	for i, l := range order {
		comps[i] = subjectComponent{
			Label:    int(l),
			Rect:     rectOf(l),
			Area:     areaOf(l),
			MaskPx:   int(sc.count[l]),
			Centroid: image.Point{X: int(sc.sumX[l] / int64(sc.count[l])), Y: int(sc.sumY[l] / int64(sc.count[l]))},
			Accepted: 100*float64(areaOf(l))/float64(largestArea) >= 20,
		}
	}
	d.Components = comps

	rows := make([][]int, h)
	for y := 0; y < h; y++ {
		src := sc.labels[y*w : (y+1)*w]
		row := make([]int, w)
		for x := range row {
			row[x] = int(src[x])
		}
		rows[y] = row
	}
	d.Labels = rows
	return d
}

// subjectDetailFromBinary runs the shared subject-extraction stage in full -
// every island, pool and label the overlay layers draw.
func subjectDetailFromBinary(binaryImg *image.Gray) subjectDetail {
	return (&subjectScan{}).measure(binaryImg, true)
}

// subjectsFromBinary is the two-number summary of the same measurement: the
// union master bbox and the subject count.
func subjectsFromBinary(binaryImg *image.Gray) (image.Rectangle, int) {
	d := (&subjectScan{}).measure(binaryImg, false)
	return d.Master, d.Subjects
}

// maskStats counts the white mask pixels inside r and their centre of mass.
// Zero count pairs with a zero point: "no reading", never a coordinate.
// Out-of-frame pixels read as background, exactly as the GrayAt walk did.
func maskStats(img *image.Gray, r image.Rectangle) (int, image.Point) {
	b := img.Bounds()
	r = r.Intersect(b)
	var totalX, totalY, count int64
	for y := r.Min.Y; y < r.Max.Y; y++ {
		rowOff := (y-b.Min.Y)*img.Stride + (r.Min.X - b.Min.X)
		for x, v := range img.Pix[rowOff : rowOff+r.Dx()] {
			if v == 255 {
				totalX += int64(r.Min.X + x)
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
