package holocropper

import (
	"bufio"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	vidio "github.com/AlexEidt/Vidio"
	"github.com/schollz/progressbar/v3"
)

func (hc *HoloCropper) initialResize() error {
	// Read the actual source dimensions: the analysis frame preserves the
	// source aspect ratio instead of assuming 16:9 (HIGH-ASPECT-RATIO-FIXES.md F1).
	srcW, srcH, err := hc.sourceDimensions()
	if err != nil {
		return err
	}
	hc.srcW, hc.srcH = srcW, srcH

	// Compute the analysis (tiny) frame size
	cropWidth, cropHeight := hc.calculateAnalysisSize()

	// Call resize function
	if err := hc.resizeTiny(filepath.Join(hc.tempDirTinyVid, getBasenameWithoutExt(hc.inputVideo)+".mp4"), cropWidth, cropHeight); err != nil {
		return err
	}
	return nil
}

// sourceDimensions opens the input video just long enough to read its size.
func (hc *HoloCropper) sourceDimensions() (int, int, error) {
	video, err := vidio.NewVideo(hc.inputVideo)
	if err != nil {
		return 0, 0, err
	}
	defer video.Close()
	return video.Width(), video.Height(), nil
}

// calculateAnalysisSize returns the analysis frame size. The analysis height is
// hc.AnalysisHeight (default 480) but never below the requested output height,
// so a crop can always be scaled DOWN to the output (never upscaled). The
// analysis width follows the true source aspect ratio. Both values are even.
func (hc *HoloCropper) calculateAnalysisSize() (width, height int) {
	height = hc.AnalysisHeight
	if height <= 0 {
		height = defaultAnalysisHeight
	}
	if height < hc.Height {
		height = hc.Height
	}

	width = height * hc.srcW / hc.srcH
	if width < 16 {
		width = 16
	}

	if width%2 != 0 {
		width++
	}
	if height%2 != 0 {
		height++
	}

	return width, height
}

// Resize to base tiny size with FFMPEG
func (hc *HoloCropper) resizeTiny(outputFile string, width, height int) error {
	if hc.FPS == 0 {
		return fmt.Errorf("Error: Must specify FPS")
	}

	// No unsharp here: sharpening fragments the binary masks that drive
	// subject detection and adds framing jitter (HIGH-ASPECT-RATIO-FIXES.md F6).
	cmd := exec.Command("ffmpeg",
		"-i", hc.inputVideo,
		"-vf", "scale="+strconv.Itoa(width)+":"+strconv.Itoa(height),
		"-sws_flags", "lanczos+accurate_rnd", // Use Lanczos resampling with accurate rounding
		"-r", strconv.FormatFloat(hc.FPS, 'f', -1, 64),
		outputFile,
		"-y", // Overwrite output file without asking
	)

	fmt.Println(cmd.String())

	cmdReader, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	// Start the command
	err = cmd.Start()
	if err != nil {
		return err
	}

	// Create a progress bar
	totalFrames, err := hc.countFrames(hc.inputVideo)
	if err != nil {
		return err
	}
	hc.totalFrames = totalFrames

	scanner := bufio.NewScanner(cmdReader)
	scanner.Split(bufio.ScanWords)
	getF := false
	barp := progressbar.Default(int64(totalFrames))
	for scanner.Scan() {
		line := scanner.Text()
		// fmt.Println(line)
		if getF {
			getF = false
			cf, err := strconv.Atoi(line)
			if err != nil {
				continue
			}
			barp.Set(cf)
		}
		if strings.Contains(line, "frame=") {
			getF = true
		}
	}

	// Wait for command to finish
	err = cmd.Wait()
	if err != nil {
		return err
	}

	barp.Close()

	return nil
}

func (hc *HoloCropper) countFrames(videoFile string) (int, error) {
	// Open the video file
	video, err := vidio.NewVideo(videoFile)
	if err != nil {
		return 0, err
	}
	defer video.Close()

	return video.Frames(), nil
}
