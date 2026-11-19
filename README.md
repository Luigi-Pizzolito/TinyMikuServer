# TinyMikuServer
 Server for streaming data to tiny Miku Holocube AI Assistant

## HoloCropper

Content-aware video cropper used to prepare videos for the tiny Miku holocube displays. It lives in `pkg/holocropper` (Go package) with a CLI at `cmd/holocropper` (built to `bin/holocropper` via `make build-holocropper`).

```sh
bin/holocropper -i vids/song.mp4 -o out-dir -w 200 -h 648 -fps 24
bin/holocropper -i vids/song.mp4 -o out-dir -w 648 -h 200 -fps 24 -auto-orient
```

Any output aspect ratio works (1:1, tall portrait `200x648`, wide landscape `648x200`). Optional tuning flags: `-analysis-height` (default 480), `-zoom-margin` (default 1.12), `-max-zoom` (default 2.0). Setting `HOLOCROPPER_DEBUG_BOXES=boxes.csv` writes a per-frame framing log (subject box, smoothed box, applied crop) for verification.

### Auto-orient (`-auto-orient`)

Give any output resolution — e.g. `-w 648 -h 200` — and let the cropper pick the orientation for you. A preprocessing probe decodes up to 128 evenly spread frames of the input through the same subject-detection machinery the cropper uses, measures each frame's **master bounding box** (one box covering ALL detected subjects), and averages those aspect ratios. The requested `WxH` is compared with its transpose `HxW` by containment score (how much of the output a subject can fill at that ratio) and the better orientation is rendered.

- Tall solo subjects flip a landscape request to portrait; side-by-side groups flip a portrait request to landscape.
- The video is never rotated or flipped: only the crop orientation transposes, and the source always stays upright.
- `1:1` outputs bypass the probe completely (there is no portrait/landscape choice to make).
- The flag is opt-in. Without it `-w`/`-h` are used exactly as requested, so you can still render a portrait and a landscape version of the same video by hand. A probe failure or a subjectless video falls back to the requested orientation.

### Walk mode (landscape to landscape)

When a landscape source video is cropped to a landscape output, the subject's left/right choreography is *kept* instead of being tracked away: the crop locks its horizontal position and Miku visibly walks left and right across the output frame, just like in the source. The crop only pans when the walk runs out of slack, and then it follows continuously (a deadzone leash - freeze, edge-chase, no bang-bang switching, no jitter).

- Always on - no flag. It activates when the output aspect is as wide as or wider than the source aspect (with a small leniency), e.g. 16:9, 16:10 or `648x200` output from a 16:9 source. Portrait, square and 1:1 outputs are untouched: their close crops have no slack to walk in anyway.
- The subject never walks off screen: a leash margin keeps her inside the crop edges while she walks, and the crop itself never leaves the frame (or its stage-padding allowance).
- If the output aspect is *narrower* than the source aspect (e.g. 4:3 output from a 16:9 source), the source's full walk range cannot fit the output width, so centered follow-tracking is kept, exactly as before.
- If the subject (or a side-by-side group) is wider than the crop minus two margins, those frames automatically fall back to centered tracking with velocity lead.
- Vertical framing is unchanged: the zoom headroom reduction with feet anchoring and the floor pin still apply; walk mode only affects the horizontal target.
- Landscape crops always keep empty stage padding on the left and right: the landscape subject fit uses a 1.18 zoom-margin floor (the same padding factor portrait uses), so subjects and side-by-side groups never graze or crop at the output's L/R edges - "like the headroom on top".

Design and measured results: [HIGH-ASPECT-RATIO-FIXES.md](HIGH-ASPECT-RATIO-FIXES.md).

<details>
<summary>Key differences vs the original 1-1-Content-Aware-Video-Crop algorithm</summary>

The original algorithm (https://github.com/Luigi-Pizzolito/1-1-Content-Aware-Video-Crop) targeted 1:1 output only and left visible artifacts at other ratios. This rework:

- **Any output aspect ratio.** Framing is aspect-locked to the requested output. The original fixed the analysis frame to `Height x 16/9` and a square crop, so a 648x200 output analyzed a 356x200 frame and guaranteed 292+ px of padding; extreme ratios jittered and filled with black bars (measured: bars up to 372 px on 200x648).
- **No black padding on portrait/square.** Crop rectangles are clamped inside the frame and scaled to exactly fill the output, replacing the old expand-only rectangle + fit-resize that letterboxed with black. Landscape may extend the crop past the frame's left/right edges as black stage padding (up to a full-frame-height crop, ~41% per side on 648x200; see below); vertical padding never occurs.
- **Fixed PID.** The original `Update()` computed `dt` from wall-clock epoch seconds, and its gains were calibrated against that bug. The new controller is pure PI stepping at frame time (1/FPS) — derivative kicks were a measured jitter source on real footage — with anti-windup, low-passed actuator commands (the crop accelerates in a smooth ramp instead of snapping frame-to-frame), crop-relative pan rate caps (limits scale with the crop's own size, so a tight portrait slit pans proportionally faster than a square crop), a smoothed velocity-lead on the pan targets (walking subjects are tracked within a couple of output px), a deadband, continuous target EMAs, and a fast-retarget allowance gated to reacquisition after subject loss. Behaviour is deterministic and reproducible.
- **Subject now fills the frame.** The original could only zoom out, never in (coverage 50-75% on portrait). The new zoom floors (never upscale past source resolution, policy `maxZoom`) give 82-87% subject-height coverage on portrait (89% median) while 1:1 mode keeps the original ~88% look.
- **Landscape framing contains the whole subject, anchored on the floor.** For ratios like 648x200 the zoom window is sized to hold the entire body — head and feet, even in arm-raised poses — and its bottom is pinned to the stage floor. Subjects standing near the floor are ground-snapped: the fit extends to the floor even when the mask misses dark footwear, and grounded crops pin their bottom to the floor exactly, so the feet can never lag the zoom. The crop can always grow to the full frame height, spilling past the left/right edges as black stage padding (up to ~41% per side; the dark stage backgrounds hide it). Subject scale can be small at extreme ratios — full containment wins; a dedicated landscape scale pass is planned separately. The landscape fit enforces a minimum zoom margin (1.18, §14 review): the crop is always wider than the subject, so dancers and groups never graze the output's left/right edges. The original left a constant ~150 px empty strip above the head and cut the legs.
- **Smoother tracking.** EMA subject bounding box with scene-cut reset, Otsu threshold hysteresis, and no unsharp filter on the analysis stream. Dropout holds the last framing and opens gradually instead of the original `panAdj = 0` hard snap (single-frame jumps up to 71 px in 1:1 mode).
- **Tall ratios contain, anchored on the feet.** Portrait crops (e.g. 200x648) contain the whole subject and anchor it so the feet sit just above the bottom edge with a small pad and the slack above the head — a real-corpus sweep showed any tighter fit clips the feet on most floor-standing frames.
- **Faster.** 25-30% faster on portrait and 1:1 despite larger analysis frames; landscape pays a small increase for the larger analysis frame that removes its structural padding.
- **Auto-orient probe (`-auto-orient`).** Estimates the average master-bbox aspect ratio of the subjects (all subjects, per frame, sampled through the detection pipeline) and renders the requested resolution or its transpose, whichever keeps them best framed. Never rotates the video; bypassed for 1:1. The original had no equivalent - the output orientation was fixed by the caller.
- **Walk mode (landscape to landscape).** A continuous deadzone pan leash: the crop holds its horizontal position so the subject walks left/right in the output exactly as she does in the source, panning smoothly only when the walk runs out of slack. The subject can never cross a screen edge. Active whenever the output aspect is at least as wide as the source aspect; portrait/square framing untouched. The original always centered the subject, which made dancing subjects appear stationary.
- **Tooling the original had none of:** 36 unit tests on the framing controller, geometry, walk-mode leash, and the auto-orient estimator, a CSV instrumentation hook for ground-truth verification, and CLI-tunable framing parameters.

</details>
