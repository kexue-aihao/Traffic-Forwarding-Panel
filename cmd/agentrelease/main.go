// Command agentrelease prepares the manifest carried with panel releases.
package main

import (
	"flag"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/agentdist"
	"log"
)

func main() {
	dir := flag.String("dir", ".", "Agent binary directory")
	version := flag.String("version", "", "panel release version")
	flag.Parse()
	if *version == "" {
		log.Fatal("version required")
	}
	if err := agentdist.WriteReleaseManifest(*dir, *version); err != nil {
		log.Fatal(err)
	}
}
