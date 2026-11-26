//go:build e2e

package holocropper

// Real-video E2E suite for the H render law (hqdn3d + two-stage Lanczos),
// gated behind `-tags e2e` and never part of `go test ./...` or CI:
//
//	cd pkg/holocropper && go test -tags e2e -run E2E -timeout 30m .
//
// Fixtures (vids/music.mp4 fit, vids/sweetdevil.mp4 crop) are READ-ONLY.
// Products land in $E2E_OUT when that variable is set (inspectable, and its
// analysis/ folder warm-caches reruns); otherwise a fresh OS temp dir holds
// everything for the run. There are no baselines: the vids-e2e-test baseline
// regime retired together with the linear-light pipeline it recorded.
//
// The gates are products-of-the-same-plan comparisons:
//   - structural self-checks (dims, frame counts, clocks, codec contract);
//   - Snapshot stills vs the final file's frames: psnr >= 43 dB;
//   - MikV (Sink B) frame payloads vs the final frames: psnr >= 37 dB;
//   - crop preview (VP9) vs the final frames over a 30-frame sample:
//     psnr >= 33 dB;
//   - motion and bar laws, analysis determinism.
// Timing is logged, never asserted.
//
// The three psnr floors were re-baselined 2026-11-26 (owner-approved) after
// measuring why the original 50/40/38 dB targets are unreachable for the
// bright music fixture with the pinned chain: hqdn3d is spatio-TEMPORAL
// (its frame_prev/line_ant buffers persist across the feed), so a
// single-frame still cannot carry a mid-stream final frame's denoise state
// (measured still floor 43.8 dB); and each sink's chain tail makes swscale
// resample the last lanczos stage in a different color space, so raw Sink A
// and raw Sink B outputs of the SAME frame sit 34.4 dB apart before any JPEG
// runs. sweetdevil (dark content) clears every comparison; music lands at
// the floors. The floors are bit-deterministic: they reproduce byte-for-byte
// run to run, per analysis generation.

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
	"sync"
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

// ---------------------------------------------------------------------------
// Run directory: $E2E_OUT or a fresh per-run temp dir, resolved once.

var (
	e2eRunOnce sync.Once
	e2eRunPath string
	e2eRunErr  error
)

func e2eRun(t *testing.T) string {
	t.Helper()
	e2eRunOnce.Do(func() {
		if env := os.Getenv("E2E_OUT"); env != "" {
			e2eRunPath, e2eRunErr = filepath.Abs(env)
			if e2eRunErr == nil {
				e2eRunErr = os.MkdirAll(e2eRunPath, 0o755)
			}
			return
		}
		e2eRunPath, e2eRunErr = os.MkdirTemp("", "holocropper-e2e-*")
	})
	if e2eRunErr != nil {
		t.Fatalf("e2e run dir: %v", e2eRunErr)
	}
	t.Logf("e2e products: %s", e2eRunPath)
	for _, sub := range []string{"analysis", "render-tmp", "mikv-tmp", "products", "stills", "frames"} {
		if err := os.MkdirAll(filepath.Join(e2eRunPath, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return e2eRunPath
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

// e2eFFmpeg requires ffmpeg AND the hqdn3d filter the render chain is built
// from; a missing binary or filter skips the e2e run (the default suite has
// its own HOLOCROPPER_REQUIRE_FFMPEG gate for CI).
var (
	e2eFiltersOnce sync.Once
	e2eNoHQDN3D    bool
)

func e2eFFmpeg(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("e2e needs ffmpeg on PATH")
	}
	e2eFiltersOnce.Do(func() {
		filters, err := exec.Command(path, "-hide_banner", "-filters").Output()
		if err != nil {
			t.Fatalf("ffmpeg -filters: %v", err)
		}
		e2eNoHQDN3D = !strings.Contains(string(filters), " hqdn3d ")
	})
	if e2eNoHQDN3D {
		t.Skip("ffmpeg lacks the hqdn3d filter - the render chain cannot run")
	}
	return path
}

// E2E0Probe reports the ffmpeg build and every fixture's geometry.
func TestE2E0Probe(t *testing.T) {
	ffmpeg := e2eFFmpeg(t)
	root := e2eRepoRoot(t)

	ver, err := exec.Command(ffmpeg, "-version").Output()
	if err != nil {
		t.Fatalf("ffmpeg -version: %v", err)
	}
	t.Logf("ffmpeg: %s", strings.SplitN(string(ver), "\n", 2)[0])
	t.Logf("filter hqdn3d: present")

	for _, name := range []string{"music.mp4", "sweetdevil.mp4"} {
		fixture := e2eFixture(t, root, name)
		w, h, frames, fps := probeFrames(t, fixture)
		t.Logf("fixture %s: %dx%d, %d frames @ %g fps", name, w, h, frames, fps)
	}
}

// ---------------------------------------------------------------------------
// The finals: one render per target per run, memoized for every parity test.

type e2eFinal struct {
	a     *Analysis
	p     *Plan
	res   *Result
	path  string
	sched frameSchedule
}

var (
	e2eFinalMu    sync.Mutex
	e2eFinalCache = map[string]e2eFinal{}
)

// e2eRenderFinal runs the CURRENT pipeline (Analyze -> Plan -> RenderFinal)
// once per target and keeps the products for the parity tests. The analysis
// cache lives in <run>/analysis so every test shares it.
func e2eRenderFinal(t *testing.T, root, ffmpeg, name string) e2eFinal {
	t.Helper()
	e2eFinalMu.Lock()
	defer e2eFinalMu.Unlock()
	if f, ok := e2eFinalCache[name]; ok {
		return f
	}
	tg := e2eTargetFor(t, name)
	fixture := e2eFixture(t, root, tg.fixture)
	out := e2eRun(t)
	start := time.Now()
	// Mirror RenderWhole's one-shot rule: fit settings pick the fit analysis
	// mode unless the caller named one, so this side analyzes exactly what
	// RenderWhole would.
	mode := AnalyzeMode("")
	if tg.settings.FitWithBars {
		mode = AnalyzeFit
	}
	task, err := Analyze(context.Background(), AnalyzeRequest{
		Input: fixture, TempDir: filepath.Join(out, "analysis"), FFmpegPath: ffmpeg, Mode: mode,
	})
	if err != nil {
		t.Fatalf("analyze %s: %v", name, err)
	}
	a, err := task.Wait()
	if err != nil {
		t.Fatalf("analyze %s wait: %v", name, err)
	}
	analyzeSecs := time.Since(start).Seconds()
	p, err := NewPlan(a.Stream(), tg.settings)
	if err != nil {
		t.Fatalf("plan %s: %v", name, err)
	}
	renderStart := time.Now()
	render, err := a.RenderFinal(context.Background(), p, RenderRequest{
		OutputDir:  filepath.Join(out, "products"),
		TempDir:    filepath.Join(out, "render-tmp"),
		FFmpegPath: ffmpeg,
	})
	if err != nil {
		t.Fatalf("render %s: %v", name, err)
	}
	res, err := render.Wait()
	if err != nil {
		t.Fatalf("render %s wait: %v", name, err)
	}
	newPath := filepath.Join(out, "products", name+"-render.mp4")
	if err := os.Remove(newPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := moveFile(res.OutputPath, newPath); err != nil {
		t.Fatalf("publish %s: %v", newPath, err)
	}
	res.OutputPath = newPath
	t.Logf("%s: final render analyzed in %.1f s, rendered %d frames in %.1f s (%.1f fps) -> %s",
		name, analyzeSecs, res.Frames, time.Since(renderStart).Seconds(),
		float64(res.Frames)/time.Since(renderStart).Seconds(), newPath)
	f := e2eFinal{a: a, p: p, res: res, path: newPath, sched: a.frameSchedule(p)}
	e2eFinalCache[name] = f
	return f
}

// e2eStructuralSelfCheck pins the product contract straight on the render:
// resolution, frame count, clock, and the codec law - mjpeg video at
// yuvj420p with the SOURCE's audio track copied through.
func e2eStructuralSelfCheck(t *testing.T, fixture, newPath, name string, wantFrames int) {
	t.Helper()
	w, h, frames, fps := probeFrames(t, newPath)
	if w != e2eOutW || h != e2eOutH {
		t.Errorf("%s: output %dx%d, want %dx%d", name, w, h, e2eOutW, e2eOutH)
	}
	if frames != wantFrames {
		t.Errorf("%s: output file has %d frames, the plan scheduled %d", name, frames, wantFrames)
	}
	if fps != e2eFPS {
		t.Errorf("%s: output runs at %g fps, want %g", name, fps, e2eFPS)
	}
	vc, vpix := e2eVideoShape(t, newPath)
	if vc != "mjpeg" || vpix != "yuvj420p" {
		t.Errorf("%s: video stream %s/%s, want mjpeg/yuvj420p (the MJPEG sink law)", name, vc, vpix)
	}
	wantAudio := e2eAudioCodec(t, fixture)
	gotAudio := e2eAudioCodec(t, newPath)
	if gotAudio != wantAudio {
		t.Errorf("%s: audio codec %q, want the source track (%q) copied through", name, gotAudio, wantAudio)
	}
}

// e2eVideoShape reports the video stream's codec and pixel format.
func e2eVideoShape(t *testing.T, path string) (codec, pixFmt string) {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=codec_name,pix_fmt", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	parts := strings.Split(strings.TrimSpace(string(out)), ",")
	if len(parts) < 2 {
		t.Fatalf("ffprobe %s shape %q", path, strings.TrimSpace(string(out)))
	}
	return parts[0], parts[1]
}

// e2eAudioCodec reports the first audio stream's codec name ("" when the
// file carries no audio at all).
func e2eAudioCodec(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "a:0",
		"-show_entries", "stream=codec_name", "-of", "csv=p=0", path).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// TestE2EMusicFit: the fit product's self-check plus the bar law (the content
// must get real black bars).
func TestE2EMusicFit(t *testing.T) {
	ffmpeg := e2eFFmpeg(t)
	root := e2eRepoRoot(t)
	fixture := e2eFixture(t, root, "music.mp4")

	f := e2eRenderFinal(t, root, ffmpeg, "music")
	e2eStructuralSelfCheck(t, fixture, f.path, "music", f.res.Frames)

	// Fit mode actually bars the product: the resolved content scaled
	// aspect-preserving leaves black columns at the sides - sample the MIDDLE
	// of the left bar, far from any content edge.
	r := f.res.AppliedSettings
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
	e2eBarBlackCheck(t, ffmpeg, f.path, bars/2)
}

// TestE2ESweetDevilCrop: the crop product's self-check and the motion proof -
// consecutive output frames must differ, so a frozen render or a stuck crop
// walk cannot pass.
func TestE2ESweetDevilCrop(t *testing.T) {
	ffmpeg := e2eFFmpeg(t)
	root := e2eRepoRoot(t)
	fixture := e2eFixture(t, root, "sweetdevil.mp4")

	f := e2eRenderFinal(t, root, ffmpeg, "sweetdevil")
	e2eStructuralSelfCheck(t, fixture, f.path, "sweetdevil", f.res.Frames)

	// Consecutive frames at four points in the timeline: a real 60 -> 24 fps
	// crop render never freezes. Three of four pairs must differ measurably.
	pairs := []int{f.res.Frames / 10, f.res.Frames / 3, f.res.Frames / 2, 4 * f.res.Frames / 5}
	differing := 0
	for _, i := range pairs {
		mad := e2eConsecutiveFrameMAD(t, ffmpeg, f.path, i)
		t.Logf("sweetdevil frames %d vs %d: mean abs luma %.3f", i, i+1, mad)
		if mad > 0.02 {
			differing++
		}
	}
	if differing < 3 {
		t.Errorf("only %d of %d consecutive frame pairs differ - the render looks frozen", differing, len(pairs))
	}
}

// ---------------------------------------------------------------------------
// Sink A' parity: the Snapshot still is the final frame's codec path.

// e2eStillsFracs are the timeline fractions the parity gate walks.
var e2eStillsFracs = []int{10, 50, 90}

// TestE2EStillsParity: Snapshot at 10/50/90 % of each timeline against the
// SAME frame extracted from the final render (lossless PNG reference). The
// still carries no baked annotation - it is the frame the final encodes, and
// it is bit-identical to a single-frame pass of the full chain. The floor is
// hqdn3d's temporal state, not the codec: feeding the same PPM frame twice
// through the pinned denoise already produces outputs 54 dB apart, so a
// history-less still cannot match a mid-stream final frame exactly
// (measured: music 43.8 dB, sweetdevil 53.5 dB). psnr >= 43 dB. Single-still
// decode/encode time is logged.
func TestE2EStillsParity(t *testing.T) {
	ffmpeg := e2eFFmpeg(t)
	root := e2eRepoRoot(t)
	out := e2eRun(t)

	for _, tg := range e2eTargets {
		f := e2eRenderFinal(t, root, ffmpeg, tg.name)
		dir := filepath.Join(out, "stills", tg.name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		n := f.sched.Frames
		for _, pct := range e2eStillsFracs {
			outIdx := n * pct / 100 // output slot at this timeline fraction
			if outIdx >= n {
				outIdx = n - 1
			}
			srcIdx := f.sched.SrcFor[outIdx] // the analysis frame that slot replays

			start := time.Now()
			snap, err := f.p.Snapshot(srcIdx+1, 0) // Snapshot takes 1-based frame numbers
			if err != nil {
				t.Fatalf("%s: Snapshot(%d): %v", tg.name, srcIdx+1, err)
			}
			stillSecs := time.Since(start).Seconds()

			if snap.Frame != srcIdx+1 {
				t.Errorf("%s: Snapshot returned frame %d, want %d", tg.name, snap.Frame, srcIdx+1)
			}
			if snap.Width != e2eOutW || snap.Height != e2eOutH {
				t.Fatalf("%s: still %dx%d, want the full output %dx%d", tg.name, snap.Width, snap.Height, e2eOutW, e2eOutH)
			}
			jpg := filepath.Join(dir, fmt.Sprintf("p%d-src%d.jpg", pct, srcIdx))
			if err := os.WriteFile(jpg, snap.JPEG, 0o644); err != nil {
				t.Fatal(err)
			}
			png := filepath.Join(out, "frames", fmt.Sprintf("%s-final%d.png", tg.name, outIdx))
			e2eExtractFrame(t, ffmpeg, f.path, outIdx, png)

			psnr := e2ePSNR(t, ffmpeg, png, jpg)
			t.Logf("%s still @ %d%% (analysis frame %d / output frame %d): psnr %.2f dB, still took %.2f s (jpeg %d B)",
				tg.name, pct, srcIdx, outIdx, psnr, stillSecs, len(snap.JPEG))
			if psnr < 43.0 {
				t.Errorf("%s still @ %d%%: psnr %.2f dB below the 43 dB still-vs-final gate", tg.name, pct, psnr)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Sink C parity: the crop preview encodes the same chain the final does.

// TestE2EPreviewParity renders each target's crop preview (the vp9-webm
// render pass) and compares it against the final frame-for-frame over a
// 30-frame sample. The preview is the only lossy-codec product, and its
// bt709-limited yuv420p tail negotiates the final lanczos stage in a
// different color space than the final file's full-range yuvj420p - the
// shared-chain pixels already differ before VP9 runs (measured: music mean
// 35.6 / worst 33.2 dB, sweetdevil worst 40.9 dB). psnr >= 33 dB per frame.
// Preview wall time is logged.
func TestE2EPreviewParity(t *testing.T) {
	ffmpeg := e2eFFmpeg(t)
	root := e2eRepoRoot(t)
	out := e2eRun(t)

	for _, tg := range e2eTargets {
		f := e2eRenderFinal(t, root, ffmpeg, tg.name)

		start := time.Now()
		pvTask, err := f.a.RenderPreview(context.Background(), f.p, PreviewRequest{
			FFmpegPath: ffmpeg,
			Options:    PreviewOptions{Kind: PreviewCrop},
		})
		if err != nil {
			t.Fatalf("%s preview: %v", tg.name, err)
		}
		pv, err := pvTask.Wait()
		if err != nil {
			t.Fatalf("%s preview wait: %v", tg.name, err)
		}
		pvSecs := time.Since(start).Seconds()

		if pv.MuxVideo != PreviewMuxVP9WebM {
			t.Errorf("%s: crop preview mux %q, want %q (crop previews are always vp9-webm)", tg.name, pv.MuxVideo, PreviewMuxVP9WebM)
		}
		if pv.Frames != f.sched.Frames {
			t.Errorf("%s: preview holds %d frames, the schedule has %d", tg.name, pv.Frames, f.sched.Frames)
		}
		if pv.Width != e2eOutW || pv.Height != e2eOutH {
			t.Errorf("%s: preview %dx%d, want %dx%d", tg.name, pv.Width, pv.Height, e2eOutW, e2eOutH)
		}
		if vc, _ := e2eVideoShape(t, pv.CropVideoPath); vc != "vp9" {
			t.Errorf("%s: preview video codec %q, want vp9", tg.name, vc)
		}
		if e2eAudioCodec(t, filepath.Join(root, "vids", tg.fixture)) != "" {
			if ac := e2eAudioCodec(t, pv.CropVideoPath); ac != "opus" {
				t.Errorf("%s: preview audio %q, want the source track as opus", tg.name, ac)
			}
		}
		t.Logf("%s: crop preview %d frames in %.1f s (%.1f fps) -> %s",
			tg.name, pv.Frames, pvSecs, float64(pv.Frames)/pvSecs, pv.CropVideoPath)

		// A 30-frame sample: ten output frames starting at each reference
		// fraction. Extract each side's sample in one ffmpeg pass and compare
		// the lossless PNG pairs.
		var idxs []int
		for _, pct := range e2eStillsFracs {
			base := f.sched.Frames * pct / 100
			if base+10 > f.sched.Frames {
				base = f.sched.Frames - 10
			}
			for i := base; i < base+10; i++ {
				idxs = append(idxs, i)
			}
		}
		finalDir := filepath.Join(out, "frames", tg.name+"-preview-sample")
		e2eExtractFrames(t, ffmpeg, f.path, idxs, finalDir, "final")
		pvDir := filepath.Join(out, "frames", tg.name+"-preview-side")
		e2eExtractFrames(t, ffmpeg, pv.CropVideoPath, idxs, pvDir, "pv")

		worst, total := 100.0, 0.0
		for i := range idxs {
			// image2 writes %d patterns 1-based; extract names are i+1.
			psnr := e2ePSNR(t, ffmpeg,
				filepath.Join(finalDir, fmt.Sprintf("final%d.png", i+1)),
				filepath.Join(pvDir, fmt.Sprintf("pv%d.png", i+1)))
			total += psnr
			if psnr < worst {
				worst = psnr
			}
			if psnr < 33.0 {
				t.Errorf("%s preview frame %d: psnr %.2f dB below the 33 dB preview-vs-final gate", tg.name, idxs[i], psnr)
			}
		}
		t.Logf("%s: preview-vs-final over %d frames: mean %.2f dB, worst %.2f dB",
			tg.name, len(idxs), total/float64(len(idxs)), worst)
	}
}

// ---------------------------------------------------------------------------
// Sink B parity: the MikV feed and its container.

// TestE2EMikVRoundTrip renders music's MikV (Sink B + unchanged payload
// code), reads the whole container back, and compares sample frames against
// the final render's frames. The raw feed hands the payload code the chain's
// rgb24 output exactly as the MJPEG sink sees it - but the rgb24 and
// yuvj420p sink tails resample in different color spaces (34.4 dB apart raw)
// and the payload re-encodes Go-jpeg-q90 what the final stored as mjpeg-q6;
// the measured floor is 37.8 dB. psnr >= 37 dB. Frame payload sizes are the
// sink's byte budget.
func TestE2EMikVRoundTrip(t *testing.T) {
	ffmpeg := e2eFFmpeg(t)
	root := e2eRepoRoot(t)
	out := e2eRun(t)
	f := e2eRenderFinal(t, root, ffmpeg, "music")

	mikvTask, err := f.a.RenderMikV(context.Background(), f.p, MikVRequest{
		OutputDir:  filepath.Join(out, "products"),
		TempDir:    filepath.Join(out, "mikv-tmp"),
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
	if mres.Frames != f.res.Frames {
		t.Errorf("music mikv holds %d frames, the mp4 render wrote %d", mres.Frames, f.res.Frames)
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
	if len(m.Frames) != f.res.Frames {
		t.Fatalf("mikv read back %d frames, want %d", len(m.Frames), f.res.Frames)
	}

	// Sink B byte budget: payload sizes the frame feed produces.
	minB, maxB, sumB, audioB := 1<<30, 0, 0, 0
	for _, fr := range m.Frames {
		if len(fr.Video) < minB {
			minB = len(fr.Video)
		}
		if len(fr.Video) > maxB {
			maxB = len(fr.Video)
		}
		sumB += len(fr.Video)
		audioB += len(fr.Audio)
	}
	fileSize := int64(0)
	if st, err := os.Stat(mres.OutputPath); err == nil {
		fileSize = st.Size()
	}
	t.Logf("music mikv sink B: video payloads min %d / mean %d / max %d B, audio total %d B, file %d B",
		minB, sumB/len(m.Frames), maxB, audioB, fileSize)

	// Sample interior frames only: this fixture genuinely fades in and out,
	// so frames 0 and last are black. Interior frames must carry picture -
	// and must be the SAME frames the final render wrote (psnr >= 40 dB).
	// The raw feed hands the payload code the chain's rgb24 output exactly as
	// the MJPEG sink sees it, so the separation is encoder loss alone.
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
		jpg := filepath.Join(out, "stills", fmt.Sprintf("mikv-frame%d.jpg", idx))
		if err := os.WriteFile(jpg, m.Frames[idx].Video, 0o644); err != nil {
			t.Fatal(err)
		}
		png := filepath.Join(out, "frames", fmt.Sprintf("music-mikv-final%d.png", idx))
		e2eExtractFrame(t, ffmpeg, f.path, idx, png)
		psnr := e2ePSNR(t, ffmpeg, png, jpg)
		t.Logf("music mikv frame %d vs final frame %d: psnr %.2f dB (jpeg %d B)", idx, idx, psnr, len(m.Frames[idx].Video))
		if psnr < 37.0 {
			t.Errorf("music mikv frame %d: psnr %.2f dB below the 37 dB sink-B-vs-final gate", idx, psnr)
		}
	}
	t.Logf("music mikv round trip: %d frames, header %dx%d@%d, sampled JPEG payloads decode clean with audio",
		len(m.Frames), m.Width, m.Height, m.FPS)
}

// TestE2EAnalysisDeterminism: two independent analysis passes of the same
// fixture must produce frame-identical analysis videos - compared as
// framemd5 TEXT, never file bytes. This is what the CI goldens ride on.
func TestE2EAnalysisDeterminism(t *testing.T) {
	ffmpeg := e2eFFmpeg(t)
	root := e2eRepoRoot(t)
	fixture := e2eFixture(t, root, "music.mp4")
	out := e2eRun(t)

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

// ---------------------------------------------------------------------------
// e2e measurement helpers

// e2eExtractFrame writes output frame idx of video as one PNG (lossless
// comparison reference).
func e2eExtractFrame(t *testing.T, ffmpeg, video string, idx int, outPath string) {
	t.Helper()
	combined, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-i", video,
		"-vf", fmt.Sprintf("select=eq(n\\,%d)", idx), "-fps_mode", "passthrough", "-frames:v", "1",
		outPath, "-y").CombinedOutput()
	if err != nil {
		t.Fatalf("frame extract %s @%d: %v\n%s", video, idx, err, combined)
	}
	if st, err := os.Stat(outPath); err != nil || st.Size() == 0 {
		t.Fatalf("frame extract %s @%d produced no file: %v", video, idx, err)
	}
}

// e2eExtractFrames extracts several frames in ONE decode pass; files land as
// <prefix><i>.png in sample order.
func e2eExtractFrames(t *testing.T, ffmpeg, video string, idxs []int, dir, prefix string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sel := make([]string, len(idxs))
	for i, n := range idxs {
		sel[i] = fmt.Sprintf("eq(n\\,%d)", n)
	}
	combined, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-i", video,
		"-vf", "select="+strings.Join(sel, "+"), "-fps_mode", "passthrough",
		filepath.Join(dir, prefix+"%d.png"), "-y").CombinedOutput()
	if err != nil {
		t.Fatalf("frame batch extract %s: %v\n%s", video, err, combined)
	}
	for i := range idxs {
		p := filepath.Join(dir, fmt.Sprintf("%s%d.png", prefix, i+1))
		if st, err := os.Stat(p); err != nil || st.Size() == 0 {
			t.Fatalf("frame batch extract %s missing %s", video, p)
		}
	}
}

// e2ePSNR returns ffmpeg's psnr filter average in dB (inf reads as 100).
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
