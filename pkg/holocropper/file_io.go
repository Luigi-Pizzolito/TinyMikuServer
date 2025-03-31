package holocropper

import (
	"bufio"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/schollz/progressbar/v3"
)

func convertToAbsolutePath(path string) (string, error) {
	// Check if the path is already absolute
	if filepath.IsAbs(path) {
		return path, nil
	}

	// Convert relative path to absolute path
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}

	return absPath, nil
}

func getBasenameWithoutExt(absPath string) string {
	// Get the base name of the file
	base := filepath.Base(absPath)

	// Remove the file extension
	ext := filepath.Ext(base)
	basename := base[:len(base)-len(ext)]

	return basename
}

func (hc *HoloCropper) cleanPathsAndTempDir() error {
	var err error
	hc.inputVideo, err = convertToAbsolutePath(hc.inputVideo)
	if err != nil {
		fmt.Println("Couldn't find absolute path of input video")
		return err
	}

	hc.outputDir, err = convertToAbsolutePath(hc.outputDir)
	if err != nil {
		fmt.Println("Couldn't find absolute path of output directory")
		return err
	}

	// Create a temporary directory for frames
	hc.tempDirFrames, err = os.MkdirTemp("", "tinyframes")
	if err != nil {
		fmt.Println(err.Error())
		return err
	}

	// Create a temporary directory for tiny video
	hc.tempDirTinyVid, err = os.MkdirTemp("", "tinyvideo")
	if err != nil {
		fmt.Println(err.Error())
		return err
	}

	return nil
}

func (hc *HoloCropper) rmTempDirs() error {
	// fmt.Println("Cleaned up.")
	return os.RemoveAll(hc.tempDirTinyVid)
}

// copyAndWrite copies the image.RGBA and passes it to a video writer.
func (hc *HoloCropper) copyAndWrite(img *image.RGBA) error {
	// Copy the image.RGBA to a new image.RGBA
	copiedImg := image.NewRGBA(image.Rect(0, 0, hc.Width, hc.Height)) //! output is not the crop bounds size, but the output requested size

	// Fill the background with black
	draw.Draw(copiedImg, copiedImg.Bounds(), &image.Uniform{color.RGBA{0, 0, 0, 255}}, image.ZP, draw.Src)
	draw.Draw(copiedImg, copiedImg.Bounds(), img, img.Bounds().Min, draw.Src)

	// Write the frame to the image
	abspath := filepath.Join(hc.tempDirFrames, fmt.Sprintf("%d.jpg", hc.currentFrame))

	f, err := os.Create(abspath)
	if err != nil {
		return err
	}
	jpeg.Encode(f, copiedImg, nil)
	f.Close()

	return nil
}

func (hc *HoloCropper) joinOutputImgs() error {
	abspath, err := filepath.Abs(hc.tempDirFrames)
	if err != nil {
		return err
	}
	err = hc.convertImagesToMP4(abspath, hc.inputVideo, filepath.Join(hc.outputDir, getBasenameWithoutExt(hc.inputVideo)+".mp4"), hc.FPS)
	if err != nil {
		return err
	}
	return nil
}

func (hc *HoloCropper) convertImagesToMP4(inputFolder, audioFile, outputFile string, fps float64) error {
	cmd := exec.Command("ffmpeg",
		"-r", strconv.FormatFloat(fps, 'f', -1, 64),
		"-framerate", strconv.FormatFloat(fps, 'f', -1, 64),
		"-i", inputFolder+"/%d.jpg",
		"-i", audioFile,
		"-map", "0:v:0",
		"-map", "1:a:0",
		// "-c:v", "copy",
		"-c:v", "mjpeg",
		"-q:v", "6",
		"-c:a", "copy", //! keep audio as is
		// "-c:a", "pcm_s16le",
		// "-ar", "44100",
		outputFile, "-y", // Offmpegverwrite output file without asking
	)

	fmt.Println(cmd.String())

	// Create a progress bar
	files, err := os.ReadDir(inputFolder)
	if err != nil {
		return err
	}
	totalFrames := len(files)
	barp := progressbar.Default(int64(totalFrames))
	cmdReader, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	// Start the command
	err = cmd.Start()
	if err != nil {
		return err
	}

	// Read progress in real time
	scanner := bufio.NewScanner(cmdReader)
	scanner.Split(bufio.ScanWords)
	getF := false
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

	// Wait for the command to finish
	err = cmd.Wait()
	if err != nil {
		return err
	}

	barp.Close()

	//! Must clear output folder image sequence after, or it might get appended to other videos being processed
	err = hc.deleteFilesInFolder(inputFolder)
	if err != nil {
		return err
	}

	return nil
}

func (hc *HoloCropper) deleteFilesInFolder(folderPath string) error {
	err := filepath.Walk(folderPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			err := os.Remove(path)
			if err != nil {
				return err
			}
			// fmt.Printf("Deleted file: %s\n", path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return nil
}
