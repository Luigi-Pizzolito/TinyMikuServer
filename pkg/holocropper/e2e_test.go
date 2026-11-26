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

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	e2eOutW = 648
	e2eOutH = 200
	e2eFPS  = 24.0 // the same render clock on both sides of every comparison
)

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

// E2EBaselines records the 648x200 products of the CURRENT pipeline for both
// fixtures, plus reference stills at 5%..90% of each timeline. An existing
// baseline is never touched (SHARPER_PLAN §5): delete baseline/ by hand only
// when the baseline pipeline itself legitimately changes.
func TestE2EBaselines(t *testing.T) {
	ffmpeg := e2eFFmpeg(t)
	root := e2eRepoRoot(t)
	out := e2eOut(t, root)

	type target struct {
		fixture, name string
		settings      Settings
	}
	targets := []target{
		{"music.mp4", "music", Settings{Width: e2eOutW, Height: e2eOutH, FPS: e2eFPS, FitWithBars: true}},
		{"sweetdevil.mp4", "sweetdevil", Settings{Width: e2eOutW, Height: e2eOutH, FPS: e2eFPS}},
	}

	for _, tg := range targets {
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
