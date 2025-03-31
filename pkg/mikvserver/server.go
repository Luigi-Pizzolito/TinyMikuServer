package mikvserver

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

func (ms *MikVServer) startServer() {
	// Create a new HTTP server
	ms.mux = http.NewServeMux()
	ms.mux.HandleFunc("/video", ms.videoHandler)
	server := &http.Server{
		Addr:    ":" + strconv.Itoa(ms.Port),
		Handler: ms.mux,
	}

	// Store the server instance
	ms.server = server

	// Start the server in a goroutine
	go func() {
		ms.started = true
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			panic(err)
		}
	}()
}

func (ms *MikVServer) streamer() {
	// Take input frames and set lastframe
	for {
		select {
		case frame, ok := <-ms.Stream:
			if !ok {
				// Input channel closed
				return
			}

			// Grab frame lock & set last frame
			ms.frameLock.Lock()
			ms.lastFrame = frame
			ms.frameLock.Unlock()
		}
	}
}

func (ms *MikVServer) videoHandler(w http.ResponseWriter, r *http.Request) {
	// Grab frame lock
	ms.frameLock.Lock()
	defer ms.frameLock.Unlock()

	// Check if we have a frame
	if len(ms.lastFrame.Video) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Set headers
	w.Header().Set("Content-Type", "application/octet-stream")

	audioFrameLength := len(ms.lastFrame.Audio)
	videoFrameLength := len(ms.lastFrame.Video)
	totalFrameLength := audioFrameLength + videoFrameLength
	w.Header().Set("Content-Length", strconv.Itoa(totalFrameLength))
	w.Header().Set("X-Video-Length", strconv.Itoa(videoFrameLength))

	// Prepare the response
	totalFrameBuffer := make([]byte, totalFrameLength)
	copy(totalFrameBuffer, ms.lastFrame.Video)
	copy(totalFrameBuffer[videoFrameLength:], ms.lastFrame.Audio)

	// Write the response
	w.WriteHeader(http.StatusOK)
	num, err := w.Write(totalFrameBuffer)
	if err != nil {
		fmt.Printf("Error streaming frame: %v\n", err)
	} else if num != totalFrameLength {
		fmt.Printf("Error streaming frame: Only %d/%d bytes sent.\n", num, totalFrameLength)
	}
}

func (ms *MikVServer) stopServer() {
	if ms.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ms.server.Shutdown(ctx)
		ms.started = false
	}
}
