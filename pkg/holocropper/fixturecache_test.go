package holocropper

/* Shared warm fixtures and analysis caches for the pipeline-consumer tests.

   The CI suite budget (SHARPER_PLAN §7) cannot pay a full Analyze pass -
   lavfi build, lossless prepare re-encode, detection - per consuming test:
   the same canonical clip was being analyzed 20+ times per run. This harness

     1. builds each fixture VARIANT once per test-binary run under a shared
        scratch root,
     2. runs one full Analyze per fixture variant and mode (crop = prepare +
        detect; fit = prepare-only),
     3. hands every consumer test a PRIVATE copy of the finished cache
        directory, where the cache-aware Analyze attaches immediately
        ("a valid cache hit completes the Task immediately", analysis.go).

   Isolation is preserved: a copied cache dir is a different path, so the
   analysis registry (preview bookkeeping, render slot), the preview files,
   per-task scratch and any cancellation a test performs all stay per-test.
   The records/meta the tests read are byte-for-byte what the one warm pass
   produced, so detection-derived assertions hold exactly.

   Tests that OBSERVE the analysis machinery live - cache round trips,
   supersede of running passes, cancel mid-detect, resume semantics, the
   golden parity replay - keep their own full passes; this file is only for
   consumers. */

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// sharedFixtureRoot is the run-wide scratch area holding the warm fixtures
// and their warm caches; TestMain removes it after the run.
var sharedFixtureRoot = func() string {
	dir, err := os.MkdirTemp("", "holocropper-fixtures-*")
	if err != nil {
		panic(fmt.Sprintf("holocropper test harness: shared fixture root: %v", err))
	}
	return dir
}()

func TestMain(m *testing.M) {
	code := m.Run()
	os.RemoveAll(sharedFixtureRoot)
	os.Exit(code)
}

// ---------------------------------------------------------------------------
// Warm fixture variants (built once per run)

type warmFixture struct {
	once sync.Once
	path string
}

var (
	warmBox     warmFixture // the canonical buildFixture clip
	warmGray    warmFixture // fit_test's plain gray clip
	warmClock12 warmFixture // the 12 fps source-clock variant
)

// sharedBoxFixture returns the run's canonical lavfi clip path.
func sharedBoxFixture(t *testing.T, ffmpeg string) string {
	t.Helper()
	warmBox.once.Do(func() {
		dir := filepath.Join(sharedFixtureRoot, "box")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		warmBox.path = buildFixture(t, ffmpeg, dir)
	})
	return warmBox.path
}

// sharedGrayFixture returns the plain gray clip the fit chain tests render.
func sharedGrayFixture(t *testing.T, ffmpeg string) string {
	t.Helper()
	warmGray.once.Do(func() {
		dir := filepath.Join(sharedFixtureRoot, "gray")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(dir, "gray.mp4")
		cmd := exec.Command(ffmpeg,
			"-hide_banner", "-loglevel", "error", "-y",
			"-f", "lavfi", "-i", fmt.Sprintf("color=c=gray:s=%dx%d:d=%d:r=%d", fixtureW, fixtureH, fixtureSeconds, fixtureFPS),
			"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:duration=%d", fixtureSeconds),
			"-map", "0:v", "-map", "1:a",
			"-x264-params", "cpu-independent=1",
			"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest",
			out,
		)
		if combined, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("harness gray fixture build failed: %v\n%s", err, combined)
		}
		warmGray.path = out
	})
	return warmGray.path
}

// sharedClock12Fixture returns the slow-source-clock clip the frame-clock
// tests replay at Settings.FPS.
func sharedClock12Fixture(t *testing.T, ffmpeg string) string {
	t.Helper()
	warmClock12.once.Do(func() {
		dir := filepath.Join(sharedFixtureRoot, "clock12")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		warmClock12.path = buildFixtureAt(t, ffmpeg, dir, 12)
	})
	return warmClock12.path
}

// ---------------------------------------------------------------------------
// Warm analysis caches + attach

var warmCacheMu sync.Mutex

// warmCacheDirs maps a fixture path + mode to its finished cache dir inside
// the shared root. Guarded by warmCacheMu; tests run sequentially.
var warmCacheDirs = map[string]string{}

// warmCache runs the one full Analyze per fixture variant and mode (lazily
// - a variant nobody consumes is never analyzed). Crop mode is the full
// prepare+detect pass; fit mode is the prepare-only pass whose stream
// carries no subject readouts.
func warmCache(t *testing.T, ffmpeg, fixture string, mode AnalyzeMode) string {
	t.Helper()
	warmCacheMu.Lock()
	defer warmCacheMu.Unlock()
	key := fixture + "|" + string(mode)
	if dir, ok := warmCacheDirs[key]; ok {
		return dir
	}
	ra, err := resolveAnalyze(AnalyzeRequest{Input: fixture, TempDir: sharedFixtureRoot})
	if err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(sharedFixtureRoot, "warm-"+string(mode)+"-"+filepath.Base(ra.dir))
	if err := os.MkdirAll(temp, 0o755); err != nil {
		t.Fatal(err)
	}
	task, err := Analyze(context.Background(), AnalyzeRequest{
		Input: fixture, TempDir: temp, FFmpegPath: ffmpeg, Mode: mode,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := task.Wait(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(temp, filepath.Base(ra.dir))
	warmCacheDirs[key] = dir
	return dir
}

// attachWarmAnalysis gives the test a private TempDir holding a copy of the
// warm cache and the Analysis the cache-aware Analyze attaches from it.
// The attach runs the full Analyze API path (validation, identity,
// registry), it just completes immediately from the copied entry.
//
// Crop attaches over the detected crop warm cache; fit attaches over the
// fit-skipped warm cache, so subject-free semantics survive.
func attachWarmAnalysis(t *testing.T, ffmpeg, fixture string, mode AnalyzeMode) (*Analysis, string) {
	t.Helper()
	warm := warmCache(t, ffmpeg, fixture, mode)
	local := t.TempDir()
	if err := copyTree(warm, filepath.Join(local, filepath.Base(warm))); err != nil {
		t.Fatalf("attach warm cache: %v", err)
	}
	task, err := Analyze(context.Background(), AnalyzeRequest{
		Input: fixture, TempDir: local, FFmpegPath: ffmpeg, Mode: mode,
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := task.Wait()
	if err != nil {
		t.Fatal(err)
	}
	return a, local
}

// copyTree replicates a finished cache directory (files and subdirs).
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
}
