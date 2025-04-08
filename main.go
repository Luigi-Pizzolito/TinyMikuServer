package main

import (
	"fmt"

	"github.com/Luigi-Pizzolito/TinyMikuServer/pkg/mikvformat"
	"github.com/Luigi-Pizzolito/TinyMikuServer/pkg/mikvserver"
)

func main() {
	fmt.Println("Starting...")

	mikv := mikvformat.NewMikVFormat()
	mikv.Load("./assets/mikv/39.mikv")

	fmt.Println("Loaded MikVFormat")

	server := mikvserver.NewMikVServer()
	server.Start()

	fmt.Println("Started MikVServer")

	mikv.Stream(server.Stream, nil)
}
