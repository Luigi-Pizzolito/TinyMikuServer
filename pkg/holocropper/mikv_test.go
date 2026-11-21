package holocropper

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Luigi-Pizzolito/TinyMikuServer/pkg/mikvformat"
)

/* MikV output pass tests (story D2). ffmpeg-gated like the render tests.

The chain under test is Analyze -> NewPlan -> RenderMikV: the legacy
default output (DEFLATE RGB565 + fixed-budget raw PCM, header encodings
0x00/0x00), the MJPEG+OGG-Opus pairing (header 0x01/0x01, chunks that
reassemble into a plain Ogg stream), naming and OutputName override,
cancel mid-render, the shared render slot, and the option validation that
guards the container's integer fps / divisible PCM budget. */

// setupMikVAnalysis analyzes the standard fixture once for the MikV tests.
func setupMikVAnalysis(t *testing.T, ffmpegPath string, fps float64) (*Analysis, *Plan, string) {
	t.Helper()
	inputDir, tempDir := t.TempDir(), t.TempDir()
	fixture := buildFixture(t, ffmpegPath, inputDir)
	task, err := Analyze(context.Background(), AnalyzeRequest{Input: fixture, TempDir: tempDir, FFmpegPath: ffmpegPath})
	if err != nil {
		t.Fatal(err)
	}
	a, err := task.Wait()
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewPlan(a.Stream(), Settings{Width: 40, Height: 40, FPS: fps})
	if err != nil {
		t.Fatal(err)
	}
	return a, p, tempDir
}

// runMikVRender executes one RenderMikV to completion.
func runMikVRender(t *testing.T, a *Analysis, p *Plan, ffmpegPath, tempDir string, req MikVRequest) *MikVResult {
	t.Helper()
	req.FFmpegPath = ffmpegPath
	if req.TempDir == "" {
		req.TempDir = tempDir
	}
	task, err := a.RenderMikV(context.Background(), p, req)
	if err != nil {
		t.Fatal(err)
	}
	res, err := task.Wait()
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// inflateFrame undoes the MikV DEFLATE frame layout: inflate, then undo
// the writer's byte swap, yielding the RGB565 little-endian buffer.
func inflateFrame(t *testing.T, packed []byte) []byte {
	t.Helper()
	zr, err := zlib.NewReader(bytes.NewReader(packed))
	if err != nil {
		t.Fatalf("zlib reader: %v", err)
	}
	raw, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		t.Fatalf("inflate frame: %v", err)
	}
	for i := 0; i+1 < len(raw); i += 2 {
		raw[i], raw[i+1] = raw[i+1], raw[i]
	}
	return raw
}

// rgb565HasLight reports whether the frame carries bright pixels (the
// fixture's white subject box must survive the crop). pix is the little-
// endian RGB565 buffer: low byte first.
func rgb565HasLight(pix []byte) bool {
	for i := 0; i+1 < len(pix); i += 2 {
		v := uint16(pix[i+1])<<8 | uint16(pix[i])
		if v&0xF800 > 0xE000 && v&0x07E0 > 0x0600 && v&0x001F > 0x18 {
			return true
		}
	}
	return false
}

func checkMikVHeaderBytes(t *testing.T, path string) (videoEnc, audioEnc, reserved byte) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 16 || string(b[:4]) != "MikV" {
		t.Fatalf("%s is not a MikV file", path)
	}
	return b[13], b[14], b[15]
}

// ---------------------------------------------------------------------------

func TestMikVRenderLegacyDefaultOutput(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	// 20 fps: the default 16000 Hz PCM budget divides evenly (800
	// samples per frame) - the legacy converter pairing.
	a, p, tempDir := setupMikVAnalysis(t, ffmpeg, 20)
	outDir := t.TempDir()

	res := runMikVRender(t, a, p, ffmpeg, tempDir, MikVRequest{OutputDir: outDir})

	// The picture follows the Plan's clock, not the analysis clock: 4 s at the
	// plan's 20 fps is 80 frames, resampled (nearest-frame, dropping every
	// sixth) from the 96 frames the fixture's 24 fps analysis clock holds.
	if res.Frames != fixtureSeconds*20 {
		t.Fatalf("frames = %d, want %d: 4 s at the plan's 20 fps", res.Frames, fixtureSeconds*20)
	}
	warned := ""
	for _, w := range res.Warnings {
		if strings.Contains(w, "become") {
			warned = w
		}
	}
	if warned == "" {
		t.Errorf("the pass resampled %d analysis frames to %d output frames without saying so in Warnings",
			fixtureSeconds*fixtureFPS, res.Frames)
	} else {
		t.Logf("frame clock: %s", warned)
	}
	want := filepath.Join(outDir, "fixture-40x40.mikv")
	if res.OutputPath != want {
		t.Fatalf("output path %s, want %s", res.OutputPath, want)
	}
	if res.FPS != 20 || res.OutputW != 40 || res.OutputH != 40 {
		t.Fatalf("header dims %+v", res)
	}
	if res.VideoEncoding != mikvformat.EncodingDeflateRGB565 || res.AudioEncoding != mikvformat.EncodingRawPCM {
		t.Fatalf("encodings %d/%d, want the legacy 0/0", res.VideoEncoding, res.AudioEncoding)
	}
	if v, au, r := checkMikVHeaderBytes(t, want); v != 0 || au != 0 || r != 0 {
		t.Fatalf("header bytes 13/14/15 = %d/%d/%d, want 0/0/0", v, au, r)
	}

	mikv := mikvformat.NewMikVFormat()
	if err := mikv.Load(want); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(mikv.Frames) != res.Frames {
		t.Fatalf("loaded %d frames, wrote %d", len(mikv.Frames), res.Frames)
	}
	if mikv.AudioSampleRate != 16000 || mikv.AudioChannels != 1 || mikv.AudioBitsPerSample != 16 {
		t.Fatalf("audio params %d/%d/%d, want the legacy 16000/1/16",
			mikv.AudioSampleRate, mikv.AudioChannels, mikv.AudioBitsPerSample)
	}

	// Every frame: DEFLATEd RGB565 of exactly 40x40 pixels, and the
	// fixed legacy audio budget of 800 16-bit mono samples (1600 bytes,
	// the silent tail zero-padded like the original converter).
	litFrames := 0
	for i, f := range mikv.Frames {
		if f.VideoEncoding != mikvformat.EncodingDeflateRGB565 || f.AudioEncoding != mikvformat.EncodingRawPCM {
			t.Fatalf("frame %d stamps %d/%d", i, f.VideoEncoding, f.AudioEncoding)
		}
		pix := inflateFrame(t, f.Video)
		if len(pix) != 40*40*2 {
			t.Fatalf("frame %d RGB565 is %d bytes, want %d", i, len(pix), 40*40*2)
		}
		if rgb565HasLight(pix) {
			litFrames++
		}
		if len(f.Audio) != 1600 {
			t.Fatalf("frame %d audio chunk %d bytes, want 1600", i, len(f.Audio))
		}
	}
	// The leashed crop walks across the whole master path before the
	// subject box dominates the frame, so the bright-box frames are a
	// healthy majority rather than "every frame" - a majority of the frames
	// the render clock actually wrote.
	if litFrames*2 <= len(mikv.Frames) {
		t.Fatalf("only %d/%d frames contain the white subject box", litFrames, len(mikv.Frames))
	}

	// The legacy PCM budget is sampleRate/fps samples per frame, and the
	// picture now measures exactly the 4 s the track holds: 80 frames x 800
	// samples = 64000 samples. The silent tail the old frame-clock mismatch
	// produced (96 frames at 20 fps against a 4 s track) is gone.
	silent := 0
	for _, f := range mikv.Frames {
		if bytes.Trim(f.Audio, "\x00") == nil {
			silent++
		}
	}
	if silent > 1 {
		t.Fatalf("%d audio chunks are silent: a 4 s picture at 20 fps wants the whole 4 s track under it", silent)
	}
}

func TestMikVRenderJpegOpusPairing(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	// The firmware MJPEG+Opus pairing: 24 fps video, 48 kHz audio,
	// 40 ms Opus packets.
	a, p, tempDir := setupMikVAnalysis(t, ffmpeg, 24)
	outDir := t.TempDir()

	res := runMikVRender(t, a, p, ffmpeg, tempDir, MikVRequest{
		OutputDir: outDir,
		Options:   MikVOptions{VideoJPEG: true, AudioOpus: true, OpusBitrateKbps: 32},
	})

	if res.FPS != 24 || res.Frames != fixtureSeconds*fixtureFPS {
		t.Fatalf("result %+v", res)
	}
	if res.AudioSampleRate != 48000 || res.AudioChannels != 1 {
		t.Fatalf("opus header audio %d Hz / %d ch", res.AudioSampleRate, res.AudioChannels)
	}
	path := filepath.Join(outDir, "fixture-40x40.mikv")
	if v, au, _ := checkMikVHeaderBytes(t, path); v != 1 || au != 1 {
		t.Fatalf("header bytes 13/14 = %d/%d, want 1/1 (JPEG + OGG-Opus)", v, au)
	}

	mikv := mikvformat.NewMikVFormat()
	if err := mikv.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}

	var joined []byte
	for i, f := range mikv.Frames {
		if f.VideoEncoding != mikvformat.EncodingJPEG || f.AudioEncoding != mikvformat.EncodingOGGOpus {
			t.Fatalf("frame %d stamps %d/%d", i, f.VideoEncoding, f.AudioEncoding)
		}
		img, err := jpeg.Decode(bytes.NewReader(f.Video))
		if err != nil {
			t.Fatalf("frame %d is not a JPEG: %v", i, err)
		}
		if img.Bounds().Dx() != 40 || img.Bounds().Dy() != 40 {
			t.Fatalf("frame %d JPEG is %v, want 40x40", i, img.Bounds())
		}
		joined = append(joined, f.Audio...)
	}

	// Concatenating the per-frame audio chunks must give back an ordinary
	// OGG-Opus stream - the splitter itself is the re-parse proof (the
	// frame schedule is the same one the MikV was cut on).
	if _, err := mikvformat.SplitOGGOpus(joined, 24, len(mikv.Frames)); err != nil {
		t.Fatalf("opus chunks do not reassemble: %v", err)
	}
	if n := bytes.Count(joined, []byte("OggS")); n < 3 {
		t.Fatalf("joined stream has %d Ogg pages, want the full track", n)
	}
	// The OpusHead packet must lead the track (chunk 0 carries it).
	if !bytes.HasPrefix(joined, []byte("OggS")) {
		t.Fatal("joined track does not start with an Ogg page")
	}
	headAt := bytes.Index(joined, []byte("OpusHead"))
	// 27-byte Ogg page header + one OpusHead segment entry.
	if headAt < 0 || headAt > 32 {
		t.Fatalf("OpusHead at offset %d, want inside the first page", headAt)
	}
}

func TestMikVRenderOutputNameOverride(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	a, p, tempDir := setupMikVAnalysis(t, ffmpeg, 24)
	outDir := t.TempDir()

	res := runMikVRender(t, a, p, ffmpeg, tempDir, MikVRequest{
		OutputDir:  outDir,
		OutputName: "custom.mikv",
		Options:    MikVOptions{AudioSampleRate: 48000},
	})
	want := filepath.Join(outDir, "custom.mikv")
	if res.OutputPath != want {
		t.Fatalf("output path %s, want %s", res.OutputPath, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatal(err)
	}
}

func TestMikVRenderCancelMidFrames(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	a, p, tempDir := setupMikVAnalysis(t, ffmpeg, 24)
	outDir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	task, err := a.RenderMikV(ctx, p, MikVRequest{
		OutputDir:  outDir,
		TempDir:    tempDir,
		FFmpegPath: ffmpeg,
		Options:    MikVOptions{AudioSampleRate: 48000},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Wait until the frame loop is visibly running, then cancel it.
	deadline := time.Now().Add(60 * time.Second)
	for {
		info := task.Progress()
		if info.Phase == PhaseMikVFrames && info.Done > 0 {
			break
		}
		if info.State == StateCompleted || info.State == StateFailed ||
			info.State == StateCancelled || info.State == StateSuperseded {
			t.Fatalf("mikv pass reached %s before the test could cancel it: %+v", info.State, info)
		}
		if time.Now().After(deadline) {
			t.Fatal("mikv pass never reached the frame phase")
		}
		time.Sleep(2 * time.Millisecond)
	}
	task.Cancel()

	if _, err := task.Wait(); !errors.Is(err, ErrCancelled) {
		t.Fatalf("want ErrCancelled, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "fixture-40x40.mikv")); !os.IsNotExist(err) {
		t.Fatalf("cancelled pass published an output (or stat failed): %v", err)
	}
	for _, s := range globRenderScratch(t, tempDir) {
		t.Fatalf("cancelled pass left scratch %s behind", s)
	}
}

// waitForMikVSibling blocks until a sibling render on the shared slot is
// visibly in its frame phase (so the supersede cannot race a finished pass).
func waitForMikVSibling(t *testing.T, tb *taskBase, phase Phase) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		info := tb.Progress()
		if info.Phase == phase && info.Done > 0 {
			return
		}
		if info.State == StateCompleted || info.State == StateFailed ||
			info.State == StateCancelled || info.State == StateSuperseded {
			t.Fatalf("sibling render reached %s before the test could act on it: %+v", info.State, info)
		}
		if time.Now().After(deadline) {
			t.Fatalf("sibling render never reached the %s phase: %+v", phase, info)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestMikVSupersedesFinalRender(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	a, p, tempDir := setupMikVAnalysis(t, ffmpeg, 24)
	outDir := t.TempDir()

	first, err := a.RenderFinal(context.Background(), p, RenderRequest{
		OutputDir: outDir, TempDir: tempDir, FFmpegPath: ffmpeg,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForMikVSibling(t, first.taskBase, PhaseRenderFrames)
	mikvTask, err := a.RenderMikV(context.Background(), p, MikVRequest{
		OutputDir: outDir, TempDir: tempDir, FFmpegPath: ffmpeg,
		Options: MikVOptions{AudioSampleRate: 48000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Wait(); err != nil {
		var se *SupersededError
		if !errors.As(err, &se) {
			t.Fatalf("want *SupersededError, got %v", err)
		}
		if se.NewTaskID != mikvTask.ID() {
			t.Fatalf("SupersededError.NewTaskID = %d, want %d", se.NewTaskID, mikvTask.ID())
		}
	} else {
		t.Fatal("the mp4 render was not superseded by the MikV render")
	}
	if _, err := mikvTask.Wait(); err != nil {
		t.Fatalf("superseding MikV render failed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Validation (no ffmpeg, no analysis needed: the request plane answers
// before the pass could touch anything).

func TestMikVRequestValidation(t *testing.T) {
	if errs := (MikVRequest{}).Validate(); len(errs) != 2 {
		t.Fatalf("empty request: %v", errs)
	}
	for _, bad := range []string{".mikv", "a/b.mikv", "../out.mikv", "video.mp4", "no-suffix"} {
		r := MikVRequest{OutputDir: "o", TempDir: "t", OutputName: bad}
		if errs := r.Validate(); len(errs) == 0 {
			t.Errorf("OutputName %q accepted", bad)
		}
	}
	for _, good := range []string{"film.mikv", "film-240x240.mikv"} {
		r := MikVRequest{OutputDir: "o", TempDir: "t", OutputName: good}
		if errs := r.Validate(); len(errs) != 0 {
			t.Errorf("OutputName %q rejected: %v", good, errs)
		}
	}

	cases := []struct {
		name    string
		opts    MikVOptions
		fps     int
		wantErr string // substring; "" means the options must pass
	}{
		{"legacy default at 20 fps", MikVOptions{}, 20, ""},
		{"default pcm at 24 fps", MikVOptions{}, 24, "does not divide evenly"},
		{"explicit 48000 at 24 fps", MikVOptions{AudioSampleRate: 48000}, 24, ""},
		{"opus forces 48000", MikVOptions{AudioOpus: true}, 24, ""},
		{"opus with foreign rate", MikVOptions{AudioOpus: true, AudioSampleRate: 44100}, 24, "granule clock"},
		{"opus at a non-Opus fps", MikVOptions{AudioOpus: true}, 7, "Opus"},
		{"odd channels", MikVOptions{AudioChannels: 5, AudioSampleRate: 48000}, 24, "channels"},
		{"odd bit depth", MikVOptions{AudioBitsPerSample: 24, AudioSampleRate: 48000}, 24, "bits"},
		{"8-bit pcm", MikVOptions{AudioBitsPerSample: 8, AudioSampleRate: 48000}, 24, ""},
		{"8-bit with opus", MikVOptions{AudioOpus: true, AudioBitsPerSample: 8}, 24, "16-bit nominal"},
		{"jpeg quality out of range", MikVOptions{VideoJPEG: true, JPEGQuality: 101, AudioSampleRate: 48000}, 24, "jpeg_quality"},
		{"negative opus bitrate", MikVOptions{AudioOpus: true, OpusBitrateKbps: -1}, 24, "bitrate"},
	}
	for _, c := range cases {
		errs := c.opts.validate(c.fps)
		if c.wantErr == "" {
			if len(errs) != 0 {
				t.Errorf("%s: unexpected errors %v", c.name, errs)
			}
			continue
		}
		if len(errs) == 0 || !bytes.Contains([]byte(fmt.Sprint(errs)), []byte(c.wantErr)) {
			t.Errorf("%s: want an error containing %q, got %v", c.name, c.wantErr, errs)
		}
	}
}

func TestRenderMikVRejectsFractionalFPS(t *testing.T) {
	stream := newTestStream([]streamSample{newStreamSample(image.Rect(300, 120, 360, 240), 1, image.Point{})}, 480, 270, 960, 540)
	p, err := NewPlan(stream, Settings{Width: 40, Height: 40, FPS: 23.5})
	if err != nil {
		t.Fatal(err)
	}
	var a *Analysis // never dereferenced: validation fails before the pass
	_, err = a.RenderMikV(context.Background(), p, MikVRequest{OutputDir: "o", TempDir: "t"})
	var se *SettingsError
	if !errors.As(err, &se) {
		t.Fatalf("want *SettingsError, got %v", err)
	}
	joined := fmt.Sprint(se.Errors)
	if !bytes.Contains([]byte(joined), []byte("integral frame rate")) {
		t.Fatalf("want an fps error, got %v", joined)
	}
}

func TestMikVOutputNaming(t *testing.T) {
	if got := mikvOutputName("clip", 240, 240, ""); got != "clip-240x240.mikv" {
		t.Errorf("default name %q", got)
	}
	if got := mikvOutputName("clip", 240, 240, "keep.mikv"); got != "keep.mikv" {
		t.Errorf("override name %q", got)
	}
}
