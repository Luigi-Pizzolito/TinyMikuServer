# HIGH-ASPECT-RATIO FIXES — `pkg/holocropper`

**Status:** planning document, findings + proposed changes. No algorithm code changed yet.
**Branch:** `fix/holocropper-high-aspect-ratio` (`.gitignore` now ignores `vids/`, `vids-cropped/`, `vids-cropped2/`).
**Scope:** outputs at extreme aspect ratios (`200:648` portrait, `648:200` landscape) against 16:10 sources. Square `1:1` output remains the baseline that works.
**Constraint:** all proposed implementation is **pure Go**, in the existing package structure, using only dependencies already in `go.mod` (`Vidio`, `rez`, `progressbar`). Python was used only for measurement/validation scripts (kept out of the repo, under `/tmp/opencode/analysis/`).

---

## 1. Symptoms reported

1. **Jitter** — pan/zoom framing is visibly shaky at tight framing on high-aspect outputs.
2. **No zoom-in** — the subject is never framed larger than its size in the source video; the crop box is never smaller than the full analysis-frame height, so empty/black space remains above (and around) the subject. Far more visible at `200:648` / `648:200` than at `1:1`.

Both symptoms were reproduced and quantified against the already-processed test videos.

---

## 2. Test corpus and how it was measured

| Source (`vids/`)   | Source size | Portrait out (`vids-cropped/`) | Landscape out (`vids-cropped2/`) |
|--------------------|-------------|-------------------------------|----------------------------------|
| `dissapearance.mp4`| 1920×1080   | 200×648                        | 648×200                          |
| `retrynow.mp4`     | 2560×1440   | 200×648                        | 648×200                          |
| `sweetdevil.mp4`   | 1920×1080   | 200×648                        | 648×200                          |
| `tbaka.mp4`        | 2560×1440   | —                              | 648×200                          |

Measurement methods (scripts in `/tmp/opencode/analysis/`, extracted frames/montages in `/tmp/opencode/frames/`):

- **Black-bar tracking:** `ffmpeg … fps=1,cropdetect` over each cropped video — the non-black content rectangle per second, converted into left/right/top/bottom black bars.
- **Jitter bursts:** 96 consecutive frames (4 s @ 24 fps) extracted as PNGs; bar edges measured per frame in Python; statistics on frame-to-frame change (`std`, `max|Δ/frame|`, **reversals** = direction flips, which distinguish controller hunting from subject motion).
- **Subject-scale comparison:** side-by-side montages of source frame vs both crops at identical timestamps (`<name>_montage.png` in `/tmp/opencode/frames/`).
- **Controller simulation:** exact port of `pid_controller.go` `Update()` arithmetic to reproduce its integration behaviour.

> Measurement caveat: these MMD stages have near-pure-black backgrounds, so "black bars" from black-edge detection can over-count scene background. Structural padding is still provable (e.g. the analysis frame is only 356 px wide for a 648 px-wide output — at least 292 px of padding is guaranteed by construction). Reversal statistics are unaffected by the caveat: monotonic/oscillatory behaviour is controller signature, not background.

---

## 3. What the measurements show

### 3.1 Black space (whole-video, 1 fps samples)

| Video | Output | Content coverage (mean) | Top bar mean/max | Bottom bar mean/max | Left+right bars |
|-------|--------|------------------------|------------------|---------------------|-----------------|
| dissapearance | 200×648 | **49.8%** (min 12.8%) | 119 / 176 px | 122 / 254 px | ~21+21 px |
| retrynow | 200×648 | 75.4% (min 13.8%) | 91 / 240 px | 42 / 178 px | ~9+8 px |
| sweetdevil | 200×648 | 68.9% (min 20.3%) | 73 / 96 px | 24 / **372** px | ~20+18 px |
| dissapearance | 648×200 | **11.4%** — constant 176×84 content | 58 px | 58 px | 238+234 px |
| retrynow | 648×200 | 54.6% — constant 354×200 content | 0 | 0 | **150+144 px ≈ 45% of the frame** |
| sweetdevil | 648×200 | 37.1% (min **11.3%**) | 15 / 48 px | 9 / 56 px | 162+215 px |

Key readings:

- **Portrait:** 18–37% of the output height is black bar *on average*, up to 57% (sweetdevil bottom bar 372/648). This is the "significant black space on top of the subject".
- **Landscape retrynow:** bars are *exactly constant* (150+144 = 294 px ≈ the structural minimum of 292 px): the 356-px-wide analysis frame is simply embedded in a 648-px-wide output. ~45% of the frame is dead space by construction.
- **Landscape dissapearance/sweetdevil:** coverage collapses to 11–37% — subject content renders at 176×84 / 152×96 inside 648×200 late in the videos (progressive expansion, §5 F2/F5).
- **Bottom bars exceed top bars on portrait** (e.g. dissapearance 261 vs 117 in the burst): the resized canvas is anchored **top-left** in `file_io.go copyAndWrite` (`draw.Src` from `img.Bounds().Min`), so letterbox padding piles up at bottom/right instead of being centred (§5 F3c).

### 3.2 Subject scale lost (montage comparison)

In the source, the subject occupies ≈ 85–90% of frame height. In portrait outputs the same subject measures ≈ 42–68% of output height (e.g. dissapearance at t=42 s: ~90% in source → ~58% in the 200×648 crop). The pipeline never renders the subject *larger* than source scale — exactly the reported "never zooms in more than the absolute height of the original video". See `dissapearance_montage.png`, `retrynow_montage.png`, `sweetdevil_montage.png`, `tbaka_montage.png`.

### 3.3 Jitter (96 consecutive frames, bar-edge tracking)

| Window | left bar std | max\|Δ/frame\| | contentH max\|Δ/frame\| | contentH reversals |
|--------|--------------|----------------|--------------------------|--------------------|
| c1 dissapearance @40 s | 18.1 px | 35 px | 54 px | 38 / 95 |
| c1 retrynow @80 s | 16.1 px | 13 px | 14 px | 19 / 95 |
| c1 sweetdevil @100 s | 13.4 px | 26 px | 35 px | 20 / 95 |
| c2 sweetdevil @100 s | 9.3 px | 10 px | 25 px | 23 / 95 |
| c2 retrynow @80 s | 6.4 px | 7 px | 35 px (bottom bar Δ up to 36 px) | 25 / 95 |

- On a 200-px-wide portrait output, left-bar jumps of **17–35% of the output width in a single frame** are pure camera-frame jitter, not choreography.
- 18–38 reversals per 95 frames means the framing box is *oscillating* (hunting), not following monotonic subject motion.
- At 1 fps granularity, coverage change has **zero reversals across every whole video** (0/135 … 0/220) — a slow **monotonic drift** superposed on the fast jitter: the classic signature of integral windup (§5 F5) riding on per-frame detection noise (§5 F6).

### 3.4 Controller simulation

Porting `Update()` exactly (`pid_controller.go:32–41`):

- `dt` computes as `float64(now/1000) - pid.lastError` where `lastError` is the **previous error**, not a timestamp, and `now/1000` is integer division of Unix **milliseconds** → **dt ≈ 1.76 × 10⁹ "seconds" every frame**, effectively constant, instead of `1/FPS = 0.042 s`.
- Consequence: effective per-frame gains become `Ki_eff = Ki × 1.76e9` (pan: 1.76e-3, zoom: 5.3e-4 accumulated-error units per frame) and `Kd_eff = Kd / 1.76e9 ≈ 0.114`. The gain literals (`0.003*10e-10`, `0.002*10e10`) are only stable *because they accidentally cancel the broken dt*.
- Steady state is harmless, but a sustained transient error of ~50 px lasting 100 frames ramps the zoom delta by **+26 px** (1000 frames: +260 px on a 200-px crop) — matches the monotonic whole-video drift in §3.1. Pan windup accumulates behind the ±252 clamp (`process.go` gating vs `calculateCropRect`) and discharges slowly after scene cuts.
- **Naively fixing `dt` alone makes the controller unstable**: with the current gain literals and `dt = 1/24`, the D-term kicks by ~10¹¹ px/frame and the output slams between the 0 and 1000 clamps in simulation. Any dt fix **must** retune gains (§7 Phase 2).

---

## 4. Algorithm map (as it exists today)

```
CLI cmd/holocropper/holocropper.go  -w 200 -h 648 -fps 24
  └─ Crop(height=648, width=200, fps, input, output)          holocropper.go:50
      ├─ initialResize → resizeTiny                            preprocess.go:15–112
      │    ffmpeg: unsharp=5:5:1.0, scale = (Height*16/9) × Height   ← analysis frame size
      │    portrait 200:648  → tiny 1152×648      landscape 648:200 → tiny 356×200
      └─ pipeline (6 goroutines, channels q1..q5)              pipeline.go:31–59
           1 readVideo        frames from the TINY video      process.go:10–23
           2 grayscaleVideo   RGBA→Gray                        process.go:27–44
           3 binaryVideo      Otsu threshold (per frame) → CCL/DFS
                              → boxes ≥20% of largest merged  process.go:48–100
                              video_dsp.go:19–161
                              ⇒ subjects_bbox (one total bbox)
           4 calcVideo        centroid → pan PID → crop rect → zoom PID
                              process.go:104–161 + video_dsp.go:255–303
           5 cropVideo        cropToBBox → resizeRGBA (fit+letterbox)
                              process.go:165–188, video_dsp.go:306–347
           6 saveFinalFrames  copyAndWrite onto black WxH canvas
                              process.go:191–205, file_io.go:82–101
           ⇒ JPEG sequence → ffmpeg mjpeg mux                file_io.go:103–187
```

Data structures: `ImageGrayMask` / `ImageGrayBoundMask` (bbox + subject count) / `ImageCropMask` (crop box) — `pipeline.go:12–28`. PID state: `PIDController{Kp,Ki,Kd,setpoint,integral,lastError,delta}` — `pid_controller.go:7–13`, where `delta` doubles as both controller state and the last applied adjustment (`SetDelta(delta+out)` pattern in `video_dsp.go:255–265` and `process.go:147–152`).

Geometry constants at runtime (for 200×648 output): analysis frame 1152×648; crop rect built as `200 × 648` spanning `Y=[0,648]` — **the full frame height, fixed**; zoom is a symmetric `expandRectangle` of up to `5×200` px.

---

## 5. Root causes

### F1 — Analysis frame is derived from the output height, hard-wired to 16:9
`preprocess.go:28–46`: `width = Height*16/9`, tiny frame = `(Height*16/9) × Height`. So **analysis height = output height exactly**. Combined with F2, this pins the maximum zoom to source scale. For landscape output it is worse: analysis frame 356×200 is *narrower than the requested crop width 648* → ≥ 292 px of structural padding (measured: 294 px, retrynow). The 16/9 factor also silently distorts any non-16:9 source.

### F2 — "Zoom" only ever expands; there is no zoom-in path
`process.go:141–153`: `pidz` setpoint = bbox overshoot + `padding = cropRect.Dx()/8`; `pidz.delta` clamped to `[0, 5*cropRect.Dx]`; `expandRectangle` applied. The crop rect starts at full frame height (`calculateCropRect`, `video_dsp.go:283` `cropHeight := cropWidth`, `:292` fixed `Y=[0,frameH]`) and can only grow. There is no code path that ever *shrinks* the crop below frame height → subject scale ≤ source scale, always. Headroom that exists in the frame is never used: e.g. dissapearance's subject bbox (~150×570 in the 1152×648 analysis frame) admits an aspect-locked `176×570` crop → **1.14× zoom-in would be geometrically available but is never computed**.

### F3 — Expansion + fit-resize turn padding into shrinkage (the black-space generator)
- **F3a** `expandRectangle` (`video_dsp.go:191–201`) expands **both axes** by the same delta, but the crop height already equals the frame height → every 1 px of delta adds 1 px of out-of-frame (transparent → black) rows top and bottom.
- **F3b** `resizeRGBA` (`video_dsp.go:320–347`) uses **fit** scaling (`min(scaleW, scaleH)`) onto the output canvas. Net effect for portrait output: canvas `(200+2δ)×(648+2δ)`, fit scale `s = 200/(200+2δ)`, subject rendered at `s×sourceScale`, vertical black `= 648·2δ/(200+2δ)`:

  | expansion δ | fit scale s | vertical black (portrait 200×648) |
  |---|---|---|
  | 25 px | 0.800 | 130 px (65 top + 65 bottom) |
  | 40 px | 0.714 | 185 px |
  | 60 px | 0.625 | 243 px |
  | 120 px | 0.455 | 353 px |

  These match the measured bars (§3.1: dissapearance ≈ δ 25–30; retrynow portrait ≈ δ 40–60; landscape dissapearance late ≈ δ 100+, coverage → 11%). **`padding = Dx/8 = 25 px` alone guarantees a ~130 px black band on every portrait frame even when the subject fits with room to spare.**
- **F3c** `copyAndWrite` (`file_io.go:87–88`) draws the (possibly letterboxed) canvas onto the black output with `draw.Src` anchored at `img.Bounds().Min` → **top-left anchored** content; padding stacks bottom/right instead of centred (measured: bottom bar ≫ top bar, e.g. 261 vs 117 px, dissapearance burst).

### F4 — Zoom fit check is horizontal-only; crop rect is never clamped to the frame
`minBoxHDist` (`video_dsp.go:296–303`) only measures horizontal bbox↔crop distance; vertical fit is assumed trivial (true only because crop height ≡ frame height). `cropToBBox`/`cropVideo` never clamp the crop rect to frame bounds — out-of-frame regions flow through as transparent pixels and become black in JPEG.

### F5 — `PIDController.Update` dt is broken (windup + gain mis-calibration + nondeterminism)
`pid_controller.go:32–41`: `dt = float64(now/1000) - pid.lastError` — a timestamp minus the *previous error*, with integer division truncating to whole seconds. Consequences (all verified in §3.4 simulation): integral scales by ~1.76e9; D term collapses to ~0.114/frame; wall-clock dt varies with pipeline speed → run-to-run nondeterminism; no anti-windup clamp on `integral`; the exotic gain literals exist only to compensate the bug.

### F6 — Input detection noise is fed raw into per-frame PID updates
- Otsu threshold recomputed every frame (`process.go:58`) → threshold flicker with scene brightness → bbox size flicker.
- `unsharp=5:5:1.0` in the analysis pipeline (`preprocess.go:56`) sharpens edges and fragments masks.
- Centroid = mean of white pixels inside the bbox (`video_dsp.go:235–252`) — sensitive to mask noise inside the bbox; quantised per-pixel.
- Analysis frame is tiny for landscape output (356×200): 1 px of centroid noise ≈ 1.8 px on a 200-px-tall output, amplified further by fit-scale wobble.
- No smoothing, deadband, or rate limit anywhere; pan target is a raw per-frame centroid (`video_dsp.go:255–265`). The 18–38 reversals/95 frames in §3.2 are this noise closing the loop.

### F7 — Pan clamp uses the wrong dimension; no vertical framing control exists at all
`calculateCropRect` (`video_dsp.go:278–283`): clamp `±(cropWidth/2 − frameCtr)` where `cropWidth` is the **frame height** (648/356), not the crop width (200/648). Portrait: pan restricted to ±252 px when a 200-px-wide crop in a 1152-wide frame can legally pan ±476 px → the crop jams against the clamp near frame edges → clamp-boundary chatter (visible in the burst reversals). `Min.Y=0, Max.Y=frameH` fixed → **the crop's vertical position is never controlled**, so vertical framing errors can never be corrected, only grown out of (F3).

### F8 — Extreme ratios make "contain the subject" geometrically impossible (policy gap, not just a bug)
For `648:200` output over a 16:10 source, a tall subject bbox (e.g. 83×176 within the 356×200 analysis frame) requires an aspect-locked crop of 570×176 — wider than the frame itself. **No in-frame crop at 3.24:1 can contain the subject.** Today the pipeline answers this by padding the frame with black (coverage 11%). The algorithm needs an explicit *fit policy* (§7 Phase 4): pad-and-shrink (current, worst option), clamp-to-frame allowing subject overflow with a priority region (head/torso), or zoom-cap tradeoffs.

### Why 1:1 still works
At `W = H = output height`, crop aspect = output aspect, expansion deltas are small (bbox usually fits within the crop), fit scale `s ≈ 0.86–0.92`, and the residual padding sits symmetrically on both axes where it reads as ordinary margin. The defects in F1–F7 are all silent or cosmetic only when `output aspect == analysis aspect`.

---

## 6. Fixing priorities at a glance

| # | Fix | Kills | Effort |
|---|-----|-------|--------|
| P1 | Aspect-locked crop geometry + zoom headroom (decouple analysis frame from output size) | F1, F2, F8 | large |
| P2 | Rewrite PIDController (frame-based dt, anti-windup, retuned gains) | F5, part of jitter | medium |
| P3 | Frame-clamped zoom + fill-resize + centred letterbox | F3a/b/c, F4 | small |
| P4 | Input smoothing: EMA on centroid/bbox/threshold, zoom-target hysteresis, rate limits | F6 | medium |
| P5 | Fix pan clamp dimension; add vertical-centring control (crop Y) | F7, F1 (vertical black) | medium |
| P6 | Debug instrumentation (per-frame bbox/crop/zoom CSV) + Go tests + acceptance metrics | all (verifiable) | small |

---

## 7. Proposed implementation (pure Go, phased)

### Phase 0 — Instrumentation (do first; everything else is unmeasurable otherwise)
Add a debug flag (e.g. env `HOLOCROPPER_DEBUG_BOXES=1` or `HoloCropper` option) that writes one CSV line per frame from `calcVideo`: `frame, bboxW, bboxH, bboxCX, bboxCY, cropX, cropY, cropW, cropH, panAdj, zoomDelta, otsu`. No new deps — `os.Create` + `fmt.Fprintf` beside the existing queue send in `process.go`. This replaces black-bar inference (confounded by black stage backgrounds) with ground truth, and gives the test harness (§8) something to assert on.

### Phase 1 — Aspect-locked crop geometry with real zoom headroom
Decouple analysis resolution from output size. **Analysis height `AF_H = max(outH, analysisHeight)`** with `analysisHeight` configurable, default 480: the analysis frame is never shorter than the output (so a crop is never upscaled), and when the output is smaller than 480 the fixed frame gives zoom headroom. Analysis aspect comes from the **source** (read from `vidio`/ffprobe), never assumed 16:9. Output aspect `A = outW/outH` drives crop shape:

```go
// video_dsp.go (new)
func targetCropRect(bbox, frame image.Rectangle, outW, outH int, margin float64) image.Rectangle {
    w, h := float64(bbox.Dx())*margin, float64(bbox.Dy())*margin
    A := float64(outW) / float64(outH)
    // lock to output aspect
    if w/h < A { w = h * A } else { h = w / A }
    // clamp to frame (no out-of-frame padding, no scale < source scale)
    if w > float64(frame.Dx()) { w = float64(frame.Dx()); h = w / A }
    if h > float64(frame.Dy()) { h = float64(frame.Dy()); w = h * A }
    // zoom-in floors (order matters; all in analysis px):
    //  1) no-upscale-from-source: cropH >= outH * AF_H / sourceH
    //  2) policy: cropH >= AF_H / maxZoom  (maxZoom default 2.0)
    //  3) extreme landscape (A >= 2): cropH >= 0.6 * EMA'd bbox height
    minH := math.Max(outH*float64(frame.Dy())/float64(sourceH), float64(frame.Dy())/maxZoom)
    if A >= 2 { minH = math.Max(minH, 0.6*bh) }
    if h < minH { h, w = minH, minH * A }
    return centerClamped(priorityCentroid(bbox), image.Rect(0, 0, int(w+0.5), int(h+0.5)), frame)
}
```

The zoom-in floor via *source resolution* (not `outH`) matters: with `AF_H = 648` over a 1080-tall source, a 389-px analysis crop still downscales from real source pixels — so `minCrop = outH` was too strict for portrait output (a simulation artifact, caught in §11).

`calcVideo` then smooths `(cropX, cropY, cropH)` toward the target with two PIDs (pan Y is new — closes F7's vertical gap), and `cropVideo` maps the crop to the output by **fill** scaling `s = outH/cropH ≥ 1` (zoom-in) instead of fit scaling. Worked examples from the test corpus (verified in simulation, §11):

- `dissapearance` portrait (AF 1152×648): bbox ≈ 150×570 → aspect-locked target 197×638 → zoom 1.02; subject fills ~89% of height with **zero padding** (vs today's measured 42–68% coverage + 130–240 px bars). Zoom-in headroom down to the 389 px floor stays available for smaller subjects/wide shots.
- `dissapearance` landscape (AF 854×480): bbox ≈ 111×422 → aspect-locked target 1533×473 exceeds the frame → clamp to 854×264 with head/torso priority centroid (§Phase 4) → zoom 1.82×; subject fills 88–100% of output height, **zero padding**, instead of today's 11% coverage inside ≥45% padding.

### Phase 2 — PID rewrite (`pid_controller.go`)
Proper discrete PID with caller-supplied `dt`, anti-windup, and state separated from the plant:

```go
type PIDController struct {
    Kp, Ki, Kd     float64
    integral       float64
    lastError      float64
    integralLimit  float64     // anti-windup clamp
    outputMin, Max float64     // actuator clamps live here, not in callers
}
func (p *PIDController) Step(measured, target, dt float64) float64 {
    e := target - measured
    p.integral += e * dt
    p.integral = clampF(p.integral, -p.integralLimit, p.integralLimit)
    d := (e - p.lastError) / dt
    p.lastError = e
    out := p.Kp*e + p.Ki*p.integral + p.Kd*d
    return clampF(out, p.outputMin, p.outputMax)
}
```

`calcVideo` keeps explicit plant state (`panX, panY, cropH`) and calls `Step(panX, targetPanX, 1.0/fps)`. **Gains must be retuned for dt = 1/FPS** (simulation in §3.4 shows current literals explode under correct dt). Starting points (frame-time units, to tune in tests): pan `Kp 0.12, Ki 0.0005, Kd 0.03`, `integralLimit ≈ 250 px·frames`; zoom (on crop-height error, px) `Kp 0.25, Ki 0.001, Kd 0.05`, `integralLimit ≈ 100 px·frames`. Deterministic: dt comes from the frame counter, never the wall clock.

### Phase 3 — Temporal smoothing of detection inputs (jitter suppression)
In `binaryVideo`/`calcVideo` (all fixed-size state, pure Go):

- EMA on bbox (`α ≈ 0.2`) and centroid (`α ≈ 0.25`), reset EMA on hard scene-cut detection (subject count or bbox jump > 40% of frame).
- Otsu: recompute every 4th frame, lerp ±10 max change per update (`threshold = clamp(tNew, tOld−10, tOld+10)`); drop `unsharp` from the **analysis** stream (keep it for output quality only if ever used there).
- Zoom target recomputed from the EMA'd bbox every 6th frame, quantised in 1%-of-analysis-height steps with ≥ 1 step hysteresis.
- Rate limits at the actuator: pan ≤ 4 px/frame; zoom **asymmetric** — zoom-out fast (≤ 4 px/frame, safety) but zoom-in slow (≤ 0.5 px/frame, prevents "breathing" during pose changes, see §11); plus a 1 px output deadband — small noise simply does not move the crop.
- Subject-lost handling: when the gate trips (perCovered < 0.1%), *hold the last crop target* and drift toward full frame at rate-limit speed. Today the gate forces `panAdj = 0`, snapping the crop back to frame center in one frame (simulation: 40–70 px jumps on dark passages) — this is a deliberate, documented behaviour change from the original.
- Optional stronger mode: 31-frame centred moving average on `(cropX, cropY, cropH)` via a pure-Go ring buffer; the pipeline already computes boxes before cropping, so the delay only costs 31 frames of output buffering.

### Phase 4 — Fit policy for extreme ratios (F8), explicit and configurable
When the aspect-locked target crop cannot contain the EMA'd bbox inside the frame, choose in this default order:
1. `clamp-to-frame` at maximum legal zoom, centred on a **priority centroid** — weight the top 60% of the bbox ×1.5 when computing the centroid (MMD framing priority = head/torso; legs may overflow).
2. Only if the subject is *smaller* than the frame in both axes: allow padding mode (current behaviour) with centred letterbox (§Phase 3 of F3 fix).
3. Never expand out-of-frame for padding on landscape 200-tall outputs — padding there occupies 45%+ of the frame by construction (F1's 356×200 analysis frame).

### Phase 5 — Geometry/crop fixes (`video_dsp.go`, `file_io.go`)
- Clamp every crop rect to frame bounds right after PID smoothing (replaces transparent-black regions with real pixels).
- Replace fit-scale `resizeRGBA` with **fill-scale** + centred placement; when aspect-locked crops are used the letterbox disappears entirely; keep centred `draw` in `copyAndWrite` as a fallback (`dst.Min + ((W-w)/2, (H-h)/2)`).
- Fix pan clamp: use actual crop width (`clamp(panAdj, -(frameW-cropW)/2, +(frameW-cropW)/2)`), and add the symmetric clamp for crop Y once Phase 1 lands.
- `minBoxHDist` → symmetric `minBoxDist` checking both axes.
- Remove `padding = cropRect.Dx()/8` as a *mandatory* zoom-out; make it an asymmetric margin applied only on the axis where the bbox actually overflows, clamped so the crop stays in-frame (kills the guaranteed 130 px portrait bars).

---

## 8. Verification plan (pure Go in-repo; Python only for frame-level cross-checks)

1. **Unit tests** (new `pid_controller_test.go`, table-driven): convergence to setpoint with no steady-state error; windup clamp (step of 50 px sustained 1000 frames → output bounded, recovery < 200 frames); noise attenuation (white-noise setpoint σ=5 px → output σ < input σ); determinism (same input sequence → identical output twice).
2. **Geometry tests** (`video_dsp_test.go`): aspect-locked target rect always inside frame; zoom ∈ [1, frameH/outH]; `targetCropRect` golden values for the four test bboxes (§Phase 1 examples); fill-resize leaves zero black rows for a synthetic centred subject.
3. **Pipeline-level acceptance metrics** via the Phase 0 CSV on the four corpus videos:
   - content coverage (crop covers subject bbox) ≥ 90% of frames where geometrically possible;
   - in-frame padding ≤ 8% of each output axis;
   - crop-edge jitter: `max|ΔcropX| ≤ 3 px/frame`, `reversals ≤ 8 per 95 frames` on static-camera segments;
   - no monotonic crop-size drift across a whole video (last-100-frame mean cropH within 5% of first-100 mean).
4. **Frame-level regression** (external Python scripts already written: `cropdetect` 1 fps + 96-frame bar-series under `/tmp/opencode/analysis/`) run before/after each phase on reprocessed `vids/` corpus; regression numbers recorded in this file per phase.

---

## 9. File-by-file change map

| File | Changes |
|------|---------|
| `pid_controller.go` | Rewrite per Phase 2 (Step API, dt from caller, anti-windup, actuator clamps). |
| `preprocess.go` | `calculateCropWidthHeight` → fixed analysis resolution from **source** aspect; remove 16/9 assumption; drop `unsharp` from analysis encode (Phase 3). |
| `process.go` | `calcVideo`: plant state + two-axis pan PID + zoom-height PID + EMA smoothing + rate limits (Phases 1–3); Phase 0 CSV hook; replace mandatory `Dx/8` padding. |
| `video_dsp.go` | New `targetCropRect`, `centerClamped`, symmetric `minBoxDist`; fix `calculateCropRect` clamps (crop dims, Y axis); fill-scale `resizeRGBA`; priority centroid variant (Phase 4). |
| `file_io.go` | Centred placement in `copyAndWrite`. |
| `pipeline.go` / `holocropper.go` | Plumb optional debug hook + smoothing window config; (unrelated robustness: `Done()` busy-wait at `holocropper.go:107`, `Cancel()` double-close race — fix while touching). |
| new `*_test.go` | Phase 8 table-driven tests. |

## 10. Known constraints / watch-outs

- `rez` downscaling of out-of-bounds crops is currently how padding survives; after Phase 5 clamps, crops are always in-frame and `rez` usage becomes a plain scale — no API change needed.
- Output encoding (`mjpeg -q 6`, JPEG-frame temp dirs) is unrelated to framing quality but inflates test-video sizes; out of scope here.
- Corpus videos have near-black stage backgrounds: bar-based validation (cropdetect/PIL edges) overestimates black space; trust the Phase 0 CSV for acceptance, use bar scripts only as a regression smell-test.
- Re-tuning PID gains without fixing `dt` — or vice versa — makes things worse (§3.4): Phase 2 must land as one commit with its unit tests.

## 11. 1:1 compatibility simulation (proposed algorithm vs current)

The full proposed pipeline (Phases 1–5 geometry + PID + smoothing) and the current pipeline were both simulated end to end over 1400 frames at 24 fps on the **same** synthetic subject model with detection noise, to verify the redesign stays compatible with the original 1:1 mode (improvements welcome, regressions not). Script: `/tmp/opencode/analysis/sim_1to1.py` (Python = measurement only; the spec it validates is pure Go).

Scenario (1920×1080 source): subject drifting ±160 px horizontally, crouch pose (height 950→800), wide-arm pose (width 260→430), hard scene cut at frame 700, big pose filling the frame at frames 1000–1099, detection dropout (dark passage) at frames 1100–1171; per-pixel detection noise σ≈1.5 px + 3% Otsu-flicker frames. Current-pipeline side reproduces `Update()` exactly (buggy dt) and `calculateCropRect` clamps; proposed side uses the §7 spec verbatim (AF_H = max(outH, 480), margin 1.12, EMA 0.2/0.25, 1%-quantised zoom target, rate limits, asymmetric zoom, dropout hold, maxZoom 2.0).

### Results

| Metric | current 1:1 (240×240) | proposed 1:1 (240×240) | proposed 200×648 | proposed 648×200 | proposed 1:1 (648×648) |
|---|---|---|---|---|---|
| Subject height in output (mean) | 88.1% | **88.8%** | **88.8%** | 99.2%¹ | 88.8% |
| Subject height min/max | 74.0 / 96.1% | 74.1 / 96.4% | 74.3 / 96.4% | 87.9 / 100% | 74.8 / 96.4% |
| Subject area in output (mean) | 26.3% | 26.7% | **75.6%** | 15.2%¹ | 26.7% |
| Zoom factor vs source (range) | 1.00 (never zooms) | 1.00–1.04 | 1.00–1.03 (floor allows ≤1.67) | 1.00–1.82 | 1.00–1.04 |
| Letterbox/black padding | 0 (clean scene) | **0** | **0** | **0** | **0** |
| Worst crop jump, single frame | **71 px** | **2.9 px** | 6.1 px | 4.8 px | 5.7 px |
| Dropout hard re-centre snaps | 2 (40–70 px) | **0** | 0 | 0 | 0 |
| Crop follows subject (X std) | 26.3 px | 27.6 px | 76.4 px² | 45.1 px² | 75.0 px² |

¹ Landscape: a 0.26-aspect subject cannot fit a 3.24-aspect crop (§F8) — priority framing fills the frame height and lets legs overflow; area is the policy tradeoff, not a defect. Compare with the measured current landscape coverage of 11–55%.
² Different analysis-frame scales; jitter is comparable via the worst-jump column.

### Current-vs-proposed at 200×648 (same synthetic scenario)

| Metric | current algorithm (sim) | proposed (sim) |
|---|---|---|
| Subject height in output | 72.2% mean (46.4–88.0) | 88.7% mean (74.1–96.4) |
| Subject area in output | 53.6% | 75.5% |
| Fit scale `s` | mean 0.830, std 0.157, max Δ 0.486/frame | 1.005, std 0.005, max Δ 0.006/frame |
| Zoom-out expansion δ | 0–107.6 px | — (crop clamped in-frame) |
| Letterbox/black padding | 11.7% mean | 0% |
| Worst single-frame framing jump | 182 px | 5.8 px |
| Detection-dropout hard snaps | 3 | 0 |
| Zoom-in vs source | never (≤ 1.0) | 1.00–1.03 typical; floors allow ≤ 1.67 |

The sim's current-algorithm numbers sit inside the envelope of the measured 200×648 outputs (§3.1/§3.3: coverage dips 12.8–20.3%, bar maxima 176–372 px, 19–38 oscillatory reversals per 95 frames), so the proposed gains are calibrated against observed failure, not an idealized scene.

Findings:

1. **1:1 framing is preserved almost exactly** — subject height occupancy 88.1% → 88.8% (min/max bands unchanged), area +0.4 pt. The 1:1 look of the original survives; zoom only moves 1.00–1.04× for MMD-dominant subjects, so no surprising tightness.
2. **1:1 jitter improves dramatically**: worst single-frame crop jump 71 px → 2.9 px (output px), dropout snaps eliminated. The current 71 px jumps come from the `panAdj = 0` gate reset and raw-noise P tracking — exactly the reported symptom, present even in 1:1 mode.
3. **Extreme ratios reach the target framing**: portrait subject coverage 42–68% → 89% with zero bars; landscape 11% → full-height priority framing. Both at modest zoom (≤1.8×).
4. **Larger 1:1 (648×648) also works** with the `AF_H = max(outH, 480)` rule — same 88% subject framing, headroom preserved.
5. **Two spec bugs were caught by the simulation before implementation** (and fixed in §7): (a) a fixed 480 analysis height breaks when `outH > 480` — the min/max crop clamps became contradictory (crop 648 > frame 480); (b) a `minCrop = outH` zoom floor over-constrained portrait output — the correct floor is the no-upscale-from-source bound `outH·AF_H/sourceH` combined with a `maxZoom` policy cap.
6. Deliberate behaviour changes vs the original, both visible in the table: dropout no longer snaps the crop to frame center, and 1:1 mode may now zoom a few percent past full frame. If strict original behaviour is ever required, both are parameters (dropout mode, `maxZoom = 1.0`).

---

## 12. Final implementation and measured corpus results (2026-11-18/19)

The §7 algorithm is implemented on branch `fix/holocropper-high-aspect-ratio` (pure Go, existing deps only) and validated end-to-end on the real corpus: dissapearance (1920×1080), retrynow (2560×1440), sweetdevil (1920×1080), tbaka (2560×1440), outputs at `-fps 24`, ground truth from the Phase-0 debug CSVs (`HOLOCROPPER_DEBUG_BOXES=…csv` writes bbox/EMA/crop/target per frame). Unit tests: `pkg/holocropper/framing_test.go` (11 tests: PID convergence/anti-windup/noise/determinism through the real plant path, target geometry goldens per mode, crop-always-inside-frame).

### 12.1 What shipped (differs from the §7 spec where real data demanded it)

1. **Dominant-axis fit instead of full-bbox aspect lock.** Real portrait bboxes are ~150–265 px wide in an 1152-wide frame; containing the *full bbox width* at 200:648 was width-dominant in 99–100% of frames and ballooned crop height to the frame cap (coverage stuck at 58–70%). `aspectLockedTarget` now requires the full subject *height* (×`zoomMargin`) and at least `widthCoverFrac(aspect)` of its width: 0.55 for aspect < 0.5 (portrait), 0.85 for aspect < 1, 1.0 for square/wide. Trailing limbs clip rather than the crop inflating — natural portrait behaviour, verified on frame montages.
2. **Landscape is top-aligned, not 0.35-biased.** When the subject is taller than the crop, `cy = bbox.Top + cropH/2` (subject top = crop top → no empty strip above the head; legs overflow the bottom). When the subject is shorter, slack is split 25/75 top/bottom so the head still hugs the top edge.
3. **Controller damping after real-corpus measurement**: pan deadband 0.75 → 1.5 analysis px, pan Kd 0.03 → 0.02, pan targets pass through an EMA (`targetSmoothAlpha` 0.2, snapped on scene cuts).

CLI: `-analysis-height`, `-zoom-margin`, `-max-zoom` (0 = built-in defaults 480 / 1.12 / 2.0).

### 12.2 Measured results (final build; output px where applicable)

Portrait 200×648:

| Metric | old algorithm (measured §3) | new algorithm (measured) |
|---|---|---|
| Subject height mean | 49.8–75.4% | **82.2 / 81.8 / 86.8%** (dissapearance / retrynow / sweetdevil) |
| Subject height median | — | **89.3%** (all three) |
| Out-of-frame black bars | up to 372 px, drift monotone | **0 (crop provably in-frame)** |
| Median per-frame crop motion | jumps to 71 px reported | **0.25 px**; p90 1.6–2.0 px |
| Frames moving > 3 px | 19–38 oscillatory reversals / 95 frames | 1.7–8.6% of frames, all legitimate re-framing events (cuts, zoom transitions); hunting reversals reduced further by the damping in 12.1(3) |

Square 1:1 (240×240) compatibility:

| Metric | old | new |
|---|---|---|
| Subject height mean | ~88% | **87.1 / 85.7 / 87.5%** |
| Subject bbox fully contained | n/a | **96.8–98.4% of frames** mean |
| Worst jump (output px) | 71 px snaps (sim §11) | 4.8–16.8 px, only at cuts/re-targets |
| Out-of-frame padding | 0 (clean scenes) | **0 by construction** |

Landscape 648×200 (the "remove the empty top area" requirement):

| Metric | old algorithm (measured §3.2) | new algorithm (measured) |
|---|---|---|
| Structural frame shortfall | analysis 356×200 < 648 output width → ≥292 px guaranteed padding | analysis 854×480; crop = full frame width, **no structural padding** |
| Top pad above subject | retrynow: constant ~150 px above / 144 px below | **mean 6.3 / 4.5 / 4.2 / 12.6 px** (diss/retry/sweet/tbaka); 52–57% of frames exactly zero (tbaka: subject bboxes include raised twin-tails, head stays just below the edge) |
| Bottom pad | — | mean 0–4.3 px; legs overflow when subject is taller than crop (documented policy) |
| Subject containment (tbaka, 3 dancers) | 11–55% coverage | **contain 99.9% mean / 92.5% min** |

### 12.3 Tuning iterations that the real corpus forced (simulation was not enough)

1. Full-width aspect lock → coverage 58–70% (width-dominant in ~100% of portrait frames). Fixed by the dominant-axis fit; first cap 0.65 gave 78–85% mean.
2. CSV replay search over `widthCoverFrac` (no re-encode needed): cap 0.75 → 0.65 → 0.55 → 0.45 showed coverage means 74.6→79.7→84.2→87.1 (dissapearance) with horizontal coverage losing only ~3 pt from 0.65→0.55 while 0.45 gained little. **0.55 chosen**: median coverage 89.3% = golden bar, montages confirmed acceptable limb clipping.
3. Landscape 0.35·bbox bias left a fixed ~46 px analysis headroom whenever the subject fit the crop (tbaka mean top pad 34.8 px). Top-alignment + 25/75 slack split cut top pad means to 4.2–12.6 px without stranding short subjects.

### 12.4 Performance (same videos, same machine, `-fps 24`, dissapearance)

| Mode | old binary | new binary | analysis frame |
|---|---|---|---|
| 200×648 | 51 s | **36 s** | 1152×648 both |
| 240×240 | 20 s | **15 s** | old 426×240 → new 854×480 |
| 648×200 | 17 s | **19 s** | old 356×200 → new 854×480 |

Landscape is ~2 s slower because its analysis frame grew 6× to remove the structural padding (§F1) — that is the deliberate trade; portrait and 1:1 are 25–30% faster despite larger analysis frames because `unsharp` was dropped from the analysis stream and the framing controller replaced per-frame centroid scans. `AnalysisHeight` is exposed on the CLI if analysis cost ever needs to be traded for stability.

### 12.5 Acceptance verdict

Golden bars from §8/§11: 1:1 subject ≈ 88% ± 2 pt — **met** (85.7–87.5% mean, 96.8–98.4% containment). Zero out-of-frame padding — **met by construction in every mode**. Worst jump ≤ single-digit px steady-state — **met** (median 0.25 px; all >3 px events are cuts/re-targets). Landscape top dead strip — **removed** (mean top pad 4.2–12.6 px out of 200, majority of frames exactly 0; previously a constant 150 px strip plus ≥292 px structural shortfall).

### 12.6 Post-review tuning: crop-relative pan rates (2026-11-19)

Visual review of the first re-render flagged a tracking fault: in portrait the crop is only ~160 analysis px wide (of 1152), but pan limits were absolute (2.0/1.5 px per frame), so a walking subject outran the crop — **subject center outside the crop on 4–16% of frames** (dissapearance 8.0%, sweetdevil 16.2%), max lag 353 px against an 80 px half-crop. The absolute caps were the wrong unit: pan speed must scale with the crop's own size, because tighter output ratios mean tighter crops.

Changes (all in the pan loop; zoom policy untouched):

1. `panRateLimit(cropDim) = clamp(0.05 x crop dimension, floor 2.0/1.5, cap 12 px/frame)` — ratio-awareness falls out of the geometry: a 160 px portrait slit pans at 8 px/frame, a 473 px square crop at the 12 px cap. No per-aspect table needed.
2. Retarget threshold now 8% of the **crop span** (not frame), and in retarget mode the command switches to **proportional pursuit** (`retargetGain` 0.45, capped at 0.30 x crop per frame). Raising the clamp on the gentle PID output was not enough: with small gains the clamp never bound and the lag grew unbounded while the crop chased an empty stage.
3. Faster subject state: bbox EMA α 0.2 -> 0.3; pan-target smoothing uses α 0.5 (instead of 0.2) when the target jumps >15% of the crop — noise damping preserved for small drifts, walking subjects followed quickly.

Measured after the fix (same corpus, debug CSVs): out-of-frame frames 1.8 / 2.0 / 2.9 / 1.0% (dissapearance / retrynow / sweetdevil / tbaka portrait), max x-lag roughly halved (353 -> 132 px on sweetdevil); landscape out-of-frame 0–0.1%; 1:1 out-of-frame 0%. Steady-state motion stays sub-pixel in 1:1 (median 0.03–0.07 px out). Coverage/zoom unchanged by the tuning (portrait means 81–87%). Unit tests: +`TestPanRateIsCropRelative`, +`TestPanTracksWalkingSubjectPortrait` (13 total).

Visual-review notes for future work: single-subject framing is stable while walking. Two geometric edge cases remain, both detection/policy rather than controller issues: (a) when a subject's twin-tails dominate the merged bbox, the crop centers on the hair mass and the torso clips at an edge (dissapearance ~91s); (b) three-dancer groups in a 200-wide portrait crop cannot fit (tbaka) — coverage 52% there is the aspect-ratio geometry, matching the §F8 analysis.

These same scenarios (1:1 baseline, scene cut, dropout, big pose, wide pose) are promoted into the Go test suite in §8 as golden regression tests: every proposed change must reproduce "1:1 subject height ≈ 88% ± 2 pt, zero padding, worst jump ≤ 3 px" before the extreme-ratio work ships.

### 12.7 Post-review tuning, part 2: portrait composition + controller redesign (2026-11-19)

Second visual review of the §12.6 renders flagged three framing-quality faults, all in portrait: feet pressed against or clipped by the crop bottom on floor-standing frames, composition zoomed in too tightly, and motion visibly less smooth than the hand-tuned original 1:1 PID (residual frame-to-frame snapping, occasional discrete zoom steps). The owner's spec for portrait: **feet stable and always in frame at the bottom with ~2% padding, subject never too zoomed in**.

**Tuning method.** Because re-rendering every parameter change takes minutes, the controller was tuned in a Python *replay simulator* (outside the repo) that replays the shipped plant exactly against real-corpus debug-CSV bbox streams, with a staged coordinate-descent sweep (composition policy → pan/targets → zoom) over 4 videos x 2 windows each. The objective is vision-informed: vertical-visibility floors and feet-clip weighted 3x, out-of-frame guard, a coverage floor (never too zoomed out), sub-pixel steady motion, and bounded zoom jerk. Python was measurement only; the shipped controller stays pure Go, and the port was verified against the sim (below). Two sweep bugs found along the way: a modulo phase error that froze the target update when `targetEveryFrames = 1`, and a metrics tuple-corruption that zeroed all zoom-velocity stats in earlier runs - fixed before the winner was selected.

**Composition (geometry).** A grid sweep of the portrait width-containment fraction proved that any policy narrower than full containment clips the feet on 73-100% of floor-standing frames. Portrait therefore switched from the height-dominant fit (§12.3) to a **containment fit**: the crop contains the full padded subject bbox - `h = max(bbox_h, bbox_w / aspect) x margin` with a portrait-only padding floor of 1.18 (`portraitContainMargin`; landscape/square keep `-zoom-margin`) - anchored on the feet: `cy = bbox.Max.Y - h/2 + 0.05·h`. Headroom slack on 1440p sources (twin-tail width forces the crop toward the full analysis height) is the accepted cost: vision review confirmed full-body framing beats maximal coverage in a 200:648 slit. Dominant-fit candidates were rendered and rejected by visual comparison (feet on the edge, too tight).

**Controller.** Re-tuned end to end (all gains re-confirmed on the real corpus, not synthetic tracks):

1. **PI only** - the derivative term was removed (`Kd = 0`): D kicks on moving/updated targets were the measured jitter source. Pan 0.25/0.002, zoom 0.10/0.001, integral limits unchanged.
2. **Continuous per-frame targets** from the EMA bbox (α 0.3), target EMAs α 0.7 pan / 0.15 zoom, updated every frame instead of every 6th - quantised/stale targets were producing visible zoom steps.
3. **Actuator command low-pass** (α 0.5) on all three axes: the applied command is an EMA of the controller output, so the crop accelerates in a smooth ramp instead of snapping to whatever the PID output is that frame - this rebuilds, structurally, the smoothness the original hand-tuned controller was known for.
4. **Crop-relative pan cap replaces the absolute 12 px cap**: `clamp(0.05·span + 0.10·|error|, floor 2.0/1.5, 0.06·span)`; the §12.6 retarget-pursuit branch is gone.
5. **Smoothed velocity-lead feedforward** on pan targets (3 frames, velocity EMA α 0.5): cancels the target-EMA lag so walking subjects are followed without raising gains; steady tracking lag dropped from 13-20 px out to 0.7-2.5 px out.
6. **Zoom rate caps** 0.25 in / 4.0 out px/frame, with fast pursuit (1.5/10) gated to gaps > 25% of the *frame* (reacquire after subject loss) - ordinary target drift can no longer trigger a zoom step. A 6%-of-frame gate was tried and rejected: it fired on normal drift and raised feet-clip to 13-18%.

**Fidelity of the port.** Go render CSVs vs sim predictions on all 12 corpus renders: mean |diff| 0.02-1.04 analysis px per axis (p99 ≤ 10 px only at cuts/reacquires where per-run detection jitter compounds briefly). Portrait crop_w/crop_h match to the cent in most frames.

**Measured after the change (sweep windows, output px).** Out-of-frame portrait frames 0.0 / 0.0 / 0.6 / 0.0 (dissapearance / retrynow / sweetdevil / tbaka; §12.6: 1.8 / 2.0 / 2.9 / 1.0); full-corpus out-of-frame 0.0-0.2%. Mean x-lag 1.8 / 0.7 / 2.5 / 2.2 (was 13-20). Steady pan motion median 0.11-0.54 px, p99 ≤ 2.1. Zoom: dH p99 ≤ 1.5 px/frame, zoom jerk p99 ≤ 0.31 - no discrete zoom steps remain (§12.3 era: 6.5 px single-frame jumps). Landscape and square regressed to nothing: out-of-frame 0%, landscape dH p99 ≤ 0.1, square coverage medians 58.7-88.0 (golden bar intact for single-subject videos). Portrait coverage medians 72/58/62/51 - the containment trade, accepted by vision: every frame shows the whole subject with feet ~1-4 px above the crop bottom. Residual measured feet-clip on retrynow/sweetdevil (18%/10%) is entirely frames where the detection mask already touches the analysis-frame bottom (gap < 2 px, geometrically unmeasurable, visually fine on the contact sheets).

**Visual verification.** Side-by-side montages (sim crop applied to original 60 fps frames vs shipped renders) selected the containment candidate over dominant-fit and tighter-margin variants; final contact sheets at every previously flagged timestamp (dissapearance f2184 torso-clip, walking frames, floor-standing poses, kneeling poses, arm-spread poses, tbaka group) show feet stable at the bottom with pad, full subject in frame, and no excessive zoom.

Unit tests: 16 total (+`TestZoomDriftStaysSmooth`, +`TestZoomRetargetAllowanceGated`, +`TestAspectLockedTargetPortraitContainsWideSubject`, +`TestAspectLockedTargetPortraitFeetAnchor`, +`TestPanRateLimitsAreCropRelative`; walk-tracking and plant tests rewritten against the new PI + command-EMA signatures).

### 12.8 Landscape containment fit: floor-anchored zoom window with capped stage padding (2026-11-19)

Third visual review: landscape 648x200 framing cut feet. The owner's spec for landscape: **"fit full subject in is the most important"** - the whole body, above all the feet, must always be in frame; a smaller subject scale is accepted and deferred to a dedicated landscape-scale pass.

**The structural problem.** The Phase-4 landscape policy capped the crop at the frame width: max in-frame height 854 / 3.24 = **264 analysis px**, while the corpus' standing subjects are 275-301 px tall (merged bboxes to 353 px with hair, 387-480 px in extreme poses). Full containment was geometrically impossible inside the frame, so the top-anchor 25/75 slack split kept the head framed and cut feet instead: measured true feet-clips (crop shorter than the bbox while the subject stands fully inside the frame) on the sweep windows: **235/394, 935/1158, 1117/1242, 29/339** frames (dissapearance / retrynow / sweetdevil / tbaka), worst cuts 51-89 px out.

**Fix (geometry + plant).** Landscape (aspect >= 2) now uses the same containment policy as portrait, plus a bounded escape past the frame's sides:

1. **Containment fit**: `h = max(bbox_h, bbox_w / aspect) x zoomMargin` (1.12). The old `0.6 x bbox_h` landscape floor and the top-anchor slack split are gone.
2. **Capped side padding**: the crop width may overflow the frame's left/right edges by `landscapePadCapFrac = 0.17` per side - `maxCropW = 1.34 x frameW` (1144 analysis px), max crop height 353 px. **Vertical padding is never allowed**: cy stays fully in-frame, so F3/F4 (no out-of-frame leakage) hold vertically exactly as before. `cropToBBox` composes out-of-frame columns as transparent-black stage padding; `cropRectFromFraming` takes a `padAllow` parameter, and portrait/square call it with 0 (identical clamp behaviour to §12.7).
3. **Zoom floor relaxation**: the no-upscale floor (`outH x frameH / srcH`) remains absolute; the policy `maxZoom` floor yields to the containment fit, in both `aspectLockedTarget` and the plant's `cropFloor`. The subject-loss branch's open-to-full-frame target is capped at `maxCropW / aspect` so the lost-state crop also respects the padding cap.
4. **Feet anchor unified with portrait**: `cy = bbox.Max.Y - h/2 + 0.05·h`, clamped in-frame. For grounded subjects the clamp pins the crop bottom **exactly to the stage floor** (feetGap median was -28.9 / -4.7 / -13.8 px clipped; now 0.0 / 0.8 / 0.7 px pad), which is what makes the residual clips on uncontainable extreme poses fall on head/arms instead of feet.

**Accepted trade.** Subjects taller than 353 analysis px (extreme arm-raised/leaning poses, maxBH 430-480) need >38% side padding to contain; that exceeds the cap and is not paid for. On those frames the feet anchor moves the cut to the head/arms. Vision review confirmed this is the right casualty order.

**Measured after the change** (sweep windows, output px, shipped-Go debug CSVs old vs new renders):

| | dissapearance | retrynow | sweetdevil | tbaka |
|---|---|---|---|---|
| true feet-clip | 235/394 -> **67/395** | 935/1158 -> **19/1158** | 1117/1242 -> **49/1240** | 29/339 -> **0/341** |
| worst cut | 65 -> 33 px | 89 -> 83 px | 52 -> 15 px | 9 -> 0 px |
| visual loss | 17.4% -> **3.4%** | 4.9% -> **0.6%** | 12.8% -> **0.4%** | 0.25% -> 0.31% |
| vertical visibility p05 | 68 -> 91% | 86 -> 98% | 78 -> 98% | 98 -> 98% |
| coverage median | 114 -> 98 | 104 -> 89 | 112 -> 89 | 94 -> 89 |
| feet gap median | -28.9 -> 0.0 | -4.7 -> +0.8 | -13.8 -> +0.7 | 0 -> 0 |
| out-of-frame | 0% | 0% | 0% | 0% |
| zoom dH p99 / jerk p99 | 0.15 / 0.10 | 0.98 / 0.18 | 0.61 / 0.23 | 0.44 / 0.10 |
| mean x-lag | 34.9 | 2.3 (was 6.3) | 83.0 | 9.0 (was 45.5) |
| pan dX p50 / p99 | 0.0 / 4.0 | 0.4 / 4.2 | 0.3 / 5.7 | 0.6 / 9.5 |

Motion stays smooth: dH p99 <= 1.0 px/frame, zoom jerk p99 <= 0.23, tracking lag improved (retarget gates unchanged, §12.7). dX p99 rose from 0.4-1.0 to 4.0-9.5 px/frame while p50 stayed 0.0-0.6: padded crops hit the widened clamp window at frame edges and cuts, where the crop pans faster - expected, sub-10 px events only. Max side padding reaches exactly the cap (145 analysis px = 110 out px, 17% per side) on all four videos and is invisible on the corpus' dark stages.

**Sim fidelity.** The Python replay sim (measurement only) was mirrored to the new geometry (`ls_margin 1.12`, `pad_cap 0.17`, feet anchor, relaxed floors) before rendering; its predictions matched the shipped Go renders (dissapearance: trueClip 68 sim vs 67 real, visual loss 3.42 vs 3.41, pad 145.2 both).

**Visual verification.** Contact sheets per video at max-padding, max-bbox-height, worst-clip, and mid-window normal frames: full head-to-feet framing everywhere, feet at or pinned to the bottom edge, padding invisible against dark stage backgrounds, tbaka group intact, sweetdevil fade-out fallback acceptable. The only visible residual cut is the extreme arm-raised pose (head/arms), never feet.

Unit tests: 19 total (+`TestAspectLockedTargetLandscapeContainment`, +`TestAspectLockedTargetLandscapeFeetAnchor`, +`TestAspectLockedTargetLandscapePadCap`, +`TestCropRectLandscapePadding`, +`TestCropToBBoxBlackPadding`; the two §12.4-era landscape geometry tests rewritten to the containment/floor-anchor policy; `TestCropRectAlwaysInsideFrame` keeps the padAllow=0 in-frame guarantee for portrait/square).
