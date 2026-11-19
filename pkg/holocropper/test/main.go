package main

import (
	"context"
	"fmt"
	"time"

	"github.com/Luigi-Pizzolito/TinyMikuServer/pkg/holocropper"
)

// Scratch harness (untracked, like patchwork.mp4 next to it): run the whole
// embeddable chain on one local corpus file the way the CLI does. Story C3
// removed the fused Crop() path this used to call.

func main() {
	inputVideo := "patchwork.mp4"
	outputDir := "crop_test"
	tempDir := "."
	width, height := 240, 240
	fps := 20

	observer := &holocropper.Observer{
		OnTask: func(ti holocropper.TaskInfo) {
			if ti.Total > 0 {
				fmt.Printf("%s %s %d/%d\n", ti.Kind, ti.Phase, ti.Done, ti.Total)
			}
		},
		OnWarning: func(msg string) { fmt.Println("ffmpeg warning:", msg) },
	}

	fmt.Println("Starting processing...")
	start := time.Now()

	analyzeTask, err := holocropper.Analyze(context.Background(), holocropper.AnalyzeRequest{
		Input: inputVideo, TempDir: tempDir, Observer: observer,
	})
	if err != nil {
		panic(err)
	}
	analysis, err := analyzeTask.Wait()
	if err != nil {
		panic(err)
	}

	plan, err := holocropper.NewPlan(analysis.Stream(), holocropper.Settings{
		Width: width, Height: height, FPS: float64(fps),
	})
	if err != nil {
		panic(err)
	}

	renderTask, err := analysis.RenderFinal(context.Background(), plan, holocropper.RenderRequest{
		OutputDir: outputDir, TempDir: tempDir, Observer: observer,
	})
	if err != nil {
		panic(err)
	}
	res, err := renderTask.Wait()
	if err != nil {
		panic(err)
	}

	fmt.Printf("Output: %s (%dx%d, %d frames, %.2f fps, %.2fs, wall %.2fs)\n",
		res.OutputPath, res.OutputW, res.OutputH, res.Frames, res.FPS, res.ElapsedSeconds, time.Since(start).Seconds())
	for _, w := range res.Warnings {
		fmt.Println("ffmpeg warning:", w)
	}

	fmt.Println("Processing complete!")
}
