package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/Luigi-Pizzolito/TinyMikuServer/pkg/holocropper"
)

var holoCroppers map[string]*holocropper.HoloCropper

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

	fmt.Println("Starting processing...")

	var wg sync.WaitGroup

	holoCroppers = make(map[string]*holocropper.HoloCropper)
	mu := sync.Mutex{}

	for _, inputFile := range inputFiles {
		wg.Add(1)
		go func(file string) {
			defer wg.Done()
			// Create a new HoloCropper object
			hc := holocropper.NewHoloCropper()
			hc.AnalysisHeight = *analysisHeight
			hc.ZoomMargin = *zoomMargin
			hc.MaxZoom = *maxZoom

			// Auto-orient: probe the subjects' master-bbox aspect ratios
			// and transpose the requested resolution when the opposite
			// orientation keeps them better framed. Never rotates the
			// video; 1:1 outputs bypass the probe.
			width, height := *width, *height
			if *autoOrient {
				if width == height {
					fmt.Printf("Auto-orient: 1:1 output for %s - orientation probe bypassed\n", file)
				} else {
					fmt.Printf("Auto-orient: probing subjects in %s...\n", file)
					outW, outH, st, err := hc.AutoOrient(file, width, height)
					if err != nil {
						fmt.Printf("Auto-orient probe failed for %s: %v - keeping requested %dx%d\n", file, err, width, height)
					} else {
						decision := "keeping requested orientation"
						if st.Flip {
							decision = fmt.Sprintf("FLIPPING to %dx%d", outW, outH)
						}
						fmt.Printf("Auto-orient %s: subjects in %d/%d sampled frames, master bbox AR mean %.2f (geo %.2f, median %.2f); coverage %.3f at %dx%d vs %.3f at %dx%d -> %s\n",
							file, st.FramesWithSubjects, st.FramesExamined, st.MeanAR, st.GeoMeanAR, st.MedianAR,
							st.KeepScore, width, height, st.FlipScore, height, width, decision)
						width, height = outW, outH
					}
				}
			}

			// Store the HoloCropper object in the map
			mu.Lock()
			holoCroppers[file] = hc
			mu.Unlock()

			if err := hc.Crop(height, width, *fps, file, *outputDir); err != nil {
				log.Printf("Error during cropping for file %s: %v", file, err)
			}

			fmt.Printf("Processing complete for %s\n", file)
		}(inputFile)
	}

	// Wait for all goroutines to finish
	wg.Wait()

	fmt.Println("Processing complete!")
}

func catchExit() {
	gracefulShutdown := make(chan os.Signal, 1)
	signal.Notify(gracefulShutdown, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-gracefulShutdown
		fmt.Println("\n\nCaught EXIT")

		// Cleanup
		for _, hc := range holoCroppers {
			hc.Cancel()
		}

		os.Exit(1)
	}()
}
