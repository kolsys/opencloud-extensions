// Command file-activity turns the file events of OpenCloud into a pull feed
// of changes with a cursor.
package main

import (
	"os"

	"github.com/kolsys/opencloud-extensions/file-activity/internal/command"
)

// version is set at build time.
var version = "dev"

func main() {
	os.Exit(command.Execute(version, os.Args[1:]))
}
