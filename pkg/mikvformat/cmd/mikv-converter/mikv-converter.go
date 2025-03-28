package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/schollz/progressbar/v3"

	"github.com/Luigi-Pizzolito/TinyMikuServer/pkg/mikvformat"
)

func main() {
	// Define CLI flags
	input := flag.String("i", ".", "Input MP4 file or directory (default: current directory)")
	output := flag.String("o", ".", "Output directory for MikV files (default: current directory)")

	audioSampleRate := flag.Uint("sampleRate", 16000, "Audio sample rate (default: 16000)")
	audioChannels := flag.Uint("channels", 1, "Number of audio channels (default: 1)")
	audioBitsPerSample := flag.Uint("bitsPerSample", 16, "Audio bits per sample (default: 16)")

	verbose := flag.Bool("silent", false, "Disable verbose output")
	flag.Parse()

	// Resolve input and output paths
	inputPath, err := filepath.Abs(*input)
	if err != nil {
		log.Fatalf("Error resolving input path: %v", err)
	}
	outputPath, err := filepath.Abs(*output)
	if err != nil {
		log.Fatalf("Error resolving output path: %v", err)
	}

	// Ensure output directory exists
	if err := os.MkdirAll(outputPath, os.ModePerm); err != nil {
		log.Fatalf("Error creating output directory: %v", err)
	}

	// Process input
	info, err := os.Stat(inputPath)
	if err != nil {
		log.Fatalf("Error accessing input path: %v", err)
	}

	var files []string
	if info.IsDir() {
		// Collect all MP4 files in the directory
		err := filepath.Walk(inputPath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if strings.HasSuffix(strings.ToLower(info.Name()), ".mp4") {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			log.Fatalf("Error reading directory: %v", err)
		}
	} else {
		// Single file input
		if strings.HasSuffix(strings.ToLower(info.Name()), ".mp4") {
			files = append(files, inputPath)
		} else {
			log.Fatalf("Input file is not an MP4 file")
		}
	}

	// Convert each file
	// Initialize progress bar if there are multiple files
	var progressBar *progressbar.ProgressBar
	if len(files) > 1 {
		progressBar = progressbar.Default(int64(len(files)), "Converting files")
		defer progressBar.Finish()
	}

	for _, file := range files {
		fmt.Printf("Processing file: %s\n", file)

		// Create a new MikVFormat instance
		mikv := mikvformat.NewMikVFormat()
		mikv.Verbose = !(*verbose)

		// Set audio parameters
		mikv.SetAudioParams(uint16(*audioSampleRate), uint8(*audioChannels), uint8(*audioBitsPerSample))

		// Load MP4 file
		if err := mikv.LoadFromMP4(file); err != nil {
			log.Printf("Error loading MP4 file %s: %v", file, err)
			continue
		}

		// Save MikV file
		outputFile := filepath.Join(outputPath, strings.TrimSuffix(filepath.Base(file), ".mp4")+".mikv")
		if err := mikv.Save(outputFile); err != nil {
			log.Printf("Error saving MikV file %s: %v", outputFile, err)
			continue
		}

		fmt.Printf("Successfully converted %s to %s\n", file, outputFile)
		if progressBar != nil {
			progressBar.Add(1)
		}
	}
}
