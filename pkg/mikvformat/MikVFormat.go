package mikvformat

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"image"
	"io"
	"os"
	"os/exec"
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
 * - Offset 13 (3 bytes):	Reserved (padding)							[ [3]byte ]
 *
 * 2. FRAME (variable size)
 * ------------------------
 * Each frame consists of a **video** frame followed by an **audio** chunk:
 * - Offset 0 (4 bytes):		Video frame length (compressed)				[ uint32 ]
 * - Offset 4 (N bytes):		Compressed video frame data (DEFLATE)		[ []byte ]
 * - Offset 4+N (4 bytes):		Audio frame length (uncompressed)			[ uint32 ]
 * - Offset 4+N+4 (M bytes):	Uncompressed audio frame data (PCM)			[ []byte ]
 * Where:
 * - N = Video frame length (compressed)
 * - M = Audio frame length (uncompressed)
 *
 * Example Binary Layout:
 * ----------------------
 *  0x0000:  4D 69 6B 56 14 00 F0 00 F0 00 80 3E 01 10 00 00 00  (Header: "MikV", FPS=20, 240x240, 16kHz, 1ch, 16-bit)
 *  0x0010:  70 2F 00 00 [...12144 bytes compressed video data...] 40 06 00 00 [...1600 bytes PCM audio data...]
 *  0x4000:  EC 2E 00 00 [...12012 bytes compressed video data...] 40 06 00 00 [...1600 bytes PCM audio data...]
 *  0x8000:  28 2D 00 00 [...11560 bytes compressed video data...] 40 06 00 00 [...1600 bytes PCM audio data...]
 *  ...
 *
 * Notes:
 * - The video frame data is stored as DEFLATE compressed RGB565 data (uint16/2bytes per pixel).
 * - The audio frame data is stored as raw PCM data (int16/2bytes per sample).
 * - The audio frame length is fixed and depends on the audio sample rate, channels, and bits per sample.
 * - The audio frame length is calculated as (AudioSampleRate/FPS)*AudioChannels*AudioBitsPerSample/8.
 * - The video frame length is variable and depends on the video frame size and compression ratio.
 * - ZLib's DEFLATE implementation is used, not libdeflate or other compression libraries.
 * todo: add compression for raw audio samples too
 * - All fields are saved in LittleEndian byte order.
 * - The reserved bytes in the header are used for future expansion.
 * - The magic number "MikV" is used to identify the file format.
 * - The audio frame data is interleaved for multiple channels.
 * - Audio data is typically stored as 16-bit signed PCM data, but support for compressed formats may be added.
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

// Frame represents a single video and audio frame
type MikVFrame struct {
	Video []byte
	Audio []byte
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
	Verbose            bool
}

// Header structure
var MikVHeader = struct {
	Magic              [4]byte
	FPS                uint8
	Height             uint16
	Width              uint16
	AudioSampleRate    uint16
	AudioChannels      uint8
	AudioBitsPerSample uint8
	Reserved           [3]byte
}{
	Magic:    [4]byte{'M', 'i', 'k', 'V'},
	Reserved: [3]byte{0, 0, 0}, // Default reserved bytes
}

// Constructor
func NewMikVFormat() *MikVFormat {
	return &MikVFormat{
		Verbose: false,
	}
}

// IO functions

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

	// Read frames
	for {
		frame, errrf := m.readFrame(file)
		if errrf == io.EOF {
			break
		}
		if errrf != nil {
			return err
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
func (m *MikVFormat) AddFrame(rawImage []byte, audioSamples []byte) error {
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
		Video: compressedFrame.Bytes(),
		Audio: audioSamples,
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

	for video.Read() {
		// 2. Convert the frames to RGB565 format
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
		rawImage := make([]byte, len(rgb565buf)*2)
		for i, v := range rgb565buf {
			rawImage[i*2] = byte(v)
			rawImage[i*2+1] = byte(v >> 8)
		}

		if m.Verbose {
			vbar.Add(1)
		}

		imageBytes[currentFrameI] = rawImage
		currentFrameI++
	}

	if m.Verbose {
		vbar.Finish()
	}

	// 3. Use ffmpeg to convert the audio to the right format
	audioFrameLength := int((int(m.AudioSampleRate) / int(m.FPS)) * int(m.AudioChannels) * int(m.AudioBitsPerSample) / 8)

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
	var audioBytes []byte
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

	// 4. Add the  frames to the MikVFormat
	var mbar *progressbar.ProgressBar
	if m.Verbose {
		mbar = progressbar.Default(int64(len(imageBytes)), "Adding frames to MikV")
	}

	for i, rawImage := range imageBytes {
		start := i * audioFrameLength
		end := start + audioFrameLength
		if end > len(audioBytes) {
			end = len(audioBytes)
		}

		audioFrame := make([]byte, audioFrameLength)
		copy(audioFrame, audioBytes[start:end])
		if err := m.AddFrame(rawImage, audioFrame); err != nil {
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
	// Read the header from the file
	err := binary.Read(file, binary.LittleEndian, &MikVHeader)
	if err != nil {
		return fmt.Errorf("Error reading header: %v", err)
	}

	// Check magic number
	if MikVHeader.Magic != [4]byte{'M', 'i', 'k', 'V'} {
		return fmt.Errorf("Invalid file format")
	}

	// Assign to struct fields
	m.FPS = MikVHeader.FPS
	m.Height = MikVHeader.Height
	m.Width = MikVHeader.Width
	m.AudioSampleRate = MikVHeader.AudioSampleRate
	m.AudioChannels = MikVHeader.AudioChannels
	m.AudioBitsPerSample = MikVHeader.AudioBitsPerSample

	return nil
}

// -- readFrame function
func (m *MikVFormat) readFrame(file *os.File) (*MikVFrame, error) {
	var frame MikVFrame

	// Read video frame length (uint32) in little-endian format
	var videoLength uint32
	if err := binary.Read(file, binary.LittleEndian, &videoLength); err != nil {
		return nil, fmt.Errorf("Error reading video frame length: %v", err)
	}

	// Read video frame data
	frame.Video = make([]byte, videoLength)
	if _, err := io.ReadFull(file, frame.Video); err != nil {
		return nil, fmt.Errorf("Error reading video data: %v", err)
	}

	// Read audio frame length (uint32) in little-endian format
	var audioLength uint32
	if err := binary.Read(file, binary.LittleEndian, &audioLength); err != nil {
		return nil, fmt.Errorf("Error reading audio frame length: %v", err)
	}

	// Read audio chunk data
	frame.Audio = make([]byte, audioLength)
	if _, err := io.ReadFull(file, frame.Audio); err != nil {
		return nil, fmt.Errorf("Error reading audio frame data: %v", err)
	}

	return &frame, nil

}

// -- writeHeader function
func (m *MikVFormat) writeHeader(file *os.File) error {
	// Set header values dynamically from MikVFormat
	MikVHeader.Magic = [4]byte{'M', 'i', 'k', 'V'}
	MikVHeader.FPS = m.FPS
	MikVHeader.Height = m.Height
	MikVHeader.Width = m.Width
	MikVHeader.AudioSampleRate = m.AudioSampleRate
	MikVHeader.AudioChannels = m.AudioChannels
	MikVHeader.AudioBitsPerSample = m.AudioBitsPerSample
	MikVHeader.Reserved = [3]byte{0, 0, 0}

	// Write the header to the file in little-endian format
	if err := binary.Write(file, binary.LittleEndian, MikVHeader); err != nil {
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
