package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/Luigi-Pizzolito/TinyMikuServer/pkg/mikvformat"
)

func main() {
	fmt.Println("MikVFormat Example")

	// Create a new MikVFormat instance
	mikv := mikvformat.NewMikVFormat()
	mikv.Verbose = true

	// Set audio parameters
	mikv.SetAudioParams(16000, 1, 16)

	// Load MikV from an MP4 file
	mp4File := "patchwork.mp4"
	if err := mikv.LoadFromMP4(mp4File); err != nil {
		log.Fatalf("Error loading MP4 file: %v", err)
	}

	// Save MikV to a new file
	mikvFile := "patchwork.mikv"
	startSave := time.Now()

	if err := mikv.Save(mikvFile); err != nil {
		log.Fatalf("Error saving MikV file: %v", err)
	}

	elapsedSave := time.Since(startSave)
	fmt.Printf("Successfully saved %s in %s\n", mikvFile, elapsedSave)

	fmt.Printf("Successfully converted %s to %s\n", mp4File, mikvFile)

	// Measure the time it takes to load a MikV file
	// Delete the current MikVFormat instance and create a new one
	mikv2 := mikvformat.NewMikVFormat()
	mikv2.Verbose = true
	start := time.Now()

	if err := mikv2.Load(mikvFile); err != nil {
		log.Fatalf("Error reading MikV file: %v", err)
	}

	elapsed := time.Since(start)
	fmt.Printf("Successfully read %s in %s\n", mikvFile, elapsed)

	// Compare mikv and mikv2 to check if they are the same
	if len(mikv.Frames) != len(mikv2.Frames) {
		fmt.Println("mikv and mikv2 have a different number of frames.")
	} else {
		identical := true
		for i := 0; i < len(mikv.Frames); i++ {
			if !bytes.Equal(mikv.Frames[i].Video, mikv2.Frames[i].Video) || !bytes.Equal(mikv.Frames[i].Audio, mikv2.Frames[i].Audio) {
				fmt.Printf("Frame %d is different.\n", i)
				identical = false
				break
			}
		}
		for i := 0; i < len(mikv.Frames); i++ {
			if !bytes.Equal(mikv.Frames[i].Audio, mikv2.Frames[i].Audio) {
				fmt.Printf("Audio frame %d is different.\n", i)
				identical = false
				break
			}
		}
		if identical {
			fmt.Println("mikv and mikv2 frames are identical.")
		} else {
			fmt.Println("mikv and mikv2 frames are different.")
		}
	}

	// Remove the MikV file after processing
	if err := os.Remove(mikvFile); err != nil {
		log.Fatalf("Error removing MikV file: %v", err)
	} else {
		fmt.Printf("Successfully removed %s\n", mikvFile)
	}
}
