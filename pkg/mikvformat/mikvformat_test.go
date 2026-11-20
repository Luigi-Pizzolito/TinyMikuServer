package mikvformat

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// jpegFixture returns a tiny but complete JPEG image.
func jpegFixture(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 80), G: uint8(y * 80), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: DefaultJPEGQuality}); err != nil {
		t.Fatalf("jpeg fixture: %v", err)
	}
	return buf.Bytes()
}

func patternFill(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i%251)
	}
	return b
}

// TestDefaultEncodingsAreLegacyByteCompatible is the backwards-compatibility
// anchor: a writer with the encoding fields left at their zero value must
// produce exactly the original format - encoding 0/0 header tail, zlib
// DEFLATEd (byte-swapped) RGB565 video, fixed-length raw PCM audio.
func TestDefaultEncodingsAreLegacyByteCompatible(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.mikv")

	mikv := NewMikVFormat()
	mikv.FPS = 20
	mikv.Height = 8
	mikv.Width = 8
	mikv.SetAudioParams(16000, 1, 16)

	raw := patternFill(8*8*2, 0x10)
	pcm := patternFill(1600, 0x40)

	// Legacy quirk to preserve: AddFrame byte-swaps the caller's RGB565
	// buffer in place before DEFLATE.
	swapped := make([]byte, len(raw))
	for i := 0; i < len(raw); i += 2 {
		swapped[i], swapped[i+1] = raw[i+1], raw[i]
	}
	if err := mikv.AddFrame(raw, pcm); err != nil {
		t.Fatalf("AddFrame: %v", err)
	}
	if !bytes.Equal(raw, swapped) {
		t.Fatal("AddFrame no longer byte-swaps the RGB565 buffer in place (legacy behaviour must be preserved)")
	}

	if err := mikv.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	fileBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(fileBytes) < 16 {
		t.Fatalf("file shorter than header")
	}

	// Header: magic, fps, H, W, SR, ch, bits, then the three encoding-era
	// tail bytes - all zero for a legacy-default writer.
	wantHeader := []byte{
		'M', 'i', 'k', 'V', 20, 8, 0, 8, 0, 0x80, 0x3E, 01, 16,
		EncodingDeflateRGB565, EncodingRawPCM, 0x00,
	}
	if !bytes.Equal(fileBytes[:16], wantHeader) {
		t.Fatalf("header tail mismatch:\ngot  % X\nwant % X", fileBytes[:16], wantHeader)
	}

	// Frame layout: u32 videoLen | DEFLATE payload | u32 audioLen | PCM.
	off := 16
	videoLen := binary.LittleEndian.Uint32(fileBytes[off : off+4])
	off += 4
	payload := fileBytes[off : off+int(videoLen)]
	if len(payload) == 0 || payload[0] != 0x78 {
		t.Fatalf("video payload is not a zlib stream (first byte 0x%02X)", payload[0])
	}
	off += int(videoLen)
	audioLen := binary.LittleEndian.Uint32(fileBytes[off : off+4])
	off += 4
	if audioLen != 1600 {
		t.Fatalf("PCM audio chunk = %d bytes, want fixed 1600", audioLen)
	}
	if !bytes.Equal(fileBytes[off:off+1600], pcm) {
		t.Fatal("PCM payload altered on disk")
	}

	// Load back: metadata, encoding stamps, and the DEFLATE payload
	// inflating to the byte-swapped RGB565 the original writer produced.
	back := NewMikVFormat()
	if err := back.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if back.FPS != 20 || back.Height != 8 || back.Width != 8 ||
		back.AudioSampleRate != 16000 || back.AudioChannels != 1 || back.AudioBitsPerSample != 16 {
		t.Fatalf("metadata round-trip wrong: %+v", back.MikVFormatMetadata())
	}
	if back.VideoEncoding != EncodingDeflateRGB565 || back.AudioEncoding != EncodingRawPCM {
		t.Fatalf("default encodings = %d/%d, want 0/0", back.VideoEncoding, back.AudioEncoding)
	}
	if len(back.Frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(back.Frames))
	}
	f := back.Frames[0]
	if f.VideoEncoding != EncodingDeflateRGB565 || f.AudioEncoding != EncodingRawPCM {
		t.Fatalf("frame stamps = %d/%d, want 0/0", f.VideoEncoding, f.AudioEncoding)
	}
	zr, err := zlib.NewReader(bytes.NewReader(f.Video))
	if err != nil {
		t.Fatalf("zlib.NewReader: %v", err)
	}
	inflated, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("inflate: %v", err)
	}
	if !bytes.Equal(inflated, swapped) {
		t.Fatal("DEFLATE payload does not inflate to the byte-swapped RGB565 frame")
	}
}

// MikVFormatMetadata is a tiny test aid: print-safe metadata snapshot.
func (m *MikVFormat) MikVFormatMetadata() map[string]interface{} {
	return map[string]interface{}{
		"fps": m.FPS, "h": m.Height, "w": m.Width,
		"sr": m.AudioSampleRate, "ch": m.AudioChannels, "bits": m.AudioBitsPerSample,
	}
}

// TestHandWrittenLegacyFileLoads simulates a pre-encoding-ID file: header
// tail zeros, plain zlib (no sync-flush trick), original frame layout. The
// reader must load it unchanged.
func TestHandWrittenLegacyFileLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "handwritten.mikv")

	var buf bytes.Buffer
	hdr := []byte{'M', 'i', 'k', 'V', 20, 8, 0, 8, 0, 0x80, 0x3E, 1, 16, 0, 0, 0}
	buf.Write(hdr)

	raw := patternFill(8*8*2, 0x77)
	var deflated bytes.Buffer
	zw := zlib.NewWriter(&deflated) // plain Close, like a foreign writer
	zw.Write(raw)
	zw.Close()

	var frame bytes.Buffer
	binary.Write(&frame, binary.LittleEndian, uint32(deflated.Len()))
	frame.Write(deflated.Bytes())
	pcm := patternFill(1600, 0x02)
	binary.Write(&frame, binary.LittleEndian, uint32(len(pcm)))
	frame.Write(pcm)
	buf.Write(frame.Bytes())

	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	back := NewMikVFormat()
	if err := back.Load(path); err != nil {
		t.Fatalf("legacy file must load: %v", err)
	}
	if back.VideoEncoding != 0 || back.AudioEncoding != 0 {
		t.Fatalf("encodings read as %d/%d, want 0/0", back.VideoEncoding, back.AudioEncoding)
	}
	if len(back.Frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(back.Frames))
	}
	zr, _ := zlib.NewReader(bytes.NewReader(back.Frames[0].Video))
	inflated, _ := io.ReadAll(zr)
	if !bytes.Equal(inflated, raw) {
		t.Fatal("hand-written DEFLATE payload did not inflate to the original bytes")
	}
}

func TestJPEGEncodingRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jpeg.mikv")
	img := jpegFixture(t)

	mikv := NewMikVFormat()
	mikv.FPS = 24
	mikv.Height = 4
	mikv.Width = 4
	mikv.SetAudioParams(16000, 1, 16)
	mikv.VideoEncoding = EncodingJPEG

	pcm := patternFill(1600, 0x09)
	if err := mikv.AddFrame(img, pcm); err != nil {
		t.Fatalf("AddFrame: %v", err)
	}
	// JPEG frames are stored untouched - not byte-swapped, not deflated.
	if !bytes.Equal(mikv.Frames[0].Video, img) {
		t.Fatal("JPEG payload was altered by AddFrame")
	}
	if err := mikv.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	fileBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if fileBytes[13] != byte(EncodingJPEG) || fileBytes[14] != byte(EncodingRawPCM) || fileBytes[15] != 0 {
		t.Fatalf("header encoding bytes = % X, want 01 00 00", fileBytes[13:16])
	}
	if binary.LittleEndian.Uint32(fileBytes[16:20]) != uint32(len(img)) {
		t.Fatal("video length prefix wrong for JPEG frame")
	}

	back := NewMikVFormat()
	if err := back.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if back.VideoEncoding != EncodingJPEG {
		t.Fatalf("VideoEncoding = %d, want %d", back.VideoEncoding, EncodingJPEG)
	}
	f := back.Frames[0]
	if f.VideoEncoding != EncodingJPEG || f.AudioEncoding != EncodingRawPCM {
		t.Fatalf("frame stamps = %d/%d, want 1/0", f.VideoEncoding, f.AudioEncoding)
	}
	if !bytes.Equal(f.Video, img) {
		t.Fatal("JPEG payload not byte-identical through Save/Load")
	}
	decoded, _, err := image.Decode(bytes.NewReader(f.Video))
	if err != nil {
		t.Fatalf("payload does not decode as JPEG: %v", err)
	}
	if bounds := decoded.Bounds(); bounds.Dx() != 4 || bounds.Dy() != 4 {
		t.Fatalf("decoded JPEG = %dx%d, want 4x4", bounds.Dx(), bounds.Dy())
	}
}

func TestWriterRejectsUnknownEncodings(t *testing.T) {
	dir := t.TempDir()

	mikv := NewMikVFormat()
	mikv.FPS = 20
	mikv.Height = 8
	mikv.Width = 8
	mikv.SetAudioParams(16000, 1, 16)

	mikv.VideoEncoding = 0x02
	if err := mikv.AddFrame(patternFill(16, 1), patternFill(16, 2)); err == nil {
		t.Fatal("AddFrame accepted unknown video encoding 0x02")
	}
	mikv.VideoEncoding = 0
	mikv.AudioEncoding = 0x07
	if err := mikv.Save(filepath.Join(dir, "bad.mikv")); err == nil {
		t.Fatal("Save accepted unknown audio encoding 0x07")
	}
	mikv.AudioEncoding = 0

	// OGG-Opus requires the 48 kHz granule clock and 1-2 channels.
	mikv.AudioEncoding = EncodingOGGOpus
	mikv.SetAudioParams(44100, 1, 16)
	if err := mikv.Save(filepath.Join(dir, "bad2.mikv")); err == nil {
		t.Fatal("Save accepted OGG-Opus at 44100 Hz")
	}
	mikv.SetAudioParams(48000, 3, 16)
	if err := mikv.Save(filepath.Join(dir, "bad3.mikv")); err == nil {
		t.Fatal("Save accepted OGG-Opus with 3 channels")
	}
}

func TestReaderRejectsUnknownEncodings(t *testing.T) {
	dir := t.TempDir()

	mikv := NewMikVFormat()
	mikv.FPS = 20
	mikv.Height = 8
	mikv.Width = 8
	mikv.SetAudioParams(16000, 1, 16)
	if err := mikv.AddFrame(patternFill(16, 1), patternFill(32, 2)); err != nil {
		t.Fatal(err)
	}

	patch := func(name string, idx int, val byte) string {
		p := filepath.Join(dir, name)
		if err := mikv.Save(p); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		b[idx] = val
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	if err := NewMikVFormat().Load(patch("badvideo.mikv", 13, 0x02)); err == nil {
		t.Fatal("Load accepted video encoding 0x02")
	} else if !bytes.Contains([]byte(err.Error()), []byte("video encoding")) {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := NewMikVFormat().Load(patch("badaudio.mikv", 14, 0x09)); err == nil {
		t.Fatal("Load accepted audio encoding 0x09")
	} else if !bytes.Contains([]byte(err.Error()), []byte("audio encoding")) {
		t.Fatalf("unexpected error: %v", err)
	}

	// Reserved byte 15 is not an encoding: any value stays loadable and is
	// not re-emitted (always written back as zero).
	p := patch("reserved.mikv", 15, 0xAB)
	back := NewMikVFormat()
	if err := back.Load(p); err != nil {
		t.Fatalf("reserved byte must not affect loading: %v", err)
	}
	if err := back.Save(p + ".resave"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p + ".resave")
	if b[15] != 0 {
		t.Fatalf("reserved byte re-emitted as 0x%02X, want 0x00", b[15])
	}
}

// TestConcurrentSaveLoad guards the header data race fix: Load/Save must not
// touch the package-level MikVHeader template (run this under -race).
func TestConcurrentSaveLoad(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			mikv := NewMikVFormat()
			mikv.FPS = uint8(10 + i)
			mikv.Height = 8
			mikv.Width = 8
			mikv.SetAudioParams(16000, 1, 16)
			mikv.AddFrame(patternFill(64, byte(i)), patternFill(64, byte(i*2)))
			p := filepath.Join(dir, fmt.Sprintf("f%d.mikv", i))
			if err := mikv.Save(p); err != nil {
				errs <- err
				return
			}
			back := NewMikVFormat()
			if err := back.Load(p); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Save/Load: %v", err)
	}
}
