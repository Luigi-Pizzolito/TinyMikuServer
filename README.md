# TinyMikuServer

Server for streaming data to tiny Miku Holocube AI Assistant

## Overview

TinyMikuServer is a specialized video streaming server designed to serve content to the Tiny Miku Holocube AI Assistant. The project includes a custom binary video format (MikV), a streaming server, and utilities for video processing and conversion.

## Features

- **Custom MikV Format**: A lightweight binary format optimized for storing synchronized video and audio frames with DEFLATE compression
- **Streaming Server**: HTTP-based server for real-time streaming of MikV content
- **Video Converter**: Tool to convert MP4 files to the MikV format
- **HoloCropper**: Content-aware video cropping tool for preparing hologram-style videos (1:1 aspect ratio)
- **Optimized for Edge Devices**: Designed for efficient streaming to embedded systems and IoT devices

## Project Structure

```
TinyMikuServer/
├── main.go                 # Main server application
├── pkg/
│   ├── mikvformat/        # MikV format library
│   │   ├── MikVFormat.go  # Core format implementation
│   │   └── cmd/
│   │       └── mikv-converter/  # MP4 to MikV converter CLI
│   ├── mikvserver/        # HTTP streaming server
│   │   ├── MikVServer.go  # Server implementation
│   │   └── server.go      # HTTP handlers
│   └── holocropper/       # Video cropping utility
│       ├── holocropper.go # Core cropping logic
│       └── cmd/
│           └── holocropper/     # HoloCropper CLI
├── bin/                   # Compiled binaries (generated)
└── assets/                # Video assets (not tracked)
    ├── mp4/              # Processed MP4 files
    ├── mp4-full/         # Original full videos
    └── mikv/             # Converted MikV files
```

## Prerequisites

- Go 1.24.1 or later
- FFmpeg (for video processing)

## Installation

### Using Go Install

```bash
go install github.com/Luigi-Pizzolito/TinyMikuServer@latest
```

### Building from Source

1. Clone the repository:
```bash
git clone https://github.com/Luigi-Pizzolito/TinyMikuServer.git
cd TinyMikuServer
```

2. Build all components:
```bash
make all
```

This will compile:
- `bin/TinyMikuServer` - Main streaming server
- `bin/mikv-converter` - MP4 to MikV converter
- `bin/holocropper` - Video cropping tool

### Building Individual Components

```bash
# Build only the server
make build-server

# Build only the converter
make build-mikv-converter

# Build only the holocropper
make build-holocropper
```

## Usage

### 1. Running the Server

The main server loads a MikV file and streams it over HTTP:

```bash
./bin/TinyMikuServer
```

By default, it:
- Loads `./assets/mikv/39.mikv`
- Starts HTTP server on port 8000

### 2. Converting Videos to MikV Format

Convert a single MP4 file:

```bash
./bin/mikv-converter -i input.mp4 -o ./output
```

Convert all MP4 files in a directory:

```bash
./bin/mikv-converter -i ./videos -o ./output
```

Options:
- `-i`: Input MP4 file or directory (default: current directory)
- `-o`: Output directory for MikV files (default: current directory)
- `-sampleRate`: Audio sample rate (default: 16000)
- `-channels`: Number of audio channels (default: 1)
- `-bitsPerSample`: Audio bits per sample (default: 16)
- `-silent`: Disable verbose output

### 3. Cropping Videos with HoloCropper

HoloCropper performs content-aware cropping to create 1:1 aspect ratio videos suitable for holographic displays:

```bash
./bin/holocropper -i input.mp4 -o ./output -w 240 -h 240 -fps 20
```

Options:
- `-i`: Input video file or directory
- `-o`: Output directory (required)
- `-w`: Width of cropped video (default: 240)
- `-h`: Height of cropped video (default: 240)
- `-fps`: Frames per second (default: 20)

### 4. Complete Workflow with Make

Process full videos through the entire pipeline:

```bash
# Step 1: Crop full videos to 1:1 aspect ratio
make convert-full

# Step 2: Convert cropped MP4s to MikV format
make convert-assets

# Or do both in one command:
make full-convert-assets
```

## MikV File Format Specification

The MikV format is a custom binary format optimized for synchronized video and audio streaming.

### Recommended Settings

- **FPS**: 20
- **Video Resolution**: 240×240
- **Audio Sample Rate**: 16000 Hz
- **Audio Channels**: 1 (mono)
- **Audio Bits per Sample**: 16
- **Compression**: DEFLATE (zlib)

### File Structure

```
[HEADER (16 bytes)] → [FRAME 1] → [FRAME 2] → ... → [FRAME N]
```

#### Header Format (16 bytes)

| Offset | Size | Type   | Description                  |
|--------|------|--------|------------------------------|
| 0      | 4    | bytes  | Magic number "MikV"          |
| 4      | 1    | uint8  | Frames per second (FPS)      |
| 5      | 2    | uint16 | Video frame height           |
| 7      | 2    | uint16 | Video frame width            |
| 9      | 2    | uint16 | Audio sample rate            |
| 11     | 1    | uint8  | Audio channels               |
| 12     | 2    | uint16 | Audio bits per sample        |
| 14     | 2    | uint16 | Reserved (padding)           |

#### Frame Structure

Each frame contains:
- **Video Size** (4 bytes): uint32 - Size of compressed video data
- **Video Data** (variable): DEFLATE compressed RGB565 frame
- **Audio Data** (fixed): Raw PCM audio samples

## API Reference

### MikVFormat

```go
// Create a new MikV format instance
mikv := mikvformat.NewMikVFormat()

// Load from file
mikv.Load("path/to/file.mikv")

// Save to file
mikv.Save("path/to/output.mikv")

// Create from MP4
mikv.FromMP4("path/to/video.mp4")

// Stream frames
frameChan := make(chan mikvformat.MikVFrame)
mikv.Stream(frameChan, nil)
```

### MikVServer

```go
// Create and start server
server := mikvserver.NewMikVServer()
server.Start(8000) // Optional port parameter

// Stream frames to connected clients
server.Stream <- frame

// Stop server
server.Stop()
```

### HoloCropper

```go
// Create cropper instance
cropper := holocropper.NewHoloCropper()

// Process video
cropper.Process("input.mp4", "output", 240, 240, 20.0)
```

## Development

### Running Tests

```bash
go test ./...
```

### Cleaning Build Artifacts

```bash
make clean
```

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

Copyright (c) 2025 Luigi Pizzolito

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## Acknowledgments

- Built with [Vidio](https://github.com/AlexEidt/Vidio) for video processing
- Uses [progressbar](https://github.com/schollz/progressbar) for CLI feedback
- HoloCropper is a refactor of [1-1-Content-Aware-Video-Crop](https://github.com/Luigi-Pizzolito/1-1-Content-Aware-Video-Crop)
