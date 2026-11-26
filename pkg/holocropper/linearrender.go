package holocropper

/* Linear-light render passes (SHARPER_PLAN.md §4): the production renders
(RenderFinal, RenderMikV) no longer resize Go images cut from the 480p
analysis cache. The pass instead

  1. decodes the FULL-RES source through ffmpeg to rgb24 frames,
  2. composes the Plan's crop rectangle at SOURCE pixels in Go (no resize -
     out-of-frame regions compose black, the cropToBBoxCalc law §12.8), and
  3. hands each crop as one self-describing PPM frame to a single ffmpeg
     encoder running the linear-light resize law:

       setparams (stamp the PPM feed)
       -> zscale to BT.709 linear light (tin/pin bt709, npl 100, gbrpf32le)
       -> zscale lanczos to the framed output size (+ pad to output for fit)
       -> zscale gamma back to BT.709
       -> cas 0.4 LAST at final size.

The setparams stamp and the gamma-back's m=bt709 are the verified feed form
(SHARPER_PLAN §3): zimg refuses an UNTAGGED continuous RGB feed (code 3074,
no path between colorspaces) and has no trichromatic linear->BT.1886 path;
neither changes the resize law itself. Verified end-to-end on ffmpeg 9.0.2
and on the CI-pinned 6.1.1 build.

Variable-size crops are what makes the PPM image pipe necessary: rawvideo
requires one constant frame size for the whole stream, while the Plan's crop
rect walks frame to frame. The encoder resamples every input frame to the
exact framed size regardless.

Previews, overlay videos and stills keep the analysis-cache pipeline
(plannedFrames/runCropLoop); only the production passes use this file. */

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

// The linear-light law, as two ends around the resize. Stage 0 stamps the
// untagged PPM feed with its BT.709 identity (setparams changes tags only,
// never pixels), converts to linear light, and materializes gbrpf32le; the
// back end returns the resized linear frames to BT.709 transfer/matrix and
// sharpens with CAS at the final size. cas is LAST, after the pad, so bars
// are never sharpened.
const (
	linearStage0 = "setparams=color_primaries=bt709:color_trc=bt709:range=pc," +
		"zscale=tin=bt709:pin=bt709:t=linear:npl=100,format=gbrpf32le"
	linearBack = "zscale=tin=linear:pin=bt709:t=bt709:m=bt709,cas=0.4"
)

// linearFillChain is the content-aware law: the crop arrives aspect-locked by
// aspectLockedTargetCalc, so lanczos maps it straight onto the output
// (exactly what fillResizeTo does today, in linear light from full-res detail).
func linearFillChain(outW, outH int) string {
	return linearStage0 + fmt.Sprintf(",zscale=w=%d:h=%d:f=lanczos,", outW, outH) + linearBack
}

// linearFitChain is the fit-with-bars law (§6.10 + the 825 amendment): the
// content crop is scaled aspect-preserving to the fit box computed with
// fitResizeTo's own rounding, padded to the output on black in linear light,
// then gamma-returned and sharpened.
func linearFitChain(contentW, contentH, outW, outH int) string {
	scale := math.Min(float64(outW)/float64(contentW), float64(outH)/float64(contentH))
	sw := int(math.Round(float64(contentW) * scale))
	sh := int(math.Round(float64(contentH) * scale))
	if sw < 1 {
		sw = 1
	}
	if sh < 1 {
		sh = 1
	}
	return linearStage0 + fmt.Sprintf(",zscale=w=%d:h=%d:f=lanczos,pad=w=%d:h=%d:x=%d:y=%d:color=black,",
		sw, sh, outW, outH, (outW-sw)/2, (outH-sh)/2) + linearBack
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

// sourceRenderOpts drives one decode -> crop -> linear-light encode pass.
// Exactly one sink is active:
//
//   - tmpOut != "": Sink A, the mjpeg production file (RenderFinal). The
//     source file is re-opened as ffmpeg input 1 so its audio rides along
//     with -c:a copy, exactly as the old muxFrameSequence mux did.
//   - onRawFrame != nil: Sink B, the MikV frame feed. The encoder writes
//     rawvideo rgb24 frames at exactly outW x outH to stdout; the callback
//     receives them in output order and turns them into payloads.
//
// rectFor(srcIdx) is the SOURCE-pixel crop for one source frame; srcFrames
// is the plan's record count, which the passthrough analysis guarantees
// equals the source frame count.
type sourceRenderOpts struct {
	tb         *taskBase
	ffmpegPath string
	sourceFile string
	srcW, srcH int
	srcFrames  int

	sched   frameSchedule
	rectFor func(srcIdx int) image.Rectangle
	chain   string
	outW    int
	outH    int
	fps     float64

	phase    Phase
	totalOut int // scheduled output frames the pass must produce

	tmpOut     string
	copyAudio  bool
	onRawFrame func(outIdx int, rgb []byte) error
}

// runSourceRender executes one full pipeline pass. Cancel/supersede semantics
// match runFFmpegProgress: an external context cancel returns the context
// error verbatim (Task maps it to cancelled); a failed subprocess returns a
// *FFmpegError carrying the stderr tail; a callback failure surfaces verbatim
// and kills both subprocesses. Progress comes from the encoder's own `frame=`
// stats, so one phase covers feed, resize and mux.
func runSourceRender(ctx context.Context, o sourceRenderOpts) error {
	if o.tmpOut == "" && o.onRawFrame == nil {
		return errors.New("holocropper: runSourceRender needs a sink")
	}
	ffmpegPath := resolveFFmpegPath(o.ffmpegPath)

	// decCtx outlives nothing useful once the pump has fed its last scheduled
	// frame: a render clock slower than the source's clock needs only the
	// first records' worth of decode, and cancelling decCtx stops ffmpeg
	// decoding the remainder while the encoder finishes what it was fed.
	decCtx, decCancel := context.WithCancel(ctx)
	defer decCancel()
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel() // internal aborts + a safety net on every return path

	dec := exec.CommandContext(decCtx, ffmpegPath,
		"-hide_banner", "-loglevel", "error",
		"-i", o.sourceFile, "-map", "0:v:0",
		"-fps_mode", "passthrough",
		"-f", "rawvideo", "-pix_fmt", "rgb24", "pipe:1")
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
	}
	encArgs = append(encArgs, "-vf", o.chain)
	if o.tmpOut != "" {
		encArgs = append(encArgs, "-map", "0:v:0")
		if o.copyAudio {
			encArgs = append(encArgs, "-map", "1:a:0", "-c:a", "copy")
		}
		// yuvj420p pins today's product pixel format: the chain hands the
		// encoder bt709-tagged full-range frames and swscale converts with
		// the right matrix - same container/codec/format pairing the UMP PC
		// player already accepts.
		encArgs = append(encArgs, "-c:v", "mjpeg", "-q:v", "6", "-pix_fmt", "yuvj420p", o.tmpOut, "-y")
	} else {
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
	// usedFrames is the last source index the schedule plays + 1: a slower
	// render clock stops the decoder early instead of draining the rest.
	usedFrames := o.srcFrames
	if o.sched.Frames > 0 {
		usedFrames = o.sched.SrcFor[o.sched.Frames-1] + 1
	}
	frame := make([]byte, o.srcW*o.srcH*3)
	decoded, out := 0, 0
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
		crop := o.rectFor(decoded - 1)
		for out < o.sched.Frames && o.sched.SrcFor[out] == decoded-1 {
			if err := writePPMCrop(encIn, frame, o.srcW, o.srcH, crop); err != nil {
				pumpErr = err // a broken pipe here means the encoder already died
				break
			}
			out++
		}
	}

	// EOF on stdin lets a healthy encoder finish. The decoder: a slower
	// render clock stops it deliberately once its last played frame is fed
	// (ffmpeg is then killed mid-decode - an intentional kill whose exit
	// code must not surface as a pass failure); otherwise it is drained to
	// its natural EOF so a clean exit 0 is reported.
	encIn.Close()
	earlyDecKill := pumpErr == nil && rawErr == nil && usedFrames < o.srcFrames
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
	if out != o.sched.Frames {
		return fmt.Errorf("holocropper: the frame schedule wanted %d output frames, the pass wrote %d",
			o.sched.Frames, out)
	}
	if o.onRawFrame != nil && rawFrames != o.sched.Frames {
		return fmt.Errorf("holocropper: the frame pipe delivered %d output frames, the schedule wanted %d",
			rawFrames, o.sched.Frames)
	}
	return nil
}
