// Command ostree is the pure-Go replacement for the C ostree CLI.
// All behavior lives in internal/cli and internal/cmd; this file only wires
// up the process streams, the command table and the exit code.
package main

import (
	"os"

	"ostree-go/internal/cli"
	"ostree-go/internal/cmd"
)

func main() {
	app := cli.NewApp()
	cmd.Setup(app)
	os.Exit(app.Run(os.Args))
}
