package cli

// Version is stamped in at build time:
//
//	go build -ldflags "-X github.com/wallacegibbon/voiceime/internal/cli.Version=v1.2.3"
//
// `make build` does this with `git describe`, and it falls back to "dev" for a
// plain `go build`. It is a variable rather than a constant for that reason, and
// `capabilities` reports it so a caller can record what it configured against.
var Version = "dev"
