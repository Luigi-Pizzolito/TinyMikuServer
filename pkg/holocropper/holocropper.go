package holocropper

import (
	"fmt"
	"image"

	vidio "github.com/AlexEidt/Vidio"
	"github.com/schollz/progressbar/v3"
)

/**
 * Refactor of https://github.com/Luigi-Pizzolito/1-1-Content-Aware-Video-Crop into a Go package
 *
 */

/* HoloCropper class */
type HoloCropper struct {
	/* meta */
	Height int
	Width  int
	FPS    float64

	/* private */

	/** pipeline **/
	procFrameQueue1 chan *image.RGBA
	procFrameQueue2 chan *ImageGrayMask
	procFrameQueue3 chan *ImageGrayBoundMask
	procFrameQueue4 chan *ImageCropMask
	procFrameQueue5 chan *image.RGBA

	zoom bool

	video        *vidio.Video
	totalFrames  int
	currentFrame int

	inputVideo     string
	outputDir      string
	tempDirTinyVid string
	tempDirFrames  string

	bart *progressbar.ProgressBar
}

func NewHoloCropper() *HoloCropper {
	return &HoloCropper{}
}

func (hc *HoloCropper) Crop(height, width int, fps float64, input, output string) error {
	// Set the parameters
	hc.Height = height
	hc.Width = width
	hc.FPS = fps
	hc.inputVideo = input
	hc.outputDir = output
	hc.zoom = true

	// Check if width and height are set
	if hc.Width == 0 || hc.Height == 0 {
		return fmt.Errorf("Error: Must specify either width or height")
	}
	// Check if output FPS is set
	if hc.FPS == 0 {
		return fmt.Errorf("Error: Must specify FPS")
	}

	// Create temp paths and clean up
	if err := hc.cleanPathsAndTempDir(); err != nil {
		return err
	}
	defer hc.rmTempDirs()

	// Initial resize
	if err := hc.initialResize(); err != nil {
		return err
	}

	// Init progress bar
	fmt.Printf("Starting crop processing for %s\n", hc.inputVideo)
	hc.bart = progressbar.Default(int64(hc.totalFrames))

	// Start the pipeline
	hc.establishPipes()
	if err := hc.startPipeline(); err != nil {
		return err
	}

	// Wait for pipeline to complete
	hc.tickPipeline()

	hc.bart.Close()

	// Join final output images to output MP4
	if err := hc.joinOutputImgs(); err != nil {
		return err
	}

	return nil
}

func (hc *HoloCropper) Progress() float64 {
	return float64(float64(hc.currentFrame) / float64(hc.totalFrames))
}

func (hc *HoloCropper) Done() bool {
	for hc.totalFrames == 0 {
	}
	return hc.currentFrame == hc.totalFrames
}

func (hc *HoloCropper) Cancel() {
	if hc.procFrameQueue1 != nil {
		close(hc.procFrameQueue1)
	}
	if hc.procFrameQueue2 != nil {
		close(hc.procFrameQueue2)
	}
	if hc.procFrameQueue3 != nil {
		close(hc.procFrameQueue3)
	}
	if hc.procFrameQueue4 != nil {
		close(hc.procFrameQueue4)
	}
	if hc.procFrameQueue5 != nil {
		close(hc.procFrameQueue5)
	}
	hc.rmTempDirs()
}
