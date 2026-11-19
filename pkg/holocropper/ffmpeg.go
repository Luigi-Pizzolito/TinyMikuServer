package holocropper

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

/* Shared ffmpeg execution for the embeddable passes (story A1/A4/B1/B2).
Every pass runs under the Task context (exec.CommandContext kills the
subprocess on cancel/supersede), feeds ffmpeg's `frame= N` stats tokens into
the Task's phase progress, surfaces error and warning tokens through
Observer.OnWarning and Task.Warnings (results embed them via their
Warnings field), and wraps failures in a *FFmpegError carrying a stderr
tail. The fused CLI path (resizeTiny/convertImagesToMP4) is untouched until
story C3. */

// resolveFFmpegPath maps the configurable FFmpegPath setting ("" = PATH).
func resolveFFmpegPath(p string) string {
	if p == "" {
		return "ffmpeg"
	}
	return p
}

// ffmpegStderrTailTokens bounds how much stderr is kept for FFmpegError.
const ffmpegStderrTailTokens = 4096

// runFFmpegProgress runs an ffmpeg pass with progress reporting into tb's
// current phase. If ctx is cancelled the subprocess is killed and the
// context error is returned verbatim (so Task.finish maps it to cancelled).
func runFFmpegProgress(ctx context.Context, ffmpegPath string, args []string, phase Phase, tb *taskBase) error {
	cmd := exec.CommandContext(ctx, resolveFFmpegPath(ffmpegPath), args...)
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("ffmpeg %s: stderr pipe: %w", phase, err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg %s: start: %w", phase, err)
	}

	// ffmpeg interleaves progress and diagnostics on stderr separated by
	// spaces/carriage returns; word scanning is what the existing CLI parses.
	var tail []string
	push := func(tok string) {
		tail = append(tail, tok)
		if len(tail) > ffmpegStderrTailTokens {
			tail = tail[1:]
		}
	}

	scanner := bufio.NewScanner(stderrPipe)
	scanner.Split(bufio.ScanWords)
	getF := false
	for scanner.Scan() {
		tok := scanner.Text()
		push(tok)
		if getF {
			getF = false
			if cf, err := strconv.Atoi(tok); err == nil {
				tb.setDone(cf)
			}
			continue
		}
		if tok == "frame=" {
			getF = true
			continue
		}
		if isDiagnosticWord(tok) {
			tb.warn(tok)
		}
	}
	if err := cmd.Wait(); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		code := 1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		return &FFmpegError{Phase: phase, Command: cmd.String(), ExitCode: code, StderrTail: strings.Join(tail, " ")}
	}
	return nil
}

// isDiagnosticWord reports whether one stderr word is a pure ffmpeg
// error/warning marker. Word matching must be on the marker itself (after
// trimming the punctuation ffmpeg glues onto it) - a whole output-file
// path that merely *contains* "error" is not a diagnostic, and mux
// summary lines echo those paths every pass.
func isDiagnosticWord(tok string) bool {
	start, end := 0, len(tok)
	isLetter := func(b byte) bool {
		return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
	}
	for start < end && !isLetter(tok[start]) {
		start++
	}
	for end > start && !isLetter(tok[end-1]) {
		end--
	}
	switch tok[start:end] {
	case "Error", "error", "Errors", "errors", "Warning", "warning", "Warnings", "warnings", "failed", "Failed":
		return true
	}
	return false
}
