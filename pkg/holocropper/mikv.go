package holocropper

import (
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Luigi-Pizzolito/TinyMikuServer/pkg/mikvformat"
)

/* The MikV output pass (HOLOCROPPER-API-PLAN.md §6.11, story D2).

RenderMikV replays a Plan against the cached Analysis video exactly like
RenderFinal does - same frame loop, same supersede slot - but instead of
muxing an mp4 it writes the firmware's MikV container straight from the
planned frames:

  - video: the legacy default - RGB565 packed and DEFLATEd per frame
    (header byte 13 = 0x00); MikVOptions.VideoJPEG switches to one JPEG
    per frame (byte 13 = 0x01, MJPEG-in-MikV);
  - audio: the legacy raw PCM track cut to sampleRate/fps samples per
    frame (header byte 14 = 0x00); MikVOptions.AudioOpus encodes the
    whole track as ONE OGG-Opus stream at the packet duration the fps
    picks and cuts it at Ogg page boundaries into one chunk per frame
    (byte 14 = 0x01) - concatenating the chunks reproduces a plain .ogg
    the firmware's ogg_pages/opus decoder plays untouched.

24 fps video with 48 kHz OGG-Opus audio (40 ms packets) is the pairing
the firmware MJPEG+Opus path is designed for. The default options write
byte-identical legacy MikV: DEFLATE RGB565 + raw PCM. */

// MikV audio defaults mirror the mikv-converter's: the values the
// original tooling wrote for every file the firmware plays today.
const (
	defaultMikVAudioSampleRate    = 16000
	defaultMikVAudioChannels      = 1
	defaultMikVAudioBitsPerSample = 16
)

// MikVOptions picks the container's payload encodings and audio shape.
// The zero value is the legacy format: DEFLATEd RGB565 video, raw PCM
// audio at 16000 Hz mono 16-bit.
type MikVOptions struct {
	// VideoJPEG stores each frame as a complete JPEG image (video
	// encoding 0x01) instead of the DEFLATEd RGB565 default (0x00).
	VideoJPEG bool
	// JPEGQuality applies to VideoJPEG frames; 0 = mikvformat default.
	JPEGQuality int

	// AudioOpus stores the audio track as OGG-Opus page segments (audio
	// encoding 0x01) instead of raw PCM (0x00); it forces the 48000 Hz
	// granule clock the Opus timeline requires.
	AudioOpus bool
	// OpusBitrateKbps is the -b:a value handed to ffmpeg's libopus;
	// 0 keeps libopus's own default bitrate.
	OpusBitrateKbps int

	// AudioSampleRate / AudioChannels / AudioBitsPerSample fill the
	// header audio fields. 0 selects the legacy defaults above. OGG-Opus
	// replaces them with its own (48000 Hz, the channel count, 16-bit
	// nominal); the OpusHead packet stays authoritative for playback.
	AudioSampleRate    int
	AudioChannels      int
	AudioBitsPerSample int
}

// validate reports every option violation at once against the integer fps
// the Plan will write. fps is already known integral and bounded here.
func (o MikVOptions) validate(fps int) []ValidationError {
	var errs []ValidationError

	if o.JPEGQuality < 0 || o.JPEGQuality > 100 {
		errs = append(errs, ValidationError{Field: "options.jpeg_quality", Reason: "must be 0 (use the default) or 1-100"})
	}
	if o.OpusBitrateKbps < 0 {
		errs = append(errs, ValidationError{Field: "options.opus_bitrate_kbps", Reason: "must be 0 (encoder default) or positive"})
	}

	rate := o.AudioSampleRate
	if o.AudioOpus {
		if rate != 0 && rate != mikvformat.OGGOpusSampleRate {
			errs = append(errs, ValidationError{Field: "options.audio_sample_rate",
				Reason: fmt.Sprintf("OGG-Opus audio runs on the %d Hz granule clock", mikvformat.OGGOpusSampleRate)})
		}
		rate = mikvformat.OGGOpusSampleRate
	} else {
		if rate != 0 && (rate < 1000 || rate > 65535) {
			errs = append(errs, ValidationError{Field: "options.audio_sample_rate", Reason: "must be 0 (16000 default) or 1000-65535"})
		}
		if rate == 0 {
			rate = defaultMikVAudioSampleRate
		}
		// Raw PCM chunks are fixed-length: the firmware takes
		// sampleRate/fps samples per frame, so the rate must divide -
		// the default rate included.
		if rate%fps != 0 {
			errs = append(errs, ValidationError{Field: "options.audio_sample_rate",
				Reason: fmt.Sprintf("%d Hz does not divide evenly over %d fps (PCM frames take sampleRate/fps whole samples); use OGG-Opus audio, or plan at an fps that divides %d: %s",
					rate, fps, rate, divisibleFPS(rate))})
		}
	}

	switch o.AudioChannels {
	case 0, 1, 2:
	default:
		errs = append(errs, ValidationError{Field: "options.audio_channels", Reason: "must be 0 (mono default), 1 or 2"})
	}

	switch {
	case o.AudioBitsPerSample != 0 && o.AudioBitsPerSample != 8 && o.AudioBitsPerSample != 16:
		errs = append(errs, ValidationError{Field: "options.audio_bits_per_sample", Reason: "must be 0 (16-bit default), 8 or 16"})
	case o.AudioOpus && o.AudioBitsPerSample == 8:
		errs = append(errs, ValidationError{Field: "options.audio_bits_per_sample", Reason: "OGG-Opus audio writes the 16-bit nominal header; 8 does not apply"})
	}

	if o.AudioOpus {
		if _, err := mikvformat.OGGOpusFrameDurationMs(uint8(fps)); err != nil {
			errs = append(errs, ValidationError{Field: "fps", Reason: err.Error()})
		}
	}
	return errs
}

// divisibleFPS lists the integral frame rates (2-120, the Settings range)
// that a sample rate divides evenly - for the divisibility error message.
func divisibleFPS(rate int) string {
	var parts []string
	for f := 2; f <= 120; f++ {
		if rate%f == 0 {
			parts = append(parts, strconv.Itoa(f))
		}
	}
	return strings.Join(parts, ", ")
}

// resolved fills the option defaults validate accepted.
func (o MikVOptions) resolved() (sampleRate int, channels int, bits int) {
	sampleRate, channels, bits = o.AudioSampleRate, o.AudioChannels, o.AudioBitsPerSample
	if o.AudioOpus {
		if sampleRate == 0 {
			sampleRate = mikvformat.OGGOpusSampleRate
		}
		bits = defaultMikVAudioBitsPerSample
	} else if sampleRate == 0 {
		sampleRate = defaultMikVAudioSampleRate
	}
	if channels == 0 {
		channels = defaultMikVAudioChannels
	}
	if bits == 0 {
		bits = defaultMikVAudioBitsPerSample
	}
	return
}

// MikVRequest asks RenderMikV for the Plan's production file in MikV.
type MikVRequest struct {
	OutputDir   string      `json:"output_dir"`  // required, must exist
	TempDir     string      `json:"temp_dir"`    // required scratch parent
	FFmpegPath  string      `json:"ffmpeg_path"` // "" = PATH
	OutputName  string      `json:"output_name"` // final file name inside OutputDir; "" = <basename>-<WxH>.mikv
	KeepWorkDir bool        `json:"keep_work_dir"`
	Options     MikVOptions `json:"options"`
	Observer    *Observer   `json:"-"`
}

// Validate reports ALL field violations at once (directory existence is
// checked when the pass resolves the request).
func (r MikVRequest) Validate() []ValidationError {
	var errs []ValidationError
	if r.OutputDir == "" {
		errs = append(errs, ValidationError{Field: "output_dir", Reason: "an existing output directory is required"})
	}
	if r.TempDir == "" {
		errs = append(errs, ValidationError{Field: "temp_dir", Reason: "a scratch parent directory is required"})
	}
	if r.OutputName != "" && !validMikVFileName(r.OutputName) {
		errs = append(errs, ValidationError{Field: "output_name", Reason: "must be a plain .mikv file name (no path separators)"})
	}
	return errs
}

// validMikVFileName accepts only a bare .mikv file name: OutputName never
// steers the write outside OutputDir.
func validMikVFileName(name string) bool {
	return name != ".mikv" && filepath.Base(name) == name && strings.HasSuffix(name, ".mikv")
}

// MikVResult is what a finished MikVTask Wait() returns.
type MikVResult struct {
	InputPath  string `json:"input_path"`
	OutputPath string `json:"output_path"`
	WorkDir    string `json:"work_dir"` // render scratch; "" when it was cleaned up

	SourceW   int `json:"source_w"`
	SourceH   int `json:"source_h"`
	AnalysisW int `json:"analysis_w"`
	AnalysisH int `json:"analysis_h"`
	OutputW   int `json:"output_w"`
	OutputH   int `json:"output_h"`

	AppliedSettings Resolved         `json:"applied_settings"`
	Orientation     *OrientationStat `json:"orientation,omitempty"`

	Frames int   `json:"frames"`
	FPS    uint8 `json:"fps"` // the MikV header's 8-bit frame rate

	// What the container actually holds (header byte 13/14 values):
	// mikvformat.EncodingDeflateRGB565 / EncodingJPEG and
	// EncodingRawPCM / EncodingOGGOpus, plus the header audio fields.
	VideoEncoding      uint8  `json:"video_encoding"`
	AudioEncoding      uint8  `json:"audio_encoding"`
	AudioSampleRate    uint16 `json:"audio_sample_rate"`
	AudioChannels      uint8  `json:"audio_channels"`
	AudioBitsPerSample uint8  `json:"audio_bits_per_sample"`

	ElapsedSeconds float64  `json:"elapsed_seconds"`
	Warnings       []string `json:"warnings,omitempty"`
}

// MikVTask is the live handle of a MikV render.
type MikVTask struct {
	*taskBase

	mu      sync.Mutex
	result  *MikVResult
	scratch string
	keep    bool
}

// Wait blocks until the Task is terminal and returns the finished MikVResult.
func (t *MikVTask) Wait() (*MikVResult, error) {
	if err := t.waitTerminal(); err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.result, nil
}

// RenderMikV starts the MikV production render for p: the planned frames
// written straight into a MikV container in req.OutputDir, with the audio
// track encoded per req.Options (raw PCM default, OGG-Opus on request).
// The file is <basename>-<WxH>.mikv unless req.OutputName overrides it.
// fps must be integral (the MikV header stores it in one byte, and the
// PCM/Opus frame budgets are derived from it).
//
// It shares the Analysis render slot with RenderPreview and RenderFinal:
// whichever starts later supersedes the others.
func (a *Analysis) RenderMikV(ctx context.Context, p *Plan, req MikVRequest) (*MikVTask, error) {
	errs := req.Validate()

	fpsF := planFPS(p)
	fps := int(fpsF)
	if fpsF != float64(fps) || fps < 1 || fps > 255 {
		errs = append(errs, ValidationError{Field: "fps",
			Reason: fmt.Sprintf("MikV writes an integral frame rate; the Plan replays at %g", fpsF)})
	} else {
		errs = append(errs, req.Options.validate(fps)...)
	}

	if r := p.resolved; r.OutputW > 65535 || r.OutputH > 65535 {
		errs = append(errs, ValidationError{Field: "output",
			Reason: fmt.Sprintf("%dx%d exceeds the MikV header's 16-bit frame dimensions", r.OutputW, r.OutputH)})
	}

	if len(errs) > 0 {
		return nil, &SettingsError{Errors: errs}
	}
	if err := a.planFits(p); err != nil {
		return nil, err
	}
	outDir, err := convertToAbsolutePath(req.OutputDir)
	if err != nil {
		return nil, fmt.Errorf("holocropper: mikv output dir: %w", err)
	}
	if st, err := os.Stat(outDir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("holocropper: mikv output dir %s is not a directory", outDir)
	}

	task := &MikVTask{
		taskBase: newTaskBase(ctx, TaskRenderMikV, req.Observer),
		keep:     req.KeepWorkDir,
	}
	task.scratch = renderScratchDir(req.TempDir, task.ID())
	a.renderSlot.install(task.taskBase)
	go runRenderMikV(a, task, p, req, outDir, fps)
	return task, nil
}

func runRenderMikV(a *Analysis, task *MikVTask, p *Plan, req MikVRequest, outDir string, fps int) {
	tb := task.taskBase
	defer func() {
		if r := recover(); r != nil {
			tb.finish(fmt.Errorf("holocropper: mikv pass panicked: %v", r))
		}
	}()
	tb.begin()

	res, err := renderMikVWork(task.ctx, a, task, p, req, outDir, fps)
	if err != nil {
		task.cleanupScratch()
		tb.finish(err)
		return
	}
	if task.keep {
		res.WorkDir = task.scratch
	} else {
		task.cleanupScratch()
		res.WorkDir = ""
	}
	task.mu.Lock()
	task.result = res
	task.mu.Unlock()
	tb.finish(nil)
}

func (t *MikVTask) cleanupScratch() {
	if t.scratch != "" {
		os.RemoveAll(t.scratch)
	}
}

func renderMikVWork(ctx context.Context, a *Analysis, task *MikVTask, p *Plan, req MikVRequest, outDir string, fps int) (*MikVResult, error) {
	tb := task.taskBase
	r := p.resolved
	start := time.Now()

	if err := os.MkdirAll(task.scratch, 0o755); err != nil {
		return nil, err
	}

	sampleRate, channels, bits := req.Options.resolved()
	mikv := mikvformat.NewMikVFormat()
	mikv.FPS = uint8(fps)
	mikv.Width, mikv.Height = uint16(r.OutputW), uint16(r.OutputH)
	mikv.SetAudioParams(uint16(sampleRate), uint8(channels), uint8(bits))
	if req.Options.VideoJPEG {
		mikv.VideoEncoding = mikvformat.EncodingJPEG
		mikv.JPEGQuality = req.Options.JPEGQuality
	}
	if req.Options.AudioOpus {
		mikv.AudioEncoding = mikvformat.EncodingOGGOpus
		mikv.OpusBitrateKbps = req.Options.OpusBitrateKbps
	}

	// The frame schedule decides how many frames this container holds - and
	// therefore how many audio chunks the track must be cut into. Both the
	// frame loop and the audio track read the same schedule, so AddFrame never
	// runs out of chunks.
	sched := a.frameSchedule(p)
	warnAudioCutToPicture(tb, a, sched, fps)

	// 1. The audio track first: the frame loop feeds it straight into
	// AddFrame, and an encode failure should fail the pass before any
	// crop work is spent.
	chunks, err := mikVAudioTrack(ctx, tb, req.FFmpegPath, a.meta.Input, mikv, sched.Frames, task.scratch)
	if err != nil {
		return nil, err
	}

	// 2. The planned frames, straight into the container. The production law
	// (the judged "H" chain) runs on the full-res source through the same
	// pipeline as RenderFinal, Sink B: the encoder emits rawvideo rgb24
	// frames at exactly the output size and each one goes into the payload
	// code unchanged (rgb565LE / jpeg.Encode at the setting quality).
	jpegQuality := req.Options.JPEGQuality
	if jpegQuality == 0 {
		jpegQuality = mikvformat.DefaultJPEGQuality
	}
	rectFor := func(srcIdx int) image.Rectangle {
		return scaleRectToSource(p.cropRectAt(srcIdx), r.AnalysisW, r.AnalysisH, r.SourceW, r.SourceH)
	}
	mode := finalModeCrop
	fitW, fitH, padX, padY := r.OutputW, r.OutputH, 0, 0
	if p.settings.FitWithBars {
		// The fit content rect resolves once per stream (plan §6.10), so the
		// fit box is constant and the pad law can be baked into the chain.
		mode = finalModeFit
		fitW, fitH, padX, padY = fitBoxToOutput(rectFor(0).Dx(), rectFor(0).Dy(), r.OutputW, r.OutputH)
	}
	err = runSourceRender(ctx, sourceRenderOpts{
		tb: tb, ffmpegPath: req.FFmpegPath, sourceFile: a.meta.Input,
		srcW: r.SourceW, srcH: r.SourceH, srcFrames: len(p.records),
		sched: sched, rectFor: rectFor, mode: mode, sink: sinkMJPEG,
		fitW: fitW, fitH: fitH, padX: padX, padY: padY,
		outW: r.OutputW, outH: r.OutputH, fps: planFPS(p),
		phase: PhaseMikVFrames, totalOut: sched.Frames,
		onRawFrame: func(outIdx int, rgb []byte) error {
			out := rgbaFromRGB24(rgb, r.OutputW, r.OutputH)
			var payload []byte
			if req.Options.VideoJPEG {
				var buf jpegBuffer
				if err := jpeg.Encode(&buf, out, &jpeg.Options{Quality: jpegQuality}); err != nil {
					return fmt.Errorf("holocropper: mikv jpeg frame %d: %w", outIdx+1, err)
				}
				payload = buf.data
			} else {
				payload = rgb565LE(out)
			}
			return mikv.AddFrame(payload, chunks[outIdx])
		},
	})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	frames := sched.Frames

	// 3. Write the container to scratch and publish it in one move.
	tb.setPhase(PhaseMikVWrite, 1)
	basename := getBasenameWithoutExt(a.meta.Input)
	outPath := filepath.Join(outDir, mikvOutputName(basename, r.OutputW, r.OutputH, req.OutputName))
	tmp := filepath.Join(task.scratch, "final.mikv")
	if err := mikv.Save(tmp); err != nil {
		return nil, fmt.Errorf("holocropper: mikv write %s: %w", outPath, err)
	}
	if err := moveFile(tmp, outPath); err != nil {
		return nil, fmt.Errorf("holocropper: mikv publish %s: %w", outPath, err)
	}
	tb.setDone(1)

	res := &MikVResult{
		InputPath:          a.meta.Input,
		OutputPath:         outPath,
		WorkDir:            task.scratch,
		SourceW:            r.SourceW,
		SourceH:            r.SourceH,
		AnalysisW:          r.AnalysisW,
		AnalysisH:          r.AnalysisH,
		OutputW:            r.OutputW,
		OutputH:            r.OutputH,
		AppliedSettings:    r,
		Frames:             frames,
		FPS:                uint8(fps),
		VideoEncoding:      mikv.VideoEncoding,
		AudioEncoding:      mikv.AudioEncoding,
		AudioSampleRate:    mikv.AudioSampleRate,
		AudioChannels:      mikv.AudioChannels,
		AudioBitsPerSample: mikv.AudioBitsPerSample,
		ElapsedSeconds:     time.Since(start).Seconds(),
	}
	if stat := p.Orientation(); stat.FramesExamined > 0 || stat.Flip {
		res.Orientation = &stat
	}
	tb.mu.Lock()
	res.Warnings = append(res.Warnings, tb.warnings...)
	tb.mu.Unlock()
	return res, nil
}

// mikvOutputName picks the MikV file name: dimension-tagged so renders
// at different targets never collide; req.OutputName (validated bare
// .mikv) overrides.
func mikvOutputName(basename string, w, h int, override string) string {
	if override != "" {
		return override
	}
	return fmt.Sprintf("%s-%dx%d.mikv", basename, w, h)
}

// jpegBuffer is the smallest io.Writer jpeg.Encode needs; keeping the
// encoded bytes here avoids pulling bytes.Buffer into this file's use.
type jpegBuffer struct{ data []byte }

func (b *jpegBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	return len(p), nil
}

// rgb565LE packs an RGBA image as the RGB565 little-endian buffer
// AddFrame expects for the DEFLATE path - the same bit trimming
// LoadFromMP4's At()-based packing produces.
func rgb565LE(img *image.RGBA) []byte {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	out := make([]byte, w*h*2)
	pix, i := img.Pix, 0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			o := (y*w + x) * 4
			v := uint16(pix[o]>>3)<<11 | uint16(pix[o+1]>>2)<<5 | uint16(pix[o+2]>>3)
			out[i] = byte(v)
			out[i+1] = byte(v >> 8)
			i += 2
		}
	}
	return out
}

// warnAudioCutToPicture states the container's own budget out loud: a MikV's
// audio is as long as its frames, so a source whose track runs past the last
// frame window loses that tail. The analysis cache carries the source's frame
// clock, which is where the comparison comes from.
func warnAudioCutToPicture(tb *taskBase, a *Analysis, sched frameSchedule, fps int) {
	if a.meta.FPS <= 0 || a.meta.Frames <= 0 || fps <= 0 {
		return
	}
	source := float64(a.meta.Frames) / a.meta.FPS
	picture := float64(sched.Frames) / float64(fps)
	if source <= picture+0.05 {
		return
	}
	tb.warn(fmt.Sprintf("the source runs %.2fs and the picture holds %.2fs at %d fps: the MikV's audio is cut to its frames",
		source, picture, fps))
}

// mikVAudioTrack produces exactly len(frames) audio chunks for mikv's
// declared encoding:
//
//   - EncodingRawPCM: the whole track transcoded to interleaved PCM by
//     ffmpeg, cut into fixed sampleRate/fps-sample chunks (the legacy
//     budget); a short tail zero-pads exactly like the original converter.
//   - EncodingOGGOpus: the whole track encoded as ONE ordinary OGG-Opus
//     stream at the packet duration the fps selects, then cut at page
//     boundaries by mikvformat.SplitOGGOpus - which drops whatever completes
//     past the last frame window, the same budget the PCM chunks cut to.
//
// A source without an audio track renders the PCM path as silence (with a
// warning, like the converter's); OGG-Opus has nothing to encode and fails
// loudly.
func mikVAudioTrack(ctx context.Context, tb *taskBase, ffmpegPath, input string, mikv *mikvformat.MikVFormat, frames int, scratch string) ([][]byte, error) {
	hasAudio, err := sourceHasAudio(ctx, ffmpegPath, input)
	if err != nil {
		return nil, err
	}
	if !hasAudio && mikv.AudioEncoding == mikvformat.EncodingOGGOpus {
		return nil, fmt.Errorf("holocropper: %s has no audio track to encode as OGG-Opus", input)
	}

	tb.setPhase(PhaseMikVAudio, 0)

	if mikv.AudioEncoding == mikvformat.EncodingOGGOpus {
		frameDurMs, err := mikvformat.OGGOpusFrameDurationMs(mikv.FPS)
		if err != nil {
			return nil, fmt.Errorf("holocropper: ogg-opus setup: %w", err)
		}
		oggPath := scratchName(scratch, "audio.ogg", tb.id)
		args := []string{
			"-hide_banner",
			"-i", input,
			"-vn",
			"-ar", strconv.Itoa(mikvformat.OGGOpusSampleRate),
			"-ac", strconv.Itoa(int(mikv.AudioChannels)),
			"-c:a", "libopus",
			"-frame_duration", strconv.FormatFloat(frameDurMs, 'f', -1, 64),
		}
		if mikv.OpusBitrateKbps > 0 {
			args = append(args, "-b:a", fmt.Sprintf("%dk", mikv.OpusBitrateKbps))
		}
		args = append(args, oggPath, "-y")
		if err := runFFmpegProgress(ctx, ffmpegPath, args, PhaseMikVAudio, tb); err != nil {
			return nil, err
		}
		oggBytes, err := os.ReadFile(oggPath)
		if err != nil {
			return nil, fmt.Errorf("holocropper: ogg-opus track: %w", err)
		}
		chunks, err := mikvformat.SplitOGGOpus(oggBytes, mikv.FPS, frames)
		if err != nil {
			return nil, fmt.Errorf("holocropper: ogg-opus segmenting: %w", err)
		}
		return chunks, nil
	}

	// Raw PCM: the legacy ffmpeg pipe, landed in scratch instead.
	chunkBytes := (int(mikv.AudioSampleRate) / int(mikv.FPS)) * int(mikv.AudioChannels) * int(mikv.AudioBitsPerSample) / 8
	pcmFormat := "s16le"
	if mikv.AudioBitsPerSample == 8 {
		pcmFormat = "u8"
	}
	chunks := make([][]byte, frames)
	for i := range chunks {
		chunks[i] = make([]byte, chunkBytes)
	}
	if !hasAudio {
		tb.warn("source has no audio track - writing silence")
		return chunks, nil
	}

	pcmPath := scratchName(scratch, "audio.raw", tb.id)
	args := []string{
		"-hide_banner",
		"-i", input,
		"-vn",
		"-ar", strconv.Itoa(int(mikv.AudioSampleRate)),
		"-ac", strconv.Itoa(int(mikv.AudioChannels)),
		"-f", pcmFormat,
		pcmPath,
		"-y",
	}
	if err := runFFmpegProgress(ctx, ffmpegPath, args, PhaseMikVAudio, tb); err != nil {
		return nil, err
	}
	audioBytes, err := os.ReadFile(pcmPath)
	if err != nil {
		return nil, fmt.Errorf("holocropper: pcm track: %w", err)
	}
	for i := range chunks {
		start := i * chunkBytes
		if start >= len(audioBytes) {
			break // silence tail, as the original converter leaves it
		}
		end := start + chunkBytes
		if end > len(audioBytes) {
			end = len(audioBytes)
		}
		copy(chunks[i], audioBytes[start:end])
	}
	return chunks, nil
}

// sourceHasAudio reports whether input carries at least one audio stream,
// using ffmpeg's own stream mapping as the authority.
func sourceHasAudio(ctx context.Context, ffmpegPath, input string) (bool, error) {
	cmd := exec.CommandContext(ctx, resolveFFmpegPath(ffmpegPath),
		"-hide_banner",
		"-loglevel", "error",
		"-i", input,
		"-map", "0:a:0",
		"-f", "null", "-",
	)
	if err := cmd.Run(); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return false, cerr
		}
		return false, nil // ffmpeg's "matches no streams" verdict
	}
	return true, nil
}
