package mikvformat

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"os"
	"os/exec"
	"strconv"
	"time"

	vidio "github.com/AlexEidt/Vidio"
	"github.com/schollz/progressbar/v3"
)

/**
 * MikV Binary Format Specification
 * --------------------------------
 * The MikV file format is a simple custom binary format designed for storing
 * synchronised video and audio frames. Each file consists of a **header**
 * containing metadata followed by a sequence of **video/audio** frames.
 * The MikVFormat struct provides methods for loading and saving MikV files,
 * methods for creating a new MikVformat from scratch, from an MP4 file,
 *  and a method for streaming frames to a MikVFrame channel in real-time.
 *
 * The default metadata and format recommendations are as follows:
 * - Frames per second (FPS): 20
 * - Video frame height: 240
 * - Video frame width: 240
 * - Audio sample rate: 16000
 * - Audio channels: 1 (mono)
 * - Audio bits per sample: 16
 * This means that each video frame will be a variable 240x240x2=115200 bytes DEFLATE compressed,
 * and each audio frame will be a fixed (16000/20)*2=1600 bytes.
 *
 * File Structure:
 * ---------------
 * [HEADER] -> [FRAME 1] -> [FRAME 2] -> ... -> [FRAME N]
 *
 * 1. HEADER (16 bytes total)
 * --------------------------
 * The first 16 bytes of the file contain essential metadata for decoding:
 * - Offset 0 (4 bytes):	Magic number "MikV" (0x4D 0x69 0x6B 0x56)	[ [4]byte ]
 * - Offset 4 (1 byte): 	Frames per second (FPS)						[ uint8 ]
 * - Offset 5 (2 bytes):	Video frame height							[ uint16 ]
 * - Offset 7 (2 bytes):	Video frame width							[ uint16 ]
 * - Offset 9 (2 bytes):	Audio sample rate							[ uint16 ]
 * - Offset 11 (1 byte):	Audio channels								[ uint8 ]
 * - Offset 12 (1 byte):	Audio bits per sample						[ uint8 ]
 * - Offset 13 (1 byte):	Video encoding ID							[ uint8 ]
 * - Offset 14 (1 byte):	Audio encoding ID							[ uint8 ]
 * - Offset 15 (1 byte):	Reserved (padding)							[ uint8 ]
 *
 * Video encoding (header byte 13):
 * - 0x00	EncodingDeflateRGB565: DEFLATE-compressed RGB565 frame data.
 *		This byte was reserved padding in the original spec and every
 *		writer until then stored 0 there, so 0x00 is the legacy default
 *		by construction: pre-existing files stay valid unchanged.
 * - 0x01	EncodingJPEG: the video frame payload is one complete JPEG
 *		image, stored raw (no DEFLATE layer) - effectively MJPEG inside
 *		MikV (the frame data the holocropper render passes already
 *		produce, packed without a re-encode).
 * Audio encoding (header byte 14):
 * - 0x00	EncodingRawPCM: raw interleaved PCM audio frames - the original
 *		fixed-length chunks described below.
 * - 0x01	EncodingOGGOpus: the audio track is one OGG-Opus stream cut at
 *		Ogg page boundaries into per-video-frame segments. Chunk k holds
 *		the complete Ogg pages whose packets finish within frame k's time
 *		window (48000/FPS samples, in Ogg granule units); chunks may be
 *		empty (codec preroll, or a page that covers several frames), and
 *		whatever completes past the last frame window is dropped, so the
 *		audio track is exactly as long as the frames. Concatenating every
 *		audio chunk in file order therefore reproduces a valid .ogg - a
 *		decoder that already has an Ogg demuxer consumes the chunks
 *		sequentially as a plain byte stream. Opus packet duration is
 *		picked for the fps (40 ms at 24 fps), the stream's OpusHead is
 *		authoritative for sample rate and channels, and the header audio
 *		fields carry the nominal values written (48000 Hz for this
 *		encoding).
 * A reader MUST reject a file whose encoding ID is neither 0x00 nor 0x01
 * rather than misread its frames. Files written with 0x00/0x00 are byte-
 * compatible with the original format: header, frame layout and payloads
 * are exactly what the original writer produced.
 *
 * 2. FRAME (variable size)
 * ------------------------
 * Each frame consists of a **video** frame followed by an **audio** chunk:
 * - Offset 0 (4 bytes):		Video frame length (payload)					[ uint32 ]
 * - Offset 4 (N bytes):		Video frame payload (per the header encoding ID: DEFLATE RGB565 for 0x00, raw JPEG for 0x01)	[ []byte ]
 * - Offset 4+N (4 bytes):		Audio frame length (PCM: fixed; OGG-Opus: variable)	[ uint32 ]
 * - Offset 4+N+4 (M bytes):	Audio frame payload (PCM, or Ogg page segments - see the encoding legend)	[ []byte ]
 * Where:
 * - N = Video frame length (compressed)
 * - M = Audio frame length (uncompressed)
 *
 * Example Binary Layout:
 * ----------------------
 *  0x0000:  4D 69 6B 56 14 F0 00 F0 00 80 3E 01 10 00 00 00  (Header: "MikV", FPS=20, 240x240, 16kHz, 1ch, 16-bit, video encoding 0x00, audio encoding 0x00, reserved 0x00)
 *  0x0010:  70 2F 00 00 [...12144 bytes compressed video data...] 40 06 00 00 [...1600 bytes PCM audio data...]
 *  0x4000:  EC 2E 00 00 [...12012 bytes compressed video data...] 40 06 00 00 [...1600 bytes PCM audio data...]
 *  0x8000:  28 2D 00 00 [...11560 bytes compressed video data...] 40 06 00 00 [...1600 bytes PCM audio data...]
 *  ...
 *
 * Notes:
 * - The video frame payload depends on header byte 13. Encoding 0x00: DEFLATE
 *   compressed RGB565 data (uint16/2bytes per pixel); the bytes are stored
 *   byte-swapped (high byte first) before DEFLATE, exactly as the original
 *   writer produced them. Encoding 0x01: one complete JPEG image per frame,
 *   stored raw.
 * - The audio frame payload depends on header byte 14. Encoding 0x00: raw PCM
 *   data (int16/2bytes per sample), fixed length:
 * - The audio frame length is fixed and depends on the audio sample rate, channels, and bits per sample.
 * - The audio frame length is calculated as (AudioSampleRate/FPS)*AudioChannels*AudioBitsPerSample/8.
 *   Encoding 0x01: OGG-Opus page segments (see the audio encoding legend);
 *   the chunk length is variable and the uint32 prefix already allows it.
 * - The video frame length is variable and depends on the video frame size and compression ratio.
 * - ZLib's DEFLATE implementation is used, not libdeflate or other compression libraries.
 * - All fields are saved in LittleEndian byte order.
 * - Header byte 13 is the video encoding ID and byte 14 the audio encoding
 *   ID (see the encoding legends above); byte 15 is reserved padding and is
 *   always written as zero.
 * - The magic number "MikV" is used to identify the file format.
 * - The audio frame data is interleaved for multiple channels.
 * - Audio data for encoding 0x00 is 16-bit signed PCM; encoding 0x01 stores
 *   the compressed OGG-Opus page segments described in the audio legend.
 * - The container's sound is exactly as long as its frames. Encoding 0x00 cuts
 *   the track to (AudioSampleRate/FPS) samples per frame; encoding 0x01 keeps
 *   only the packets that complete inside a frame window and drops what runs
 *   past the last frame. A file therefore never plays audio with no picture to
 *   play it against, and a shorter joined .ogg is the expected result when the
 *   source's track outlived its video.
 * - Reading is strict: a file that ends exactly at a frame boundary is a
 *   complete container, and one that ends INSIDE a frame record is a partial
 *   product that a reader must refuse (mikvformat.ErrTruncatedRecord) rather
 *   than report as a shorter valid file. A declared payload length bigger than
 *   the rest of the file is the same refusal.
 *
 * Example Usage:
 * -------------
 * Load a MikV file:
 * 	mikv := NewMikVFormat()
 * 	if err:=mikv.Load("my_video.mikv") ;err != nil {
 * 		fmt.Println("Error loading file:", err)
 * 	}
 *
 * Save a MikV file:
 * 	mikv := NewMikVFormat()
 * 	// Add video/audio frames to mikv.Frames
 * 	if err:=mikv.Save("my_video.mikv") ;err != nil {
 * 		fmt.Println("Error saving file:", err)
 * 	}
 *
 * Create a new MikV object and push image and audio frames:
 * 	mikv := NewMikVFormat()
 *  mikv.FPS = 20
 *  mikv.Height = 240
 *  mikv.Width = 240
 *  mikv.AudioSampleRate = 16000
 *  mikv.AudioChannels = 1
 *  mikv.AudioBitsPerSample = 16
 *  // Add one frame
 *	videoFrame := []byte{...} // Raw video frame data as RGB565
 *	audioFrame := []byte{...} // Raw audio frame data as PCM
 *  mikv.AddFrame(videoFrame, audioFrame)
 *
 * Create a new MikV object from an MP4 file:
 * 	mikv := NewMikVFormat()
 *  mikv.SetAudioParams(16000, 1, 16)
 * 	if err:=mikv.LoadFromMP4("my_video.mp4") ;err != nil {
 * 		fmt.Println("Error loading MP4 file:", err)
 * 	}
 *
 * Stream a MikV file to a MikVFrame channel:
 *	frameChan := make(chan MikVFrame)
 * 	go mikv.Stream(frameChan, nil)
 *  // Receive frames from channel
 * 	for frame := range frameChan {
 *		fmt.Printf("Received Frame, Video: %d bytes, Audio: %d bytes\n", len(frame.Video), len(frame.Audio))
 * 	}
 *
 */

// Video encoding IDs, stored in header byte 13 (see the format spec above).
const (
	// EncodingDeflateRGB565 (0x00): zlib-DEFLATEd RGB565 frame data.
	// The legacy default - the byte was reserved padding in the original
	// spec and old writers always stored 0 there, so old files are
	// encoding-0 files by construction.
	EncodingDeflateRGB565 uint8 = 0x00

	// EncodingJPEG (0x01): one complete JPEG image per video frame, stored
	// raw with no DEFLATE layer - effectively MJPEG inside MikV.
	EncodingJPEG uint8 = 0x01
)

// Audio encoding IDs, stored in header byte 14 (same reserved-padding
// origin story as byte 13: 0x00 is the legacy encoding by construction).
const (
	// EncodingRawPCM (0x00): raw interleaved PCM audio frames - the legacy
	// default, fixed-length as the original spec describes.
	EncodingRawPCM uint8 = 0x00

	// EncodingOGGOpus (0x01): the audio track is one OGG-Opus stream, cut
	// at page boundaries into per-video-frame segments. Each frame's audio
	// chunk holds whole Ogg pages (possibly none); packets that complete
	// past the last frame window are dropped, so concatenating every audio
	// chunk in file order reproduces a valid .ogg as long as the frames.
	// Chunk lengths are therefore variable (the uint32 prefix already
	// allows it) and the Ogg stream itself (OpusHead) is authoritative for
	// sample rate/channels - the header audio fields stay filled with the
	// nominal values used when writing.
	EncodingOGGOpus uint8 = 0x01
)

// DefaultJPEGQuality is used for JPEG-encoded frames when
// MikVFormat.JPEGQuality is unset.
const DefaultJPEGQuality = 90

// Frame represents a single video and audio frame
type MikVFrame struct {
	Video []byte
	Audio []byte
	// VideoEncoding is the encoding ID of the Video payload (EncodingDeflateRGB565
	// or EncodingJPEG). It rides on the file header, not on each frame:
	// Load/readFrame stamp it from the header and AddFrame from the
	// writer's setting, so consumers of Stream()/Frames always know how to
	// read Video without a second lookup. AudioEncoding likewise tags the
	// Audio payload (raw PCM or Ogg page segments).
	VideoEncoding uint8
	AudioEncoding uint8
}

// MikVFormat is a struct that represents a video/audio file
type MikVFormat struct {
	Frames             []MikVFrame
	FPS                uint8
	Height             uint16
	Width              uint16
	AudioSampleRate    uint16
	AudioChannels      uint8
	AudioBitsPerSample uint8
	// VideoEncoding is the encoding ID written to (or read from) header
	// byte 13: EncodingDeflateRGB565 (0, the default) or EncodingJPEG (1).
	VideoEncoding uint8
	// AudioEncoding is the encoding ID written to (or read from) header
	// byte 14: EncodingRawPCM (0, the default) or EncodingOGGOpus (1).
	AudioEncoding uint8
	// JPEGQuality applies to JPEG-encoded frames (AddFrame/LoadFromMP4);
	// 0 selects DefaultJPEGQuality.
	JPEGQuality int
	// OpusBitrateKbps is the -b:a value (kbps) passed to ffmpeg's libopus
	// encoder in LoadFromMP4's EncodingOGGOpus audio path; 0 leaves the
	// bitrate choice to libopus's own default.
	OpusBitrateKbps int
	Verbose         bool
}

// Header structure (16 bytes; byte 13 is the video encoding ID, byte 14
// the audio encoding ID, byte 15 the reserved padding). Kept as a
// reference template - Load and Save work on per-call copies so
// concurrent file IO cannot race through this package-level value.
var MikVHeader = struct {
	Magic              [4]byte
	FPS                uint8
	Height             uint16
	Width              uint16
	AudioSampleRate    uint16
	AudioChannels      uint8
	AudioBitsPerSample uint8
	VideoEncoding      uint8
	AudioEncoding      uint8
	Reserved           uint8
}{
	Magic:    [4]byte{'M', 'i', 'k', 'V'},
	Reserved: 0, // Reserved padding is always zero
}

// validVideoEncoding reports whether id is a video encoding this
// reader/writer understands.
func validVideoEncoding(id uint8) bool {
	return id == EncodingDeflateRGB565 || id == EncodingJPEG
}

// validAudioEncoding reports whether id is an audio encoding this
// reader/writer understands.
func validAudioEncoding(id uint8) bool {
	return id == EncodingRawPCM || id == EncodingOGGOpus
}

// Constructor
func NewMikVFormat() *MikVFormat {
	return &MikVFormat{
		Verbose: false,
	}
}

// IO functions

// ErrTruncatedRecord reports a MikV frame record that ends before the lengths
// it declares are filled. A file that stops exactly at a record boundary is a
// complete container; one that stops inside a record is a partial product, and
// Load refuses it instead of reporting the frames that survived as a shorter
// valid file. Consumers can test this with errors.Is.
var ErrTruncatedRecord = errors.New("truncated frame record")

// -- Load function
func (m *MikVFormat) Load(filename string) error {
	file, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("Error opening file: %v", err)
	}
	defer file.Close()

	// Read file header
	err = m.readHeader(file)
	if err != nil {
		return err
	}

	// Read frames. io.EOF here means the file ended at a record boundary,
	// which is what a complete container looks like; any other read failure is
	// a cut record (ErrTruncatedRecord), and the frames this Load managed to
	// read before the cut are dropped rather than reported as a whole file.
	kept := len(m.Frames)
	for {
		frame, errrf := m.readFrame(file)
		if errors.Is(errrf, io.EOF) {
			break
		}
		if errrf != nil {
			m.Frames = m.Frames[:kept]
			return errrf
		}
		m.Frames = append(m.Frames, *frame)
	}
	return nil
}

// -- Save function
func (m *MikVFormat) Save(filename string) error {
	file, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("Error creating file: %v", err)
	}
	defer file.Close()

	// Write file header
	err = m.writeHeader(file)
	if err != nil {
		return err
	}

	// Write frames
	for _, frame := range m.Frames {
		err = m.writeFrame(file, frame)
		if err != nil {
			return err
		}
	}
	return nil
}

// Real-Time functions

// -- Stream function
func (m *MikVFormat) Stream(frameChan chan<- MikVFrame, stopChan <-chan struct{}) {
	frameTime := time.Duration(float64(time.Second) / float64(m.FPS))

	ticker := time.NewTicker(frameTime)
	defer ticker.Stop()

	for _, frame := range m.Frames {
		select {

		case <-stopChan: // Stop if requested
			// fmt.Println("Stopping stream")
			return

		case <-ticker.C: // Send the frame at the correct time
			frameChan <- frame
		}
	}

	// Do not close the channel here so that it can be reused
}

// Conversion functions

// -- AddFrame function
// rawImage is an RGB565 frame buffer for EncodingDeflateRGB565 (byte order
// is swapped for the DEFLATE stream, as the original writer did) and a
// complete JPEG image for EncodingJPEG (stored untouched, no DEFLATE).
func (m *MikVFormat) AddFrame(rawImage []byte, audioSamples []byte) error {
	if !validVideoEncoding(m.VideoEncoding) {
		return fmt.Errorf("Unsupported video encoding 0x%02X", m.VideoEncoding)
	}
	if !validAudioEncoding(m.AudioEncoding) {
		return fmt.Errorf("Unsupported audio encoding 0x%02X", m.AudioEncoding)
	}

	if m.VideoEncoding == EncodingJPEG {
		frame := MikVFrame{
			Video:         rawImage,
			Audio:         audioSamples,
			VideoEncoding: EncodingJPEG,
			AudioEncoding: m.AudioEncoding,
		}
		m.Frames = append(m.Frames, frame)
		return nil
	}

	// Swap byte order for zlib
	for i := 0; i < len(rawImage); i += 2 {
		rawImage[i], rawImage[i+1] = rawImage[i+1], rawImage[i]
	}

	// Create a new deflate writer
	var compressedFrame bytes.Buffer
	writer, err := zlib.NewWriterLevel(&compressedFrame, zlib.BestSpeed)
	if err != nil {
		return fmt.Errorf("Error creating DEFLATE writer: %v", err)
	}

	// Write the raw image data to the DEFLATE writer
	if _, err := writer.Write(rawImage); err != nil {
		return fmt.Errorf("Error writing DEFLATE data: %v", err)
	}

	// Make sure all data is flushed to the buffer
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("Error flushing DEFLATE data: %v", err)
	}

	// Close the writer
	if err := writer.Close(); err != nil {
		return fmt.Errorf("Error closing DEFLATE writer: %v", err)
	}

	// Create a new frame with compressed video data and raw audio data
	frame := MikVFrame{
		Video:         compressedFrame.Bytes(),
		Audio:         audioSamples,
		VideoEncoding: EncodingDeflateRGB565,
		AudioEncoding: m.AudioEncoding,
	}

	// Append the frame to the MikVFormat
	m.Frames = append(m.Frames, frame)
	return nil
}

// -- SetAudioParams function
func (m *MikVFormat) SetAudioParams(sampleRate uint16, channels uint8, bitsPerSample uint8) {
	m.AudioSampleRate = sampleRate
	m.AudioChannels = channels
	m.AudioBitsPerSample = bitsPerSample
}

// -- LoadFromMP4 function
func (m *MikVFormat) LoadFromMP4(filename string) error {
	// Load an MP4 file and extract frames
	if m.Verbose {
		fmt.Printf("Converting MP4 file %s to MikV format\n", filename)
	}

	// 0. Check if MikVFormat has audio params
	if m.AudioSampleRate == 0 || m.AudioChannels == 0 || m.AudioBitsPerSample == 0 {
		return fmt.Errorf("Audio parameters not set")
	}

	// Encodings must be valid - and OGG-Opus needs its declared
	// parameters - before any decode work starts.
	if !validVideoEncoding(m.VideoEncoding) {
		return fmt.Errorf("Unsupported video encoding 0x%02X", m.VideoEncoding)
	}
	if !validAudioEncoding(m.AudioEncoding) {
		return fmt.Errorf("Unsupported audio encoding 0x%02X", m.AudioEncoding)
	}
	if m.AudioEncoding == EncodingOGGOpus && (m.AudioSampleRate != OGGOpusSampleRate || m.AudioChannels > 2) {
		return fmt.Errorf("OGG-Opus audio requires sample rate %d and 1-2 channels (got %d Hz, %d ch)",
			OGGOpusSampleRate, m.AudioSampleRate, m.AudioChannels)
	}

	// 1. Use VidIO to extract video frames
	video, err := vidio.NewVideo(filename)
	if err != nil {
		return fmt.Errorf("Error opening video file: %v", err)
	}
	m.FPS = uint8(video.FPS())
	m.Width = uint16(video.Width())
	m.Height = uint16(video.Height())

	var imageBytes [][]byte
	imageBytes = make([][]byte, video.Frames())
	vbuffer := image.NewRGBA(image.Rect(0, 0, int(m.Width), int(m.Height)))
	video.SetFrameBuffer(vbuffer.Pix)
	currentFrameI := 0

	var vbar *progressbar.ProgressBar
	if m.Verbose {
		vbar = progressbar.Default(int64(video.Frames()), "Converting video frames")
	}

	jpegQuality := m.JPEGQuality
	if jpegQuality == 0 {
		jpegQuality = DefaultJPEGQuality
	}

	for video.Read() {
		// 2. Encode the frame per the video encoding ID: RGB565 packed the
		// way AddFrame expects for the DEFLATE path, or a complete JPEG
		// image per frame for EncodingJPEG (stored untouched).
		var frameBytes []byte
		if m.VideoEncoding == EncodingJPEG {
			var jpegBuf bytes.Buffer
			if err := jpeg.Encode(&jpegBuf, vbuffer, &jpeg.Options{Quality: jpegQuality}); err != nil {
				return fmt.Errorf("Error encoding JPEG frame: %v", err)
			}
			frameBytes = jpegBuf.Bytes()
		} else {
			rgb565buf := make([]uint16, m.Width*m.Height)

			for y := 0; y < int(m.Height); y++ {
				for x := 0; x < int(m.Width); x++ {
					r, g, b, _ := vbuffer.At(x, y).RGBA()
					r5 := uint16((r >> 11) & 0x1F)
					g6 := uint16((g >> 10) & 0x3F)
					b5 := uint16((b >> 11) & 0x1F)
					rgb565buf[y*int(m.Width)+x] = (r5 << 11) | (g6 << 5) | b5
				}
			}
			frameBytes = make([]byte, len(rgb565buf)*2)
			for i, v := range rgb565buf {
				frameBytes[i*2] = byte(v)
				frameBytes[i*2+1] = byte(v >> 8)
			}
		}

		if m.Verbose {
			vbar.Add(1)
		}

		imageBytes[currentFrameI] = frameBytes
		// imageBytes = append(imageBytes, rawImage)
		currentFrameI++
	}

	if m.Verbose {
		vbar.Finish()
	}

	// 3. Audio pipeline, per header byte 14.
	// EncodingOGGOpus: ONE ordinary OGG-Opus stream, encoded at the frame
	// duration that matches the fps, then cut at page boundaries into one
	// chunk per video frame (see ogg_opus.go).
	// EncodingRawPCM: the legacy raw PCM track.
	var audioChunks [][]byte // OGG-Opus: exactly len(imageBytes) chunks
	var audioBytes []byte    // raw PCM: the full interleaved track
	audioFrameLength := 0

	if m.AudioEncoding == EncodingOGGOpus {
		frameDurMs, err := OGGOpusFrameDurationMs(m.FPS)
		if err != nil {
			return fmt.Errorf("Error setting up OGG-Opus audio: %v", err)
		}
		if m.Verbose {
			fmt.Printf("Encoding OGG-Opus audio (%.1f ms packets at %d fps)\n", frameDurMs, m.FPS)
		}
		args := []string{"-i", filename, "-vn",
			"-ar", fmt.Sprintf("%d", OGGOpusSampleRate),
			"-ac", fmt.Sprintf("%d", m.AudioChannels),
			"-c:a", "libopus",
			"-frame_duration", strconv.FormatFloat(frameDurMs, 'f', -1, 64)}
		if m.OpusBitrateKbps > 0 {
			args = append(args, "-b:a", fmt.Sprintf("%dk", m.OpusBitrateKbps))
		}
		args = append(args, "-f", "ogg", "pipe:1")
		cmd := exec.Command("ffmpeg", args...)
		oggBytes, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("Error encoding OGG-Opus audio: %v", err)
		}
		audioChunks, err = SplitOGGOpus(oggBytes, m.FPS, len(imageBytes))
		if err != nil {
			return fmt.Errorf("Error segmenting OGG-Opus audio: %v", err)
		}
	} else {
		audioFrameLength = int((int(m.AudioSampleRate) / int(m.FPS)) * int(m.AudioChannels) * int(m.AudioBitsPerSample) / 8)

		var abar *progressbar.ProgressBar
		if m.Verbose {
			abar = progressbar.Default(int64(len(imageBytes)*audioFrameLength), "Transcoding audio")
		}
		var audioFormat string
		if m.AudioBitsPerSample == 16 {
			audioFormat = "s16le"
		} else if m.AudioBitsPerSample == 8 {
			audioFormat = "u8"
		}
		cmd := exec.Command("ffmpeg", "-i", filename, "-ar", fmt.Sprintf("%d", m.AudioSampleRate), "-f", audioFormat, "-ac", fmt.Sprintf("%d", m.AudioChannels), "pipe:1")
		audioReader, err := cmd.StdoutPipe()
		if err != nil {
			return fmt.Errorf("Error creating audio pipe: %v", err)
		}
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("Error starting ffmpeg: %v", err)
		}
		// 3.1 Read audio frames from the pipe
		stdoutBuffer := make([]byte, 4096)
		for {
			n, err := audioReader.Read(stdoutBuffer)
			if n > 0 {
				audioBytes = append(audioBytes, stdoutBuffer[:n]...)

				if m.Verbose {
					abar.Add(n)
				}
			}
			if err != nil {
				if err == io.EOF {
					break
				}
				return fmt.Errorf("Error reading audio data: %v", err)
			}
		}
		if err := cmd.Wait(); err != nil {
			return fmt.Errorf("Error waiting for ffmpeg: %v", err)
		}

		if m.Verbose {
			abar.Finish()
		}
	}

	// 4. Add the  frames to the MikVFormat
	var mbar *progressbar.ProgressBar
	if m.Verbose {
		mbar = progressbar.Default(int64(len(imageBytes)), "Adding frames to MikV")
	}

	for i, frameBytes := range imageBytes {
		var audioFrame []byte
		if m.AudioEncoding == EncodingOGGOpus {
			audioFrame = audioChunks[i]
		} else {
			start := i * audioFrameLength
			end := start + audioFrameLength
			if end > len(audioBytes) {
				end = len(audioBytes)
			}
			if start > len(audioBytes) {
				fmt.Println("Audio data shorter than video, leaving tail as silence")
				break
			}

			audioFrame = make([]byte, audioFrameLength)
			copy(audioFrame, audioBytes[start:end])
		}
		if err := m.AddFrame(frameBytes, audioFrame); err != nil {
			return fmt.Errorf("Error adding frame: %v", err)
		}

		if m.Verbose {
			mbar.Add(1)
		}
	}

	if m.Verbose {
		fmt.Printf("Loaded %d frames from MP4 file\n", len(m.Frames))
	}

	return nil
}

// Internal functions

// -- readHeader function
func (m *MikVFormat) readHeader(file *os.File) error {
	// Read the header from the file into a per-call copy of the template
	h := MikVHeader
	err := binary.Read(file, binary.LittleEndian, &h)
	if err != nil {
		return fmt.Errorf("Error reading header: %v", err)
	}

	// Check magic number
	if h.Magic != [4]byte{'M', 'i', 'k', 'V'} {
		return fmt.Errorf("Invalid file format")
	}

	// Encoding IDs must be known before any frame is touched: a payload
	// read under the wrong encoding is silent garbage.
	if !validVideoEncoding(h.VideoEncoding) {
		return fmt.Errorf("Unsupported video encoding 0x%02X", h.VideoEncoding)
	}
	if !validAudioEncoding(h.AudioEncoding) {
		return fmt.Errorf("Unsupported audio encoding 0x%02X", h.AudioEncoding)
	}

	// Assign to struct fields
	m.FPS = h.FPS
	m.Height = h.Height
	m.Width = h.Width
	m.AudioSampleRate = h.AudioSampleRate
	m.AudioChannels = h.AudioChannels
	m.AudioBitsPerSample = h.AudioBitsPerSample
	m.VideoEncoding = h.VideoEncoding
	m.AudioEncoding = h.AudioEncoding

	return nil
}

// -- readFrame function
// A clean io.EOF means the file ended exactly where the next record's length
// would start: the container is complete. Anything shorter is a cut record and
// comes back as ErrTruncatedRecord.
func (m *MikVFormat) readFrame(file *os.File) (*MikVFrame, error) {
	var frame MikVFrame

	// Read video frame length (uint32) in little-endian format
	var videoLength uint32
	if err := binary.Read(file, binary.LittleEndian, &videoLength); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("%w: the file stops where a video frame length belongs: %v", ErrTruncatedRecord, err)
	}

	// A payload longer than the rest of the file is a cut record on the spot:
	// checking it here is what stops a corrupt length from asking for a buffer
	// the file could never fill.
	left, err := bytesLeftInFile(file)
	if err != nil {
		return nil, fmt.Errorf("Error checking file size: %v", err)
	}
	if int64(videoLength) > left {
		return nil, fmt.Errorf("%w: video payload wants %d bytes, %d remain", ErrTruncatedRecord, videoLength, left)
	}

	// Read video frame data
	frame.Video = make([]byte, videoLength)
	if n, err := io.ReadFull(file, frame.Video); err != nil {
		return nil, fmt.Errorf("%w: video payload wants %d bytes, %d arrived: %v", ErrTruncatedRecord, videoLength, n, err)
	}

	// Read audio frame length (uint32) in little-endian format. Even a plain
	// EOF here is a cut record: the video payload above already opened this
	// record, so the file is one field short of closing it.
	var audioLength uint32
	if err := binary.Read(file, binary.LittleEndian, &audioLength); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: the file stops where an audio frame length belongs", ErrTruncatedRecord)
		}
		return nil, fmt.Errorf("%w: audio frame length: %v", ErrTruncatedRecord, err)
	}

	left, err = bytesLeftInFile(file)
	if err != nil {
		return nil, fmt.Errorf("Error checking file size: %v", err)
	}
	if int64(audioLength) > left {
		return nil, fmt.Errorf("%w: audio payload wants %d bytes, %d remain", ErrTruncatedRecord, audioLength, left)
	}

	// Read audio chunk data
	frame.Audio = make([]byte, audioLength)
	if n, err := io.ReadFull(file, frame.Audio); err != nil {
		return nil, fmt.Errorf("%w: audio payload wants %d bytes, %d arrived: %v", ErrTruncatedRecord, audioLength, n, err)
	}

	// The encodings ride on the header; stamp them onto the frame so a
	// consumer can interpret both payloads from the frame alone.
	frame.VideoEncoding = m.VideoEncoding
	frame.AudioEncoding = m.AudioEncoding

	return &frame, nil

}

// bytesLeftInFile reports how many bytes a seekable file still holds from its
// current read position.
func bytesLeftInFile(file *os.File) (int64, error) {
	pos, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	if info.Size() < pos {
		return 0, nil
	}
	return info.Size() - pos, nil
}

// -- writeHeader function
func (m *MikVFormat) writeHeader(file *os.File) error {
	if !validVideoEncoding(m.VideoEncoding) {
		return fmt.Errorf("Unsupported video encoding 0x%02X", m.VideoEncoding)
	}
	if !validAudioEncoding(m.AudioEncoding) {
		return fmt.Errorf("Unsupported audio encoding 0x%02X", m.AudioEncoding)
	}
	if m.AudioEncoding == EncodingOGGOpus {
		// The frame-window arithmetic is granule-based: this encoding only
		// works with the 48 kHz Ogg granule clock and Opus's channel limit.
		if m.AudioSampleRate != OGGOpusSampleRate || m.AudioChannels < 1 || m.AudioChannels > 2 {
			return fmt.Errorf("OGG-Opus audio requires sample rate %d and 1-2 channels (got %d Hz, %d ch)",
				OGGOpusSampleRate, m.AudioSampleRate, m.AudioChannels)
		}
	}

	// Set header values dynamically from MikVFormat, on a per-call copy of
	// the template
	h := MikVHeader
	h.Magic = [4]byte{'M', 'i', 'k', 'V'}
	h.FPS = m.FPS
	h.Height = m.Height
	h.Width = m.Width
	h.AudioSampleRate = m.AudioSampleRate
	h.AudioChannels = m.AudioChannels
	h.AudioBitsPerSample = m.AudioBitsPerSample
	h.VideoEncoding = m.VideoEncoding
	h.AudioEncoding = m.AudioEncoding
	h.Reserved = 0

	// Write the header to the file in little-endian format
	if err := binary.Write(file, binary.LittleEndian, h); err != nil {
		return fmt.Errorf("Error writing header: %v", err)
	}

	return nil
}

// -- writeFrame function
func (m *MikVFormat) writeFrame(file *os.File, frame MikVFrame) error {
	// Write the video frame length (uint32) in Little Endian format
	videoLength := uint32(len(frame.Video))
	if err := binary.Write(file, binary.LittleEndian, videoLength); err != nil {
		return fmt.Errorf("Error writing video length: %v", err)
	}

	// Write the video frame data ([]byte) to the file
	if _, err := file.Write(frame.Video); err != nil {
		return fmt.Errorf("Error writing video data: %v", err)
	}

	// Write the audio frame length (uint32) in Little Endian format
	audioLength := uint32(len(frame.Audio))
	if err := binary.Write(file, binary.LittleEndian, audioLength); err != nil {
		return fmt.Errorf("Error writing audio length: %v", err)
	}

	// Write the uncompressed audio frame data ([]byte) to the file
	if _, err := file.Write(frame.Audio); err != nil {
		return fmt.Errorf("Error writing audio data: %v", err)
	}

	return nil
}
