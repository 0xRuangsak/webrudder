package main

import (
	"flag"
	"log"

	"github.com/0xRuangsak/webrudder/internal/browser"
	"github.com/0xRuangsak/webrudder/internal/server"
)

// @title        webrudder API
// @version      1.0
// @description  Browser automation daemon — navigate, interact, and extract from a live headless browser. One browser per port.
// @license.name MIT
// @BasePath     /
func main() {
	port := flag.Int("port", 10000, "HTTP port (auto-increments if busy; 0 = OS-assigned)")
	downloads := flag.String("downloads", "", "download directory (default: OS temp dir)")
	flag.Parse()

	// flag.Arg(0) is the optional entry URL; blank starts on about:blank.
	sess, err := browser.New(flag.Arg(0), *downloads)
	if err != nil {
		log.Fatalf("webrudder: %v", err)
	}
	defer sess.Close()

	if err := server.Run(sess, *port); err != nil {
		log.Fatalf("webrudder: %v", err)
	}
}
