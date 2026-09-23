// Command video-thumbnails generates thumbnails for the videos stored in
// OpenCloud and serves them to the web and to the API clients.
package main

import (
	"os"

	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/command"
)

// version is set at build time.
var version = "dev"

func main() {
	os.Exit(command.Execute(version, os.Args[1:]))
}
