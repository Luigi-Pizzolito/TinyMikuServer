//go:build e2e

package holocropper

// Real-video E2E suite (SHARPER_PLAN.md §5/§6 Step 7), gated behind
// `-tags e2e` and never part of `go test ./...` or CI:
//
//	go test -tags e2e -run E2E -timeout 30m ./pkg/holocropper
//
// Fixtures (vids/music.mp4 fit, vids/sweetdevil.mp4 crop) are READ-ONLY.
// All products live under vids-e2e-test/ at the repo root (gitignored):
// baseline/ is recorded from the pipeline as it exists when it was first run
// and is never overwritten; new/ is the current pipeline's output and is
// recreated on every run.
//
// The acceptance gate is the owner-resolved corruption guard (SHARPER_PLAN
// §8): every new-vs-baseline video comparison asserts psnr >= 20 dB AND mean
// abs luma <= 12, alongside strict structural checks. Timing is logged,
// never asserted.

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Luigi-Pizzolito/TinyMikuServer/pkg/mikvformat"
)

const (
	e2eOutW = 648
	e2eOutH = 200
	e2eFPS  = 24.0 // the same render clock on both sides of every comparison
)

// targetSpec is one fixture's render target: music plays in fit mode
// (648x200 with bars), sweetdevil in crop mode.
type targetSpec struct {
	fixture, name string
	settings      Settings
}

var e2eTargets = []targetSpec{
	{"music.mp4", "music", Settings{Width: e2eOutW, Height: e2eOutH, FPS: e2eFPS, FitWithBars: true}},
	{"sweetdevil.mp4", "sweetdevil", Settings{Width: e2eOutW, Height: e2eOutH, FPS: e2eFPS}},
}

func e2eTargetFor(t *testing.T, name string) targetSpec {
	t.Helper()
	for _, tg := range e2eTargets {
		if tg.name == name {
			return tg
		}
	}
	t.Fatalf("no e2e target %q", name)
	return targetSpec{}
}

// e2eRepoRoot walks up from the package dir to the repository root - the
// directory holding the root go.mod AND the vids/ fixture folder. (The walk
// requires both so the nested pkg/ go.mods are skipped.)
func e2eRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			if st, err := os.Stat(filepath.Join(dir, "vids")); err == nil && st.IsDir() {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("no repository root (go.mod + vids/) found walking up from the package dir")
	return ""
}

// e2eOut is the shared vids-e2e-test output area under the repo root, with
// the subdirs the suite manages.
func e2eOut(t *testing.T, root string) string {
	t.Helper()
	out := filepath.Join(root, "vids-e2e-test")
	for _, sub := range []string{"baseline", filepath.Join("baseline", "stills"), "new", filepath.Join("new", "stills")} {
		if err := os.MkdirAll(filepath.Join(out, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// e2eFixture resolves a read-only fixture under the repo's vids/ or skips the test.
func e2eFixture(t *testing.T, root, name string) string {
	t.Helper()
	p := filepath.Join(root, "vids", name)
	st, err := os.Stat(p)
	if err != nil || st.Size() == 0 {
		t.Skipf("fixture %s not present - e2e skipped", p)
	}
	return p
}

// e2eFFmpeg requires ffmpeg; a missing binary skips the e2e run (the default
// suite has its own HOLOCROPPER_REQUIRE_FFMPEG gate for CI).
func e2eFFmpeg(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("e2e needs ffmpeg on PATH")
	}
	return path
}

// E2E0Probe reports the ffmpeg build and the filters the render chain needs,
// plus every fixture's geometry (SHARPER_PLAN Step 7a).
func TestE2E0Probe(t *testing.T) {
	ffmpeg := e2eFFmpeg(t)
	root := e2eRepoRoot(t)

	ver, err := exec.Command(ffmpeg, "-version").Output()
	if err != nil {
		t.Fatalf("ffmpeg -version: %v", err)
	}
	t.Logf("ffmpeg: %s", strings.SplitN(string(ver), "\n", 2)[0])

	filters, err := exec.Command(ffmpeg, "-hide_banner", "-filters").Output()
	if err != nil {
		t.Fatalf("ffmpeg -filters: %v", err)
	}
	for _, need := range []string{"zscale", "cas"} {
		if !strings.Contains(string(filters), " "+need+" ") {
			t.Skipf("ffmpeg lacks the %s filter - the linear-light render cannot run", need)
		}
	}
	t.Logf("filters zscale + cas: present")

	for _, name := range []string{"music.mp4", "sweetdevil.mp4"} {
		fixture := e2eFixture(t, root, name)
		w, h, frames, fps := probeFrames(t, fixture)
		t.Logf("fixture %s: %dx%d, %d frames @ %g fps", name, w, h, frames, fps)
	}
}

// E2EBaselines records the 648x200 products of the pipeline as it stands when
// the baseline is FIRST recorded, plus reference stills at 5%..90% of each
// timeline. An existing baseline is never touched (SHARPER_PLAN §5): delete
// baseline/ by hand only when the baseline pipeline itself legitimately
// changes. The pre-change baselines were recorded on 2026-11-26 at commit
// 2356fa1^ (the analysis-cache render pipeline).
func TestE2EBaselines(t *testing.T) {
	ffmpeg := e2eFFmpeg(t)
	root := e2eRepoRoot(t)
	out := e2eOut(t, root)

	for _, tg := range e2eTargets {
		fixture := e2eFixture(t, root, tg.fixture)
		baseline := filepath.Join(out, "baseline", tg.name+"-render.mp4")
		if st, err := os.Stat(baseline); err == nil && st.Size() > 0 {
			t.Logf("baseline %s already recorded (%d bytes) - left untouched", baseline, st.Size())
			continue
		}
		start := time.Now()
		res, err := RenderWhole(context.Background(),
			AnalyzeRequest{Input: fixture, TempDir: filepath.Join(out, "baseline-analysis"), FFmpegPath: ffmpeg},
			tg.settings,
			RenderRequest{OutputDir: filepath.Join(out, "baseline"), TempDir: filepath.Join(out, "baseline-render-tmp"), FFmpegPath: ffmpeg},
		)
		if err != nil {
			t.Fatalf("baseline render %s: %v", tg.name, err)
		}
		if res.OutputW != e2eOutW || res.OutputH != e2eOutH {
			t.Fatalf("baseline %s: output %dx%d, want %dx%d", tg.name, res.OutputW, res.OutputH, e2eOutW, e2eOutH)
		}
		if err := os.Rename(res.OutputPath, baseline); err != nil {
			t.Fatalf("publish baseline %s: %v", baseline, err)
		}
		t.Logf("baseline %s recorded: %d frames, %.1f s elapsed -> %s", tg.name, res.Frames, time.Since(start).Seconds(), baseline)
		e2eRecordStills(t, ffmpeg, baseline, filepath.Join(out, "baseline", "stills", tg.name))
	}
}

// e2eReferenceStills are the timeline fractions 7e compares.
var e2eReferenceFracs = []int{5, 15, 25, 35, 45, 55, 65, 75, 85, 90}

// e2eRecordStills extracts one PNG per reference fraction from a rendered file.
func e2eRecordStills(t *testing.T, ffmpeg, video, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	w, h, frames, fps := probeFrames(t, video)
	if fps <= 0 {
		t.Fatalf("cannot extract stills: %dx%d %d frames @ %g fps", w, h, frames, fps)
	}
	duration := float64(frames) / fps
	for _, pct := range e2eReferenceFracs {
		seek := duration * float64(pct) / 100.0
		out := filepath.Join(dir, "p"+strconv.Itoa(pct)+".png")
		cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error",
			"-ss", strconv.FormatFloat(seek, 'f', 3, 64), "-i", video,
			"-frames:v", "1", out, "-y")
		if combined, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("still at %d%%: %v\n%s", pct, err, combined)
		}
	}
	t.Logf("stills recorded at %d%%..%d%% -> %s", e2eReferenceFracs[0], e2eReferenceFracs[len(e2eReferenceFracs)-1], dir)
}

// ---------------------------------------------------------------------------
// 7b-7f: the new pipeline against the baselines

// e2eRenderNew runs the CURRENT pipeline (Analyze -> Plan -> RenderFinal) for
// one target into new/ and returns the analysis, plan, result and product
// path. The analysis cache lives in new-analysis so every new-side test
// shares it - and it is deliberately NOT the baseline-analysis cache: the
// baseline was recorded from the pre-change analysis pixels and stays that way.
func e2eRenderNew(t *testing.T, root, out, ffmpeg, name string) (*Analysis, *Plan, *Result, string) {
	t.Helper()
	tg := e2eTargetFor(t, name)
	fixture := e2eFixture(t, root, tg.fixture)
	start := time.Now()
	// Mirror RenderWhole's one-shot rule: fit settings pick the fit analysis
	// mode unless the caller named one, so this side analyzes exactly what
	// RenderWhole would.
	mode := AnalyzeMode("")
	if tg.settings.FitWithBars {
		mode = AnalyzeFit
	}
	task, err := Analyze(context.Background(), AnalyzeRequest{
		Input: fixture, TempDir: filepath.Join(out, "new-analysis"), FFmpegPath: ffmpeg, Mode: mode,
	})
	if err != nil {
		t.Fatalf("analyze %s: %v", name, err)
	}
	a, err := task.Wait()
	if err != nil {
		t.Fatalf("analyze %s wait: %v", name, err)
	}
	p, err := NewPlan(a.Stream(), tg.settings)
	if err != nil {
		t.Fatalf("plan %s: %v", name, err)
	}
	render, err := a.RenderFinal(context.Background(), p, RenderRequest{
		OutputDir:  filepath.Join(out, "new"),
		TempDir:    filepath.Join(out, "new-render-tmp"),
		FFmpegPath: ffmpeg,
	})
	if err != nil {
		t.Fatalf("render %s: %v", name, err)
	}
	res, err := render.Wait()
	if err != nil {
		t.Fatalf("render %s wait: %v", name, err)
	}
	newPath := filepath.Join(out, "new", name+"-render.mp4")
	if err := os.Remove(newPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := moveFile(res.OutputPath, newPath); err != nil {
		t.Fatalf("publish %s: %v", newPath, err)
	}
	res.OutputPath = newPath
	t.Logf("%s: new pipeline rendered in %.1f s (result elapsed_seconds %.1f) -> %s", name, time.Since(start).Seconds(), res.ElapsedSeconds, newPath)
	return a, p, res, newPath
}

// e2eNeedBaseline resolves a recorded baseline or skips with instructions.
func e2eNeedBaseline(t *testing.T, out, name string) string {
	t.Helper()
	p := filepath.Join(out, "baseline", name+"-render.mp4")
	if st, err := os.Stat(p); err != nil || st.Size() == 0 {
		t.Skipf("baseline %s not recorded - run TestE2EBaselines first (with the pipeline to baseline)", p)
	}
	return p
}

// e2eStructuralCheck proves the product is the same picture the baseline is:
// resolution, frame count, clock, and the container contract - mjpeg video
// with the source audio track copied through, identical stream formats on
// both sides.
func e2eStructuralCheck(t *testing.T, baseline, newPath, name string) {
	t.Helper()
	bw, bh, bf, bfps := probeFrames(t, baseline)
	nw, nh, nf, nfps := probeFrames(t, newPath)
	if bw != e2eOutW || bh != e2eOutH {
		t.Errorf("%s: baseline %dx%d, want %dx%d - baseline/ is corrupt or was recorded at another target", name, bw, bh, e2eOutW, e2eOutH)
	}
	if nw != e2eOutW || nh != e2eOutH {
		t.Errorf("%s: new output %dx%d, want %dx%d", name, nw, nh, e2eOutW, e2eOutH)
	}
	if bf != nf {
		t.Errorf("%s: new has %d frames, baseline has %d", name, nf, bf)
	}
	if bfps != nfps {
		t.Errorf("%s: new runs at %g fps, baseline at %g", name, nfps, bfps)
	}
	nf2, bf2 := e2eStreamFormats(t, newPath), e2eStreamFormats(t, baseline)
	if strings.Join(nf2, ",") != strings.Join(bf2, ",") {
		t.Errorf("%s: stream formats changed against the baseline (new %v, baseline %v)", name, nf2, bf2)
	}
}

// e2eStreamFormats lists each stream's codec (+ pixel format) via ffprobe.
func e2eStreamFormats(t *testing.T, video string) []string {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error",
		"-show_entries", "stream=codec_name,pix_fmt",
		"-of", "compact=p=0:nk=0", video).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", video, err)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			lines = append(lines, strings.ReplaceAll(l, "|", " "))
		}
	}
	return lines
}

// e2eGuardCompare is the §8 acceptance gate: the structural check already
// proved these are the same video; the guard catches plumbing corruption
// (wrong crops, a broken chain, frozen frames) without pretending the old
// resize law is the ground truth. psnr >= 20 dB AND mean abs luma <= 12.
func e2eGuardCompare(t *testing.T, ffmpeg, name, baseline, newPath string) {
	t.Helper()
	psnr, mad := e2ePSNRMad(t, ffmpeg, baseline, newPath)
	t.Logf("%s new-vs-baseline: psnr %.2f dB, mean abs luma %.3f (guard: >= 20 dB AND <= 12)", name, psnr, mad)
	if psnr < 20.0 {
		t.Errorf("%s: psnr %.2f dB below the 20 dB corruption guard", name, psnr)
	}
	if mad > 12.0 {
		t.Errorf("%s: mean abs luma %.3f above the 12 corruption guard", name, mad)
	}
}

func e2ePSNR(t *testing.T, ffmpeg, a, b string) float64 {
	t.Helper()
	combined, err := exec.Command(ffmpeg, "-hide_banner", "-i", a, "-i", b,
		"-lavfi", "[0:v][1:v]psnr", "-f", "null", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("psnr %s vs %s: %v\n%s", a, b, err, combined)
	}
	m := regexp.MustCompile(`average:([0-9.inf]+)`).FindStringSubmatch(string(combined))
	if m == nil {
		t.Fatalf("no psnr average:\n%s", combined)
	}
	if m[1] == "inf" {
		return 100
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatalf("parse psnr %q: %v", m[1], err)
	}
	return v
}

// e2eMeanAbsLuma is the mean absolute luma difference: blend=difference into
// signalstats YAVG, averaged over the sampled frames.
func e2eMeanAbsLuma(t *testing.T, ffmpeg, a, b string) float64 {
	t.Helper()
	combined, err := exec.Command(ffmpeg, "-hide_banner",
		"-i", a, "-i", b,
		"-lavfi", "[0:v][1:v]blend=all_mode=difference,signalstats,metadata=print:key=lavfi.signalstats.YAVG",
		"-f", "null", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("mad %s vs %s: %v\n%s", a, b, err, combined)
	}
	ms := regexp.MustCompile(`YAVG=([0-9.]+)`).FindAllStringSubmatch(string(combined), -1)
	if len(ms) == 0 {
		t.Fatalf("no YAVG samples:\n%s", combined)
	}
	var total float64
	for _, s := range ms {
		v, err := strconv.ParseFloat(s[1], 64)
		if err != nil {
			t.Fatalf("parse YAVG: %v", err)
		}
		total += v
	}
	return total / float64(len(ms))
}

func e2ePSNRMad(t *testing.T, ffmpeg, a, b string) (float64, float64) {
	t.Helper()
	return e2ePSNR(t, ffmpeg, a, b), e2eMeanAbsLuma(t, ffmpeg, a, b)
}

// TestE2EMusicFit (7b): the fit product against its baseline, the bar law
// (the content must get real black bars), and the MikV round trip - a whole
// production container rendered from the same Plan and read back through
// mikvformat.
func TestE2EMusicFit(t *testing.T) {
	ffmpeg := e2eFFmpeg(t)
	root := e2eRepoRoot(t)
	out := e2eOut(t, root)
	baseline := e2eNeedBaseline(t, out, "music")

	a, p, res, newPath := e2eRenderNew(t, root, out, ffmpeg, "music")
	e2eStructuralCheck(t, baseline, newPath, "music")
	e2eGuardCompare(t, ffmpeg, "music", baseline, newPath)

	// Fit mode actually bars the product: the resolved content scaled
	// aspect-preserving leaves black columns at the sides - sample the MIDDLE
	// of the left bar, far from any content edge.
	r := res.AppliedSettings
	cw, ch := r.ContentW, r.ContentH
	if cw == 0 || ch == 0 {
		cw, ch = r.AnalysisW, r.AnalysisH
	}
	scale := e2eMinF(float64(r.OutputW)/float64(cw), float64(r.OutputH)/float64(ch))
	scaledW := int(float64(cw)*scale + 0.5)
	bars := (r.OutputW - scaledW) / 2
	t.Logf("music fit: content %dx%d analysis px -> %d px of content in the output, %d px bars per side", cw, ch, scaledW, bars)
	if bars < 40 {
		t.Fatalf("music fit produced only %d px of bars - the fit law is not holding", bars)
	}
	e2eBarBlackCheck(t, ffmpeg, newPath, bars/2)

	// MikV round trip from the same Plan (Sink B + unchanged payload code).
	// The audio track rides the OGG-Opus path: the default 16 kHz PCM clock
	// does not divide evenly over this 24 fps render (MikVRequest.Validate
	// says so), and 48 kHz Opus divides exactly (2000 samples per frame).
	mikvTask, err := a.RenderMikV(context.Background(), p, MikVRequest{
		OutputDir:  filepath.Join(out, "new"),
		TempDir:    filepath.Join(out, "new-mikv-tmp"),
		OutputName: "music-frames.mikv",
		FFmpegPath: ffmpeg,
		Options:    MikVOptions{VideoJPEG: true, AudioOpus: true},
	})
	if err != nil {
		t.Fatalf("music mikv: %v", err)
	}
	mres, err := mikvTask.Wait()
	if err != nil {
		t.Fatalf("music mikv wait: %v", err)
	}
	t.Logf("music mikv: %d frames written in %.1f s -> %s", mres.Frames, mres.ElapsedSeconds, mres.OutputPath)
	if mres.Frames != res.Frames {
		t.Errorf("music mikv holds %d frames, the mp4 render wrote %d", mres.Frames, res.Frames)
	}

	m := mikvformat.NewMikVFormat()
	if err := m.Load(mres.OutputPath); err != nil {
		t.Fatalf("music mikv load: %v", err)
	}
	if int(m.Width) != e2eOutW || int(m.Height) != e2eOutH {
		t.Errorf("mikv header %dx%d, want %dx%d", m.Width, m.Height, e2eOutW, e2eOutH)
	}
	if int(m.FPS) != int(e2eFPS) {
		t.Errorf("mikv header fps %d, want %d", m.FPS, int(e2eFPS))
	}
	if len(m.Frames) != res.Frames {
		t.Fatalf("mikv read back %d frames, want %d", len(m.Frames), res.Frames)
	}
	// Sample interior frames only: this fixture genuinely fades in and out,
	// so frames 0 and last are black IN THE BASELINE TOO (measured: baseline
	// frame 0 YMAX=2, frame 3279 YMAX=0). Interior frames must carry picture.
	for _, idx := range []int{len(m.Frames) / 4, len(m.Frames) / 2, 3 * len(m.Frames) / 4} {
		img, err := jpeg.Decode(bytes.NewReader(m.Frames[idx].Video))
		if err != nil {
			t.Fatalf("mikv frame %d is not a decodable JPEG: %v", idx, err)
		}
		if b := img.Bounds(); b.Dx() != e2eOutW || b.Dy() != e2eOutH {
			t.Fatalf("mikv frame %d decodes to %dx%d, want %dx%d", idx, b.Dx(), b.Dy(), e2eOutW, e2eOutH)
		}
		if e2eMaxLuma(img) < 16 {
			t.Errorf("mikv frame %d is black - the frame feed went wrong", idx)
		}
		if len(m.Frames[idx].Audio) == 0 {
			t.Errorf("mikv frame %d carries no audio payload", idx)
		}
	}
	t.Logf("music mikv round trip: %d frames, header %dx%d@%d, sampled JPEG payloads decode clean with audio",
		len(m.Frames), m.Width, m.Height, m.FPS)

	// Reference stills for 7e.
	e2eRecordStills(t, ffmpeg, newPath, filepath.Join(out, "new", "stills", "music"))
}

// TestE2ESweetDevilCrop (7c): the crop product against its baseline, and the
// motion proof - consecutive output frames must differ, so a frozen render or
// a stuck crop walk cannot pass.
func TestE2ESweetDevilCrop(t *testing.T) {
	ffmpeg := e2eFFmpeg(t)
	root := e2eRepoRoot(t)
	out := e2eOut(t, root)
	baseline := e2eNeedBaseline(t, out, "sweetdevil")

	_, _, res, newPath := e2eRenderNew(t, root, out, ffmpeg, "sweetdevil")
	e2eStructuralCheck(t, baseline, newPath, "sweetdevil")
	e2eGuardCompare(t, ffmpeg, "sweetdevil", baseline, newPath)

	// Consecutive frames at four points in the timeline: a real 60 -> 24 fps
	// crop render never freezes. Three of four pairs must differ measurably.
	pairs := []int{res.Frames / 10, res.Frames / 3, res.Frames / 2, 4 * res.Frames / 5}
	differing := 0
	for _, i := range pairs {
		mad := e2eConsecutiveFrameMAD(t, ffmpeg, newPath, i)
		t.Logf("sweetdevil frames %d vs %d: mean abs luma %.3f", i, i+1, mad)
		if mad > 0.02 {
			differing++
		}
	}
	if differing < 3 {
		t.Errorf("only %d of %d consecutive frame pairs differ - the render looks frozen", differing, len(pairs))
	}

	e2eRecordStills(t, ffmpeg, newPath, filepath.Join(out, "new", "stills", "sweetdevil"))
}

// TestE2EAnalysisDeterminism (7d): two independent analysis passes of the
// same fixture must produce frame-identical analysis videos - compared as
// framemd5 TEXT, never file bytes. This is what the CI goldens ride on.
func TestE2EAnalysisDeterminism(t *testing.T) {
	ffmpeg := e2eFFmpeg(t)
	root := e2eRepoRoot(t)
	out := e2eOut(t, root)
	fixture := e2eFixture(t, root, "music.mp4")

	dirs := []string{filepath.Join(out, "determinism-a"), filepath.Join(out, "determinism-b")}
	defer func() {
		for _, d := range dirs {
			os.RemoveAll(d)
		}
	}()
	hashes := make([]string, 2)
	for i, dir := range dirs {
		start := time.Now()
		task, err := Analyze(context.Background(), AnalyzeRequest{Input: fixture, TempDir: dir, FFmpegPath: ffmpeg})
		if err != nil {
			t.Fatalf("determinism analyze %d: %v", i, err)
		}
		a, err := task.Wait()
		if err != nil {
			t.Fatalf("determinism analyze %d wait: %v", i, err)
		}
		hashes[i] = e2eFramemd5(t, ffmpeg, a.AnalysisVideoPath())
		t.Logf("determinism pass %d: analysis in %.1f s", i, time.Since(start).Seconds())
	}
	if hashes[0] != hashes[1] {
		t.Errorf("two analysis passes produced different frames:\nrun A head: %s\nrun B head: %s",
			e2eHeadLines(hashes[0], 3), e2eHeadLines(hashes[1], 3))
	} else {
		t.Logf("analysis determinism: %d frame hashes identical across two passes", strings.Count(hashes[0], "\n")+1)
	}
}

// TestE2EStills (7e): the reference stills (5%..90%) of each new render
// against the baseline stills - frame alignment through the whole timeline.
func TestE2EStills(t *testing.T) {
	ffmpeg := e2eFFmpeg(t)
	root := e2eRepoRoot(t)
	out := e2eOut(t, root)
	for _, tg := range e2eTargets {
		newVideo := filepath.Join(out, "new", tg.name+"-render.mp4")
		if st, err := os.Stat(newVideo); err != nil || st.Size() == 0 {
			t.Skipf("%s: new render missing - run the render tests first", tg.name)
		}
		newDir := filepath.Join(out, "new", "stills", tg.name)
		baseDir := filepath.Join(out, "baseline", "stills", tg.name)
		for _, pct := range e2eReferenceFracs {
			newP := filepath.Join(newDir, "p"+strconv.Itoa(pct)+".png")
			baseP := filepath.Join(baseDir, "p"+strconv.Itoa(pct)+".png")
			if _, err := os.Stat(baseP); err != nil {
				t.Skipf("%s: baseline still p%d missing - record baselines first", tg.name, pct)
			}
			if _, err := os.Stat(newP); err != nil {
				t.Skipf("%s: new still p%d missing - run the render tests first", tg.name, pct)
			}
			psnr, mad := e2ePSNRMad(t, ffmpeg, baseP, newP)
			t.Logf("%s still @ %d%%: psnr %.2f dB, mean abs luma %.3f", tg.name, pct, psnr, mad)
			if psnr < 20.0 || mad > 12.0 {
				t.Errorf("%s still @ %d%% breaks the corruption guard (psnr %.2f, mad %.3f)", tg.name, pct, psnr, mad)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// e2e measurement helpers

// e2eBarBlackCheck samples a 20 px column at x=barX over the whole video and
// requires every frame's mean bar luma to stay black (<= 2 on a full-range
// mjpeg decode).
func e2eBarBlackCheck(t *testing.T, ffmpeg, video string, barX int) {
	t.Helper()
	combined, err := exec.Command(ffmpeg, "-hide_banner", "-i", video,
		"-lavfi", fmt.Sprintf("crop=w=20:h=ih:x=%d:y=0,signalstats,metadata=print:key=lavfi.signalstats.YAVG", barX),
		"-f", "null", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("bar luma: %v\n%s", err, combined)
	}
	ms := regexp.MustCompile(`YAVG=([0-9.]+)`).FindAllStringSubmatch(string(combined), -1)
	if len(ms) == 0 {
		t.Fatalf("no bar YAVG samples:\n%s", combined)
	}
	worst := 0.0
	for _, s := range ms {
		v, err := strconv.ParseFloat(s[1], 64)
		if err != nil {
			t.Fatalf("parse YAVG: %v", err)
		}
		if v > worst {
			worst = v
		}
	}
	t.Logf("fit bars: worst frame bar luma %.3f over %d sampled frames (x=%d)", worst, len(ms), barX)
	if worst > 2.0 {
		t.Errorf("fit bars are not black: worst frame bar luma %.3f", worst)
	}
}

// e2eConsecutiveFrameMAD extracts output frames idx and idx+1 and measures
// their mean absolute luma difference.
func e2eConsecutiveFrameMAD(t *testing.T, ffmpeg, video string, idx int) float64 {
	t.Helper()
	dir := t.TempDir()
	combined, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-i", video,
		"-vf", fmt.Sprintf("select=eq(n\\,%d)+eq(n\\,%d)", idx, idx+1),
		"-fps_mode", "passthrough", filepath.Join(dir, "pair_%d.png"), "-y").CombinedOutput()
	if err != nil {
		t.Fatalf("frame pair extract %d: %v\n%s", idx, err, combined)
	}
	a, b := filepath.Join(dir, "pair_1.png"), filepath.Join(dir, "pair_2.png")
	for _, p := range []string{a, b} {
		if st, err := os.Stat(p); err != nil || st.Size() == 0 {
			t.Fatalf("frame pair extract produced %v (%v)", p, err)
		}
	}
	return e2eMeanAbsLuma(t, ffmpeg, a, b)
}

// e2eFramemd5 returns the frame-hash lines of ffmpeg's framemd5 muxer (the
// comment header stripped) as one comparable string.
func e2eFramemd5(t *testing.T, ffmpeg, video string) string {
	t.Helper()
	out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-i", video, "-f", "framemd5", "-").Output()
	if err != nil {
		t.Fatalf("framemd5 %s: %v", video, err)
	}
	var lines []string
	for _, l := range strings.Split(string(out), "\n") {
		if l != "" && !strings.HasPrefix(l, "#") {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n")
}

func e2eHeadLines(s string, n int) string {
	parts := strings.SplitN(s, "\n", n+1)
	if len(parts) > n {
		parts = parts[:n]
	}
	return strings.Join(parts, " | ")
}

// e2eMaxLuma samples an 8 px grid over an image and returns the brightest
// sample found.
func e2eMaxLuma(img image.Image) float64 {
	b := img.Bounds()
	worst := 0.0
	for y := b.Min.Y; y < b.Max.Y; y += 8 {
		for x := b.Min.X; x < b.Max.X; x += 8 {
			r, g, bl, _ := img.At(x, y).RGBA()
			luma := 0.299*float64(r>>8) + 0.587*float64(g>>8) + 0.114*float64(bl>>8)
			if luma > worst {
				worst = luma
			}
		}
	}
	return worst
}

func e2eMinF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
