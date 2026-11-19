package holocropper

import (
	"fmt"
	"image"
	"sync"
	"sync/atomic"
	"time"

	vidio "github.com/AlexEidt/Vidio"
	"github.com/schollz/progressbar/v3"
)

/**
 * Refactor of https://github.com/Luigi-Pizzolito/1-1-Content-Aware-Video-Crop into a Go package
 *
 */

// defaultAnalysisHeight is the height of the internal analysis frame when
// AnalysisHeight is not set. The analysis frame is never shorter than the
// requested output (crops are only ever scaled down from it).
const defaultAnalysisHeight = 480

// Framing parameter defaults shared by the CLI instance accessors, the
// embeddable Settings resolution and the Schema form spec.
const (
	defaultZoomMargin = 1.12 // subject bbox -> crop padding factor
	defaultMaxZoom    = 2.0  // zoom-in policy cap
)

/* HoloCropper class */
type HoloCropper struct {
	/* meta */
	Height int
	Width  int
	FPS    float64

	/* optional framing parameters; zero values select built-in defaults */
	AnalysisHeight int     // analysis frame height (default 480, never below Height)
	ZoomMargin     float64 // subject bbox -> crop padding factor (default 1.12)
	MaxZoom        float64 // zoom-in policy cap (default 2.0)

	/* private */

	/** pipeline **/
	procFrameQueue1 chan *image.RGBA
	procFrameQueue2 chan *ImageGrayMask
	procFrameQueue3 chan *ImageGrayBoundMask
	procFrameQueue4 chan *ImageCropMask
	procFrameQueue5 chan *image.RGBA
	pipeOnce        sync.Once

	zoom bool

	video        *vidio.Video
	totalFrames  int
	currentFrame atomic.Int64 // written by saveFinalFrames, read by tickPipeline (S4: -race clean)

	inputVideo     string
	outputDir      string
	tempDirTinyVid string
	tempDirFrames  string

	srcW, srcH int

	bart *progressbar.ProgressBar
}

func NewHoloCropper() *HoloCropper {
	return &HoloCropper{}
}

// zoomMargin returns the bbox -> crop padding factor (default 1.12).
func (hc *HoloCropper) zoomMargin() float64 {
	if hc.ZoomMargin > 0 {
		return hc.ZoomMargin
	}
	return defaultZoomMargin
}

// maxZoom returns the zoom-in policy cap (default 2.0).
func (hc *HoloCropper) maxZoom() float64 {
	if hc.MaxZoom > 0 {
		return hc.MaxZoom
	}
	return defaultMaxZoom
}

// framingParams snapshots the framing-relevant settings of this instance for
// the instance-free geometry functions and the replay engine.
func (hc *HoloCropper) framingParams() framingParams {
	return framingParams{
		Margin:  hc.zoomMargin(),
		MaxZoom: hc.maxZoom(),
		OutH:    hc.Height,
		SrcH:    hc.srcH,
	}
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

	// Keep output dimensions even, as before
	if hc.Height%2 != 0 {
		hc.Height++
	}
	if hc.Width%2 != 0 {
		hc.Width++
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
	if hc.totalFrames == 0 {
		return 0
	}
	return float64(float64(hc.currentFrame.Load()) / float64(hc.totalFrames))
}

func (hc *HoloCropper) Done() bool {
	// Wait (without spinning on the CPU) until the frame count is known
	for hc.totalFrames == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	return hc.currentFrame.Load() == int64(hc.totalFrames)
}

// closePipes closes all pipeline channels exactly once, so tickPipeline and
// Cancel can never double-close them.
func (hc *HoloCropper) closePipes() {
	hc.pipeOnce.Do(func() {
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
	})
}

func (hc *HoloCropper) Cancel() {
	hc.closePipes()
	hc.rmTempDirs()
}
