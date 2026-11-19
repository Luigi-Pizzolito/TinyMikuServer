package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/Luigi-Pizzolito/TinyMikuServer/pkg/holocropper"
	"github.com/schollz/progressbar/v3"
)

/* The holocropper CLI, story C3: this is now a thin front end over the
embeddable API (Analyze -> NewPlan -> Analysis.RenderFinal), the same
surface the Wails app consumes. The old fused one-shot Crop pipeline lived
inside the package; the framing decisions themselves are byte-identical
(see TestReplayParityVsDebugCSV), so the CLI keeps its flags, its messages,
its <basename>.mp4 output naming (RenderRequest.OutputName) and its
Ctrl-C-cancels-everything behavior while dropping the private pipeline. */

// cancelable is the Task surface catchExit needs to stop every running pass.
type cancelable interface{ Cancel() }

var (
	cancelMu    sync.Mutex
	runningJobs = map[string][]cancelable{} // per input file, for Ctrl-C
)

func registerCancel(file string, t cancelable) {
	cancelMu.Lock()
	runningJobs[file] = append(runningJobs[file], t)
	cancelMu.Unlock()
}

func main() {
	// gracefully catch exit
	catchExit()

	// Define CLI flags
	inputVideo := flag.String("i", ".", "Path to the input video file (default: current folder)")
	outputDir := flag.String("o", "", "Directory to save the cropped video")
	width := flag.Int("w", 240, "Width of the cropped video")
	height := flag.Int("h", 240, "Height of the cropped video")
	fps := flag.Float64("fps", 20, "Frames per second for the cropped video")
	analysisHeight := flag.Int("analysis-height", 0, "Height of the internal analysis frame (0 = default 480; never below -h)")
	zoomMargin := flag.Float64("zoom-margin", 0, "Subject bbox to crop padding factor (0 = default 1.12)")
	maxZoom := flag.Float64("max-zoom", 0, "Maximum zoom-in factor (0 = default 2.0)")
	autoOrient := flag.Bool("auto-orient", false, "Probe the input's subject aspect ratios and automatically pick portrait (HxW) or landscape (WxH) output orientation; skipped for 1:1 outputs")
	tempDir := flag.String("temp-dir", os.TempDir(), "Scratch + analysis-cache parent directory (cached analyses make repeated corpus renders skip the detect pass)")
	ffmpegPath := flag.String("ffmpeg-path", "", "Path to the ffmpeg binary (default: ffmpeg from PATH)")

	flag.Parse()

	// Validate output folder
	if *outputDir == "" {
		log.Fatal("Error: output directory not specified")
	}
	if _, err := os.Stat(*outputDir); os.IsNotExist(err) {
		log.Fatalf("Error: output directory does not exist: %v", err)
	}

	// Validate and gather input files
	var inputFiles []string
	fileInfo, err := os.Stat(*inputVideo)
	if err != nil {
		log.Fatalf("Error: unable to access input path: %v", err)
	}

	if fileInfo.IsDir() {
		// Iterate over all .mp4 files in the directory
		files, err := os.ReadDir(*inputVideo)
		if err != nil {
			log.Fatalf("Error: unable to read input directory: %v", err)
		}
		for _, file := range files {
			if !file.IsDir() && filepath.Ext(file.Name()) == ".mp4" {
				inputFiles = append(inputFiles, filepath.Join(*inputVideo, file.Name()))
			}
		}
		if len(inputFiles) == 0 {
			log.Fatal("Error: no .mp4 files found in the input directory")
		}
	} else {
		// Check if the input file is an .mp4 file
		if filepath.Ext(*inputVideo) != ".mp4" {
			log.Fatal("Error: input file is not an .mp4 file")
		}
		inputFiles = append(inputFiles, *inputVideo)
	}

	// Print all the parameters
	fmt.Printf("Input Video Path: %s\n", *inputVideo)
	fmt.Printf("Output Directory: %s\n", *outputDir)
	fmt.Printf("Crop Width: %d\n", *width)
	fmt.Printf("Crop Height: %d\n", *height)
	fmt.Printf("Frames Per Second: %.2f\n", *fps)
	fmt.Printf("Analysis Height: %d (0 = auto)\n", *analysisHeight)
	fmt.Printf("Zoom Margin: %.2f / Max Zoom: %.2f (0 = auto)\n", *zoomMargin, *maxZoom)
	fmt.Printf("Auto Orient: %t\n", *autoOrient)
	fmt.Printf("Temp Dir: %s\n", *tempDir)
	fmt.Printf("FFmpeg Path: %s\n", orDefault(*ffmpegPath, "ffmpeg (PATH)"))

	fmt.Println("Starting processing...")

	var wg sync.WaitGroup

	for _, inputFile := range inputFiles {
		wg.Add(1)
		go func(file string) {
			defer wg.Done()
			cropFile(file, *width, *height, *fps, *analysisHeight, *zoomMargin, *maxZoom, *autoOrient, *outputDir, *tempDir, *ffmpegPath)
			fmt.Printf("Processing complete for %s\n", file)
		}(inputFile)
	}

	// Wait for all goroutines to finish
	wg.Wait()

	fmt.Println("Processing complete!")
}

// cropFile runs the full API chain for one input video: Analyze (prepare +
// detect, cached), NewPlan (in-process framing replay), RenderFinal (crop
// sequence + audio mux at output resolution).
func cropFile(file string, width, height int, fps float64, analysisHeight int,
	zoomMargin, maxZoom float64, autoOrient bool, outputDir, tempDir, ffmpegPath string) {

	// Legacy input checks with the legacy messages, so scripted invocations
	// keep the same failure surface.
	if width == 0 || height == 0 {
		log.Printf("Error during cropping for file %s: %v", file, fmt.Errorf("Error: Must specify either width or height"))
		return
	}
	if fps == 0 {
		log.Printf("Error during cropping for file %s: %v", file, fmt.Errorf("Error: Must specify FPS"))
		return
	}

	// The analysis frame is never shorter than the output (flipped included),
	// matching the legacy rule; 0 keeps the API default (480).
	ah := analysisHeight
	if ah == 0 {
		if m := max(width, height); m > 480 {
			ah = m
		}
	}

	bart := progressbar.NewOptions(1)
	var barMu sync.Mutex
	observer := &holocropper.Observer{
		OnTask: func(ti holocropper.TaskInfo) {
			barMu.Lock()
			defer barMu.Unlock()
			if ti.Total > 0 {
				bart.ChangeMax(ti.Total)
			}
			bart.Set(ti.Done)
		},
		OnWarning: func(msg string) {
			fmt.Fprintf(os.Stderr, "ffmpeg: %s\n", msg)
		},
	}

	fmt.Printf("Starting crop processing for %s\n", file)

	analyzeTask, err := holocropper.Analyze(context.Background(), holocropper.AnalyzeRequest{
		Input:          file,
		TempDir:        tempDir,
		FFmpegPath:     ffmpegPath,
		AnalysisHeight: ah,
		Observer:       observer,
	})
	if err != nil {
		log.Printf("Error during cropping for file %s: %v", file, err)
		return
	}
	registerCancel(file, analyzeTask)
	analysis, err := analyzeTask.Wait()
	if err != nil {
		log.Printf("Error during cropping for file %s: %v", file, err)
		return
	}

	// Auto-orient: the orientation decision now rides the Plan (the cached
	// stream already holds every frame's subject aspect ratio - strictly
	// more evidence than the old probe sampled). 1:1 outputs bypass it.
	settings := holocropper.Settings{
		Width:      width,
		Height:     height,
		FPS:        fps,
		ZoomMargin: zoomMargin,
		MaxZoom:    maxZoom,
		AutoOrient: autoOrient,
	}
	if autoOrient && width == height {
		fmt.Printf("Auto-orient: 1:1 output for %s - orientation probe bypassed\n", file)
	}

	plan, err := holocropper.NewPlan(analysis.Stream(), settings)
	if err != nil {
		log.Printf("Error during cropping for file %s: %v", file, err)
		return
	}

	if autoOrient && width != height {
		st := plan.Orientation()
		r := plan.Resolved()
		decision := "keeping requested orientation"
		if st.Flip {
			decision = fmt.Sprintf("FLIPPING to %dx%d", r.OutputW, r.OutputH)
		}
		fmt.Printf("Auto-orient %s: subjects in %d/%d sampled frames, master bbox AR mean %.2f (geo %.2f, median %.2f); coverage %.3f at %dx%d vs %.3f at %dx%d -> %s\n",
			file, st.FramesWithSubjects, st.FramesExamined, st.MeanAR, st.GeoMeanAR, st.MedianAR,
			st.KeepScore, width, height, st.FlipScore, height, width, decision)
	}

	// Legacy output naming: <basename>.mp4 in the output dir, overwritten on
	// re-render, exactly like the old fused path.
	legacyName := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file)) + ".mp4"

	renderTask, err := analysis.RenderFinal(context.Background(), plan, holocropper.RenderRequest{
		OutputDir:  outputDir,
		TempDir:    tempDir,
		FFmpegPath: ffmpegPath,
		OutputName: legacyName,
		Observer:   observer,
	})
	if err != nil {
		log.Printf("Error during cropping for file %s: %v", file, err)
		return
	}
	registerCancel(file, renderTask)
	res, err := renderTask.Wait()
	if err != nil {
		log.Printf("Error during cropping for file %s: %v", file, err)
		return
	}

	bart.Close()
	fmt.Printf("Output: %s (%dx%d, %d frames, %.2f fps, %.2fs)\n",
		res.OutputPath, res.OutputW, res.OutputH, res.Frames, res.FPS, res.ElapsedSeconds)
	for _, w := range res.Warnings {
		fmt.Printf("ffmpeg warning: %s\n", w)
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func catchExit() {
	gracefulShutdown := make(chan os.Signal, 1)
	signal.Notify(gracefulShutdown, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-gracefulShutdown
		fmt.Println("\n\nCaught EXIT")

		// Cleanup: cancel every live pass (the Tasks kill their ffmpeg
		// subprocesses and clean their scratch; analysis caches stay, they
		// are reusable).
		cancelMu.Lock()
		defer cancelMu.Unlock()
		for _, tasks := range runningJobs {
			for _, t := range tasks {
				t.Cancel()
			}
		}

		os.Exit(1)
	}()
}
