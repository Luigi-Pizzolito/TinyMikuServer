package holocropper

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Luigi-Pizzolito/TinyMikuServer/pkg/mikvformat"
)

/* Frame-clock pins (renders.go frameSchedule).

An Analysis holds the SOURCE's frame clock: prepare re-encodes with
-fps_mode passthrough, and the Plan carries one record per analysis frame. The
render clock is Settings.FPS. Left unmapped, a 12 fps source rendered at a 24
fps preset wrote 2 seconds of picture under a 4 second audio track - and the
MikV export cut its sound to sampleRate/fps per frame, so half the song went
missing. The schedule maps one clock onto the other by nearest frame:
duplicates when the render clock is faster, drops when it is slower, never
reorders, never blends. */

func TestFrameScheduleMapsTheRenderClock(t *testing.T) {
	cases := []struct {
		name         string
		records      int
		analysisFPS  float64
		renderFPS    float64
		wantFrames   int
		wantResample bool
	}{
		{"clocks agree: identity", 96, 24, 24, 96, false},
		{"cache that never recorded a clock", 96, 0, 24, 96, false},
		{"render clock missing", 96, 24, 0, 96, false},
		{"12 fps source at the 24 fps preset", 48, 12, 24, 96, true},
		{"24 fps analysis at the 20 fps PCM pairing", 96, 24, 20, 80, true},
		{"30 fps source at 24 fps", 120, 30, 24, 96, true},
		{"60 fps source at 12 fps", 240, 60, 12, 48, true},
		{"23.976 into 24", 96, 23.976, 24, 96, true},
	}
	for _, c := range cases {
		s := frameScheduleFor(c.records, c.analysisFPS, c.renderFPS)
		if s.Frames != c.wantFrames {
			t.Errorf("%s: %d frames, want %d", c.name, s.Frames, c.wantFrames)
			continue
		}
		if s.Resampled != c.wantResample {
			t.Errorf("%s: Resampled = %v, want %v", c.name, s.Resampled, c.wantResample)
		}
		if len(s.SrcFor) != s.Frames {
			t.Errorf("%s: SrcFor has %d entries for %d frames", c.name, len(s.SrcFor), s.Frames)
			continue
		}
		prev := -1
		for i, src := range s.SrcFor {
			if src < prev {
				t.Errorf("%s: output frame %d replays analysis frame %d after %d: the schedule reordered",
					c.name, i, src, prev)
			}
			prev = src
			if src < 0 || src >= c.records {
				t.Errorf("%s: output frame %d replays analysis frame %d, which the plan does not have",
					c.name, i, src)
			}
		}
		if s.Frames > 0 && s.SrcFor[0] != 0 {
			t.Errorf("%s: output frame 0 replays analysis frame %d, want the first", c.name, s.SrcFor[0])
		}
		// The product must measure the same number of seconds the analysis does.
		if c.analysisFPS > 0 && c.renderFPS > 0 {
			want := float64(c.records) / c.analysisFPS
			got := float64(s.Frames) / c.renderFPS
			tol := 1.0 / math.Min(c.analysisFPS, c.renderFPS)
			if math.Abs(got-want) > tol {
				t.Errorf("%s: the picture is %.4fs, the analysis ran %.4fs (tolerance %.4fs)",
					c.name, got, want, tol)
			}
		}
	}

	// A doubled clock repeats EVERY analysis frame exactly twice: nothing is
	// skipped, which is what keeps the Plan's controller motion intact.
	dup := frameScheduleFor(48, 12, 24)
	for j := 0; j < 48; j++ {
		n := 0
		for _, src := range dup.SrcFor {
			if src == j {
				n++
			}
		}
		if n != 2 {
			t.Fatalf("analysis frame %d is played %d times at 12->24 fps, want exactly 2", j, n)
		}
	}

	// A halved clock drops frames instead of repeating any: the frames it does
	// play are distinct analysis frames, in order.
	drop := frameScheduleFor(96, 24, 12)
	played := map[int]int{}
	for _, src := range drop.SrcFor {
		played[src]++
	}
	if len(played) != drop.Frames {
		t.Fatalf("the 24->12 fps schedule plays %d distinct analysis frames for %d output frames",
			len(played), drop.Frames)
	}
	for src, n := range played {
		if n != 1 {
			t.Fatalf("analysis frame %d is played %d times at a slower render clock", src, n)
		}
	}
}

// probeMediaDuration reads a media file's length with ffprobe. It fails the
// test when ffprobe is missing: this is a duration pin, not an optional extra.
func probeMediaDuration(t *testing.T, path string) float64 {
	t.Helper()
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Fatalf("ffprobe is not on PATH and this test measures durations: %v", err)
	}
	out, err := exec.Command(ffprobe, "-v", "error", "-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var dur float64
	if _, err := fmt.Sscanf(string(out), "%f", &dur); err != nil {
		t.Fatalf("ffprobe duration for %s: %q: %v", path, out, err)
	}
	return dur
}

func warnedResample(warnings []string) bool {
	for _, w := range warnings {
		// The frame-clock warning's own words, so a warning about something
		// else never stands in for it.
		if strings.Contains(w, "the analysis runs at") {
			return true
		}
	}
	return false
}

// TestRenderPassesFollowThePlansFrameClock is the acceptance run on a real
// fixture: a 12 fps source, a Plan at 24 fps, and every output pass measured
// against the 4 seconds the source actually ran.
func TestRenderPassesFollowThePlansFrameClock(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	const srcFPS = 12
	const wantFrames = fixtureSeconds * fixtureFPS // 4 s at the plan's 24 fps

	inputDir, tempDir := t.TempDir(), t.TempDir()
	fixture := buildFixtureAt(t, ffmpeg, inputDir, srcFPS)
	task, err := Analyze(context.Background(), AnalyzeRequest{Input: fixture, TempDir: tempDir, FFmpegPath: ffmpeg})
	if err != nil {
		t.Fatal(err)
	}
	a, err := task.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if a.meta.FPS != srcFPS {
		t.Fatalf("the analysis recorded %g fps, want the source's %d", a.meta.FPS, srcFPS)
	}
	p, err := NewPlan(a.Stream(), Settings{Width: 40, Height: 40, FPS: fixtureFPS})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(p.Records()); got != fixtureSeconds*srcFPS {
		t.Fatalf("the plan holds %d records, want one per analysis frame (%d)", got, fixtureSeconds*srcFPS)
	}
	if got := a.frameSchedule(p).Frames; got != wantFrames {
		t.Fatalf("the schedule plans %d output frames, want %d (4 s at %d fps)", got, wantFrames, fixtureFPS)
	}

	// The preview pair: both files hold the scheduled frames, at the plan's
	// fps, and the pass says out loud that it resampled.
	pt, err := a.RenderPreview(context.Background(), p, PreviewRequest{FFmpegPath: ffmpeg})
	if err != nil {
		t.Fatal(err)
	}
	res, err := pt.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if res.Frames != wantFrames || res.FPS != fixtureFPS {
		t.Errorf("preview result = %d frames at %g fps, want %d at %d", res.Frames, res.FPS, wantFrames, fixtureFPS)
	}
	if !warnedResample(res.Warnings) {
		t.Errorf("the preview pass resampled %d analysis frames into %d without warning: %+v",
			fixtureSeconds*srcFPS, res.Frames, res.Warnings)
	}
	for _, path := range []string{res.CropVideoPath, res.OverlayVideoPath} {
		if path == "" {
			t.Fatalf("preview pair missing a file: %+v", res)
		}
		frames, w, h, _ := frameDigest(t, path)
		if frames != wantFrames || w != 40 || h != 40 {
			t.Errorf("%s decodes to %d frames at %dx%d, want %d at 40x40", path, frames, w, h, wantFrames)
			continue
		}
		if d := probeMediaDuration(t, path); d < 3.9 || d > 4.1 {
			t.Errorf("%s measures %.2fs, want the source's 4 s", path, d)
		}
	}

	// The MikV export: the container carries the scheduled frames AND the
	// sound under them. Before the schedule it held 48 frames - 2 s of picture
	// against a track that ran the full 4 s.
	outDir := t.TempDir()
	mk := runMikVRender(t, a, p, ffmpeg, tempDir, MikVRequest{
		OutputDir: outDir,
		Options:   MikVOptions{VideoJPEG: true, AudioOpus: true, OpusBitrateKbps: 32},
	})
	if mk.Frames != wantFrames || int(mk.FPS) != fixtureFPS {
		t.Fatalf("MikV result = %d frames at %d fps, want %d at %d", mk.Frames, mk.FPS, wantFrames, fixtureFPS)
	}
	if d := float64(mk.Frames) / float64(mk.FPS); d < 3.9 || d > 4.1 {
		t.Errorf("the container's picture is %.2fs, want the source's 4 s", d)
	}

	mikv := mikvformat.NewMikVFormat()
	if err := mikv.Load(mk.OutputPath); err != nil {
		t.Fatalf("Load on the export: %v", err)
	}
	if len(mikv.Frames) != mk.Frames {
		t.Fatalf("the export holds %d frames, the result claimed %d", len(mikv.Frames), mk.Frames)
	}
	var joined []byte
	for _, f := range mikv.Frames {
		joined = append(joined, f.Audio...)
	}
	ogg := filepath.Join(t.TempDir(), "track.ogg")
	if err := os.WriteFile(ogg, joined, 0o644); err != nil {
		t.Fatal(err)
	}
	if d := probeMediaDuration(t, ogg); d < 3.8 || d > 4.1 {
		t.Errorf("the export's audio runs %.2fs against a %.2fs picture: the track must cover the frames it ships with",
			d, float64(mk.Frames)/float64(mk.FPS))
	}
	// The same frames still reassemble as one ordinary Ogg stream.
	if _, err := mikvformat.SplitOGGOpus(joined, mk.FPS, len(mikv.Frames)); err != nil {
		t.Errorf("the per-frame Opus chunks do not reassemble: %v", err)
	}
}
