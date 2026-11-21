package holocropper

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

/* Warnings plane (story B4 polish): ffmpeg stderr error/warning tokens
reach the API owner three ways - live through Observer.OnWarning, snapshot
through Task.Warnings() (works mid-run, after completion and after
cancel/failure), and embedded in the finished results' Warnings fields.
Errors themselves go through TaskInfo.Err / Wait() (*FFmpegError carries
the stderr tail), never silently. */

// The classifier fires on ffmpeg's marker words alone; file paths that
// merely contain them (mux summary lines echo every path) stay quiet.
func TestIsDiagnosticWord(t *testing.T) {
	fires := []string{"Error", "error:", "[error]", "Warning", "warning...", "Warnings", "failed!", "Failed"}
	quiet := []string{
		"/tmp/x/holocropper-render-5/TestWarningsQuiet/crop/%d.jpg':",
		"crop.mp4':", "video:24KiB", "frame=", "Errorres", "warningsfoo", "unrecognized",
	}
	for _, tok := range fires {
		if !isDiagnosticWord(tok) {
			t.Errorf("isDiagnosticWord(%q) = false, want true", tok)
		}
	}
	for _, tok := range quiet {
		if isDiagnosticWord(tok) {
			t.Errorf("isDiagnosticWord(%q) = true, want false", tok)
		}
	}
}

func TestTaskWarningsSnapshot(t *testing.T) {
	tb := newTaskBase(context.Background(), TaskRenderFinal, nil)

	if got := tb.Warnings(); got != nil {
		t.Errorf("fresh Task Warnings() = %v, want nil", got)
	}
	tb.warn("first")
	tb.warn("second")
	got := tb.Warnings()
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("Warnings() = %v, want [first second]", got)
	}

	// The caller owns a copy: mutating it must not touch the Task's record.
	got[0] = "mutated"
	if again := tb.Warnings(); again[0] != "first" {
		t.Errorf("Warnings() is not a copy: %v", again)
	}

	// Warnings survive the terminal state (the app can read diagnostics
	// for a Task whose Wait() returned an error).
	tb.finish(errors.New("boom"))
	if after := tb.Warnings(); len(after) != 2 {
		t.Errorf("Warnings() after finish(err) = %v, want both kept", after)
	}

	// Bounded AND deduped (plan U3 "once per pass"): a per-frame ffmpeg
	// diagnostic fires hundreds of times in one pass; the Task surfaces it
	// once, counts the repeats, and the Observer fires once per message.
	observed := 0
	chatty := newTaskBase(context.Background(), TaskRenderFinal, &Observer{
		OnWarning: func(string) { observed++ },
	})
	for i := 0; i < 250; i++ {
		chatty.warn("Error decoding frame")
	}
	if w := chatty.Warnings(); len(w) != 1 || w[0] != "Error decoding frame" {
		t.Errorf("chatty Task warnings = %v, want the single distinct message", w)
	}
	if n := chatty.WarningCounts()["Error decoding frame"]; n != 250 {
		t.Errorf("repeat count = %d, want 250", n)
	}
	if observed != 1 {
		t.Errorf("Observer.OnWarning fired %d times, want once per message per Task", observed)
	}

	// Distinct messages still bound at the cap, oldest first, and a dropped
	// message can surface again as a fresh event.
	big := newTaskBase(context.Background(), TaskRenderFinal, nil)
	for i := 0; i < 250; i++ {
		big.warn(fmt.Sprintf("distinct %d", i))
	}
	if w := big.Warnings(); len(w) != maxTaskWarnings || w[0] != "distinct 50" || w[len(w)-1] != "distinct 249" {
		t.Errorf("cap behaviour: kept %d entries, first %q, last %q", len(w), w[0], w[len(w)-1])
	}
	big.warn("distinct 0")
	if w := big.Warnings(); w[len(w)-1] != "distinct 0" || len(w) != maxTaskWarnings {
		t.Errorf("evicted message did not re-enter as a fresh event: %q (len %d)", w[len(w)-1], len(w))
	}
}

// The failure path must surface the stderr error tokens on the Task AND a
// typed error carrying the tail - the owner learns about a failing pass from
// both channels. The failing command is version-proof: opening a missing
// input aborts every ffmpeg build. (An earlier revision failed on purpose
// via the -vsync option ffmpeg 8 removed - that test silently assumed the
// runner's ffmpeg was already >= 8 and failed on Ubuntu's 6.1, where -vsync
// still parses.)
func TestRunFFmpegProgressWarningsOnFailure(t *testing.T) {
	ffmpeg := requireFFmpeg(t)

	tb := newTaskBase(context.Background(), TaskAnalyze, nil)
	err := runFFmpegProgress(context.Background(), ffmpeg, []string{
		"-hide_banner",
		"-i", "/nonexistent/absent-input.mp4", // fails in every ffmpeg build
		"-vf", "scale=64:48",
		"-f", "null", "-",
	}, PhasePrepare, tb)
	if err == nil {
		t.Fatal("ffmpeg with an unreadable input must fail")
	}
	var ferr *FFmpegError
	if !errors.As(err, &ferr) {
		t.Fatalf("error = %v (%T), want *FFmpegError", err, err)
	}
	if ferr.ExitCode == 0 || ferr.StderrTail == "" {
		t.Errorf("FFmpegError exit=%d tail=%q, want a nonzero code with the stderr tail", ferr.ExitCode, ferr.StderrTail)
	}

	warnings := tb.Warnings()
	if len(warnings) == 0 {
		t.Fatal("failed pass recorded no warnings; the owner must see ffmpeg's error tokens")
	}
	joined := strings.Join(warnings, " ")
	if !strings.Contains(joined, "rror") {
		t.Errorf("warnings %q, want ffmpeg's error tokens", joined)
	}
}

// A clean preview pass must surface zero warning noise (the classifier is
// not firing on normal ffmpeg chatter), and the finished result carries
// the field either way.
func TestPreviewWarningsQuietOnCleanRun(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	a, p := setupRenderFixture(t, ffmpeg, 240, 240)

	task, err := a.RenderPreview(context.Background(), p, PreviewRequest{
		FFmpegPath: ffmpeg,
		Options:    PreviewOptions{Kind: PreviewCropOverlay},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := task.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("clean preview produced warnings: %v", res.Warnings)
	}
	if w := task.Warnings(); len(w) != 0 {
		t.Errorf("clean preview Task Warnings() = %v, want empty", w)
	}
}
