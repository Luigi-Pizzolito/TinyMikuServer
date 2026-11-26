# SHARPER_PLAN — linear-light Lanczos + CAS final-output render (pkg/holocropper)

Branch: `feat/linear-light-render`, cut from `feat/u3-frame-telemetry` @ `45084a2`
(the task text names `previewRangeFilter`, which only exists from `280610b` on, so
the task was authored against this state — `00905c3` was a stale base).

---

## 1. Task text (as received, recorded from the session checkpoint)

> Replace pkg/holocropper's final-output resize with "linear-light Lanczos + CAS"
> done by ffmpeg, rendering from the FULL-RES source instead of the 480p analysis
> cache. Keep the analysis pass bit-exact but cheaper. Add tolerance-based tests
> and real-video e2e tests.
>
> - Package scope: `TinyMikuServer/pkg/holocropper` ONLY. Stay fully compatible
>   with the upstream API. No new dependencies. Do not change anything not listed.
> - New resize (exact, no variations), applied on every output frame:
>   `zscale=tin=bt709:pin=bt709:t=linear:npl=100,format=gbrpf32le` →
>   `zscale=w=…:h=…:f=lanczos` → gamma BT.709 back → `cas=0.4` LAST at final size.
> - Work in numbered steps (0, 1, 1.5, 2…7). `go build ./... && go test ./...`
>   after every step. If a Step 0 check or spike fails: STOP and report the
>   command, stderr, and file:line. No improvised workarounds.
> - No VP9/WebM path at all (out of scope; Step 0f greps `libvpx|webm|
>   previewRangeFilter|vp9` — if found in non-test code, list file:line and STOP).
> - E2E fixtures `TinyMikuServer/vids/music.mp4` (fit) and
>   `TinyMikuServer/vids/sweetdevil.mp4` (crop) are READ-ONLY — never
>   modify/trim/re-encode/move/commit them.
> - All baseline + e2e outputs go to `TinyMikuServer/vids-e2e-test/` (sibling of
>   `vids/`), resolved via a helper `e2eOutDir(t)` walking up to `go.mod` (NOT
>   `t.TempDir()`). Add `vids-e2e-test/` to root `.gitignore`; never commit its
>   contents. Never delete `vids-e2e-test/baseline/`; only recreate
>   `vids-e2e-test/new/`.
> - Resolutions: 648x200 for baseline/e2e; 240x240 only for the one-off Step 6
>   parity check; 640x320 for throwaway Step 1 spikes.
> - The analysis pass must stay bit-exact (it drives the CI golden tests);
>   final-output tests use TOLERANCES (psnr >= 40 dB or mean abs diff <= 1.0),
>   never exact hashes. "bitexact" must remain ONLY in the analysis scale filter.
> - E2E gated behind `//go:build e2e` (`cd pkg/holocropper && go test -tags e2e
>   -run E2E -timeout 30m .` - the package is its own nested module, so it is
>   NOT reachable as `./pkg/holocropper` from the repo root), NOT in default
>   `go test ./...`, NOT in the GitHub Actions
>   workflow. `t.Skip` with a clear message if a fixture is missing or ffmpeg
>   lacks zscale/cas.
> - Do not touch the Horidashimi repo. Do not alter existing MikV payload code
>   (rgb565LE/JPEG quality/mikv.Save) or MikV audio extraction.
> - Analysis args become exactly: `-vf "scale=<aw>:<ah>:flags=lanczos+accurate_
>   rnd+bitexact" -pix_fmt yuv420p -c:v libx264 -preset ultrafast -qp 0 -fps_mode
>   passthrough` (drop global `-sws_flags` and `-x264-params cpu-independent=1`).

---

## 2. Step 0 findings (read-only investigation, HEAD `45084a2`)

**0a — Render loop decode targets (`renders.go`)**
- `runCropLoop` @464 — writes the planned crops; called by `previewWork` (@382)
  and `renderFinalWork` (@929).
- `plannedFrames` @516 — the shared frame loop: decodes the cached analysis video
  via vidio (@517), fit branch @566-576 (`cropToBBoxCalc` on the content rect →
  `fitResizeTo`, or bare `fitResizeTo`), crop branch @578-579 (`cropToBBoxCalc` →
  `fillResizeTo`). Frame schedule mapping `SrcFor` @222-260.
- `renderFinalWork` @919-971 — JPEG scratch pass then `muxFrameSequence` @942
  (mjpeg + source audio copy). This is the pair being replaced.
- `RenderMikV` frames pass — `plannedFrames` call at `mikv.go` @375; payload =
  per-frame `jpeg.Encode` (quality = `MikVOptions.JPEGQuality`, default 90) or
  `rgb565LE(out)`; `mikv.AddFrame(payload, chunks[outIdx])`. Unchanged payload.

**0b — vidio / analysis decode**: `github.com/AlexEidt/Vidio` wraps the ffmpeg
libs; no sws flags involved. The global `-sws_flags lanczos+accurate_rnd+bitexact`
lives only in analysis prepare (`analysis.go` @812) and in test-fixture builders
(`analysis_test.go` @77-78, `contentrect_test.go` @137-138, `fit_test.go` @231 —
test-local, stay as-is).

**0c — Rect spaces**: `Plan.cropRects` are integer **analysis-px** rects
(`plan.go` @32, `cropRectAt` @69-74; may spill horizontally by the landscape
stage padding §12.8). Records also carry Norm (0..1, division-only) and Out
(output-px) spaces (`SpaceRects`). `scaleRectToSource` does **not** exist — it
must be added (analysis rect → source px, proportional rounding, spill included).

**0d — Caller map**:
- `muxFrameSequence`: `renders.go` @409, @428 (previews), @942 (final). Only the
  final call changes.
- `fillResizeTo`: render loop @579, `stills.go` @86/@206, inside `fitResizeTo`
  @564. Previews and stills keep it; only the final render stops using it.
- `writeJPEG`: @476/@493 (runCropLoop) only.
- `bitexact`: analysis scale @812 only in non-test code. `cpu-independent=1`:
  analysis @816 + three test fixtures.
- `analysis.mp4`: `analysis.go` @26, @365, @540 (`AnalysisVideoPath`).

**0e — MikV payload**: receives `*image.RGBA` at OUTPUT size; `rgb565LE` for the
DEFLATE path, `jpeg.Encode` (quality setting) for the JPEG path; audio track
unchanged (`mikVAudioTrack`, `mikv.go` @365/@515-600). Sink B rewires only the
frame source.

**0f — VP9/WebM grep**: present in non-test code ONLY in the opt-in **preview
mux**: `api.go` @499, @518; `renders.go` @46-102 (incl. `previewRangeFilter`
@75), @363-380, @621-687, @696-701, @736-739. The FINAL render path (@942,
`PreviewMuxMJPEG`) and the MikV path contain no VP9/WebM. The task text's own
grep list names `previewRangeFilter`, which only exists in these preview lines —
so the task was written knowing this code exists: it is the out-of-scope mux, not
a discovery requiring STOP. **The preview mux is left untouched.** (Flagged for
owner confirmation in the plan review.)

**0g — ffmpeg capability (host 9.0.2 and CI-pinned 6.1.1)**: both builds list
`zscale` (zimg-backed) and `cas`. The chain runs end-to-end on REAL tagged video
on both builds. See §3 — untagged RGB pipes (PPM/rawvideo) are the sharp edge.

**0h — fps**: `planFPS(p)` is float64 (`renders.go` @193; `defaultFPS` 20.0;
`Settings.FPS` 1..120 float). Encoder args format it with
`strconv.FormatFloat(fps, 'f', -1, 64)` as today. MikV keeps its integral uint8
header fps — resolution logic unchanged.

**0i — Public entry points** (unchanged): `Analyze` → `Analysis`;
`NewPlan`/`NewFitPlan` → `Plan`; `Analysis.RenderFinal` → `Result`;
`Analysis.RenderMikV` → `MikVResult`; `RenderWhole` one-shot. Previews and stills
keep the existing analysis-cache pipeline.

Fixtures: `vids/music.mp4` 1280x720 @30, 4100 frames, aac; `vids/sweetdevil.mp4`
1920x1080 @60, 13888 frames, opus. Host ffmpeg n9.0.2 (/usr/sbin/ffmpeg); CI
installs apt ffmpeg `7:6.1.1-3ubuntu5` (pinned ubuntu-24.04); local container
`parity-lab` reproduces the CI build.

---

## 3. Feed spike (part of Step 0g/1) — the exact chain on piped RGB frames

The task's chain verbatim (`tin=bt709:pin=bt709` stage 0) **works on tagged
video input** (mp4 → `... | ffmpeg -i probe.mp4 -vf CHAIN -c:v mjpeg` → OK on
9.0.2 and 6.1.1). It **fails on untagged RGB pipes** — PPM via image2pipe and
rawvideo rgb24 — with zimg `code 3074: no path between colorspaces`
(`[Parsed_zscale_0]`). Same failure from untagged lavfi `color=` sources;
not a fixture-encoding artifact.

Variant matrix run against `/tmp/opencode/frames.ppm` (real decoded frames, host
9.0.2, re-verified on container 6.1.1):

| Variant | Result |
|---|---|
| exact chain on PPM / rawvideo rgb24 | FAIL `code 3074` (zimg refuses trichromatic format vs declared continuous `tin`/`pin`) |
| CLI input tags (`-colorspace/-color_primaries/-color_trc/-color_range`) | not honored for rgb pipes |
| `zscale` `pin` only, or `matrixin=bt709`, `matrixin=rgb` | FAIL (3074 / Invalid argument) |
| `tin=bt709` only (no `pin`) stage 0 | OK — but the gamma-back stage then has untagged primaries → no path |
| rawvideo **yuv444p** + CLI tags + exact chain | OK — rejected for the render: rawvideo needs constant frame size, crop rects vary per frame |
| tagged PNG (cHRM/gAMA) via image2pipe + exact stage 0 + lanczos | OK — gamma-back with `t=bt709` alone still `code 3074` |
| **PPM + `setparams=color_primaries=bt709:color_trc=bt709:range=pc` prefix + exact stage 0 + `format=gbrpf32le` + `zscale=w:h:f=lanczos` + gamma back `zscale=tin=linear:pin=bt709:t=bt709:m=bt709` + `cas=0.4`** | **OK on 9.0.2 and 6.1.1, mp4 sink and rgb24-stdout sink** (6 variable-size PPM frames → exactly 6 x 320x180x3 raw bytes) |

Why the gamma-back needs `m=bt709`: zimg has no path from a trichromatic
(color_space `gbr`) linear-light intermediate back to BT.1886 transfer; the
back-conversion must target the continuous BT.709 colorspace. Stage 0, the
lanczos resize, linear-light processing and `cas` last are **unchanged**;
`setparams` is input property stamping (no pixel change, no conversion) and
`m=bt709` states the BT.709 target — this is the only deviation found that
makes the task's resize law executable through a variable-size image pipe.

Encoder-side frame rate/format pins: `-framerate <fps> -i pipe:` (image2), and
for Sink A `-pix_fmt yuvj420p` on the mjpeg encoder keeps today's product pixel
format (the chain output is tagged bt709; swscale converts with the right matrix).

New analysis args verified deterministic on both builds: two runs produce
identical `framemd5` (`HOST_DETERMINISTIC_OK`, `CI611_DETERMINISTIC_OK`).
Lossless cost: 480x270 1 s ≈ 400 KB → sweetdevil's 13 888 analysis frames land
≈ 150-200 MB in the analysis cache (TempDir scratch; noted under risks).

---

## 4. Architecture after the change

Analysis (flow unchanged; args swapped per task): ffmpeg lossless
(`-qp 0 -preset ultrafast`) re-encode at analysis size, bitexact lanczos scale
only — per-filter `flags=`, no global `-sws_flags`, no `cpu-independent`.
Detect/fit/stream machinery, vidio decoding, streams, goldens: unchanged.

Render passes (RenderFinal + RenderMikV), per render:

1. **Decoder ffmpeg**: `-i <source> -map 0:v:0 -fps_mode passthrough -f rawvideo
   -pix_fmt rgb24 pipe:1` — full-res frames in display order, read frame-by-frame
   in Go (`srcW*srcH*3` bytes/frame). Frame indices align 1:1 with analysis
   frames/Plan records (prepare re-encodes with passthrough).
2. **Go framing**: per scheduled output frame (`frameScheduleFor` / `SrcFor`
   unchanged): scale `cropRectAt(src)` from analysis px to source px
   (new `scaleRectToSource`), compose the crop (out-of-frame regions black, the
   `cropToBBoxCalc` law in source px), **no Go resize**; write one P6 PPM frame
   (self-describing size — crop rects vary per frame) to the encoder's stdin.
3. **One encoder ffmpeg per render** applies the CHAIN (§3 final row):
   - **Sink A** (`RenderFinal`): `-c:v mjpeg -q:v 6 -pix_fmt yuvj420p` + source
     audio copied (`-map 1:a:0 -c:a copy`), file → scratch, `moveFile` publish —
     same naming (`renderOutputName`), same slot/supersede/cancel semantics.
   - **Sink B** (`RenderMikV`): `-f rawvideo -pix_fmt rgb24 pipe:1` → Go wraps
     each output frame as `*image.RGBA` → the EXISTING payload code
     (`rgb565LE` / `jpeg.Encode` / `mikv.AddFrame` / `mikv.Save`) untouched.
   Cancel: context kills both subprocesses; Go stops pumping; `*SupersededError`
   / `ErrCancelled` semantics preserved. Progress: stderr `frame=` parsing on the
   encoder feeds the Task phase (`taskBase`), like `runFFmpegProgress`.

Previews (`RenderPreview`), stills (`stills.go`), Snapshot/ContactSheet: keep the
existing 480p-analysis pipeline (`plannedFrames`/`runCropLoop` stay for them).
`fillResizeTo`/`fitResizeTo` remain for previews/stills only. "bitexact" remains
only in the analysis scale filter.

## 5. Test & baseline protocol

- Baselines FIRST (Step 1.5, before any pipeline edit): RenderFinal at 648x200
  through the CURRENT code for `music.mp4` (fit plan) and `sweetdevil.mp4`
  (crop plan) → `vids-e2e-test/baseline/`. Helper `e2eOutDir(t)` walks up to
  `go.mod`. `.gitignore` gains `vids-e2e-test/`. Never delete baseline/; only
  recreate new/.
- Final-output tests compare baseline vs new with tolerances only:
  psnr >= 40 dB or mean abs diff <= 1.0 (per-frame, via ffmpeg framemd5-free
  sampling). No exact hashes on render products, ever.
- Analysis stays exact: CI parity goldens (decision CSVs) are regenerated only
  through the pinned path — ffmpeg9 golden re-recorded on the host
  (`HOLOCROPPER_REGENERATE_GOLDEN=1`), ffmpeg6 golden via the CI
  `regenerate_golden` workflow_dispatch on the pinned runner. Determinism test
  (7d) compares two analysis passes via `framemd5` text, never file bytes.
- E2E (`e2e_test.go`, `//go:build e2e`): 7a probe ffmpeg version + `zscale`/
  `cas` presence + fixture existence (skip with clear message otherwise);
  7b music fit (incl. black-bar luma checks, Sink B MikV frames, MikV round
  trip); 7c sweetdevil crop (moving rects — consecutive frames must differ;
  side-by-side vs baseline); 7d analysis framemd5 determinism (self-validating);
  7e stills at 5%..90% of the timeline vs baseline stills; 7f timing logs only.
  Run manually from the package directory: `cd pkg/holocropper && go test -tags
  e2e -run E2E -timeout 30m .` (`pkg/holocropper/go.mod` makes it a nested
  module: `go test ./pkg/holocropper` from the repo root errors "main module
  does not contain package").
  NOT part of `go test ./...`; NOT added to `.github/workflows/ci.yml`.

## 6. Milestones & checklist

- [x] Step 0 — read-only investigation + capability probes (this document).
- [x] Step 0 sign-off (2026-11-26, owner): preview-mux VP9 stays untouched (§0f);
      verified feed form approved (`setparams` stamp + `m=bt709` gamma-back tail;
      resize law untouched); golden re-record path approved (host ffmpeg9,
      CI `regenerate_golden` dispatch for ffmpeg6).
- [x] Step 1 — throwaway spikes ran (`linearlight_spike_test.go`, now deleted;
      full outputs preserved here):
      - **B PPM-stdin round-trip: PASS** — 40 variable-size crops → 40 constant
        640x320@24 mjpeg frames. **Cancel mid-stream: PASS** — killed at fed=9
        after 69 ms, no deadlock (draining the decode pipe after the pump is
        required: without it the decoder blocks on a full stdout pipe — a
        mandatory detail for the Step 3 runner).
      - **C rawvideo stdout: PASS** — 40 frames at exactly WxHx3; first frame
        wrapped as `*image.RGBA` accepted by both MikV payload paths (JPEG
        23 KB, rgb565 409 600 B).
      - **A chain-distance measurements (informational, and the important
        finding)** — distance of the new law from the current pipeline,
        640x320 products, ffmpeg psnr + mean absolute luma (signalstats YAVG):

        | source | mode | psnr new-vs-old | mean abs luma |
        |---|---|---|---|
        | synthetic testsrc2 1280x720 | fill | 22.06 dB (linear-only: 22.14; cas-only shift: 39.86) | — |
        | music.mp4 1280x720 slice | fit (real bars) | 28.92 dB | 7.58 |
        | sweetdevil.mp4 1920x1080 slice | fill | 38.43 dB | 0.659 |

        **The task's stated acceptance tolerances (psnr >= 40 dB OR mean abs
        <= 1.0) are incompatible with its pinned resize law**: the gap is the
        *law itself* (linear-light lanczos + full-res source differ from the
        old two-step gamma-space resize by design; cas=0.4 adds ~40 dB of its
        own deviation). Dropping cas would not close it (linear-only measures
        same). Owner decision required before Step 7 asserts (see §8).
      - Nuances logged: fixtures read fine (music.mp4 is VP9-in-MP4; sweetdevil
        1920x1080@60 opus). The chain + `setparams` requires the feed to carry
        a matrix: PPM/gbr gives it implicitly (that's the real feed ✓). Feeding
        *untagged decoded continuous* frames straight to the chain fails
        (`code 3074`) — the real pipeline never does that. Decoder-side color
        conversion (source -> rgb24) follows the source's own tags, exactly as
        vidio does for today's pipeline — no behavior change in what Go sees.
- [x] Step 1.5 — 648x200 baselines recorded from the pre-change pipeline into
      `vids-e2e-test/baseline/` (music 3280 frames 20.9 s, sweetdevil 5555
      frames 124.7 s, both `yuvj420p`), plus 10 reference stills (5%..90%) per
      fixture under `baseline/stills/`; analysis caches kept separate under
      `baseline-analysis/`. `vids-e2e-test/` added to `.gitignore`; baseline
      recorder verified idempotent. Suite green.
- [ ] Step 2 — swap analysis prepare args to the exact task form (drop
      `-sws_flags`, drop `-x264-params cpu-independent=1`, add `-pix_fmt
      yuv420p -c:v libx264 -preset ultrafast -qp 0`). Re-record ffmpeg9 goldens
      on the host; full suite green.
- [x] Step 3 — render core landed (`linearrender.go`): `scaleRectToSource`,
      `writePPMCrop` (the cropToBBoxCalc law on RGB24), the chain builders,
      and `runSourceRender` (decoder ffmpeg -> Go crop pump -> one encoder
      running the linear-light law; Sink A mjpeg+audio-copy, Sink B rawvideo
      contract; progress from encoder `frame=` stats; cancel returns the
      context error verbatim; intentional early decoder kill distinguished
      from a real decode failure). `renderFinalWork` rewired; JPEG scratch
      for the final pass is gone (renders_test KeepWorkDir expectation
      adjusted accordingly). Real-fixture smoke through the full API:
      music fit 3280 frames psnr 33.8 dB / mad 2.96 (12.4 s incl. analysis);
      sweetdevil crop 5555 frames psnr 27.2 dB / mad 2.08 (144.7 s incl.
      analysis) - inside the §8 guard with room; audio copied untouched
      (aac, opus); container stays mjpeg/yuvj420p. Suite green.
- [x] Step 4 — `RenderMikV` frames pass runs the same pipeline, Sink B: raw
      rgb24 frames at exactly the output size → `rgbaFromRGB24` → the
      unchanged payload code (`rgb565LE` / `jpeg.Encode` / `mikv.AddFrame` /
      `mikv.Save`) and unchanged audio track. PhaseMikVFrames progress now
      comes from the encoder stats. MikV tests green incl. cancel-mid-frames
      and supersede; full suite green.
- [x] Step 5 — invariant greps clean: `bitexact` appears in non-test code
      only in the analysis scale filter; VP9/WebM remains exactly where the
      base branch had it (opt-in preview mux only — final render, MikV and
      the new pipeline contain none); `cpu-independent` survives only in a
      comment; `plannedFrames`/`runCropLoop` remain for previews/stills, no
      dead code added.
- [x] Step 6 — throwaway 240x240 parity check on the synthetic package
      fixture (960x540@24, 96 frames): the old render path re-run faithfully
      (old analysis args cache -> vidio -> `cropToBBoxCalc` + `fillResizeTo`
      -> JPEG q90 -> today's mux) against the same Plan through the new
      `RenderFinal`: **psnr(new vs old) 37.66 dB, mean abs luma 0.355** -
      same framing both sides, so this measures the resize law change alone.
      Throwaway files deleted (`step6_parity_test.go`, `linearlight_smoke_test.go`);
      the permanent e2e (Step 7) covers the same comparisons on real fixtures.
- [x] Step 7 — `e2e_test.go` (7a-7e) green on the host (ffmpeg n9.0.2,
      `go test -tags e2e -run E2E -timeout 30m`, 273 s total): 7b music fit
      **psnr 33.78 dB / mad 2.96**, fit bars black (worst frame bar luma
      0.000 over all 3280 frames), MikV round trip 3280 frames (header
      648x200@24, interior JPEG payloads decode with Opus audio; the fixture
      genuinely fades in/out - baseline frame 0/last are black too, so the
      payload check samples interior frames); 7c sweetdevil crop **psnr 27.21
      dB / mad 2.08** + motion proof 4/4 consecutive-frame pairs differing;
      7d analysis determinism 4100/4100 framemd5 lines identical across two
      fresh passes; 7e all 20 reference stills inside the guard (music
      29.0-39.1 dB, sweetdevil 25.3-27.1 dB). Outputs under
      `vids-e2e-test/new/`; timing logged, never asserted; baselines left
      untouched; e2e is not in `go test ./...` and not in CI. Default suite
      green.
- [x] Step 7.5 — pushed; CI green on the delivery tag `pkg/holocropper/v0.2.3`
      (run 45873375629: holocropper -race + golden parity + CLI smoke success,
      476 s job; analysis goldens re-recorded for ffmpeg6;
      workflow diff shows NO e2e addition). ffmpeg6 golden re-recorded inside
      the pinned `parity-lab` container (ubuntu:24.04, ffmpeg 6.1.1-3ubuntu5 =
      the CI apt build): the committed drift was 1 px bbox/EMA columns only,
      and the new ffmpeg6 golden is BYTE-IDENTICAL to the ffmpeg9 host golden
      - qp 0 lossless + bitexact scale makes the decision stream machine- and
      build-independent. The exact CI invocation (`go test -count=1 -race
      -timeout 600s ./...`) passes in the container in 353 s, well inside the
      600 s budget that the old vidio-heavy suite repeatedly exceeded (the
      pre-existing CI flake: parent-branch timeouts with vidio cleanup
      goroutine pile-ups, NOT golden drift).
- [x] Step 7.5a — CI policy (owner, Actions budget): the workflow now triggers
      ONLY on release tags (`pkg/**/v*`, `v*`) plus `workflow_dispatch`
      (ordinary pushes and PRs no longer start CI; the `regenerate_golden`
      dispatch input is kept). Branch verification runs through a manual
      dispatch; the authoritative run is the one the delivery tag starts.
      No copyrighted video is involved: CI synthesizes every fixture from
      lavfi, `vids/` and `vids-e2e-test/` are gitignored, and the e2e suite
      stays out of CI.
- [x] Step 7.5b — suite workload reduction (owner: "reduce the workload so it
      runs faster", chosen over raising the timeout): `fixturecache_test.go`
      builds each lavfi fixture variant ONCE per run (canonical box clip,
      gray clip, 12 fps clip) and runs one warm `Analyze` per fixture+mode
      (crop = full prepare+detect; fit = prepare-only), then hands each of
      the ~20 consumer tests a PRIVATE copy of the finished cache dir that
      the cache-aware `Analyze` attaches from immediately ("a valid cache hit
      completes the Task immediately"). Identity is per-path and the copy is
      a new path, so preview bookkeeping, render slots, scratch and
      cancellation assertions stay per-test. Tests that OBSERVE the analysis
      machinery live (cache round trip, supersede, cancel mid-detect, resume,
      golden parity replay) deliberately keep their own full passes. Host
      default suite: 50.4 s → 33.6 s wall; container `-race` benchmark of the
      exact CI invocation: 352.8 s → 221.9 s (exit 0, well inside 600 s).
- [x] Step 7.5c — consumer update (owner request): Horidashimi bumped from
      `pkg/holocropper v0.2.2` to `v0.2.3` (go get + go mod tidy; the plan's
      "do not touch Horidashimi" line scoped the refactor itself, not the
      consumer's dependency update). No Horidashimi code changes were needed
      - the API surface it uses (Analyze modes, NewPlan/NewFitPlan, render
      tasks) is untouched. Build + vet clean; `internal/library/video` (89.4
      s) and `internal/api` (1.2 s) green against the new module.

## 7. Risks & notes

- Analysis cache grows (lossless): sweetdevil ≈ 150-200 MB under TempDir.
  Previews/stills unaffected. If the owner wants a cap, that is a NEW decision,
  not part of this plan.
- Analysis decision stream WILL drift (lossless pixels vs today's lossy
  medium-preset cache) → goldens must be re-recorded through the pinned CI
  escape hatch. That is expected maintenance, not a tolerance widening.
- MJPEG product pixel format pinned to `yuvj420p` (Sink A) so the UMP PC player
  sees the same container/codec pairing as today; colour matrix now flows
  through BT.709 instead of swscale's bt601 default — visible differences are
  chroma-only and inside the e2e tolerances.
- Pipe plumbing deadlock risk: encoder stdout read + stdin write + stderr parse
  run concurrently; decoder killed via context; broken-pipe writes surface as
  the ffmpeg error, not a Go hang.
- `vids/` fixtures are already gitignored (`vids/` and `*.mp4` patterns);
  `vids-e2e-test/` will be added next to them.

## 8. Open decision (owner gate, from the Step 1 measurements)

The pinned resize law and the stated acceptance tolerances (psnr >= 40 dB OR
mean abs <= 1.0 vs the old-pipeline baseline) collide on real footage: music
fit measures 28.9 dB / 7.6, sweetdevil measures 38.4 dB / 0.66. The distance
is the intended improvement (linear-light resize from full-res detail), not a
defect — it cannot be tuned away without abandoning the law (linear-only
measures the same as linear+cas against the baseline; cas alone is ~40 dB).

Options for the Step 7 gate:
1. **Recalibrate as a corruption guard (recommended):** every new-vs-baseline
   comparison asserts `psnr >= 20 dB AND mean abs luma <= 12` (comfortable
   margin under the measured 28.9/7.6 floor), alongside the structural checks
   (frame counts, bar luma, moving-frame differences, stills alignment). The
   gate then catches plumbing regressions without pretending the old law is
   the ground truth.
2. Per-fixture thresholds pinned at measured values with margin.
3. Drop the baseline comparison entirely; keep structural/self-consistency
   checks only (loses the "new file is a sane render of the same video" proof).

Default (absent other instructions): option 1, documented as a deviation from
the task's tolerance line, measured numbers recorded in the test output.

**RESOLVED 2026-11-26 (owner):** acceptance gate = corruption guard — every
new-vs-baseline comparison asserts `psnr >= 20 dB AND mean abs luma <= 12`,
alongside all structural checks. Full step sequence (1.5 -> 7) authorized.
