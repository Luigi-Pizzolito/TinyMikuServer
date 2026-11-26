package holocropper

/* The shared render pipeline: production renders (RenderFinal, RenderMikV),
output-resolution PREVIEWS and single-frame STILLS all run through the ONE
encoder-process starter (runSourceRender) and the ONE filter-chain builder
(finalFilterChain) in this file. The pass

  1. decodes the source through ffmpeg to rgb24 frames,
  2. composes the Plan's crop rectangle at SOURCE pixels in Go (no resize -
     out-of-frame regions compose black, the cropToBBoxCalc law §12.8), and
  3. hands each crop as one self-describing PPM frame to a single ffmpeg
     encoder running the chosen resize law ("H", judged by eye against the
     linear-light variants):

       hqdn3d=2:1.5:0:0            denoise FIRST, on the full-res crop
       scale=iw*2/3:ih*2/3:lanczos  first downscale stage
       scale=<target>:lanczos       second stage to the framed size
       (+ pad to output for fit mode)
       format=<sink format>         yuvj420p (MJPEG) / rgb24 (raw) /
                                    yuv420p bt709 limited (VP9 preview)

Everything happens in gamma-encoded 8-bit RGB. No linear light, no CAS, no
unsharp, no extra sharpening of any kind; no colour-management parameter
filters, no special scalers, no sws-flag or colour-tag overrides anywhere
on the encoder feed.

Variable-size crops are what makes the PPM image pipe necessary: rawvideo
requires one constant frame size for the whole stream, while the Plan's crop
rect walks frame to frame. The encoder resamples every input frame to the
exact framed size regardless.

Overlay previews keep the analysis-cache pipeline (plannedFrames/runOverlayLoop)
unchanged: their frames are box-drawn Go images, not a chain pass. */

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"os/exec"
	"strconv"
	"strings"
)

// The resize law ("H"), stated exactly. finalDenoise is the spatial-only
// hqdn3d (temporal strengths 0) applied FIRST on the full-res cropped frame:
// the source videos carry hatching/compression texture that full-res Lanczos
// turns into visible static, and denoising before the resize removes it. The
// 2/3 first stage and the strengths below are final - do not tune them.
const (
	finalDenoise = "hqdn3d=2:1.5:0:0"
	mjpegQ       = 6 // -q:v for every MJPEG sink (unchanged value)
)

// The mode/sink vocabulary of finalFilterChain.
const (
	finalModeCrop = "crop"
	finalModeFit  = "fit"

	sinkMJPEG = "mjpeg" // -c:v mjpeg -q:v mjpegQ -pix_fmt yuvj420p (full range)
	sinkRGB   = "rgb"   // rawvideo rgb24 pipe (MikV frame feed)
	sinkVP9   = "vp9"   // -c:v libvpx-vp9 -pix_fmt yuv420p, BT.709 LIMITED range
)

// finalFilterChain is the ONE filter-chain builder for every ffmpeg render
// pass (final, MikV, preview, still). mode is crop or fit; sink picks the
// final format stage. Fit mode scales the content crop aspect-preserving to
// the fit box (the 825 amendment's own rounding) and pads to the output on
// black AFTER the scale and BEFORE the format stage; the pad is omitted when
// the fit box already is the output.
func finalFilterChain(mode, sink string, outW, outH, fitW, fitH, padX, padY int) string {
	secondScale := fmt.Sprintf("scale=%d:%d:flags=lanczos", outW, outH)
	pad := ""
	if mode == finalModeFit {
		secondScale = fmt.Sprintf("scale=%d:%d:flags=lanczos", fitW, fitH)
		if fitW != outW || fitH != outH {
			pad = fmt.Sprintf(",pad=%d:%d:%d:%d:black", outW, outH, padX, padY)
		}
	}
	format := "format=yuvj420p"
	switch sink {
	case sinkRGB:
		format = "format=rgb24"
	case sinkVP9:
		// The browser-safe preview sink: the RGB->YUV conversion happens
		// here, on the FINAL size, with the BT.709 matrix and LIMITED range
		// (full-range VP9 is what hardware decoders refuse - PreviewLaw 2).
		// The MJPEG final converts full range instead, which is what JFIF
		// decoders assume for yuvj420p.
		secondScale += ":out_color_matrix=bt709:out_range=tv"
		format = "format=yuv420p"
	}
	return finalDenoise + ",scale=iw*2/3:ih*2/3:flags=lanczos," + secondScale + pad + "," + format
}

// fitBoxToOutput is the fit-with-bars box: the content crop scaled
// aspect-preserving into the output with the same rounding the Go fit law
// uses (math.Round, clamped to at least 1). Callers bake it into
// finalFilterChain's fitW/fitH/padX/padY.
func fitBoxToOutput(contentW, contentH, outW, outH int) (fitW, fitH, padX, padY int) {
	scale := math.Min(float64(outW)/float64(contentW), float64(outH)/float64(contentH))
	fitW = int(math.Round(float64(contentW) * scale))
	fitH = int(math.Round(float64(contentH) * scale))
	if fitW < 1 {
		fitW = 1
	}
	if fitH < 1 {
		fitH = 1
	}
	return fitW, fitH, (outW - fitW) / 2, (outH - fitH) / 2
}

// scaleRectToSource maps a Plan crop rect (analysis pixels; may spill past the
// left/right frame edges by the landscape stage padding §12.8) into source
// pixels. Each edge scales by the axis ratio with half-up rounding; vertical
// spill cannot exist (cropRectFromFramingCalc keeps verticals inside the
// frame). Sub-pixel edge differences against the analysis rect are inherent
// to any mapping and irrelevant downstream - the encoder resamples the crop
// to the exact framed output size either way.
func scaleRectToSource(r image.Rectangle, aw, ah, srcW, srcH int) image.Rectangle {
	sx := float64(srcW) / float64(aw)
	sy := float64(srcH) / float64(ah)
	x0 := int(math.Round(float64(r.Min.X) * sx))
	y0 := int(math.Round(float64(r.Min.Y) * sy))
	x1 := int(math.Round(float64(r.Max.X) * sx))
	y1 := int(math.Round(float64(r.Max.Y) * sy))
	if x1 <= x0 {
		x1 = x0 + 1 // degenerate-rect guard, mirroring cropToBBoxCalc
	}
	if y1 <= y0 {
		y1 = y0 + 1
	}
	return image.Rect(x0, y0, x1, y1)
}

// writePPMCrop composes the crop of one rgb24 frame exactly as cropToBBoxCalc
// composes it on an RGBA image - the destination is the crop's full extent,
// regions outside the frame stay black - and writes it as one binary P6 PPM
// frame. PPM is what lets a continuous image2pipe carry per-frame sizes.
func writePPMCrop(w io.Writer, frame []byte, srcW, srcH int, crop image.Rectangle) error {
	cw, ch := crop.Dx(), crop.Dy()
	if cw < 1 || ch < 1 {
		return fmt.Errorf("holocropper: degenerate crop rect %v", crop)
	}
	if _, err := fmt.Fprintf(w, "P6\n%d %d\n255\n", cw, ch); err != nil {
		return err
	}
	xa, xb := maxInt(crop.Min.X, 0), minInt(crop.Max.X, srcW)
	row := make([]byte, cw*3)
	for y := 0; y < ch; y++ {
		sy := crop.Min.Y + y
		for x := range row {
			row[x] = 0
		}
		if sy >= 0 && sy < srcH && xb > xa {
			srcOff := (sy*srcW + xa) * 3
			dstOff := (xa - crop.Min.X) * 3
			copy(row[dstOff:dstOff+(xb-xa)*3], frame[srcOff:srcOff+(xb-xa)*3])
		}
		if _, err := w.Write(row); err != nil {
			return err
		}
	}
	return nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// rgbaFromRGB24 wraps one Sink B output frame (exactly w*3*h bytes of rgb24
// from the frame pipe) as the *image.RGBA the MikV payload code has always
// been fed. The payload conversion itself (rgb565LE / jpeg.Encode) is
// unchanged.
func rgbaFromRGB24(rgb []byte, w, h int) *image.RGBA {
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < w*h; i++ {
		out.Pix[i*4+0] = rgb[i*3+0]
		out.Pix[i*4+1] = rgb[i*3+1]
		out.Pix[i*4+2] = rgb[i*3+2]
		out.Pix[i*4+3] = 255
	}
	return out
}

// sourceRenderOpts drives one decode -> crop -> encode pass. Exactly one sink
// is active:
//
//   - tmpOut != "": Sink A, the mjpeg production file (RenderFinal). The
//     source file is re-opened as ffmpeg input 1 so its audio rides along
//     with -c:a copy, exactly as the old muxFrameSequence mux did.
//   - tmpOutWebM != "": Sink C, the vp9-webm preview file. The audio source
//     (audioSource, usually the analyzed input) rides along as Opus, exactly
//     as the old VP9 preview mux did.
//   - tmpOutJPEG != "": Sink A', a single-frame JPEG file (Snapshot). Same
//     mjpeg encoder at mjpegQ as Sink A - the MJPEG sink's compressed
//     bitstream without the MP4 wrapper, so a still is the final frame's
//     codec path byte-for-byte. Expects a one-frame pass.
//   - onRawFrame != nil: Sink B, the MikV frame feed. The encoder writes
//     rawvideo rgb24 frames at exactly outW x outH to stdout; the callback
//     receives them in output order and turns them into payloads.
//
// mode and sink steer the ONE chain builder (finalFilterChain). rectFor(srcIdx)
// is the SOURCE-pixel crop for one source frame; srcFrames is the plan's record
// count, which the passthrough analysis guarantees equals the source frame
// count. startFrame/frameCount optionally narrow the pass to a window of
// OUTPUT slots (0/0 = the whole schedule): frames before the window are still
// decoded - the decode count is what proves the cache matches the plan - but
// only the window's crops are fed, and the decoder is stopped early when the
// window has been fed. seekSeconds > 0 replaces the decode-from-start pump
// with an ffmpeg INPUT seek (-ss before -i): the first decoded frame is the
// one the schedule plays at startFrame - the half-early seek law makes the
// landing frame-exact - and rectFor/slot matching is indexed from it.
type sourceRenderOpts struct {
	tb         *taskBase
	ffmpegPath string
	sourceFile string
	srcW, srcH int
	srcFrames  int

	sched   frameSchedule
	rectFor func(srcIdx int) image.Rectangle
	mode    string
	sink    string
	fitW    int
	fitH    int
	padX    int
	padY    int
	outW    int
	outH    int
	fps     float64

	startFrame  int
	frameCount  int // 0 = the whole schedule
	frameLimit  int // >0: encoder-side -frames:v cap (single-frame stills)
	seekSeconds float64 // >0: input-seek the decode (-ss before -i)

	phase    Phase
	totalOut int // scheduled output frames the pass must produce

	tmpOut      string
	tmpOutWebM  string
	tmpOutJPEG  string
	audioSource string
	copyAudio   bool
	onRawFrame  func(outIdx int, rgb []byte) error
}

// runSourceRender executes one full pipeline pass. Cancel/supersede semantics
// match runFFmpegProgress: an external context cancel returns the context
// error verbatim (Task maps it to cancelled); a failed subprocess returns a
// *FFmpegError carrying the stderr tail; a callback failure surfaces verbatim
// and kills both subprocesses. Progress comes from the encoder's own `frame=`
// stats, so one phase covers feed, resize and mux.
func runSourceRender(ctx context.Context, o sourceRenderOpts) error {
	if o.tmpOut == "" && o.tmpOutWebM == "" && o.tmpOutJPEG == "" && o.onRawFrame == nil {
		return errors.New("holocropper: runSourceRender needs a sink")
	}
	ffmpegPath := resolveFFmpegPath(o.ffmpegPath)
	chain := finalFilterChain(o.mode, o.sink, o.outW, o.outH, o.fitW, o.fitH, o.padX, o.padY)

	// decCtx outlives nothing useful once the pump has fed its last scheduled
	// frame: a render clock slower than the source's clock needs only the
	// first records' worth of decode, and cancelling decCtx stops ffmpeg
	// decoding the remainder while the encoder finishes what it was fed.
	decCtx, decCancel := context.WithCancel(ctx)
	defer decCancel()
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel() // internal aborts + a safety net on every return path

	decArgs := []string{"-hide_banner", "-loglevel", "error"}
	if o.seekSeconds > 0 {
		// Input seek (stills): -ss before -i seeks by keyframe and decodes
		// forward, landing on the target under the frame-clock law the stills
		// seek half-early rule guarantees.
		decArgs = append(decArgs, "-ss", strconv.FormatFloat(o.seekSeconds, 'f', 6, 64))
	}
	decArgs = append(decArgs,
		"-i", o.sourceFile, "-map", "0:v:0",
		"-fps_mode", "passthrough",
		"-f", "rawvideo", "-pix_fmt", "rgb24", "pipe:1")
	dec := exec.CommandContext(decCtx, ffmpegPath, decArgs...)
	decOut, err := dec.StdoutPipe()
	if err != nil {
		return fmt.Errorf("holocropper: decoder stdout pipe: %w", err)
	}
	decErr, err := dec.StderrPipe()
	if err != nil {
		return fmt.Errorf("holocropper: decoder stderr pipe: %w", err)
	}

	encArgs := []string{
		"-hide_banner",
		"-framerate", strconv.FormatFloat(o.fps, 'f', -1, 64),
		"-f", "image2pipe", "-vcodec", "ppm", "-i", "pipe:",
	}
	if o.tmpOut != "" {
		// Sink A re-reads the source file as input 1 for the audio copy.
		encArgs = append(encArgs, "-i", o.sourceFile)
	} else if o.audioSource != "" {
		// Sink C brings its audio source in as input 1, like the old mux.
		encArgs = append(encArgs, "-i", o.audioSource)
	}
	encArgs = append(encArgs, "-vf", chain)
	switch {
	case o.tmpOut != "":
		encArgs = append(encArgs, "-map", "0:v:0")
		if o.copyAudio {
			encArgs = append(encArgs, "-map", "1:a:0", "-c:a", "copy")
		}
		if o.frameLimit > 0 {
			encArgs = append(encArgs, "-frames:v", strconv.Itoa(o.frameLimit))
		}
		// yuvj420p pins today's product pixel format: the chain hands the
		// encoder full-range frames and swscale converts with the right
		// matrix - same container/codec/format pairing the UMP PC player
		// already accepts.
		encArgs = append(encArgs, "-c:v", "mjpeg", "-q:v", strconv.Itoa(mjpegQ), "-pix_fmt", "yuvj420p", o.tmpOut, "-y")
	case o.tmpOutJPEG != "":
		// Sink A': the same mjpeg encode as Sink A, written as a bare JPEG
		// file (the image2 muxer picks .jpg from the extension). This is the
		// Snapshot delivery: the compressed frame is the bitstream a final
		// frame carries, minus the MP4 wrapper.
		encArgs = append(encArgs, "-map", "0:v:0")
		if o.frameLimit > 0 {
			encArgs = append(encArgs, "-frames:v", strconv.Itoa(o.frameLimit))
		}
		encArgs = append(encArgs, "-c:v", "mjpeg", "-q:v", strconv.Itoa(mjpegQ), "-pix_fmt", "yuvj420p", o.tmpOutJPEG, "-y")
	case o.tmpOutWebM != "":
		// Sink C: the exact vp9-webm encode law the preview mux used, plus
		// the explicit BT.709 tags (the old mux inherited them implicitly;
		// stating them makes the browser-side decode law unambiguous). The
		// limited-range conversion now lives in the shared chain.
		encArgs = append(encArgs, "-map", "0:v:0",
			"-c:v", "libvpx-vp9", "-pix_fmt", "yuv420p",
			"-crf", strconv.Itoa(previewVP9CRF), "-b:v", "0",
			"-deadline", "good", "-cpu-used", strconv.Itoa(previewVP9CPUUsed),
			"-g", strconv.Itoa(previewGOPFrames(o.fps)),
			"-color_range", previewColorRange,
			"-colorspace", "bt709", "-color_primaries", "bt709", "-color_trc", "bt709")
		if o.audioSource != "" {
			encArgs = append(encArgs, "-map", "1:a:0", "-c:a", "libopus", "-b:a", "128k")
		}
		encArgs = append(encArgs, o.tmpOutWebM, "-y")
	default:
		encArgs = append(encArgs, "-f", "rawvideo", "-pix_fmt", "rgb24", "pipe:1")
	}
	enc := exec.CommandContext(runCtx, ffmpegPath, encArgs...)
	encIn, err := enc.StdinPipe()
	if err != nil {
		return fmt.Errorf("holocropper: encoder stdin pipe: %w", err)
	}
	encErr, err := enc.StderrPipe()
	if err != nil {
		return fmt.Errorf("holocropper: encoder stderr pipe: %w", err)
	}
	var encOut io.ReadCloser
	if o.onRawFrame != nil {
		encOut, err = enc.StdoutPipe()
		if err != nil {
			return fmt.Errorf("holocropper: encoder stdout pipe: %w", err)
		}
	}

	if err := dec.Start(); err != nil {
		return fmt.Errorf("holocropper: decoder start: %w", err)
	}
	if err := enc.Start(); err != nil {
		decCancel()
		dec.Wait()
		return fmt.Errorf("holocropper: encoder start: %w", err)
	}

	o.tb.setPhase(o.phase, o.totalOut)
	if o.sched.Resampled {
		// The same honest 739 wording plannedFrames speaks: the number users
		// recognize is THEIR video's frame rate.
		o.tb.warn(fmt.Sprintf("source runs at %g fps and the plan replays at %g: %d source frames become %d output frames",
			o.sched.AnalysisFPS, o.sched.RenderFPS, o.srcFrames, o.sched.Frames))
	}

	// Encoder stderr: progress tokens + diagnostics + the FFmpegError tail,
	// the exact word-scan runFFmpegProgress uses.
	encScanDone := make(chan struct{})
	var encTail []string
	go func() {
		defer close(encScanDone)
		push := func(tok string) {
			encTail = append(encTail, tok)
			if len(encTail) > ffmpegStderrTailTokens {
				encTail = encTail[1:]
			}
		}
		scanner := bufio.NewScanner(encErr)
		scanner.Split(bufio.ScanWords)
		getF := false
		for scanner.Scan() {
			tok := scanner.Text()
			push(tok)
			if getF {
				getF = false
				if cf, err := strconv.Atoi(tok); err == nil {
					o.tb.setDone(cf)
				}
				continue
			}
			if tok == "frame=" {
				getF = true
				continue
			}
			if isDiagnosticWord(tok) {
				o.tb.warn(tok)
			}
		}
	}()

	// Decoder stderr is quiet diagnostics - kept only for the failure report.
	decScanDone := make(chan struct{})
	var decTail []string
	go func() {
		defer close(decScanDone)
		scanner := bufio.NewScanner(decErr)
		scanner.Split(bufio.ScanWords)
		for scanner.Scan() {
			decTail = append(decTail, scanner.Text())
			if len(decTail) > ffmpegStderrTailTokens {
				decTail = decTail[1:]
			}
		}
	}()

	// Sink B reader: the encoder's stdout must drain while the pump feeds its
	// stdin, or the pipes deadlock at the first frames. Frames arrive in the
	// exact order the pump fed them.
	rawDone := make(chan struct{})
	rawFrames := 0
	var rawErr error
	if o.onRawFrame != nil {
		go func() {
			defer close(rawDone)
			out := make([]byte, o.outW*o.outH*3)
			for {
				if _, err := io.ReadFull(encOut, out); err != nil {
					if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) && rawErr == nil {
						rawErr = fmt.Errorf("holocropper: frame pipe: %w", err)
					}
					return
				}
				if rawErr == nil {
					if err := o.onRawFrame(rawFrames, out); err != nil {
						rawErr = err
						runCancel() // the pump must stop feeding a dead pass
					}
				}
				rawFrames++
			}
		}()
	}

	// The pump: decode one source frame at a time, feed the crops every
	// output slot replays that frame. Frames the schedule never plays are
	// still decoded - the count is what proves the cache matches the source.
	// usedFrames is the last source index the pass plays + 1: a slower render
	// clock (or a narrowed window) stops the decoder early instead of
	// draining the rest. Frames before the window start are decoded and
	// DISCARDED: correct and simple, no input seek on the range path.
	window := o.sched.Frames
	if o.frameCount > 0 && o.frameCount < window {
		window = o.frameCount
	}
	// base is the source index of the first DECODED frame: 0 when the decode
	// starts at the file's first frame; on the seek path it is the frame the
	// half-early seek law lands on, and every decoded frame is indexed from
	// it. usedFrames is how many frames the pump reads.
	base, last := 0, o.startFrame+window-1
	usedFrames := o.srcFrames
	if o.sched.Frames > 0 && last >= 0 && last < o.sched.Frames {
		if o.seekSeconds > 0 {
			base = o.sched.SrcFor[o.startFrame]
			usedFrames = o.sched.SrcFor[last] - base + 1
		} else if u := o.sched.SrcFor[last] + 1; u < usedFrames {
			usedFrames = u
		}
	}
	frame := make([]byte, o.srcW*o.srcH*3)
	decoded, out, fed := 0, 0, 0
	if o.seekSeconds > 0 {
		out = o.startFrame // the seek path never decodes frames before the window
	}
	var pumpErr error
	for decoded < usedFrames && pumpErr == nil {
		if _, err := io.ReadFull(decOut, frame); err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
				pumpErr = fmt.Errorf("holocropper: source video ran out at frame %d, plan has %d - cache is stale", decoded+1, o.srcFrames)
			} else {
				pumpErr = err
			}
			break
		}
		decoded++
		if err := ctx.Err(); err != nil {
			pumpErr = err
			break
		}
		crop := o.rectFor(base + decoded - 1)
		for out < o.sched.Frames && o.sched.SrcFor[out] == base+decoded-1 {
			if out >= o.startFrame && fed < window {
				if err := writePPMCrop(encIn, frame, o.srcW, o.srcH, crop); err != nil {
					pumpErr = err // a broken pipe here means the encoder already died
					break
				}
				fed++
			}
			out++
		}
	}

	// EOF on stdin lets a healthy encoder finish. The decoder: a slower
	// render clock (or a narrowed window) stops it deliberately once its
	// last played frame is fed (ffmpeg is then killed mid-decode - an
	// intentional kill whose exit code must not surface as a pass failure);
	// otherwise it is drained to its natural EOF so a clean exit 0 is
	// reported.
	encIn.Close()
	earlyDecKill := pumpErr == nil && rawErr == nil && decoded < o.srcFrames
	if earlyDecKill {
		decCancel()
	}
	go io.Copy(io.Discard, decOut) // reap the pipe either way

	if pumpErr != nil {
		runCancel() // a dead encoder or a cancelled pass: stop everything now
		decCancel()
	}
	if rawErr != nil {
		runCancel()
		decCancel()
	}

	if o.onRawFrame != nil {
		<-rawDone
	}
	<-encScanDone
	encWait := enc.Wait()
	<-decScanDone
	decWait := dec.Wait()

	// Error precedence mirrors runFFmpegProgress: the context error wins on a
	// cancel/supersede so Task.finish maps it to cancelled.
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	if pumpErr != nil {
		return pumpErr
	}
	if rawErr != nil {
		return rawErr
	}
	if encWait != nil {
		code := 1
		var ee *exec.ExitError
		if errors.As(encWait, &ee) {
			code = ee.ExitCode()
		}
		return &FFmpegError{Phase: o.phase, Command: enc.String(), ExitCode: code, StderrTail: strings.Join(encTail, " ")}
	}
	if decWait != nil && !earlyDecKill {
		code := 1
		var ee *exec.ExitError
		if errors.As(decWait, &ee) {
			code = ee.ExitCode()
		}
		return &FFmpegError{Phase: o.phase, Command: dec.String(), ExitCode: code, StderrTail: strings.Join(decTail, " ")}
	}
	if out != window {
		return fmt.Errorf("holocropper: the frame schedule wanted %d output frames, the pass wrote %d",
			window, out)
	}
	if o.onRawFrame != nil && rawFrames != window {
		return fmt.Errorf("holocropper: the frame pipe delivered %d output frames, the schedule wanted %d",
			rawFrames, window)
	}
	return nil
}
