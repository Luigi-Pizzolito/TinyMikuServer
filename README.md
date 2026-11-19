# TinyMikuServer
 Server for streaming data to tiny Miku Holocube AI Assistant

## HoloCropper

Content-aware video cropper used to prepare videos for the tiny Miku holocube displays. It lives in `pkg/holocropper` (Go package) with a CLI at `cmd/holocropper` (built to `bin/holocropper` via `make build-holocropper`).

```sh
bin/holocropper -i vids/song.mp4 -o out-dir -w 200 -h 648 -fps 24
```

Any output aspect ratio works (1:1, tall portrait `200x648`, wide landscape `648x200`). Optional tuning flags: `-analysis-height` (default 480), `-zoom-margin` (default 1.12), `-max-zoom` (default 2.0). Setting `HOLOCROPPER_DEBUG_BOXES=boxes.csv` writes a per-frame framing log (subject box, smoothed box, applied crop) for verification.

Design and measured results: [HIGH-ASPECT-RATIO-FIXES.md](HIGH-ASPECT-RATIO-FIXES.md).

<details>
<summary>Key differences vs the original 1-1-Content-Aware-Video-Crop algorithm</summary>

The original algorithm (https://github.com/Luigi-Pizzolito/1-1-Content-Aware-Video-Crop) targeted 1:1 output only and left visible artifacts at other ratios. This rework:

- **Any output aspect ratio.** Framing is aspect-locked to the requested output. The original fixed the analysis frame to `Height x 16/9` and a square crop, so a 648x200 output analyzed a 356x200 frame and guaranteed 292+ px of padding; extreme ratios jittered and filled with black bars (measured: bars up to 372 px on 200x648).
- **No black padding on portrait/square.** Crop rectangles are clamped inside the frame and scaled to exactly fill the output, replacing the old expand-only rectangle + fit-resize that letterboxed with black. Landscape may extend the crop past the frame's left/right edges as limited black stage padding (max 17% per side, see below); vertical padding never occurs.
- **Fixed PID.** The original `Update()` computed `dt` from wall-clock epoch seconds, and its gains were calibrated against that bug. The new controller is pure PI stepping at frame time (1/FPS) — derivative kicks were a measured jitter source on real footage — with anti-windup, low-passed actuator commands (the crop accelerates in a smooth ramp instead of snapping frame-to-frame), crop-relative pan rate caps (limits scale with the crop's own size, so a tight portrait slit pans proportionally faster than a square crop), a smoothed velocity-lead on the pan targets (walking subjects are tracked within a couple of output px), a deadband, continuous target EMAs, and a fast-retarget allowance gated to reacquisition after subject loss. Behaviour is deterministic and reproducible.
- **Subject now fills the frame.** The original could only zoom out, never in (coverage 50-75% on portrait). The new zoom floors (never upscale past source resolution, policy `maxZoom`) give 82-87% subject-height coverage on portrait (89% median) while 1:1 mode keeps the original ~88% look.
- **Landscape framing contains the whole subject, anchored on the floor.** For ratios like 648x200 the zoom window is sized to hold the entire body and its bottom is pinned to the stage floor, so the feet are never cut. When the body is taller than the widest aspect-locked crop that fits in the frame, the window spills past the left/right edges as black stage padding (capped at 17% per side; invisible on dark stages). Subject scale can be small at extreme ratios — full containment wins; a dedicated landscape scale pass is planned separately. The original left a constant ~150 px empty strip above the head and cut the legs.
- **Smoother tracking.** EMA subject bounding box with scene-cut reset, Otsu threshold hysteresis, and no unsharp filter on the analysis stream. Dropout holds the last framing and opens gradually instead of the original `panAdj = 0` hard snap (single-frame jumps up to 71 px in 1:1 mode).
- **Tall ratios contain, anchored on the feet.** Portrait crops (e.g. 200x648) contain the whole subject and anchor it so the feet sit just above the bottom edge with a small pad and the slack above the head — a real-corpus sweep showed any tighter fit clips the feet on most floor-standing frames.
- **Faster.** 25-30% faster on portrait and 1:1 despite larger analysis frames; landscape pays a small increase for the larger analysis frame that removes its structural padding.
- **Tooling the original had none of:** 19 unit tests on the framing controller and geometry, a CSV instrumentation hook for ground-truth verification, and CLI-tunable framing parameters.

</details>
