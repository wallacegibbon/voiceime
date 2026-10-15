package cli

import (
	"strings"
)

// Source is one capture device as reported by the sound server.
type Source struct {
	Name    string `json:"name"`
	Driver  string `json:"driver,omitempty"`
	State   string `json:"state,omitempty"`
	Default bool   `json:"default"`
}

func cmdDevices() *Command {
	return &Command{
		Name:    "devices",
		Summary: "List capture sources and mark the default one",
		Usage:   "voiceime devices",
		Run: func(ctx *Context, args []string) error {
			sources := listSources()
			defaultSource := defaultSource()
			for i := range sources {
				sources[i].Default = sources[i].Name == defaultSource
			}
			for _, source := range sources {
				marker := " "
				if source.Default {
					marker = "*"
				}
				ctx.Print("%s %s", marker, source.Name)
			}
			if len(sources) == 0 {
				ctx.Print("no capture sources reported by pactl")
			}
			if err := ctx.Emit("devices", map[string]any{
				"sources":        sources,
				"default_source": defaultSource,
				"configured":     ctx.Config.CaptureSource,
			}); err != nil {
				return err
			}
			if len(sources) == 0 {
				return FailureError("no capture sources: is PipeWire or PulseAudio running?")
			}
			return nil
		},
	}
}

// listSources parses `pactl list short sources`, whose tab-separated columns are
// index, name, driver and state. Only the name is strictly needed, but keeping
// driver and state makes a wrong-device report diagnosable.
func listSources() []Source {
	output, err := probe("pactl", "list", "short", "sources")
	if err != nil {
		return nil
	}
	var sources []Source
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		source := Source{Name: fields[1]}
		if len(fields) > 2 {
			source.Driver = fields[2]
		}
		if len(fields) > 3 {
			source.State = fields[3]
		}
		sources = append(sources, source)
	}
	return sources
}

// defaultSource is the source capture will use when capture_source is empty.
func defaultSource() string {
	output, err := probe("pactl", "get-default-source")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(output)
}
