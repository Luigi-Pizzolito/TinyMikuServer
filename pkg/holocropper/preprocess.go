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
	// Get the resize dimensions
	cropWidth, _ := hc.calculateCropWidthHeight()

	// Call resize function
	if err := hc.resizeTiny(filepath.Join(hc.tempDirTinyVid, getBasenameWithoutExt(hc.inputVideo)+".mp4"), cropWidth, hc.Height); err != nil {
		return err
	}
	return nil
}

// CalculateWidthHeight calculates the width of a 16:9 video given the height in pixels,
// ensuring both width and height are divisible by 2.
func (hc *HoloCropper) calculateCropWidthHeight() (width, newHeight int) {
	// Calculate width based on the 16:9 aspect ratio formula: width = (height * 16) / 9
	width = (hc.Height * 16) / 9

	// Ensure width is divisible by 2
	if width%2 != 0 {
		width++
	}

	// Ensure height is divisible by 2
	if hc.Height%2 != 0 {
		hc.Height++
	}
	if hc.Width%2 != 0 {
		hc.Width++
	}

	return width, hc.Height
}

// Resize to base tiny size with FFMPEG
func (hc *HoloCropper) resizeTiny(outputFile string, width, height int) error {
	if hc.FPS == 0 {
		return fmt.Errorf("Error: Must specify FPS")
	}

	cmd := exec.Command("ffmpeg",
		"-i", hc.inputVideo,
		"-vf", "unsharp=5:5:1.0:5:5:0.0,scale="+strconv.Itoa(width)+":"+strconv.Itoa(height),
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
