# holocropper pipeline profiling — perf/holocropper-profile

Measurement of where the H-render-law pipeline actually spends wall time on
the two corpus fixtures, and what can be cut **without touching the law**.
Everything below was measured 2026-11-26/27 on this 32-core machine while it
carried its usual background load (strata engine ~7 cores, Horidashimi
server); the CLI baselines from the v0.3.0 gate runs reproduce, so
comparisons are same-environment.

**Status 2026-11-27: proposals 1-2 are LANDED on this branch** - the
detect-internals rewrite and the buffered pump writer (§9). Proposals 3-6
remain owner decisions.

Harness: `perf_test.go` (build tag `perf`, never part of the shipped suite),
`perf-tools/ffmpeg-wrap` (per-subprocess wall/user/sys via a transparent
ffmpeg wrapper) and `perf-tools/decode-bench.sh`. `/tmp` is tmpfs with ~4 GB
free RAM, so encoder feeds are streamed into ffmpeg's stdin, never written
to disk.

## 1. Cold-run attribution (sweetdevil 648x200@24 CLI, fresh cache)

Total wall **449 s**; the render Task reports 261 s.

| phase | wall | what runs |
|---|---|---|
| metadata probe | < 1 s | ffprobe-style vidio open |
| Prepare | **15.3 s** (user 123 s) | one ffmpeg: 13888-frame decode -> bitexact lanczos to 1152x648 -> x264 qp0 ultrafast, writes 458 MB analysis.mp4 |
| Detect | **~172 s** | no subprocess except vidio's ffmpeg reader: Go `detectFrame` per frame (grayscale, Otsu, hysteresis, threshold, flood-fill components, stats, pools, centroid) |
| Render | **261.0 s** | decode ffmpeg (killed at end - only 13886 of 13888 frames consumed) running concurrently with Go pump + encode ffmpeg |

So a cold sweetdevil run is ~38% Go detect work (172/449), ~58% encoder-
bound render, 3% Prepare; the *decode* is trivial too, and it runs
concurrently with the encoder:

```
D1  decode all 13888 frames -> /dev/null   wall 22.7 s / 26.1 s (2 passes)
D2  same decode + select law (5556 frames) wall 11.3 s / 11.4 s
```

Decode overlaps the encoder and finishes ~4x faster than the encoder eats
its output; decode-side wins are contention relief, not wall time.

## 2. Render pass geometry (from the shipped law path: CachedAnalysis ->
## NewPlan -> frameSchedule -> scaleRectToSource)

sweetdevil (analysis 648, i.e. 1152x648; 13888 records; 5555 output frames):

- decoded pipe: 13886 frames x 6,220,800 B = **86.4 GB** rawvideo through
  the decoder's stdout; Go reads every one (the "decoded == records"
  invariant), **feeds only 39.66 GB (46%)** of PPM crops.
- crop feed sizes: **732 distinct, 4639 size transitions** across 5555
  frames - matches the ~4640 swscaler reconfigurations counted in the gate
  work. Typical crops are ~2.4 MP (e.g. 2640x815 median, 3500x1080 max -
  landscape §12.9 stage padding legitimately spills past the frame).
- music fit (analysis 480): 1 crop size (1280x720), 0 transitions.

The hqdn3d stage runs on the **input (crop) size**, so the denoiser processes
~2.4 MP/frame, not the 0.13 MP output.

## 3. Encoder chain stage attribution (TestPerfEncodeBench)

600 real-window frames (output slots 2400-3000), generated content streamed
to stdin, mjpeg q6 into -f null; constant control at the median 2640x815:

| variant | varied feed | constant feed |
|---|---|---|
| format only (decode+ppm+mjpeg) | 5.9 s | 1.4 s |
| + hqdn3d | 8.5 s | — |
| + scale iw*2/3 lanczos | 16.3 s | — |
| **full chain** | **20.3 s** (x2 stable) | **3.2 s** |

- The two-stage lanczos pair is the dominant encoder stage on varied feed
  (+11.8 s over hqdn3d), and lanczos1 (big input, ~1.6 MP output) dominates
  the pair.
- **Variable-size feed tax: 6.3x** (20.3 vs 3.2 s). Per-frame PPM size
  changes make the image2pipe demuxer, every filter link and swscale
  re-realize their contexts 4639 times; even the format-only variant pays
  4.2x. This tax is intrinsic to a law whose crop rect varies frame to
  frame - reducing it (crop smoothing, size bucketing, padding crops to
  buckets) changes rendered pixels and needs an owner decision.

## 4. Go pump costs

- `io.ReadFull` of one 1080p rgb24 frame: 120 us (pure copy).
- `writePPMCrop` into the encoder stdin: the writer is the raw
  `exec.StdinPipe` and each crop row is one `Write` - ~950 syscalls per
  typical frame, ~5.3M across a sweetdevil render. Bench copy cost is
  17 us/frame; with pipe syscalls the estimate is ~1 ms/frame => ~4-6 s per
  full render, and it is pure overhead: wrapping `encIn` in a 1-4 MB
  `bufio.Writer` changes no bytes (the pipe receives the identical byte
  stream) and is the cheapest safe win in the pipeline.

## 5. The detect pass is the analysis bottleneck - and it is Go, not decoding

vidio is an ffmpeg-subprocess reader (`ffmpeg -f image2pipe -pix_fmt rgba`);
its decode is **0.7 ms/frame** (10 s for 13888 frames). The 172 s is all
`detectFrame` (TestPerfDetectSplit):

```
decode only:          0.7-0.8 ms/frame
decode + detectFrame: 18-20 ms/frame at load (12.4 ms/frame avg in the real cold run)
```

video_dsp.go / subject_detail.go are textbook Go: per-frame allocation of
mono+binary images, a `[][]int` label matrix (~6 MB), a DFS stack of
`image.Point`, `image.Rect` Union per labelled pixel, and a `map[int]`
lookup per labelled pixel - on the order of 10-15 MB/frame of garbage.

### Optimized replica (perf_test.go, proof-gated) - LANDED, see §9

`perfOptState.runFrame` is a bit-identical replica: same grayscale law
(`image/color.grayModel`'s 19595/38470/7471 + 1<<15 >>24 law - verified
against `draw.Draw`, which none of the common 8-bit luma laws match), same
Otsu arithmetic sequence, same hysteresis clamp, same scan order, same
neighbour-push order and label-on-pop semantics, same pool-claim loop.
Implemented on reused flat arrays (direct Pix indexing, `[]int32` label
matrix and stack, per-label stat slices).

- **Identity: full 13888-frame run, zero mismatches** on (bbox, subjects,
  centroid, lastThr) - every frame of the real cached analysis video, both
  chains fed the same decoded pixels through the same hysteresis chain
  (200- and 2000-frame runs beforehand also matched).
- **Speed on real captured frames (3 alternating rounds): 57.5 -> 25.0
  ms/frame, 2.30x**, stable.
- Projected: the 172 s detect phase drops to ~75 s; cold sweetdevil 449 ->
  ~350 s. A faster flood-fill (scanline or two-pass union-find) is possible
  but raises the proof burden; the replica keeps identical *execution*,
  not just identical outputs.

The old shipped `subjectDetailFromBinary` iterated a Go **map** (random
order) and sorted with an unstable comparator - Master/Subjects are already
order-insensitive outputs in practice, but the landed core closes that hole
as well: labels iterate in numeric order and the sort is stable with the
exact old comparator, so every measurement is deterministic run to run.

## 6. select-filter decode (measured, low priority)

For analysisFPS/renderFPS reduced to p/q, "frame n is played" is the
integer law `ceil(q*n/p)*p - q*n < q`, which matches `frameScheduleFor`
exactly (0 mismatches on both fixtures; 60->24 gives n mod 5 in {0,2}).
ffmpeg spelling (operator form, quoted for filter commas):

```
-vf select='lt(floor((2*n+4)/5)*5-2*n,2)' -fps_mode passthrough
```

D2 (above): pipe bytes 86.4 -> 34.6 GB, decode wall 23 -> 11 s. It does
**not** shorten the render (decode already finishes ~4x earlier than the
encoder drains it); it reduces kernel pipe/syscall pressure competing with
the encoder, so treat it as contention relief with a wrinkle: the law keeps
one extra tail frame (5556 vs the clamped schedule's 5555) that the pump
would simply not feed. The "decoded == records" stale-cache guard would
also need a rethink. Keep as a parking-lot idea.

## 7. Analysis-height lever (law decision)

CLI auto-analysis height is `max(outW, outH)` when > 480 (cmd/holocropper),
so a 648x200 output analyzes at 1152x648 - **1.82x the pixels** of the API
default 480 (854x480). Prepare scales with source decode + target area;
detect scales ~linearly with analysis area (172 s -> ~95 s projected at
480). Halving the analysis frame degrades bbox resolution, which feeds the
framing law - this is the owner's call, not an optimisation, but the cost
is quantified here.

## 8. Proposals, ranked

1. **Rewrite detect internals** (video_dsp.go + subject_detail.go hot
   paths) from the replica: zero output change (identity-proven), ~2.3x
   faster detect, ~100 s saved per cold sweetdevil-sized analysis; benefits
   every cold cache and preview. Land behind the golden parity gate.
   **LANDED 2026-11-27 - §9.**
2. **Buffer the pump writer** (`bufio` on `encIn` in runSourceRender):
   identical bytes, ~4-6 s per render, trivial diff.
   **LANDED 2026-11-27 - §9.**
3. **Parallel detect pipeline** (phase 2): grayscale+Otsu is frame-parallel
   with lookahead; the hysteresis chain is scalar; binarize+components+
   pools per frame are threshold-known in advance and can run N-up with
   ordered reassembly. Another 2-4x on top of (1); needs design review.
4. **Analysis-height policy** for the CLI: quantify bbox-quality impact of
   480 vs max(W,H) before defaulting down (biggest single cold-run lever
   after detect itself).
5. **select-filter decode**: proven-exact but ~0 wall win on the final
   render; adopt only if decoder/encoder CPU contention starts mattering.
6. **Reconfig tax**: 6.3x on variable feeds is real but inseparable from
   the current crop law; any mitigation changes pixels. Documented, not
   recommended.

## 9. Landed: proposals 1-2 (2026-11-27, owner greenlight)

Both changes are commits on this branch; main (`6f69792`) is untouched and
nothing here has been pushed.

**What landed**

- `video_dsp.go`: `detectState` carries one streaming pass's reusable
  gray/binary buffers and subject scan; `writeGray` writes
  `image/color.grayModel`'s exact 16-bit law inline for opaque frames
  (generic `draw.Draw` kept for frames with transparency);
  `otsuThreshold` / `applyThreshold` / `thresholdInto` walk `Pix` rows
  directly; `detectFrame` is the fresh-state wrapper one-shot callers (and
  the law's own tests) use. The old `connectedComponentLabeling` / `dfs` /
  `combineBounds` / `totalBoundingBox` are gone - no other callers.
- `subject_detail.go`: `subjectScan.measure(binaryImg, full)` is now the
  single measurement core behind both `subjectDetailFromBinary` (full mode:
  every island, pool and label the overlay layers draw) and
  `subjectsFromBinary` (summary mode: exactly the numbers the stream
  records). Flat `[]int32` labels + flood stack, per-label stat slices,
  label-order components, `sort.SliceStable` with the exact shipped
  comparator, exact pool-claim law - the same formulation the replica proved
  bit-identical.
- `analysis.go`: `runDetectStream` runs one reusable `detectState` over the
  whole analysis video.
- `linearrender.go`: the pump feeds the encoder through a 4 MB
  `bufio.Writer` (flushed before stdin close; a flush failure surfaces as
  `pumpErr`, the same broken pipe the unbuffered pump reported). The byte
  stream reaching the encoder is unchanged.

**Proof chain**

- replica == old shipped: 13888/13888 frames, 0 mismatches (pre-landing run,
  §5).
- landed shipped == replica: 13888/13888 frames, 0 mismatches on (bbox,
  subjects, centroid, lastThr) - `TestPerfDetectOptimizedIdentity` post-
  landing, now 96 s for BOTH chains over the full sweetdevil ah648 analysis.
  Together: the landed chain equals the old shipped chain on every frame.
- default suite green (52.5 s, zero skips).
- golden parity gate (`go test -tags e2e -run E2E`, both fixtures) green:
  stills 43.83-56.57 dB (gate 43), crop preview mean 35.58 / 41.71 dB,
  worst 33.18 (gate 33), MikV 37.83-43.04 dB (gate 37), analysis
  determinism framemd5-identical, all structural self-checks pass.

**Measured after landing**

- One-shot `detectFrame` - what `TestPerfDetectSplit` times; it re-allocates
  its scratch per call, the path one-shot callers use: **18-20 -> 7.7-7.9
  ms/frame** at 1152x648 under the usual background load. The streaming hot
  path is faster still: the e2e gate's cold full analysis of 13888
  sweetdevil frames at 854x480 took **32.0 s** (Prepare + Detect together),
  so the vidio-decode+detect stream runs well under 1.5 ms/frame. The §5
  projection (cold 449 -> ~350 s at ah648) looks conservative from these
  parts, though a full cold ah648 run was not re-timed end to end.
- Real captured frames, shipped vs replica in alternating rounds
  (`TestPerfDetectRealChain`, full-detail paths): 27.5-28.2 vs 25.1-25.2
  ms/frame - the old 2.3x gap has collapsed to noise, as expected now that
  both chains are the same formulation.
- Buffered pump on the cached-analysis CLI path (sweetdevil 648x200@24,
  ffmpeg wrapper): encoder wall **260.9 -> 198.0 s**, encoder user CPU
  **383 -> 358 s**. The user-CPU delta and the removed ~5.3M write syscalls
  are the change's own signature; part of the wall delta is ordinary
  load variance on this machine. The e2e gate's own sweetdevil render
  (fresh caches end to end) ran 5555 frames in 182.8 s.

Proposals 3-6 stay in the parking lot above, unchanged, awaiting owner
decisions.

## Reproduce

```
cd pkg/holocropper
go test -tags perf -run TestPerfGeometry -v .            # geometry CSVs + stats
go test -tags perf -run TestPerfEncodeBench -v .         # stage attribution + tax
go test -tags perf -run TestPerfDetectSplit -v .         # decode vs detect split
go test -tags perf -run TestPerfDetectRealChain -v .     # 2.3x on real frames
PERF_DETECT_N=13888 go test -tags perf -run TestPerfDetectOptimizedIdentity -v .
perf-tools/decode-bench.sh                               # D1/D2 (edit VIDEO path)
# cold-run attribution: run the CLI with perf-tools/ffmpeg-wrap as -ffmpeg-path
# and read per-subprocess wall/user/sys from the wrapper's logs/summary.log
```
