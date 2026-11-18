package holocropper

import (
	"fmt"
	"image"
	"os"
)

// boxDebugger writes one CSV line per processed frame when the environment
// variable HOLOCROPPER_DEBUG_BOXES is set to an output path. It is the ground
// truth for framing behaviour (subject bbox vs applied crop) and does not
// rely on visual black-bar inference, which is confounded by dark scene
// backgrounds. See HIGH-ASPECT-RATIO-FIXES.md Phase 0.
type boxDebugger struct {
	f *os.File
}

func newBoxDebugger() *boxDebugger {
	path := os.Getenv("HOLOCROPPER_DEBUG_BOXES")
	if path == "" {
		return nil
	}
	f, err := os.Create(path)
	if err != nil {
		fmt.Printf("holocropper: could not open debug boxes file %s: %v\n", path, err)
		return nil
	}
	fmt.Fprintln(f, "frame,subjects,bbox_x0,bbox_y0,bbox_w,bbox_h,ema_x0,ema_y0,ema_w,ema_h,crop_cx,crop_cy,crop_w,crop_h,target_h")
	return &boxDebugger{f: f}
}

func (d *boxDebugger) line(frame, subjects int, bbox, ema image.Rectangle, cropCx, cropCy, cropW, cropH, targetH float64) {
	if d == nil || d.f == nil {
		return
	}
	fmt.Fprintf(d.f, "%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%.2f,%.2f,%.2f,%.2f,%.2f\n",
		frame, subjects,
		bbox.Min.X, bbox.Min.Y, bbox.Dx(), bbox.Dy(),
		ema.Min.X, ema.Min.Y, ema.Dx(), ema.Dy(),
		cropCx, cropCy, cropW, cropH, targetH)
}

func (d *boxDebugger) close() {
	if d == nil || d.f == nil {
		return
	}
	d.f.Close()
}
