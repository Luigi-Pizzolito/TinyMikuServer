package mikvserver

import (
	"net/http"
	"sync"

	"github.com/Luigi-Pizzolito/TinyMikuServer/pkg/mikvformat"
)

/* MikVServer class */
type MikVServer struct {
	/* meta */
	Port int

	/* private */
	Stream  chan mikvformat.MikVFrame
	server  *http.Server
	mux     *http.ServeMux
	started bool

	frameLock sync.Mutex
	lastFrame mikvformat.MikVFrame
}

func NewMikVServer() *MikVServer {
	return &MikVServer{started: false}
}

func (ms *MikVServer) Start(port ...int) {
	// Set the port if provided
	if len(port) > 0 {
		ms.Port = port[0]
	} else {
		ms.Port = 8000 // Default port
	}

	// Start the server
	ms.startServer()

	// Create a readiness channel
	ready := make(chan struct{})

	// Start the streamer
	go func() {
		// Start input channel
		ms.Stream = make(chan mikvformat.MikVFrame)

		// Signal ready
		close(ready)

		// Start the streamer
		ms.streamer()
	}()

	// Wait for ready
	<-ready
}

func (ms *MikVServer) Stop() {
	// Close input channel
	if ms.Stream != nil {
		close(ms.Stream)
	}

	// Stop the server
	ms.stopServer()
}
