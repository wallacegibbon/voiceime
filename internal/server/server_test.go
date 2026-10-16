package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The tests below exercise the real thing where it matters: a spawned process, a
// port, a health check, and a stop. The stand-in for audiocpp_server is a shell
// script that re-executes this test binary, which is the only way to get a real
// listener without depending on audio.cpp's build being present.
const fakeServerEnv = "VOICEIME_TEST_SERVER"

// fakeServerConfigEnv carries the generated config path to the stand-in. It is an
// environment variable rather than a command-line argument because the stand-in is
// a Go test binary, and a test binary rejects the server's own flags.
const fakeServerConfigEnv = "VOICEIME_TEST_CONFIG"

// TestStandsInForTheServer is not a test when the environment says so: it is the
// body of the stand-in server the spawn tests start.
func TestStandsInForTheServer(t *testing.T) {
	if os.Getenv(fakeServerEnv) != "1" {
		t.Skip("helper process; started by the spawn tests")
	}
	configPath := os.Getenv(fakeServerConfigEnv)
	if configPath == "" {
		t.Fatalf("stand-in server: %s is not set", fakeServerConfigEnv)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("stand-in server: %v", err)
	}
	var config wireConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatalf("stand-in server: %v", err)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(config.Port)))
	if err != nil {
		t.Fatalf("stand-in server: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		payload, _ := json.Marshal(map[string]any{
			"object": "list",
			"data":   []map[string]string{{"id": modelIDOf(config)}},
		})
		_, _ = w.Write(payload)
	})
	mux.HandleFunc("/v1/audio/transcriptions", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"text":"stand-in transcript"}`))
	})
	// A ready marker on stdout is what the spawn test waits for when it needs to
	// know the child is actually up, rather than guessing with a sleep.
	os.Stdout.WriteString("stand-in server listening\n")
	_ = http.Serve(listener, mux)
}

func modelIDOf(config wireConfig) string {
	if len(config.Models) == 0 {
		return ""
	}
	return config.Models[0].ID
}

// fakeServerBinary writes the shell script that stands in for audiocpp_server.
func fakeServerBinary(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-audiocpp-server")
	// The assignments are their own statements, not prefixed to the exec: a
	// prefixed assignment is not visible to that same command's expansion, so
	// `VAR=x exec "$VAR"` runs an empty program. The config path is found by hand
	// because it is the one argument the stand-in needs, and the rest of the
	// server's flags are dropped: a Go test binary rejects them.
	script := "#!/bin/sh\n" +
		"# Stands in for audiocpp_server by re-executing the test binary, which then\n" +
		"# answers /health and /v1/audio/transcriptions on the configured port.\n" +
		"config=''\n" +
		"previous=''\n" +
		"for argument in \"$@\"; do\n" +
		"  if [ \"$previous\" = '--config' ]; then config=\"$argument\"; fi\n" +
		"  previous=\"$argument\"\n" +
		"done\n" +
		"VOICEIME_TEST_SERVER=1\n" +
		"VOICEIME_TEST_BINARY='" + os.Args[0] + "'\n" +
		"VOICEIME_TEST_CONFIG=\"$config\"\n" +
		"export VOICEIME_TEST_SERVER VOICEIME_TEST_BINARY VOICEIME_TEST_CONFIG\n" +
		"exec \"$VOICEIME_TEST_BINARY\" -test.run='TestStandsInForTheServer'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stand-in server: %v", err)
	}
	return path
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func specFor(t *testing.T, port int, binary string) Spec {
	t.Helper()
	dir := t.TempDir()
	return Spec{
		Binary:     func() (string, error) { return binary, nil },
		Model:      "/models/refine.gguf",
		Family:     func() (string, error) { return "qwen3_asr", nil },
		ModelID:    DefaultID,
		Host:       "127.0.0.1",
		Port:       port,
		Backend:    "cpu",
		Threads:    2,
		ConfigPath: filepath.Join(dir, "server.json"),
		LogPath:    filepath.Join(dir, "server.log"),
		Autostart:  true,
	}
}

// The on-demand path, end to end: nothing is listening, so a process is started,
// it answers, the generated config says what the repair route needs, and Stop ends
// it. This is the code that decides whether the whole feature works, and every part
// of it is a place where a mistake leaves a process or a port behind.
func TestEnsureStartsAServerAndStopEndsIt(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh is needed for the stand-in server")
	}
	port := freePort(t)
	spec := specFor(t, port, fakeServerBinary(t, t.TempDir()))
	spec.RequestOptions = map[string]string{"audio_chunk_mode": "none"}
	spec.StartTimeout = 15 * time.Second

	instance, err := Ensure(context.Background(), spec)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !instance.Owned() {
		t.Error("Ensure did not report the server as its own, so nothing would stop it")
	}
	if instance.ModelID != DefaultID {
		t.Errorf("model id = %q, want %q", instance.ModelID, DefaultID)
	}

	// The generated config is what the server reads, so it has to describe the
	// offline entry and the request options a multipart upload cannot carry.
	raw, err := os.ReadFile(spec.ConfigPath)
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}
	var config wireConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatalf("generated config is not json: %v", err)
	}
	if len(config.Models) != 1 {
		t.Fatalf("config has %d models, want 1: the streaming layer stays on the CLI route",
			len(config.Models))
	}
	model := config.Models[0]
	if model.Mode != "offline" || model.Task != "asr" || model.Family != "qwen3_asr" {
		t.Errorf("model entry = %+v, want an offline asr entry for the refine family", model)
	}
	if model.DefaultRequestOptions["audio_chunk_mode"] != "none" {
		t.Errorf("default_request_options = %v, want the repair route's chunk mode",
			model.DefaultRequestOptions)
	}
	if config.Port != port {
		t.Errorf("config port = %d, want %d", config.Port, port)
	}

	if err := instance.Warm(context.Background(), 10*time.Second); err != nil {
		t.Errorf("Warm: %v", err)
	}

	instance.Stop()
	if err := probe(context.Background(), instance.BaseURL); err == nil {
		t.Error("the server is still answering after Stop")
	}
}

// A server that is already listening is used as it is and left alone: it belongs
// to whoever started it, and a dictation session must not end someone else's
// daemon.
func TestEnsureUsesAServerItDidNotStart(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"refine"}]}`))
	})
	existing := httptest.NewServer(mux)
	defer existing.Close()

	_, portText, err := net.SplitHostPort(strings.TrimPrefix(existing.URL, "http://"))
	if err != nil {
		t.Fatalf("split %s: %v", existing.URL, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("port %q: %v", portText, err)
	}

	// A spec whose binary and family both fail: neither may be consulted when a
	// server is already there, or a user running their own server would be refused
	// for lacking a local build.
	spec := Spec{
		Binary:  func() (string, error) { return "", os.ErrNotExist },
		Family:  func() (string, error) { return "", os.ErrNotExist },
		Model:   "/models/refine.gguf",
		ModelID: "refine",
		Host:    "127.0.0.1",
		Port:    port,
	}
	instance, err := Ensure(context.Background(), spec)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if instance.Owned() {
		t.Error("Ensure claimed a server it did not start, so a session would stop the user's")
	}
	instance.Stop()
	if err := probe(context.Background(), instance.BaseURL); err != nil {
		t.Errorf("the server was gone after Stop: %v", err)
	}
}

// Autostart off means exactly that: a repair route that cannot start anything
// alongside a server the user runs somewhere else they have not started yet.
func TestEnsureRefusesToStartWhenAutostartIsOff(t *testing.T) {
	spec := specFor(t, freePort(t), fakeServerBinary(t, t.TempDir()))
	spec.Autostart = false
	_, err := Ensure(context.Background(), spec)
	if err == nil {
		t.Fatal("Ensure started a server with autostart off")
	}
	if !strings.Contains(err.Error(), "asr_server_autostart") {
		t.Errorf("error %q does not name the setting that prevented it", err)
	}
}

// A server that dies during startup has to say why, and the reason is only in its
// log. Silence here would be a dictation session that quietly repairs nothing.
func TestEnsureReportsWhyAStartFailed(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "dying-server")
	script := "#!/bin/sh\n" +
		"echo 'audiocpp_server failed: missing required json key: family' >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write dying server: %v", err)
	}
	spec := specFor(t, freePort(t), binary)
	spec.StartTimeout = 5 * time.Second

	_, err := Ensure(context.Background(), spec)
	if err == nil {
		t.Fatal("Ensure succeeded for a server that exits immediately")
	}
	if !strings.Contains(err.Error(), "family") {
		t.Errorf("error %q does not carry the server's own reason", err)
	}
}

// A server that starts but never answers is stopped rather than left running, and
// the failure names the address it never answered on.
func TestEnsureStopsAServerThatNeverAnswers(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "silent-server")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nsleep 60\n"), 0o755); err != nil {
		t.Fatalf("write silent server: %v", err)
	}
	spec := specFor(t, freePort(t), binary)
	spec.StartTimeout = 700 * time.Millisecond

	started := time.Now()
	_, err := Ensure(context.Background(), spec)
	if err == nil {
		t.Fatal("Ensure succeeded for a server that never answers")
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("Ensure took %v to give up on a silent server", elapsed)
	}
	if !strings.Contains(err.Error(), spec.Host) {
		t.Errorf("error %q does not name the address", err)
	}
	// The child is gone: nothing may be left holding the port.
	waitForNoListener(t, spec.Port)
}

func waitForNoListener(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err == nil {
			listener.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("port %d is still held after the failed start", port)
}

func TestParseURL(t *testing.T) {
	cases := []struct {
		raw      string
		wantHost string
		wantPort int
		wantErr  bool
	}{
		{raw: "http://127.0.0.1:18080", wantHost: "127.0.0.1", wantPort: 18080},
		{raw: "http://localhost:8080/", wantHost: "localhost", wantPort: 8080},
		{raw: "https://example.com", wantHost: "example.com", wantPort: 443},
		{raw: "http://example.com", wantHost: "example.com", wantPort: 80},
		{raw: "127.0.0.1:18080", wantErr: true},
		{raw: "", wantErr: true},
		{raw: "http://:18080", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			host, port, err := ParseURL(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseURL(%q) succeeded, want an error", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseURL(%q): %v", tc.raw, err)
			}
			if host != tc.wantHost || port != tc.wantPort {
				t.Errorf("ParseURL(%q) = %s:%d, want %s:%d", tc.raw, host, port, tc.wantHost, tc.wantPort)
			}
		})
	}
}

// Refusing to start a process for a remote address is the difference between a
// feature and a surprise: nothing here should spawn anything for another machine.
func TestValidateRefusesToStartARemoteServer(t *testing.T) {
	spec := specFor(t, 8080, "/usr/bin/true")
	spec.Host = "192.0.2.10"
	if err := spec.validate(); err == nil {
		t.Fatal("validate accepted a remote host for autostart")
	} else if !strings.Contains(err.Error(), "this machine") {
		t.Errorf("error %q does not explain the refusal", err)
	}
}

func TestValidateNeedsAModelToServe(t *testing.T) {
	spec := specFor(t, 8080, "/usr/bin/true")
	spec.Model = ""
	if err := spec.validate(); err == nil {
		t.Fatal("validate accepted a server with no model to serve")
	}
}
