package main

import (
	"fmt"
	"time"

	"github.com/Luigi-Pizzolito/TinyMikuServer/pkg/holocropper"
)

func main() {
	inputVideo := "patchwork.mp4"
	outputDir := "crop_test"
	width, height := 240, 240
	fps := 20

	// Create a new HoloCropper object
	hc := holocropper.NewHoloCropper()
	fmt.Println("Starting processing...")
	go func() {
		if err := hc.Crop(height, width, float64(fps), inputVideo, outputDir); err != nil {
			panic(err)
		}
	}()

	// Wait for the goroutine to finish
	for !hc.Done() {
		// fmt.Printf("Processing %.02f\n", hc.Progress()*100)
		time.Sleep(500 * time.Millisecond)
	}

	fmt.Println("Processing complete!")
}
