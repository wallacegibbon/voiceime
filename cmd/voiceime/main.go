// Command voiceime is the single binary for the whole project: the daemon, the
// control surface and the IBus engine all live here, selected by subcommand.
//
// One binary is a distribution decision as much as a build one. voiceime has no
// GUI and no dynamic dependencies beyond the system's own PipeWire and IBus
// tooling, so shipping it means copying one file.
package main

import "github.com/wallacegibbon/voiceime/internal/cli"

func main() {
	cli.Main()
}
