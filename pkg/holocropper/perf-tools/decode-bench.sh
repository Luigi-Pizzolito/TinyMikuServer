#!/bin/bash
# Decode-side benches for the perf/holocropper-profile branch (encoder-side
# stage attribution lives in TestPerfEncodeBench). Sequential on purpose -
# the machine already carries the owner's workloads. Each bench runs twice
# (pass 1 warms the page cache); TIMEFORMAT collects wall/user/sys.
set -u
FF=/usr/sbin/ffmpeg
LOG=/tmp/opencode/ffprof/logs/bench.log
VIDEO=/home/luigipizzolito/Documents/GitHub/TinyMikuServer/vids/sweetdevil.mp4
mkdir -p /tmp/opencode/ffprof/logs

# srcIdx law for 60->24: kept iff ceil(2n/5)*5 - 2n < 2  (integer-safe doubles)
SEL="select='lt(floor((2*n+4)/5)*5-2*n,2)'"
FRAME_BYTES=$((1920*1080*3))

run() { # label, then argv...
  local label=$1; shift
  echo "=== $label" >> "$LOG"
  TIMEFORMAT='wall=%R user=%U sys=%S'
  { time "$@" ; } 2>> "$LOG"
}

# D1: full decode, exactly the pump's decoder argv, pipe bytes counted by wc
run 'D1 decode-full pass1' bash -c "$FF -hide_banner -loglevel error -i $VIDEO -map 0:v:0 -fps_mode passthrough -f rawvideo -pix_fmt rgb24 pipe:1 | wc -c"
run 'D1 decode-full pass2' bash -c "$FF -hide_banner -loglevel error -i $VIDEO -map 0:v:0 -fps_mode passthrough -f rawvideo -pix_fmt rgb24 pipe:1 | wc -c"

# D2: same decode work, select law applied: 5555/13888 frames through the
# pipe (byte count / FRAME_BYTES verifies the frame count)
run 'D2 decode-select pass1' bash -c "$FF -hide_banner -loglevel error -i $VIDEO -map 0:v:0 -vf \"$SEL\" -fps_mode passthrough -f rawvideo -pix_fmt rgb24 pipe:1 | wc -c"
run 'D2 decode-select pass2' bash -c "$FF -hide_banner -loglevel error -i $VIDEO -map 0:v:0 -vf \"$SEL\" -fps_mode passthrough -f rawvideo -pix_fmt rgb24 pipe:1 | wc -c"

# D3: frame-count proof: same select stream counted with md5sum of a prefix
# comparison is done separately; here just report the byte count cleanly.
echo "FRAME_BYTES=$FRAME_BYTES" >> "$LOG"
echo BENCH_DONE >> "$LOG"
