package mikvformat

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// ---------------------------------------------------------------------------
// Synthetic OGG-Opus fixtures built with the package's own page encoder, so
// split/join behaviour is tested independently of any encoder binary.

func opusHeadFixture() []byte {
	h := make([]byte, 19)
	copy(h, "OpusHead")
	h[8] = 1 // version
	h[9] = 1 // channels
	binary.LittleEndian.PutUint16(h[10:], 312)
	binary.LittleEndian.PutUint32(h[12:], 48000)
	return h
}

func opusTagsFixture() []byte {
	b := make([]byte, 0, 24)
	b = append(b, "OpusTags"...)
	b = append(b, 4, 0, 0, 0)
	b = append(b, "mikv"...)
	b = append(b, 0, 0, 0, 0) // no user comments, no framing bit
	return b
}

func fakeOpusPackets(count, size int) [][]byte {
	pkts := make([][]byte, count)
	for i := range pkts {
		p := make([]byte, size)
		p[0] = 0x01 // TOC: frame 0 config... bytes are opaque to the splitter
		for j := 1; j < size; j++ {
			p[j] = byte((i*31 + j*7) % 251)
		}
		pkts[i] = p
	}
	return pkts
}

// synthOGG assembles a valid Ogg Opus stream: OpusHead + OpusTags pages,
// then perPage packets per audio page on the uniform granule grid
// base+(packet index+1)*span. tailSlack shortens the final page's granule
// (the encoder flush); badGran overrides an audio page's granule to plant
// corruption tests.
func synthOGG(span int, base int64, perPage int, pkts [][]byte, tailSlack int64, badGran map[int]int64) []byte {
	const serial = 7
	var out []byte
	head, tags := opusHeadFixture(), opusTagsFixture()
	out = append(out, encodeOggPage(serial, 0, 0x02, 0, []byte{byte(len(head))}, head)...)
	out = append(out, encodeOggPage(serial, 1, 0, 0, []byte{byte(len(tags))}, tags)...)
	seq := uint32(2)
	audioPage := 0
	total := len(pkts)
	for start := 0; start < total; start += perPage {
		endIdx := start + perPage
		if endIdx > total {
			endIdx = total
		}
		var segs, body []byte
		for _, p := range pkts[start:endIdx] {
			for n := len(p); n >= 255; n -= 255 {
				segs = append(segs, 255)
			}
			segs = append(segs, byte(len(p)%255))
			body = append(body, p...)
		}
		if len(segs) > 255 {
			panic("synthOGG: page segment table overflow (pick a smaller perPage)")
		}
		granule := base + int64(endIdx)*int64(span)
		flags := byte(0)
		if endIdx == total {
			granule -= tailSlack
			flags = 0x04
		}
		if g, ok := badGran[audioPage]; ok {
			granule = g
		}
		out = append(out, encodeOggPage(serial, seq, flags, granule, segs, body)...)
		seq++
		audioPage++
	}
	return out
}

// splitAndVerify splits ogg and checks the joined chunks carry every packet
// byte for byte, in order, and that chunk 0 starts with the untouched
// header pages.
func splitAndVerify(t *testing.T, ogg []byte, fps uint8, span int, frames int, wantPackets int) [][]byte {
	t.Helper()
	chunks, err := SplitOGGOpus(ogg, fps, frames)
	if err != nil {
		t.Fatalf("SplitOGGOpus: %v", err)
	}
	if len(chunks) != frames {
		t.Fatalf("chunks = %d, want %d", len(chunks), frames)
	}
	header, _, _, pkts, err := parseOGGOpus(ogg, span)
	if err != nil {
		t.Fatalf("parse source: %v", err)
	}
	if len(pkts) != wantPackets {
		t.Fatalf("source parse: %d packets, want %d", len(pkts), wantPackets)
	}
	joined := JoinOGGChunks(chunks)
	if !bytes.HasPrefix(joined, header) {
		t.Fatal("chunk 0 does not start with the byte-identical header pages")
	}
	_, _, _, pktsBack, err := parseOGGOpus(joined, span)
	if err != nil {
		t.Fatalf("parse joined stream: %v", err)
	}
	if len(pktsBack) != wantPackets {
		t.Fatalf("joined stream has %d packets, want %d", len(pktsBack), wantPackets)
	}
	for i := range pktsBack {
		if !bytes.Equal(pktsBack[i].data, pkts[i].data) {
			t.Fatalf("packet %d altered through split/join", i)
		}
	}
	return chunks
}

// countTermSegs counts terminating (< 255) segments in a chunk's pages -
// each completes exactly one packet (header packets included).
func countTermSegs(chunk []byte) int {
	n := 0
	for off := 0; off+27 <= len(chunk); {
		nseg := int(chunk[off+26])
		segs := chunk[off+27 : off+27+nseg]
		bodyLen := 0
		for _, v := range segs {
			bodyLen += int(v)
			if v < 255 {
				n++
			}
		}
		off += 27 + nseg + bodyLen
	}
	return n
}

// ---------------------------------------------------------------------------

func TestOGGOpusPacketSpanTable(t *testing.T) {
	cases := []struct {
		fps  uint8
		span int
	}{
		{24, 1920}, {25, 1920}, {20, 1920}, {16, 2880}, {30, 960},
		{48, 960}, {50, 960}, {60, 480}, {100, 480}, {120, 240}, {240, 120},
	}
	for _, c := range cases {
		span, err := OGGOpusPacketSpan(c.fps)
		if err != nil {
			t.Errorf("fps %d: %v", c.fps, err)
			continue
		}
		if span != c.span {
			t.Errorf("fps %d: span %d, want %d", c.fps, span, c.span)
		}
		ms, err := OGGOpusFrameDurationMs(c.fps)
		if err != nil || ms != float64(c.span)/48.0 {
			t.Errorf("fps %d: frame duration %.4f (err %v), want %.4f", c.fps, ms, err, float64(c.span)/48.0)
		}
	}
	for _, fps := range []uint8{0, 7} {
		if _, err := OGGOpusPacketSpan(fps); err == nil {
			t.Errorf("fps %d should be rejected", fps)
		}
	}
}

func TestOGGCRC32KnownVector(t *testing.T) {
	// Canonical Ogg CRC: poly 0x04C11DB7, init 0, no reflection, no final
	// xor. Cross-checked against the CRC-32/POSIX catalogue check value
	// 0x765E7680 without its final xor (0x765E7680^0xFFFFFFFF = 0x89A1897F)
	// and a table-free bit-by-bit implementation of the same parameters.
	if got := oggCRC32([]byte("123456789")); got != 0x89A1897F {
		t.Fatalf("oggCRC32(\"123456789\") = 0x%08X, want 0x89A1897F", got)
	}
}

func TestSplitJoinRoundTripSynthetic(t *testing.T) {
	pkts := fakeOpusPackets(48, 100)
	ogg := synthOGG(1920, 0, 12, pkts, 0, nil) // 4 pages x 12 packets, clean grid
	chunks := splitAndVerify(t, ogg, 24, 1920, 48, 48)

	// The 1920-granule packets over 2000-granule windows distribute unevenly.
	// Chunk 0 carries the two header pages, so its header packets are not
	// audio.
	empty, doubled, total := 0, 0, 0
	for i, c := range chunks {
		n := countTermSegs(c)
		if i == 0 {
			n -= 2 // OpusHead + OpusTags
		}
		if n < 0 {
			t.Fatalf("chunk 0 lost the header pages")
		}
		total += n
		switch n {
		case 0:
			empty++
		case 2:
			doubled++
		}
	}
	if total != 48 {
		t.Fatalf("packet total across chunks = %d, want 48", total)
	}
	if empty == 0 || doubled == 0 {
		t.Fatalf("expected an uneven 24 fps schedule (empty + doubled frames); got %d empty, %d doubled", empty, doubled)
	}
}

func TestSplitPrerollConvention(t *testing.T) {
	// A strict encoder that bakes an 80 ms preroll (3840 granules) into the
	// granule timeline must still be accepted; the schedule just shifts.
	pkts := fakeOpusPackets(24, 90)
	ogg := synthOGG(1920, 3840, 8, pkts, 0, nil)
	chunks := splitAndVerify(t, ogg, 24, 1920, 24, 24)

	header, _, _, _, err := parseOGGOpus(ogg, 1920)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks[0]) != len(header) {
		t.Fatalf("chunk 0 holds %d bytes, want headers only (%d): preroll should leave frame 0 silent", len(chunks[0]), len(header))
	}
}

func TestSplitUniformityRejections(t *testing.T) {
	pkts := fakeOpusPackets(24, 90)

	// A middle page that ends off the uniform grid (real-world cause: the
	// ffmpeg -frame_duration did not match the fps).
	bad := synthOGG(1920, 0, 8, pkts, 0, map[int]int64{1: 8*1920 + 1000})
	if _, err := SplitOGGOpus(bad, 24, 24); err == nil || !bytes.Contains([]byte(err.Error()), []byte("uniform grid")) {
		t.Fatalf("off-grid middle page: got err %v", err)
	}

	// The final page may not overshoot the grid either.
	over := synthOGG(1920, 0, 8, pkts, -100, nil)
	if _, err := SplitOGGOpus(over, 24, 24); err == nil {
		t.Fatal("final page overshoot accepted")
	}

	// A stream encoded with 20 ms packets while the fps expects 40 ms.
	mismatch := synthOGG(960, 0, 8, pkts, 0, nil)
	if _, err := SplitOGGOpus(mismatch, 24, 24); err == nil {
		t.Fatal("wrong packet duration stream accepted")
	}

	// A preroll beyond one second is nonsense and must fail loudly.
	absurd := synthOGG(1920, 60000, 8, pkts, 0, nil)
	if _, err := SplitOGGOpus(absurd, 24, 24); err == nil {
		t.Fatal("1.25 s preroll accepted")
	}
}

func TestSplitCorruptionRejections(t *testing.T) {
	pkts := fakeOpusPackets(24, 90)

	// Corrupted payload breaks the page CRC.
	corrupt := synthOGG(1920, 0, 8, pkts, 0, nil)
	corrupt[len(corrupt)-5] ^= 0xFF
	if _, err := SplitOGGOpus(corrupt, 24, 24); err == nil || !bytes.Contains([]byte(err.Error()), []byte("CRC")) {
		t.Fatalf("corrupted body: got err %v", err)
	}

	// Truncated stream.
	if _, err := SplitOGGOpus(corrupt[:len(corrupt)-10], 24, 24); err == nil {
		t.Fatal("truncated stream accepted")
	}

	// A second serial no. means a second logical stream.
	multi := synthOGG(1920, 0, 8, pkts, 0, nil)
	off := 27 + 1 + len(opusHeadFixture()) // end of the OpusHead page
	second := binary.LittleEndian.Uint32(multi[off+14 : off+18])
	binary.LittleEndian.PutUint32(multi[off+14:off+18], second+1)
	if _, err := SplitOGGOpus(multi, 24, 24); err == nil {
		t.Fatal("multiple logical streams accepted")
	}

	// First page must be OpusHead (a Vorbis stream is not MikV opus audio).
	notOpus := encodeOggPage(3, 0, 0x02, 0, []byte{byte(len(opusTagsFixture()))}, opusTagsFixture())
	if _, err := SplitOGGOpus(notOpus, 24, 1); err == nil || !bytes.Contains([]byte(err.Error()), []byte("OpusHead")) {
		t.Fatalf("non-OpusHead first page: got err %v", err)
	}

	// A packet left uncompleted at EOF.
	dangling := encodeOggPage(3, 0, 0x02, 0, []byte{1}, opusHeadFixture())
	dangling = append(dangling, encodeOggPage(3, 1, 0, 1920, []byte{255}, make([]byte, 255))...)
	if _, err := SplitOGGOpus(dangling, 24, 1); err == nil {
		t.Fatal("dangling continuation accepted")
	}
}

func TestSplitFrameCountClamping(t *testing.T) {
	pkts := fakeOpusPackets(40, 80)
	ogg := synthOGG(1920, 0, 10, pkts, 0, nil)

	// More audio than video: the tail clamps into the last chunk.
	splitAndVerify(t, ogg, 24, 1920, 10, 40)

	// More video than audio: trailing chunks stay empty (valid MikV: the
	// frame keeps a zero-length audio chunk).
	chunks, err := SplitOGGOpus(ogg, 24, 100)
	if err != nil {
		t.Fatal(err)
	}
	for i := 50; i < 100; i++ {
		if len(chunks[i]) != 0 {
			t.Fatalf("chunk %d should be empty silence, got %d bytes", i, len(chunks[i]))
		}
	}
}

func TestSplitMultiPageFrame(t *testing.T) {
	// 130 x 255-byte packets (2 lacing segments each) clamped into a single
	// frame overflow the 255-entry segment table; buildOggPageRun must
	// split into multiple pages without ever cutting a packet.
	pkts := fakeOpusPackets(130, 255)
	ogg := synthOGG(1920, 0, 120, pkts, 0, nil)
	chunks := splitAndVerify(t, ogg, 24, 1920, 1, 130)
	if n := bytes.Count(chunks[0], []byte("OggS")); n < 3 { // headers + >=2 audio pages
		t.Fatalf("chunk 0 has %d pages, want header pages plus a split audio run", n)
	}
}

func TestJoinOGGChunks(t *testing.T) {
	got := JoinOGGChunks([][]byte{[]byte("ab"), nil, []byte("cd")})
	if string(got) != "abcd" {
		t.Fatalf("JoinOGGChunks = %q, want \"abcd\"", got)
	}
}

// ---------------------------------------------------------------------------
// Real-encoder tests: these need ffmpeg on PATH and run with the strict
// gate in CI (HOLOCROPPER_REQUIRE_FFMPEG).

func requireFFmpeg(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		if os.Getenv("HOLOCROPPER_REQUIRE_FFMPEG") != "" {
			t.Fatalf("ffmpeg not on PATH, and HOLOCROPPER_REQUIRE_FFMPEG is set: the Opus tests must run, not skip")
		}
		t.Skip("OGG-Opus encoder tests need ffmpeg on PATH")
	}
	return path
}

// encodeRealOpus produces the exact stream MikV's opus path feeds through
// SplitOGGOpus: ffmpeg's own ogg+libopus output at the frame duration the
// fps selects.
func encodeRealOpus(t *testing.T, ffmpeg string, seconds int) []byte {
	t.Helper()
	frameDur, err := OGGOpusFrameDurationMs(24)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:duration=%d", seconds),
		"-ar", "48000", "-ac", "1", "-c:a", "libopus",
		"-frame_duration", strconv.FormatFloat(frameDur, 'f', -1, 64),
		"-b:a", "32k", "-f", "ogg", "pipe:1")
	ogg, err := cmd.Output()
	if err != nil {
		t.Fatalf("libopus encode: %v", err)
	}
	return ogg
}

func TestSplitRealFFmpegOpusStream(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	ogg := encodeRealOpus(t, ffmpeg, 2)

	// The real stream: ffmpeg cancels the libopus delay out of the pts, so
	// granules run clean and the final page lands short - exactly the
	// slack SplitOGGOpus must accept.
	chunks := splitAndVerify(t, ogg, 24, 1920, 48, 51)

	// The joined chunks must be a complete .ogg ffmpeg itself accepts -
	// this is the guarantee the firmware's ogg_pages/opus decoder relies on.
	dir := t.TempDir()
	joined := filepath.Join(dir, "joined.ogg")
	if err := os.WriteFile(joined, JoinOGGChunks(chunks), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-i", joined, "-f", "null", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg rejects the joined stream: %v\n%s", err, out)
	}
}

func TestMikVOpusContainerRoundTrip(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	dir := t.TempDir()

	// A tiny 2 s / 24 fps clip with audio.
	src := filepath.Join(dir, "src.mp4")
	out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "color=c=black:s=16x16:d=2:r=24",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", "-shortest", src).CombinedOutput()
	if err != nil {
		t.Fatalf("fixture encode: %v\n%s", err, out)
	}

	mikv := NewMikVFormat()
	mikv.FPS = 24
	mikv.Width, mikv.Height = 16, 16
	mikv.SetAudioParams(48000, 1, 16)
	mikv.AudioEncoding = EncodingOGGOpus
	mikv.OpusBitrateKbps = 32
	if err := mikv.LoadFromMP4(src); err != nil {
		t.Fatalf("LoadFromMP4 opus: %v", err)
	}
	if len(mikv.Frames) != 48 {
		t.Fatalf("frames = %d, want 48", len(mikv.Frames))
	}

	path := filepath.Join(dir, "opus.mikv")
	if err := mikv.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fileBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if fileBytes[14] != byte(EncodingOGGOpus) {
		t.Fatalf("header audio encoding = 0x%02X, want 0x%02X", fileBytes[14], EncodingOGGOpus)
	}

	back := NewMikVFormat()
	if err := back.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if back.AudioEncoding != EncodingOGGOpus || back.AudioSampleRate != 48000 {
		t.Fatalf("loaded audio params %d/%d Hz", back.AudioEncoding, back.AudioSampleRate)
	}

	chunks := make([][]byte, len(back.Frames))
	for i, f := range back.Frames {
		if f.AudioEncoding != EncodingOGGOpus {
			t.Fatalf("frame %d audio stamp %d", i, f.AudioEncoding)
		}
		chunks[i] = f.Audio
	}

	joinedDir := t.TempDir()
	joined := filepath.Join(joinedDir, "joined.ogg")
	if err := os.WriteFile(joined, JoinOGGChunks(chunks), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-i", joined, "-f", "null", "-").CombinedOutput(); err != nil {
		t.Fatalf("MikV opus chunks do not reassemble into a stream ffmpeg accepts: %v\n%s", err, out)
	}

	// Video stayed the legacy DEFLATE path, audio chunks are variable-length.
	if back.Frames[0].VideoEncoding != EncodingDeflateRGB565 {
		t.Fatalf("video encoding = %d, want the DEFLATE default", back.Frames[0].VideoEncoding)
	}
}

func TestLoadFromMP4RejectsBadOpusParams(t *testing.T) {
	requireFFmpeg(t)
	mikv := NewMikVFormat()
	mikv.FPS = 24
	mikv.Width, mikv.Height = 16, 16
	mikv.SetAudioParams(44100, 1, 16)
	mikv.AudioEncoding = EncodingOGGOpus
	if err := mikv.LoadFromMP4("does-not-matter.mp4"); err == nil || !bytes.Contains([]byte(err.Error()), []byte("48000")) {
		t.Fatalf("44100 Hz opus request: got err %v", err)
	}
}
