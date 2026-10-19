// Package asr is the client side of the recognition pipeline: one streaming session
// against a resident audio.cpp server, and the offline decode that finalises what it
// drafts.
//
// There is no process to start. The server is a service on the machine (a systemd
// user unit whose models are loaded at startup), so a dictation session is a
// connection and a repair is a request. That is the whole shape of this package now:
// what used to be two subprocess adapters - one speaking a stdin protocol, one
// spawning a process per utterance - is two HTTP calls.
package asr

import "time"

// EventKind classifies what the streaming session reported.
type EventKind string

const (
	// EventReady means the session is open and audio may be written.
	EventReady EventKind = "ready"
	// EventPartial is text decoded since the previous partial. It is provisional: a
	// window-based streaming model can still change what it heard, and the repair
	// pass will.
	EventPartial EventKind = "partial"
	// EventSnapshot replaces the entire provisional text. Only a revision-capable
	// model emits it, and none voiceime selects does today; the framework defines it,
	// so the transcript model honours it.
	EventSnapshot EventKind = "snapshot"
	// EventSpeechStart / EventSpeechEnd are the model's own VAD, when it has one.
	// They are forwarded rather than relied upon: voiceime's endpointer is what
	// decides where an utterance begins and ends.
	EventSpeechStart EventKind = "speech_start"
	EventSpeechEnd   EventKind = "speech_end"
)

// Event is one observation from the running session.
type Event struct {
	Kind EventKind `json:"kind"`
	Text string    `json:"text,omitempty"`
	// FedSeconds is how much audio had been written when this arrived. With a
	// real-time feed this is the latency figure that matters: the time from "the
	// user said it" to "text appeared".
	FedSeconds float64 `json:"fed_seconds"`
	// SinceStart is wall time since the session opened.
	SinceStart time.Duration `json:"-"`
	// Sample is the model's own position for voice-activity events.
	Sample      int64   `json:"sample,omitempty"`
	Probability float64 `json:"probability,omitempty"`
}

// Result is what a finished session produced.
type Result struct {
	Text      string            `json:"text"`
	Metrics   map[string]string `json:"metrics,omitempty"`
	LoadTime  time.Duration     `json:"-"`
	TotalTime time.Duration     `json:"-"`
	AudioFed  time.Duration     `json:"-"`
	// ProcessErr is what ended the session badly, if anything did: the server
	// answered with an error event, or the connection ended without the
	// terminating chunk. Close returns the same value, so a caller that checks
	// only one of the two cannot lose the failure.
	ProcessErr error `json:"-"`
}
