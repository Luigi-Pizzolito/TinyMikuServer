package holocropper

import (
	"image"
	"path/filepath"
	"time"

	vidio "github.com/AlexEidt/Vidio"
)

/* pipeline structs */
type ImageGrayMask struct {
	Mask  *image.Gray
	Image *image.RGBA
}

type ImageGrayBoundMask struct {
	Mask  *image.Gray
	Image *image.RGBA
	Bbox  image.Rectangle
	Subj  int
}

type ImageCropMask struct {
	CBox  image.Rectangle
	Image *image.RGBA
	zoom  bool
}

/* pipeline functions */
func (hc *HoloCropper) establishPipes() {
	// Create channels for the pipeline
	hc.procFrameQueue1 = make(chan *image.RGBA)
	hc.procFrameQueue2 = make(chan *ImageGrayMask)
	hc.procFrameQueue3 = make(chan *ImageGrayBoundMask)
	hc.procFrameQueue4 = make(chan *ImageCropMask)
	hc.procFrameQueue5 = make(chan *image.RGBA)
}

func (hc *HoloCropper) startPipeline() error {
	var err error
	hc.video, err = vidio.NewVideo(filepath.Join(hc.tempDirTinyVid, getBasenameWithoutExt(hc.inputVideo)+".mp4"))
	if err != nil {
		return err
	}

	hc.totalFrames = hc.video.Frames()
	hc.currentFrame = 0

	// Start pipeline goroutines
	go hc.readVideo()
	go hc.grayscaleVideo()
	go hc.binaryVideo()
	go hc.calcVideo()
	go hc.cropVideo()
	go hc.saveFinalFrames()

	return nil
}

func (hc *HoloCropper) checkPipelineDone() bool {
	if hc.currentFrame == hc.totalFrames {
		// Close pipeline channels
		close(hc.procFrameQueue1)
		close(hc.procFrameQueue2)
		close(hc.procFrameQueue3)
		close(hc.procFrameQueue4)
		close(hc.procFrameQueue5)

		return true
	}
	return false
}

func (hc *HoloCropper) tickPipeline() {
	ticker := time.NewTicker(time.Millisecond * 16)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			hc.bart.Set(hc.currentFrame)
			if hc.checkPipelineDone() {
				return
			}
		}
	}
}
