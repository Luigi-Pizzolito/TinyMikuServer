// Profiling harness for the perf/holocropper-profile branch. Tagged `perf`
// so the shipped test suites never run it:
//
//	go test -tags perf -run TestPerfGeometry -v .        # crop-geometry CSV + stats
//	go test -tags perf -run TestPerfEncodeBench -v .     # encoder stage attribution + reconfig tax
//	go test -tags perf -run TestPerfSelectExpr -v .      # select-filter law check
//	go test -tags perf -run TestPerfDetectSplit -v .     # vidio decode vs Go detect work
//	go test -tags perf -run TestPerfDetectFormulaProbe -v .
//	go test -tags perf -run TestPerfDetectOptimizedIdentity -v .   # replica == shipped chain
//	go test -tags perf -run TestPerfDetectRealChain -v . # shipped vs replica speed on real frames
//	go test -tags perf -run NoTestMatch -bench . -benchtime 30x .
//
// PERF_FEED_N / PERF_FEED_START tune the encode bench window;
// PERF_DETECT_N tunes the detect tests.
//
// It exercises ONLY the shipped law path (Analyze cache -> Plan ->
// frameSchedule -> scaleRectToSource) so the numbers describe the pipeline
// the H law actually runs, not an approximation of it.
//
// Fixture and cache conventions mirror the CLI gate runs on this branch:
// input paths relative to this package dir (../../vids/<name>), CLI-auto
// analysis height max(W,H) > 480, temp dir /tmp/opencode/verify-h/cache4.

//go:build perf

package holocropper

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	vidio "github.com/AlexEidt/Vidio"
)

const (
	perfOutW    = 648
	perfOutH    = 200
	perfFPS     = 24.0
	perfCache   = "/tmp/opencode/verify-h/cache4"
	perfFeedDir = "/tmp/opencode/ffprof/feeds"
)

type perfSize struct{ w, h int }

func perfFixturePath(t *testing.T, name string) string {
	t.Helper()
	// Same relative spelling the CLI gate runs used, so the analysis cache
	// key matches theirs (the key hashes the input path string verbatim).
	p := filepath.Join("..", "..", "vids", name)
	if st, err := os.Stat(p); err != nil || st.Size() == 0 {
		t.Skipf("fixture %s missing", p)
	}
	return p
}

// perfAnalysis reuses the CLI run's analysis cache when the key matches and
// analyzes fresh otherwise; ah == 0 means the API default (Analyze itself
// resolves it and hits its own cache, so there is nothing to pre-look-up).
func perfAnalysis(t *testing.T, input string, ah int, fit bool) *Analysis {
	t.Helper()
	mode := AnalyzeMode("")
	if fit {
		mode = AnalyzeFit
	}
	if ah > 0 {
		if a, err := CachedAnalysis(input, perfCache, ah); err == nil && a.meta.AnalysisHeight == ah {
			t.Logf("cached analysis %s ah=%d (detected=%v)", input, ah, a.meta.Detected)
			return a
		}
	}
	task, err := Analyze(context.Background(), AnalyzeRequest{
		Input: input, TempDir: perfCache, AnalysisHeight: ah, Mode: mode,
	})
	if err != nil {
		t.Fatalf("analyze %s: %v", input, err)
	}
	a, err := task.Wait()
	if err != nil {
		t.Fatalf("analyze %s wait: %v", input, err)
	}
	return a
}

type perfGeomRow struct {
	outIdx, srcIdx, w, h int
}

// perfPlanGeometry replays exactly what renderFinalWork does: the schedule
// picks a source frame per output slot, the Plan's crop rect (analysis px)
// maps to source px, and writePPMCrop feeds a PPM of THAT size.
func perfPlanGeometry(t *testing.T, a *Analysis, p *Plan) []perfGeomRow {
	t.Helper()
	r := p.resolved
	sched := a.frameSchedule(p)
	rows := make([]perfGeomRow, 0, sched.Frames)
	for out := 0; out < sched.Frames; out++ {
		src := sched.SrcFor[out]
		crop := scaleRectToSource(p.cropRectAt(src), r.AnalysisW, r.AnalysisH, r.SourceW, r.SourceH)
		rows = append(rows, perfGeomRow{out, src, crop.Dx(), crop.Dy()})
	}
	return rows
}

func perfGeomStats(t *testing.T, name string, a *Analysis, p *Plan, rows []perfGeomRow) {
	t.Helper()
	r := p.resolved
	sched := a.frameSchedule(p)
	sizes := map[perfSize]int{}
	transitions := 0
	prev := perfSize{}
	fedBytes := int64(0)
	for i, g := range rows {
		sz := perfSize{g.w, g.h}
		sizes[sz]++
		if i > 0 && sz != prev {
			transitions++
		}
		prev = sz
		fedBytes += int64(g.w)*int64(g.h)*3 + int64(len(fmt.Sprintf("P6\n%d %d\n255\n", g.w, g.h)))
	}
	decoded := rows[len(rows)-1].srcIdx + 1
	t.Logf("%s: src=%dx%d records=%d analysis=%dx%d analysisFPS=%v render=%dx%d@%v frames=%d resampled=%v",
		name, r.SourceW, r.SourceH, len(p.records), r.AnalysisW, r.AnalysisH, a.meta.FPS,
		r.OutputW, r.OutputH, planFPS(p), sched.Frames, sched.Resampled)
	pipeBytes := float64(decoded) * float64(r.SourceW*r.SourceH*3)
	t.Logf("%s: decoded pipe=%d frames x %d B = %.1f GB; fed PPM=%d frames %.2f GB (%.0f%% of pipe)",
		name, decoded, r.SourceW*r.SourceH*3, pipeBytes/1e9,
		len(rows), float64(fedBytes)/1e9, 100*float64(fedBytes)/pipeBytes)
	t.Logf("%s: distinct crop sizes=%d size-transitions=%d (sws reconfig upper bound)",
		name, len(sizes), transitions)
	type szn struct {
		sz perfSize
		n  int
	}
	list := make([]szn, 0, len(sizes))
	for k, n := range sizes {
		list = append(list, szn{k, n})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].n > list[j].n })
	parts := make([]string, 0, 8)
	for i, s := range list {
		if i >= 8 {
			break
		}
		parts = append(parts, fmt.Sprintf("%dx%d x%d", s.sz.w, s.sz.h, s.n))
	}
	t.Logf("%s: top sizes: %s", name, strings.Join(parts, ", "))
}

func perfWriteCSV(t *testing.T, name string, rows []perfGeomRow) string {
	t.Helper()
	path := filepath.Join(perfFeedDir, "geom-"+name+".csv")
	if err := os.MkdirAll(perfFeedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<20)
	fmt.Fprintln(w, "out,src,w,h")
	for _, g := range rows {
		fmt.Fprintf(w, "%d,%d,%d,%d\n", g.outIdx, g.srcIdx, g.w, g.h)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPerfGeometry(t *testing.T) {
	if err := os.MkdirAll(perfFeedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	devilIn := perfFixturePath(t, "sweetdevil.mp4")
	a := perfAnalysis(t, devilIn, perfOutW, false) // CLI-auto ah = max(648,200) = 648
	p, err := NewPlan(a.Stream(), Settings{Width: perfOutW, Height: perfOutH, FPS: perfFPS})
	if err != nil {
		t.Fatal(err)
	}
	rows := perfPlanGeometry(t, a, p)
	t.Logf("geometry CSV: %s", perfWriteCSV(t, "sweetdevil", rows))
	perfGeomStats(t, "sweetdevil", a, p, rows)

	musicIn := perfFixturePath(t, "music.mp4")
	am := perfAnalysis(t, musicIn, 0, true) // API default 480, fit analysis
	pm, err := NewFitPlan(am.Stream(), Settings{Width: perfOutW, Height: perfOutH, FPS: perfFPS})
	if err != nil {
		t.Fatal(err)
	}
	mrows := perfPlanGeometry(t, am, pm)
	t.Logf("geometry CSV: %s", perfWriteCSV(t, "music-fit480", mrows))
	perfGeomStats(t, "music-fit480", am, pm, mrows)
}

// perfBaseImage is one smooth 3500x1080 RGB tile (gradients plus tiny
// deterministic noise so hqdn3d has something to look at); the feed frames
// are row-copy crops of it at shifting offsets, which keeps the content law
// deterministic while the generator runs at memcpy speed instead of
// per-pixel arithmetic.
var (
	perfBaseOnce sync.Once
	perfBase     []byte
)

func perfBaseImage() []byte {
	perfBaseOnce.Do(func() {
		const w, h = 3500, 1080
		perfBase = make([]byte, w*h*3)
		i := 0
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				noise := byte((x*7 + y*13) & 7)
				perfBase[i] = byte((x*255)/w+(y*70)/h) + noise
				perfBase[i+1] = byte((y*255)/h+(x*20)/w) + noise
				perfBase[i+2] = byte(((x+y)*255)/(w+h)) + noise
				i += 3
			}
		}
	})
	return perfBase
}

// perfFeedInto crops frame `frame` of the base-tile motion law into buf.
func perfFeedInto(buf []byte, w, h, frame int) {
	base := perfBaseImage()
	const bw, bh = 3500, 1080
	x0 := (frame * 29) % (bw - w + 1)
	y0 := (frame * 17) % (bh - h + 1)
	for y := 0; y < h; y++ {
		src := ((y0+y)*bw + x0) * 3
		copy(buf[y*w*3:], base[src:src+w*3])
	}
}

func perfLoadSizes(t *testing.T, csvPath string, start, n int) []perfSize {
	t.Helper()
	f, err := os.Open(csvPath)
	if err != nil {
		t.Skip("run TestPerfGeometry first:", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	var sizes []perfSize
	skipped := 0
	for sc.Scan() {
		if skipped < start {
			skipped++
			continue
		}
		cols := strings.Split(sc.Text(), ",")
		if len(cols) < 4 {
			continue
		}
		wt, _ := strconv.Atoi(cols[2])
		ht, _ := strconv.Atoi(cols[3])
		sizes = append(sizes, perfSize{wt, ht})
		if len(sizes) >= n {
			break
		}
	}
	return sizes
}

// TestPerfEncodeBench streams generated crop frames into ffmpeg chain
// variants - format-only -> +hqdn3d -> +scale1 -> full - on the REAL
// sweetdevil crop-size sequence, plus a constant-size control, every
// variant mjpeg q6 into -f null. Sequential, identical content law per
// variant, so the stage-to-stage deltas are the chain's own cost and the
// varied-vs-constant pair is the sws reconfiguration tax. The feed never
// touches disk: frames stream straight into the encoder's stdin.
func TestPerfEncodeBench(t *testing.T) {
	start, n := 2400, 600
	if v := os.Getenv("PERF_FEED_START"); v != "" {
		if m, err := strconv.Atoi(v); err == nil {
			start = m
		}
	}
	if v := os.Getenv("PERF_FEED_N"); v != "" {
		if m, err := strconv.Atoi(v); err == nil {
			n = m
		}
	}
	sizes := perfLoadSizes(t, filepath.Join(perfFeedDir, "geom-sweetdevil.csv"), start, n)
	if len(sizes) == 0 {
		t.Fatal("no sizes loaded")
	}
	ws := make([]int, len(sizes))
	for i, s := range sizes {
		ws[i] = s.w
	}
	sort.Ints(ws)
	med := ws[len(ws)/2]
	medH := 0
	for _, s := range sizes {
		if s.w == med {
			medH = s.h
			break
		}
	}
	consts := make([]perfSize, len(sizes))
	constBytes := int64(0)
	variedBytes := int64(0)
	for i := range consts {
		consts[i] = perfSize{med, medH}
		constBytes += int64(med) * int64(medH) * 3
		variedBytes += int64(sizes[i].w) * int64(sizes[i].h) * 3
	}
	t.Logf("bench window: %d frames start=%d varied=%.2f GB constant(%dx%d)=%.2f GB",
		len(sizes), start, float64(variedBytes)/1e9, med, medH, float64(constBytes)/1e9)

	chainFull := "hqdn3d=2:1.5:0:0,scale=iw*2/3:ih*2/3:flags=lanczos,scale=648:200:flags=lanczos,format=yuvj420p"
	variants := []struct {
		label string
		chain string
		sizes []perfSize
	}{
		{"E-format-varied", "format=yuvj420p", sizes},
		{"E-hqdn3d-varied", "hqdn3d=2:1.5:0:0,format=yuvj420p", sizes},
		{"E-hqdn3d+scale1-varied", "hqdn3d=2:1.5:0:0,scale=iw*2/3:ih*2/3:flags=lanczos,format=yuvj420p", sizes},
		{"E-full-varied-pass1", chainFull, sizes},
		{"E-full-varied-pass2", chainFull, sizes},
		{"E-format-constant", "format=yuvj420p", consts},
		{"E-full-constant-pass1", chainFull, consts},
		{"E-full-constant-pass2", chainFull, consts},
	}
	for _, v := range variants {
		secs, fed := perfEncodeRun(t, v.chain, v.sizes)
		t.Logf("%s: wall=%.1fs fed=%.2f GB in=%.0f MB/s", v.label, secs, float64(fed)/1e9, float64(fed)/1e6/secs)
	}
}

func perfEncodeRun(t *testing.T, chain string, sizes []perfSize) (float64, int64) {
	t.Helper()
	cmd := exec.Command("/usr/sbin/ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-framerate", "24", "-f", "image2pipe", "-vcodec", "ppm", "-i", "pipe:",
		"-vf", chain,
		"-c:v", "mjpeg", "-q:v", "6", "-pix_fmt", "yuvj420p", "-f", "null", "-")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	errBuf := &bytes.Buffer{}
	cmd.Stderr = errBuf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriterSize(in, 4<<20)
	buf := make([]byte, 3500*1080*3)
	start := time.Now()
	var fed int64
	for i, s := range sizes {
		nb := s.w * s.h * 3
		perfFeedInto(buf, s.w, s.h, i)
		fmt.Fprintf(w, "P6\n%d %d\n255\n", s.w, s.h)
		if _, err := w.Write(buf[:nb]); err != nil {
			t.Fatal(err)
		}
		fed += int64(nb)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	in.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("ffmpeg chain %q: %v\n%s", chain, err, errBuf.String())
	}
	return time.Since(start).Seconds(), fed
}

// ---------------------------------------------------------------------------
// select-filter law check
// ---------------------------------------------------------------------------

// perfKeptLaw is the candidate decoder-side select law, integer-only: with
// p/q the reduced analysisFPS/renderFPS, source frame n is played iff
// c*p - q*n < q for c = ceil(q*n/p) - i.e. [q*n, q*n+q) contains a multiple
// of p. Go's schedule is SrcFor[i] = floor(i*p/q); the two must agree
// exactly before ANY ffmpeg select expression is trusted.
func perfKeptLaw(n, p, q int) bool {
	c := (q*n + p - 1) / p
	return c*p-q*n < q
}

func perfReduce(a, b int) (int, int) {
	x, y := a, b
	for y != 0 {
		x, y = y, x%y
	}
	return a / x, b / x
}

func TestPerfSelectExpr(t *testing.T) {
	devilIn := perfFixturePath(t, "sweetdevil.mp4")
	musicIn := perfFixturePath(t, "music.mp4")
	cases := []struct {
		name  string
		input string
		ah    int
		fit   bool
	}{
		{"sweetdevil", devilIn, perfOutW, false},
		{"music", musicIn, 0, true},
	}
	for _, c := range cases {
		a := perfAnalysis(t, c.input, c.ah, c.fit)
		p, err := NewPlan(a.Stream(), Settings{Width: perfOutW, Height: perfOutH, FPS: perfFPS})
		if err != nil {
			t.Fatal(err)
		}
		sched := a.frameSchedule(p)
		fpsA, fpsR := a.meta.FPS, planFPS(p)
		// The integer law needs exact rational fps: require whole numbers.
		if fpsA != float64(int(fpsA)) || fpsR != float64(int(fpsR)) {
			t.Logf("%s: fps %v/%v not integer-rational - residue law N/A", c.name, fpsA, fpsR)
			continue
		}
		pnum, qden := perfReduce(int(fpsA), int(fpsR))
		kept := map[int]bool{}
		for i := 0; i < sched.Frames; i++ {
			kept[sched.SrcFor[i]] = true
		}
		mismatch := 0
		for n := 0; n <= sched.SrcFor[sched.Frames-1]; n++ {
			if perfKeptLaw(n, pnum, qden) != kept[n] {
				mismatch++
				if mismatch <= 5 {
					t.Errorf("%s: integer law disagrees at n=%d (kept=%v)", c.name, n, kept[n])
				}
			}
		}
		t.Logf("%s: fps %g->%g reduced p/q=%d/%d kept=%d/%d law-mismatches=%d",
			c.name, fpsA, fpsR, pnum, qden, len(kept), sched.SrcFor[sched.Frames-1]+1, mismatch)
		// The ffmpeg spelling (select keeps frames where the expr is
		// non-zero). ffmpeg's eval has no mul/sub/lt functions - only the
		// operators - and floor is exact here because every exact multiple
		// of p in the dividend divides to an exactly representable double.
		expr := fmt.Sprintf("lt(floor((%d*n+%d-1)/%d)*%d-%d*n,%d)", qden, pnum-1, pnum, pnum, qden, qden)
		t.Logf("%s: candidate -vf select=%s -fps_mode passthrough", c.name, expr)
	}
}

// ---------------------------------------------------------------------------
// detect-pass split: vidio decode vs pure-Go detection work
// ---------------------------------------------------------------------------

// TestPerfDetectSplit runs over the cached analysis video with the same
// vidio reader + detectFrame pair the shipped detect pass uses: decode-only
// passes vs decode+detectFrame passes. The delta is the pure-Go Otsu/subject
// cost; the decode-only floor is the libavcodec (cgo) cost the proposal to
// thread or replace that reader would attack.
func TestPerfDetectSplit(t *testing.T) {
	n := 2000
	if v := os.Getenv("PERF_DETECT_N"); v != "" {
		if m, err := strconv.Atoi(v); err == nil {
			n = m
		}
	}
	a := perfAnalysis(t, perfFixturePath(t, "sweetdevil.mp4"), perfOutW, false)
	videoPath := a.AnalysisVideoPath()
	if videoPath == "" {
		t.Skip("no analysis video for this cache")
	}
	runPass := func(label string, detect bool) {
		video, err := vidio.NewVideo(videoPath)
		if err != nil {
			t.Fatal(err)
		}
		defer video.Close()
		w, h := video.Width(), video.Height()
		buf := image.NewRGBA(image.Rect(0, 0, w, h))
		if err := video.SetFrameBuffer(buf.Pix); err != nil {
			t.Fatal(err)
		}
		lastThr := -1.0
		start := time.Now()
		count := 0
		for count < n && video.Read() {
			count++
			if detect {
				detectFrame(buf, &lastThr)
			}
		}
		secs := time.Since(start).Seconds()
		per := float64(count) / secs
		t.Logf("%s: %d frames in %.2fs = %.1f ms/frame (extrapolated %d frames = %.0fs)",
			label, count, secs, 1000/per, a.meta.Frames, float64(a.meta.Frames)/per)
	}
	runPass("decode-warm", false)
	runPass("decode-1", false)
	runPass("decode+detect-1", true)
	runPass("decode+detect-2", true)
	runPass("decode-2", false)
}

// ---------------------------------------------------------------------------
// detectFrame optimisation PROTOTYPE (proposal only - shipped code untouched)
// ---------------------------------------------------------------------------

// The shipped detect chain allocates per frame: mono + binary images, a
// [][]int label matrix, a map[int]Rectangle, a DFS stack of image.Point, an
// image.Rect Union per labelled pixel, and a map lookup per labelled pixel.
// The replica below keeps the EXACT same scan order, neighbour-push order,
// threshold chain and arithmetic, but on reused flat arrays with direct Pix
// indexing. If the outputs are bit-identical on real frames, the shipped
// video_dsp.go can be rewritten with zero law change.

// perfGrayFormula: candidate 8-bit luma laws to be validated against
// convertToMonochrome's draw.Draw output (TestPerfDetectFormulaProbe).
type perfGrayFormula struct {
	name  string
	apply func(p []byte) byte // p = rgb24-ish triple at pixel start
}

var perfGrayFormulas = []perfGrayFormula{
	{"go-graymodel", func(p []byte) byte {
		y := (19595*uint32(p[0])*257 + 38470*uint32(p[1])*257 + 7471*uint32(p[2])*257 + 1<<15) >> 24
		return byte(y)
	}},
	{"16bit-shift", func(p []byte) byte {
		w := 299*uint32(p[0])*257 + 587*uint32(p[1])*257 + 114*uint32(p[2])*257
		return byte((w / 1000) >> 8)
	}},
	{"8bit-floor", func(p []byte) byte { return byte((299*int(p[0]) + 587*int(p[1]) + 114*int(p[2])) / 1000) }},
	{"8bit-round", func(p []byte) byte { return byte((299*int(p[0]) + 587*int(p[1]) + 114*int(p[2]) + 500) / 1000) }},
	{"16bit-trunc", func(p []byte) byte {
		w := 299*uint32(p[0])*257 + 587*uint32(p[1])*257 + 114*uint32(p[2])*257
		return byte(w / 1000)
	}},
	{"16bit-shift-round", func(p []byte) byte {
		w := 299*uint32(p[0])*257 + 587*uint32(p[1])*257 + 114*uint32(p[2])*257
		return byte((w/1000 + 128) >> 8)
	}},
}

func TestPerfDetectFormulaProbe(t *testing.T) {
	w, h := 256, 256
	rgba := image.NewRGBA(image.Rect(0, 0, w, h))
	rnd := 1
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			rnd = (rnd*1103515245 + 12345) & 0x7fffffff
			rgba.SetRGBA(x, y, color.RGBA{R: byte(rnd >> 16), G: byte(rnd >> 10), B: byte(rnd >> 4), A: 255})
		}
	}
	want := convertToMonochrome(rgba)
	matched := -1
	firstSamples := make([]string, 0, 12)
	for fi, f := range perfGrayFormulas {
		match := true
		for y := 0; y < h && match; y++ {
			for x := 0; x < w; x++ {
				c := rgba.RGBAAt(x, y)
				p := [3]byte{c.R, c.G, c.B}
				if f.apply(p[:]) != want.GrayAt(x, y).Y {
					match = false
					break
				}
			}
		}
		t.Logf("luma law %q: match=%v", f.name, match)
		if match && matched < 0 {
			matched = fi
		}
		if fi == 0 && !match {
			for y := 0; y < 4; y++ {
				for x := 0; x < 3; x++ {
					c := rgba.RGBAAt(x, y)
					firstSamples = append(firstSamples, fmt.Sprintf("(%d,%d,%d)->%d", c.R, c.G, c.B, want.GrayAt(x, y).Y))
				}
			}
			t.Logf("draw.Draw samples: %s", strings.Join(firstSamples, " "))
		}
	}
	if matched < 0 {
		t.Fatal("no candidate luma law reproduces convertToMonochrome")
	}
	t.Logf("verified luma law: %q (perfOptLuma implements it)", perfGrayFormulas[matched].name)
}

type perfOptState struct {
	w, h   int
	dbg    func(string)
	gray   []byte
	bin    []byte
	labels []int32
	stack  []int32
	hist   []int32
	// per-label stats (indexed by label), reused, truncated to maxLabel
	ln       int
	count    []int32
	sumX     []int64
	sumY     []int64
	minX     []int32
	minY     []int32
	maxX     []int32
	maxY     []int32
	compLab  []int32
	compArea []int32
	compMinY []int32
	compMinX []int32
}

func newPerfOptState(w, h int) *perfOptState {
	st := &perfOptState{w: w, h: h,
		gray: make([]byte, w*h), bin: make([]byte, w*h), labels: make([]int32, w*h),
	}
	return st
}

// perfOptLuma is Go's grayModel law (image/color grayModel: 16-bit
// coefficients 19595/38470/7471 over c.RGBA() values, rounding +1<<15,
// >>24) for 8-bit RGBA input - re-verified by TestPerfDetectFormulaProbe.
func perfOptLuma(p []byte) byte {
	y := (19595*uint32(p[0])*257 + 38470*uint32(p[1])*257 + 7471*uint32(p[2])*257 + 1<<15) >> 24
	return byte(y)
}

// runFrame mirrors detectFrame: grayscale -> Otsu -> hysteresis clamp ->
// binary -> subjects (CCL + stats + pools + maskStats) -> maskCentroid.
func (st *perfOptState) runFrame(rgba []byte, lastThr *float64) (image.Rectangle, int, image.Point) {
	w, h := st.w, st.h
	dbg := st.dbg
	if dbg == nil {
		dbg = func(string) {}
	}
	dbg("gray")
	rgbaGray := st.gray[:0]
	for i := 0; i < w*h; i++ {
		o := i * 4
		rgbaGray = append(rgbaGray, perfOptLuma(rgba[o:o+3]))
	}
	gray := st.gray
	dbg("otsu")
	// otsuThreshold, exact same arithmetic sequence as shipped.
	hist := st.hist[:0]
	if cap(hist) < 256 {
		hist = make([]int32, 0, 256)
	}
	for i := 0; i < 256; i++ {
		hist = append(hist, 0)
	}
	totalPixels := float64(w * h)
	for _, v := range gray {
		hist[v]++
	}
	st.hist = hist
	var norm [256]float64
	for i := range hist {
		norm[i] = float64(hist[i]) / totalPixels
	}
	var cumulativeSum, cumulativeMean [256]float64
	cumulativeSum[0] = norm[0]
	cumulativeMean[0] = 0
	for i := 1; i < 256; i++ {
		cumulativeSum[i] = cumulativeSum[i-1] + norm[i]
		cumulativeMean[i] = cumulativeMean[i-1] + float64(i)*norm[i]
	}
	maxVar := float64(0)
	optThr := uint8(0)
	for tIdx := 0; tIdx < 256; tIdx++ {
		omega := cumulativeSum[tIdx]
		mu := cumulativeMean[tIdx]
		omega2 := 1 - omega
		mu2 := (cumulativeMean[255] - mu) / omega2
		betweenClassVariance := omega * omega2 * (mu - mu2) * (mu - mu2)
		if betweenClassVariance > maxVar {
			maxVar = betweenClassVariance
			optThr = uint8(tIdx)
		}
	}
	thr := float64(optThr)
	if *lastThr < 0 {
		*lastThr = thr
	} else {
		*lastThr = clampF(thr, *lastThr-10, *lastThr+10)
	}
	thrByte := byte(*lastThr)
	dbg("bin")
	for i, v := range gray {
		if v < thrByte {
			st.bin[i] = 0
		} else {
			st.bin[i] = 255
		}
	}
	bin := st.bin
	// connectedComponentLabeling: same (y,x) scan, same 8-neighbour push
	// order, label-on-pop with duplicate pushes allowed, exactly as shipped.
	labels := st.labels
	for i := range labels {
		labels[i] = 0
	}
	dbg("ccl")
	stack := st.stack[:0]
	st.ln = 0
	ensLabel := func(label int32) {
		for st.ln <= int(label) {
			st.count = append(st.count, 0)
			st.sumX = append(st.sumX, 0)
			st.sumY = append(st.sumY, 0)
			st.minX = append(st.minX, 1<<30)
			st.minY = append(st.minY, 1<<30)
			st.maxX = append(st.maxX, -1)
			st.maxY = append(st.maxY, -1)
			st.compLab = append(st.compLab, 0)
			st.compArea = append(st.compArea, 0)
			st.compMinY = append(st.compMinY, 0)
			st.compMinX = append(st.compMinX, 0)
			st.ln++
		}
	}
	label := int32(1)
	for y := 0; y < h; y++ {
		rowOff := y * w
		for x := 0; x < w; x++ {
			idx := rowOff + x
			if bin[idx] > 0 && labels[idx] == 0 {
				ensLabel(label)
				minX, minY, maxX, maxY := x, y, x, y
				stack = append(stack, int32(idx))
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
						nx, ny := cx+dx, cy-1
						if nx >= 0 && nx < w && ny >= 0 && ny < h {
							nidx := ny*w + nx
							if bin[nidx] > 0 && labels[nidx] == 0 {
								stack = append(stack, int32(nidx))
							}
						}
						ny = cy + 1
						if nx >= 0 && nx < w && ny >= 0 && ny < h {
							nidx := ny*w + nx
							if bin[nidx] > 0 && labels[nidx] == 0 {
								stack = append(stack, int32(nidx))
							}
						}
					}
					nx, ny := cx-1, cy
					if nx >= 0 && nx < w {
						nidx := ny*w + nx
						if bin[nidx] > 0 && labels[nidx] == 0 {
							stack = append(stack, int32(nidx))
						}
					}
					nx = cx + 1
					if nx < w {
						nidx := ny*w + nx
						if bin[nidx] > 0 && labels[nidx] == 0 {
							stack = append(stack, int32(nidx))
						}
					}
				}
				st.minX[label] = int32(minX)
				st.minY[label] = int32(minY)
				st.maxX[label] = int32(maxX + 1)
				st.maxY[label] = int32(maxY + 1)
				label++
			}
		}
	}
	st.stack = stack
	dbg("stats")
	maxLabel := label - 1
	if maxLabel == 0 {
		return image.Rectangle{}, 0, image.Point{}
	}
	// Stats pass over the label matrix - the shipped pass Union-combines
	// image.Rect(x,y,x+1,y+1) per labelled pixel; running min/max on the
	// same (y,x) scan order yields the identical tight rect.
	for y := 0; y < h; y++ {
		rowOff := y * w
		for x := 0; x < w; x++ {
			l := labels[rowOff+x]
			if l == 0 {
				continue
			}
			st.count[l]++
			st.sumX[l] += int64(x)
			st.sumY[l] += int64(y)
		}
	}
	// comps in label order (the shipped map order is random; Master and
	// Subjects are order-insensitive outputs - the identity test proves it).
	dbg("comps")
	nComps := 0
	for l := int32(1); l <= maxLabel; l++ {
		if st.count[l] == 0 {
			continue
		}
		area := (st.maxX[l] - st.minX[l]) * (st.maxY[l] - st.minY[l])
		st.compLab[nComps] = l
		st.compArea[nComps] = area
		st.compMinY[nComps] = st.minY[l]
		st.compMinX[nComps] = st.minX[l]
		nComps++
	}
	// sort: Area desc, then MinY asc, then MinX asc (same comparator; the
	// shipped sort's tie behaviour only permutes equal rects, which cannot
	// change Master or the pool count).
	for i := 1; i < nComps; i++ {
		for j := i; j > 0; j-- {
			a, b := j, j-1
			if st.compArea[a] > st.compArea[b] ||
				(st.compArea[a] == st.compArea[b] && (st.compMinY[a] < st.compMinY[b] ||
					(st.compMinY[a] == st.compMinY[b] && st.compMinX[a] < st.compMinX[b]))) {
				st.compLab[a], st.compLab[b] = st.compLab[b], st.compLab[a]
				st.compArea[a], st.compArea[b] = st.compArea[b], st.compArea[a]
				st.compMinY[a], st.compMinY[b] = st.compMinY[b], st.compMinY[a]
				st.compMinX[a], st.compMinX[b] = st.compMinX[b], st.compMinX[a]
			} else {
				break
			}
		}
	}
	dbg("pools")
	largestArea := st.compArea[0]
	tol := h / 20
	// pool claim loop, same order and arithmetic as subjectDetailFromBinary.
	claimed := make([]bool, nComps)
	master := image.Rectangle{}
	haveMaster := false
	pools := 0
	for i := 0; i < nComps; i++ {
		if 100*float64(st.compArea[i])/float64(largestArea) < 20 {
			continue
		}
		combined := image.Rect(int(st.compMinX[i]), int(st.compMinY[i]), int(st.maxX[st.compLab[i]]), int(st.maxY[st.compLab[i]]))
		claimed[i] = true
		for j := i + 1; j < nComps; j++ {
			if claimed[j] {
				continue
			}
			exp := expandRectangle(combined, tol)
			rj := image.Rect(int(st.compMinX[j]), int(st.compMinY[j]), int(st.maxX[st.compLab[j]]), int(st.maxY[st.compLab[j]]))
			if exp.Overlaps(rj) {
				combined = combined.Union(rj)
				claimed[j] = true
			}
		}
		if !haveMaster {
			master, haveMaster = combined, true
		} else {
			master = master.Union(combined)
		}
		pools++
	}
	if pools == 0 {
		return image.Rectangle{}, 0, image.Point{}
	}
	// maskCentroid over the master bbox, direct Pix.
	dbg("centroid")
	var totalX, totalY, count int64
	for y := master.Min.Y; y < master.Max.Y; y++ {
		rowOff := y * w
		for x := master.Min.X; x < master.Max.X; x++ {
			if bin[rowOff+x] == 255 {
				totalX += int64(x)
				totalY += int64(y)
				count++
			}
		}
	}
	var cent image.Point
	if count > 0 {
		cent = image.Point{X: int(totalX / count), Y: int(totalY / count)}
	}
	return master, pools, cent
}

// TestPerfDetectOptimizedIdentity streams the REAL cached analysis video
// through both the shipped detectFrame and the optimized replica and
// requires every (bbox, subjects, centroid, lastThr) tuple to match.
func TestPerfDetectOptimizedIdentity(t *testing.T) {
	n := 200
	if v := os.Getenv("PERF_DETECT_N"); v != "" {
		if m, err := strconv.Atoi(v); err == nil {
			n = m
		}
	}
	a := perfAnalysis(t, perfFixturePath(t, "sweetdevil.mp4"), perfOutW, false)
	video, err := vidio.NewVideo(a.AnalysisVideoPath())
	if err != nil {
		t.Fatal(err)
	}
	defer video.Close()
	w, h := video.Width(), video.Height()
	buf := image.NewRGBA(image.Rect(0, 0, w, h))
	if err := video.SetFrameBuffer(buf.Pix); err != nil {
		t.Fatal(err)
	}
	st := newPerfOptState(w, h)
	rgbaCopy := make([]byte, w*h*4)
	lastOrig, lastOpt := -1.0, -1.0
	mismatch := 0
	count := 0
	for count < n && video.Read() {
		count++
		copy(rgbaCopy, buf.Pix)
		bbox, subj, cent := detectFrame(buf, &lastOrig)
		ob, os, oc := st.runFrame(rgbaCopy, &lastOpt)
		if bbox != ob || subj != os || cent != oc || lastOrig != lastOpt {
			mismatch++
			if mismatch <= 5 {
				t.Errorf("frame %d: orig=(%v,%d,%v,thr=%.6f) opt=(%v,%d,%v,thr=%.6f)",
					count, bbox, subj, cent, lastOrig, ob, os, oc, lastOpt)
			}
		}
	}
	t.Logf("detect identity check: %d frames, %d mismatches", count, mismatch)
	if mismatch > 0 {
		t.Fail()
	}
}

// TestPerfDetectRealChain compares the shipped detect chain with the
// optimized replica on REAL frames captured from the cached analysis video
// (synthetic gradient content is pathological for the flood fill and
// distorts the ratio). Rounds alternate to cancel load drift.
func TestPerfDetectRealChain(t *testing.T) {
	n := 100
	if v := os.Getenv("PERF_DETECT_N"); v != "" {
		if m, err := strconv.Atoi(v); err == nil {
			n = m
		}
	}
	a := perfAnalysis(t, perfFixturePath(t, "sweetdevil.mp4"), perfOutW, false)
	video, err := vidio.NewVideo(a.AnalysisVideoPath())
	if err != nil {
		t.Fatal(err)
	}
	w, h := video.Width(), video.Height()
	buf := image.NewRGBA(image.Rect(0, 0, w, h))
	if err := video.SetFrameBuffer(buf.Pix); err != nil {
		t.Fatal(err)
	}
	frames := make([][]byte, 0, n)
	for len(frames) < n && video.Read() {
		cp := make([]byte, len(buf.Pix))
		copy(cp, buf.Pix)
		frames = append(frames, cp)
	}
	video.Close()
	st := newPerfOptState(w, h)
	img := &image.RGBA{Stride: 4 * w, Rect: image.Rect(0, 0, w, h)}
	t.Logf("captured %d real frames %dx%d", len(frames), w, h)
	for round := 0; round < 3; round++ {
		lastS := -1.0
		start := time.Now()
		for _, px := range frames {
			img.Pix = px
			detectFrame(img, &lastS)
		}
		ship := time.Since(start).Seconds() / float64(len(frames))
		lastO := -1.0
		start = time.Now()
		for _, px := range frames {
			st.runFrame(px, &lastO)
		}
		opt := time.Since(start).Seconds() / float64(len(frames))
		t.Logf("round %d: shipped %.1f ms/frame, optimized %.1f ms/frame (%.2fx)",
			round, ship*1000, opt*1000, ship/opt)
	}
}

func BenchmarkDetectCurrent(b *testing.B) {
	const w, h = 1152, 648
	stBuf := make([]byte, w*h*3)
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	lastThr := -1.0
	for i := 0; i < b.N; i++ {
		perfFeedInto(stBuf, w, h, i)
		for p := 0; p < w*h; p++ {
			o := p * 3
			img.Pix[p*4] = stBuf[o]
			img.Pix[p*4+1] = stBuf[o+1]
			img.Pix[p*4+2] = stBuf[o+2]
			img.Pix[p*4+3] = 255
		}
		detectFrame(img, &lastThr)
	}
}

func BenchmarkDetectOptimized(b *testing.B) {
	const w, h = 1152, 648
	rgba := make([]byte, w*h*4)
	stBuf := make([]byte, w*h*3)
	st := newPerfOptState(w, h)
	lastThr := -1.0
	for i := 0; i < b.N; i++ {
		perfFeedInto(stBuf, w, h, i)
		for p := 0; p < w*h; p++ {
			o := p * 3
			rgba[p*4] = stBuf[o]
			rgba[p*4+1] = stBuf[o+1]
			rgba[p*4+2] = stBuf[o+2]
			rgba[p*4+3] = 255
		}
		st.runFrame(rgba, &lastThr)
	}
}

// ---------------------------------------------------------------------------
// micro-benchmarks
// ---------------------------------------------------------------------------

var perfSink io.Writer = io.Discard

func BenchmarkWritePPMCropFull(b *testing.B) {
	frame := make([]byte, 1920*1080*3)
	crop := image.Rect(0, 0, 972, 300)
	for i := 0; i < b.N; i++ {
		if err := writePPMCrop(perfSink, frame, 1920, 1080, crop); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWritePPMCropBuffered(b *testing.B) {
	frame := make([]byte, 1920*1080*3)
	crop := image.Rect(0, 0, 972, 300)
	buf := bufio.NewWriterSize(io.Discard, 4<<20)
	for i := 0; i < b.N; i++ {
		if err := writePPMCrop(buf, frame, 1920, 1080, crop); err != nil {
			b.Fatal(err)
		}
		if err := buf.Flush(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWritePPMCropSpill(b *testing.B) {
	frame := make([]byte, 1920*1080*3)
	crop := image.Rect(-80, 300, 892, 600) // left edge outside the frame
	for i := 0; i < b.N; i++ {
		if err := writePPMCrop(perfSink, frame, 1920, 1080, crop); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDecodedFrameRead measures the pump's io.ReadFull copy cost for one
// 1920x1080 rgb24 frame: the memcpy half of every decoded-but-not-replayed
// frame (the pipe-syscall half shows up in the subprocess benches instead).
func BenchmarkDecodedFrameRead(b *testing.B) {
	size := 1920 * 1080 * 3
	src := make([]byte, size)
	frame := make([]byte, size)
	for i := 0; i < b.N; i++ {
		if _, err := io.ReadFull(bytes.NewReader(src), frame); err != nil {
			b.Fatal(err)
		}
	}
}
