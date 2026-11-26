package holocropper

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

/* The ONE chain builder and the passes that must route through it.
TestFinalFilterChain pins the "H" law as exact strings; the fake-ffmpeg tests
run the real preview and still code paths with an ffmpeg stand-in that records
every argv, so the -vf the passes execute is compared against
finalFilterChain's own output. */

func TestFinalFilterChain(t *testing.T) {
	cases := []struct {
		name                   string
		mode, sink             string
		outW, outH, fitW, fitH int
		padX, padY             int
		want                   string
	}{
		{"crop mjpeg", finalModeCrop, sinkMJPEG, 648, 200, 648, 200, 0, 0,
			"hqdn3d=2:1.5:0:0,scale=iw*2/3:ih*2/3:flags=lanczos,scale=648:200:flags=lanczos,format=yuvj420p"},
		{"crop rgb", finalModeCrop, sinkRGB, 648, 200, 648, 200, 0, 0,
			"hqdn3d=2:1.5:0:0,scale=iw*2/3:ih*2/3:flags=lanczos,scale=648:200:flags=lanczos,format=rgb24"},
		{"crop vp9", finalModeCrop, sinkVP9, 648, 200, 648, 200, 0, 0,
			"hqdn3d=2:1.5:0:0,scale=iw*2/3:ih*2/3:flags=lanczos,scale=648:200:flags=lanczos:out_color_matrix=bt709:out_range=tv,format=yuv420p"},
		// The owner's fit example, stated verbatim.
		{"fit mjpeg", finalModeFit, sinkMJPEG, 648, 200, 356, 200, 146, 0,
			"hqdn3d=2:1.5:0:0,scale=iw*2/3:ih*2/3:flags=lanczos,scale=356:200:flags=lanczos,pad=648:200:146:0:black,format=yuvj420p"},
		{"fit rgb", finalModeFit, sinkRGB, 648, 200, 356, 200, 146, 0,
			"hqdn3d=2:1.5:0:0,scale=iw*2/3:ih*2/3:flags=lanczos,scale=356:200:flags=lanczos,pad=648:200:146:0:black,format=rgb24"},
		{"fit vp9", finalModeFit, sinkVP9, 648, 200, 356, 200, 146, 0,
			"hqdn3d=2:1.5:0:0,scale=iw*2/3:ih*2/3:flags=lanczos,scale=356:200:flags=lanczos:out_color_matrix=bt709:out_range=tv,pad=648:200:146:0:black,format=yuv420p"},
		// No pad when the fit box already is the output.
		{"fit mjpeg no pad", finalModeFit, sinkMJPEG, 648, 200, 648, 200, 0, 0,
			"hqdn3d=2:1.5:0:0,scale=iw*2/3:ih*2/3:flags=lanczos,scale=648:200:flags=lanczos,format=yuvj420p"},
		{"fit rgb no pad", finalModeFit, sinkRGB, 240, 240, 240, 240, 0, 0,
			"hqdn3d=2:1.5:0:0,scale=iw*2/3:ih*2/3:flags=lanczos,scale=240:240:flags=lanczos,format=rgb24"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := finalFilterChain(c.mode, c.sink, c.outW, c.outH, c.fitW, c.fitH, c.padX, c.padY)
			if got != c.want {
				t.Errorf("chain\n got %s\nwant %s", got, c.want)
			}
		})
	}
	if finalDenoise != "hqdn3d=2:1.5:0:0" {
		t.Errorf("finalDenoise = %q", finalDenoise)
	}
	if mjpegQ != 6 {
		t.Errorf("mjpegQ = %d, want 6 (the value the product has always used)", mjpegQ)
	}
}

func TestFitBoxToOutput(t *testing.T) {
	cases := []struct {
		cw, ch, outW, outH     int
		fitW, fitH, padX, padY int
	}{{
		356, 200, 648, 200, 356, 200, 146, 0, // the fit example's own box
	}, {
		648, 200, 648, 200, 648, 200, 0, 0, // matching aspects: exact fill, no pad
	}, {
		854, 480, 648, 200, 356, 200, 146, 0, // 16:9 content into 3.24:1 output: height binds
	}, {
		96, 540, 240, 240, 43, 240, 98, 0, // tall content into a square: width binds, bars left/right
	}}
	for _, c := range cases {
		fitW, fitH, padX, padY := fitBoxToOutput(c.cw, c.ch, c.outW, c.outH)
		if fitW != c.fitW || fitH != c.fitH || padX != c.padX || padY != c.padY {
			t.Errorf("fitBoxToOutput(%d,%d -> %d,%d) = %d,%d pad %d,%d; want %d,%d pad %d,%d",
				c.cw, c.ch, c.outW, c.outH, fitW, fitH, padX, padY, c.fitW, c.fitH, c.padX, c.padY)
		}
	}
}

// ---------------------------------------------------------------------------
// The fake ffmpeg: records every argv (fields joined by \x1f, one call per
// line) and impersonates just enough behaviour for the passes to run -
// the rgb24/pipe:1 decoder emits FAKE_DECODE_FRAMES zero frames of
// FAKE_FRAME_BYTES, the -vf encoder drains stdin and leaves a stub sink file,
// anything else (the audio probe) exits clean.

const fakeFFmpegScript = `#!/usr/bin/env python3
import os, sys
args = sys.argv[1:]
with open(os.environ["FAKE_FFMPEG_LOG"], "a") as f:
    f.write("\x1f".join(args) + "\n")
if "rawvideo" in args and "pipe:1" in args:
    n = int(os.environ["FAKE_DECODE_FRAMES"])
    fb = int(os.environ["FAKE_FRAME_BYTES"])
    chunk = b"\0" * min(fb, 1 << 20)
    for _ in range(n):
        left = fb
        while left:
            part = chunk[:left] if left < len(chunk) else chunk
            sys.stdout.buffer.write(part)
            left -= len(part)
        sys.stdout.buffer.flush()
elif "-vf" in args:
    sys.stdin.buffer.read()
    for a in args:
        # Stub the SINK only: a file that does not exist yet. Never touch an
        # existing input (the shared warm fixtures live under /tmp too).
        if a.endswith((".mp4", ".webm", ".jpg")) and not os.path.exists(a):
            with open(a, "wb") as g:
                g.write(b"\0")
sys.exit(0)
`

func writeFakeFFmpeg(t *testing.T) (dir, logPath string) {
	t.Helper()
	dir = t.TempDir()
	logPath = filepath.Join(dir, "argv.log")
	bin := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(bin, []byte(fakeFFmpegScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_FFMPEG_LOG", logPath)
	return dir, logPath
}

// fakeFFmpegLog reads back the recorded invocations.
func fakeFFmpegLog(t *testing.T, logPath string) [][]string {
	t.Helper()
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("fake ffmpeg log: %v", err)
	}
	var calls [][]string
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if line == "" {
			continue
		}
		calls = append(calls, strings.Split(line, "\x1f"))
	}
	return calls
}

// vfOf finds the single -vf value among the recorded calls, failing if the
// pass ran zero or several filter chains.
func vfOf(t *testing.T, calls [][]string) string {
	t.Helper()
	var vf string
	n := 0
	for _, args := range calls {
		for i, a := range args {
			if a == "-vf" && i+1 < len(args) {
				vf = args[i+1]
				n++
			}
		}
	}
	if n != 1 {
		t.Fatalf("recorded %d -vf chains, want exactly the one shared chain", n)
	}
	return vf
}

// The crop PREVIEW pass must execute exactly finalFilterChain's VP9 chain -
// same builder, no private filter args - and decode from the start (no -ss).
func TestPreviewPassUsesSharedChain(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	a, p := setupRenderFixture(t, ffmpeg, 240, 240)

	fakeDir, logPath := writeFakeFFmpeg(t)
	t.Setenv("FAKE_DECODE_FRAMES", strconv.Itoa(len(p.records)))
	t.Setenv("FAKE_FRAME_BYTES", strconv.Itoa(p.resolved.SourceW*p.resolved.SourceH*3))

	task, err := a.RenderPreview(context.Background(), p, PreviewRequest{
		FFmpegPath: filepath.Join(fakeDir, "ffmpeg"),
		Options:    PreviewOptions{Kind: PreviewCrop},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := task.Wait(); err != nil {
		t.Fatal(err)
	}

	r := p.resolved
	want := finalFilterChain(finalModeCrop, sinkVP9, r.OutputW, r.OutputH, r.OutputW, r.OutputH, 0, 0)
	calls := fakeFFmpegLog(t, logPath)
	if got := vfOf(t, calls); got != want {
		t.Errorf("preview executed\n got %s\nwant %s", got, want)
	}
	dec := 0
	for _, args := range calls {
		if containsArg(args, "rawvideo") && containsArg(args, "pipe:1") {
			dec++
			if containsArg(args, "-ss") {
				t.Error("preview decoder used an input seek: previews decode from the start")
			}
			if containsArg(args, "-vf") {
				t.Error("preview decoder carried a -vf chain: only the encoder runs the chain")
			}
		}
	}
	if dec != 1 {
		t.Fatalf("recorded %d decode passes, want 1", dec)
	}
}

// The STILL pass must execute exactly finalFilterChain's MJPEG chain and seek
// the SOURCE with the half-early law. Snapshot takes no ffmpeg path (PATH
// lookup), so the fake is injected as "ffmpeg" itself - after the warm fixture
// is already built with the real one.
func TestStillsPassUsesSharedChain(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	_, p := setupRenderFixture(t, ffmpeg, 240, 240)

	fakeDir, logPath := writeFakeFFmpeg(t) // fake named "ffmpeg": Snapshot takes no path, it resolves PATH
	t.Setenv("PATH", fakeDir+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_DECODE_FRAMES", "1")
	t.Setenv("FAKE_FRAME_BYTES", strconv.Itoa(p.resolved.SourceW*p.resolved.SourceH*3))

	const frame = 48 // 1-based
	snap, err := p.Snapshot(frame, 0)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Frame != frame || snap.Width != 240 || snap.Height != 240 {
		t.Fatalf("snapshot meta %+v, want frame %d at 240x240", snap, frame)
	}

	r := p.resolved
	want := finalFilterChain(finalModeCrop, sinkMJPEG, r.OutputW, r.OutputH, r.OutputW, r.OutputH, 0, 0)
	calls := fakeFFmpegLog(t, logPath)
	if got := vfOf(t, calls); got != want {
		t.Errorf("still executed\n got %s\nwant %s", got, want)
	}
	wantSS := strconv.FormatFloat(stillSeekSeconds(frame-1, p.analysisFPS), 'f', 6, 64)
	dec := 0
	for _, args := range calls {
		if containsArg(args, "rawvideo") && containsArg(args, "pipe:1") {
			dec++
			i := indexOfArg(args, "-ss")
			if i < 0 || args[i+1] != wantSS {
				t.Errorf("still decode seek = %v, want the half-early law %s", args, wantSS)
			}
			if containsArg(args, p.videoPath) {
				t.Errorf("still decode opened the analysis proxy %s instead of the source", p.videoPath)
			}
			src, err := p.sourceInputPath()
			if err != nil {
				t.Fatal(err)
			}
			if !containsArg(args, src) {
				t.Errorf("still decode did not open the analyzed input %s: %v", src, args)
			}
		}
	}
	if dec != 1 {
		t.Fatalf("recorded %d decode passes, want 1", dec)
	}
}

func containsArg(args []string, want string) bool { return indexOfArg(args, want) >= 0 }

func indexOfArg(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}
