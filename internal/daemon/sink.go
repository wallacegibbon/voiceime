package daemon

import (
	"sync"

	"github.com/wallacegibbon/voiceime/internal/ibus"
)

type switchableSink struct {
	mu      sync.Mutex
	control ibus.Control
}

// use points the sink at an input context.
func (s *switchableSink) use(control ibus.Control) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.control = control
}

// target returns where text would go now.
func (s *switchableSink) target() ibus.Control {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.control
}

func (s *switchableSink) Commit(text string) error {
	if control := s.target(); control != nil {
		return control.Commit(text)
	}
	return nil
}

func (s *switchableSink) Preedit(text string, cursor int, visible bool) error {
	if control := s.target(); control != nil {
		return control.Preedit(text, cursor, visible)
	}
	return nil
}
