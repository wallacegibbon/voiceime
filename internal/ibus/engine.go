package ibus

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

// The D-Bus surface, established by capturing a live engine (see
// docs/ibus-protocol.md). IBus serialises its objects with the type name as the
// first field, then an attachment dictionary, then the object's own fields. The
// signatures below are not guesses: each was read off the wire.
const (
	// IBusText: (type name, attachments, text, attributes variant)
	signatureText = "(sa{sv}sv)"
	// IBusAttrList: (type name, attachments, attributes)
	signatureAttrList = "(sa{sv}as)"

	ifaceIBusEngine  = "org.freedesktop.IBus.Engine"
	ifaceIBusFactory = "org.freedesktop.IBus.Factory"
	ifaceIBusService = "org.freedesktop.IBus.Service"
	ifaceDBusProps   = "org.freedesktop.DBus.Properties"
	ifaceIntrospect  = "org.freedesktop.DBus.Introspectable"
	signalCommitText = ifaceIBusEngine + ".CommitText"
	signalPreedit    = ifaceIBusEngine + ".UpdatePreeditText"
	signalForwardKey = ifaceIBusEngine + ".ForwardKeyEvent"
)

// IBusText is the payload of CommitText and UpdatePreeditText.
//
// The field order and types are exactly what the capture showed:
//
//	variant struct { string "IBusText"; array []; string "你好"; variant struct {...} }
type IBusText struct {
	TypeName    string                  // always "IBusText"
	Attachments map[string]dbus.Variant // empty in practice
	Text        string
	Attributes  dbus.Variant // an IBusAttrList; empty list is fine
}

// NewText builds a text payload with no attributes, which is what a plain commit
// needs: attributes describe preedit underlining and are only meaningful for text
// the application shows as a composition.
func NewText(text string) IBusText {
	return IBusText{
		TypeName:    "IBusText",
		Attachments: map[string]dbus.Variant{},
		Text:        text,
		Attributes:  dbus.MakeVariant(newAttrList()),
	}
}

type ibusAttrList struct {
	TypeName    string
	Attachments map[string]dbus.Variant
	Attributes  []dbus.Variant
}

func newAttrList() ibusAttrList {
	return ibusAttrList{
		TypeName:    "IBusAttrList",
		Attachments: map[string]dbus.Variant{},
		Attributes:  []dbus.Variant{},
	}
}

// trace records every call the daemon makes, when VOICEIME_IBUS_TRACE names a
// file. The engine is spawned detached by ibus-daemon, so its stdout is lost;
// this is the only way to see where an activation stalls.
func trace(format string, args ...interface{}) {
	path := os.Getenv("VOICEIME_IBUS_TRACE")
	if path == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer file.Close()
	fmt.Fprintf(file, format+"\n", args...)
}

// Control is what a handler uses to publish text through the engine.
type Control interface {
	// Commit delivers text to the focused application. This is final: no
	// application on this desktop supports having committed text rewritten.
	Commit(text string) error
	// Preedit shows text as an in-progress composition at the caret. It is the
	// right home for provisional, still-correctable text.
	Preedit(text string, cursor int, visible bool) error
	// DeleteSurroundingText asks the application to drop the last n characters,
	// which only some applications support. Present because it is cheap, not
	// because anything relies on it. (Signature not verified on the wire.)
	DeleteSurroundingText(offset int32, chars uint32) error
}

// Handler is the dictation pipeline seen from the engine's side.
type Handler interface {
	// Enable is called when this engine becomes the active input method.
	Enable(control Control)
	// Disable is called when it stops being active.
	Disable()
	// KeyEvent is offered every key press. Returning true consumes it.
	KeyEvent(keyval, keycode, state uint32) bool
}

// Service owns the D-Bus side: the well-known name, the factory and the engine
// instances the daemon asks for.
type Service struct {
	Component Component
	Handler   Handler
	// EnableText, when non-empty, is committed by the default handler on Enable.
	// It exists so injection can be verified without speaking into a microphone,
	// which is what made the protocol investigation possible.
	EnableText string

	conn *dbus.Conn

	mu           sync.Mutex
	engines      map[dbus.ObjectPath]*engineObject
	nextEngineID int

	// closed is shut exactly once. Two paths can decide to end the service - a
	// failed liveness ping, and the daemon asking the component to Destroy - and
	// D-Bus dispatches each incoming call on its own goroutine, so they can run at
	// the same time. A check-then-close could therefore close twice and panic.
	closed    chan struct{}
	closeOnce sync.Once
}

// shutdown ends the service; safe to call from any goroutine, any number of times.
func (s *Service) shutdown() {
	s.closeOnce.Do(func() { close(s.closed) })
}

// Ping connects to an address and pings the bus, which is how a stale registry
// file is told apart from the live daemon's.
func Ping(address string) error {
	conn, err := dbus.Dial(address)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.Auth(nil); err != nil {
		return err
	}
	if err := conn.Hello(); err != nil {
		return err
	}
	return conn.BusObject().Call("org.freedesktop.DBus.Peer.Ping", 0).Err
}

// Run connects, registers the name and factory, and serves until ctx is done.
func Run(ctx context.Context, component Component, handler Handler, address string) error {
	service := &Service{
		Component: component,
		Handler:   handler,
		engines:   map[dbus.ObjectPath]*engineObject{},
		closed:    make(chan struct{}),
	}
	if err := service.connect(address); err != nil {
		return err
	}
	defer service.close()

	select {
	case <-ctx.Done():
		return nil
	case <-service.closed:
		return fmt.Errorf("IBus daemon closed the connection")
	}
}

func (s *Service) connect(address string) error {
	conn, err := dbus.Dial(address)
	if err != nil {
		return fmt.Errorf("connect to the IBus bus at %s: %w", address, err)
	}
	if err := conn.Auth(nil); err != nil {
		conn.Close()
		return fmt.Errorf("authenticate with the IBus bus: %w", err)
	}
	if err := conn.Hello(); err != nil {
		conn.Close()
		return fmt.Errorf("introduce ourselves to the IBus bus: %w", err)
	}
	s.conn = conn

	// The daemon looks for the engine process under the component name, so this
	// name is not a formatting choice: it is what makes the engine reachable.
	reply, err := conn.RequestName(s.Component.Name, dbus.NameFlagDoNotQueue)
	if err != nil {
		return fmt.Errorf("request the name %s: %w", s.Component.Name, err)
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		return fmt.Errorf("the name %s is already taken (reply %v)", s.Component.Name, reply)
	}

	if err := conn.Export(&factoryObject{service: s}, FactoryPath, ifaceIBusFactory); err != nil {
		return fmt.Errorf("export the factory: %w", err)
	}
	if err := conn.Export(&serviceObject{service: s}, FactoryPath, ifaceIBusService); err != nil {
		return fmt.Errorf("export the service interface: %w", err)
	}

	// The daemon closes its side when it exits; noticing that is better than
	// staying alive as an orphan with a name nobody will ever ask for.
	go func() {
		for {
			select {
			case <-s.closed:
				return
			case <-time.After(time.Second):
				// Peer.Ping is the cheapest proof the daemon is still there.
				if call := conn.BusObject().Call("org.freedesktop.DBus.Peer.Ping", 0); call.Err != nil {
					s.shutdown()
					return
				}
			}
		}
	}()
	return nil
}

func (s *Service) close() {
	s.mu.Lock()
	engines := make([]*engineObject, 0, len(s.engines))
	for _, engine := range s.engines {
		engines = append(engines, engine)
	}
	s.mu.Unlock()

	for _, engine := range engines {
		engine.shutdown()
	}
	if s.conn != nil {
		s.conn.Close()
	}
}

// engine returns an existing instance or creates one, which is what
// Factory.CreateEngine needs.
func (s *Service) engine(name string) (dbus.ObjectPath, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextEngineID++
	path := dbus.ObjectPath(fmt.Sprintf("%s/%d", EnginePathPrefix, s.nextEngineID))
	instance := &engineObject{
		service: s,
		path:    path,
		name:    name,
	}

	if err := s.conn.Export(instance, path, ifaceIBusEngine); err != nil {
		return "", fmt.Errorf("export the engine object: %w", err)
	}
	if err := s.conn.Export(&propertiesObject{engine: instance}, path, ifaceDBusProps); err != nil {
		return "", fmt.Errorf("export the engine properties: %w", err)
	}
	if err := s.conn.Export(instance, path, ifaceIBusService); err != nil {
		return "", fmt.Errorf("export the engine service interface: %w", err)
	}
	s.engines[path] = instance
	return path, nil
}

func (s *Service) drop(path dbus.ObjectPath) {
	s.mu.Lock()
	instance := s.engines[path]
	delete(s.engines, path)
	s.mu.Unlock()

	if instance != nil {
		instance.shutdown()
		// Every interface exported for this path has to go, or the daemon could
		// still call Destroy (and Properties) on an engine it already discarded.
		for _, iface := range []string{ifaceIBusEngine, ifaceIBusService, ifaceDBusProps} {
			_ = s.conn.Export(nil, path, iface)
		}
	}
}

// --------------------------------------------------------------------------- //
// D-Bus objects
// --------------------------------------------------------------------------- //

type factoryObject struct{ service *Service }

// CreateEngine is the only method the daemon needs from a factory.
func (f *factoryObject) CreateEngine(name string) (dbus.ObjectPath, *dbus.Error) {
	trace("CreateEngine(%q)", name)
	path, err := f.service.engine(name)
	if err != nil {
		return "/", dbus.MakeFailedError(err)
	}
	return path, nil
}

// Destroy is part of org.freedesktop.IBus.Service: the daemon asks a component to
// go away when its engines are no longer wanted.
func (f *factoryObject) Destroy() *dbus.Error {
	f.service.mu.Lock()
	empty := len(f.service.engines) == 0
	f.service.mu.Unlock()
	if empty {
		f.service.shutdown()
	}
	return nil
}

type serviceObject struct{ service *Service }

func (s *serviceObject) Destroy() *dbus.Error { return (&factoryObject{service: s.service}).Destroy() }

// engineControl publishes text for one engine instance.
//
// It is deliberately a separate type from engineObject. godbus exports *every*
// exported method of whatever object it is handed, under the interface name it is
// given, so an engine object that also implemented Commit/Preedit would expose
// those as D-Bus methods the daemon could call - harmless today, but it means the
// wire surface would silently grow with every helper added here.
type engineControl struct {
	conn *dbus.Conn
	path dbus.ObjectPath
}

func (c *engineControl) Commit(text string) error {
	if text == "" {
		return nil
	}
	return c.conn.Emit(c.path, signalCommitText, dbus.MakeVariant(NewText(text)))
}

func (c *engineControl) Preedit(text string, cursor int, visible bool) error {
	// mode 0 is IBus's "clear and replace" update; the engine has no other mode.
	return c.conn.Emit(c.path, signalPreedit, dbus.MakeVariant(NewText(text)), uint32(cursor), visible, uint32(0))
}

func (c *engineControl) DeleteSurroundingText(offset int32, chars uint32) error {
	return c.conn.Emit(c.path, ifaceIBusEngine+".DeleteSurroundingText", offset, chars)
}

func (e *engineObject) control() Control {
	return &engineControl{conn: e.service.conn, path: e.path}
}

// engineObject is one input context's engine instance.
type engineObject struct {
	service *Service
	path    dbus.ObjectPath
	name    string

	mu          sync.Mutex
	active      bool
	placeholder bool // true when a test handler is standing in for the pipeline
}

// Methods the daemon calls. Most are no-ops: voiceime does not compose text from
// keystrokes, so the interesting ones are Enable and Disable.

func (e *engineObject) Enable() *dbus.Error {
	trace("Enable()")
	e.mu.Lock()
	e.active = true
	handler, testText := e.service.Handler, e.service.EnableText
	e.mu.Unlock()

	switch {
	case handler != nil:
		handler.Enable(e.control())
	case testText != "":
		// No pipeline attached: this is a diagnostic run, whose only purpose is to
		// prove text reaches an application.
		e.mu.Lock()
		e.placeholder = true
		e.mu.Unlock()
		if err := e.control().Commit(testText); err != nil {
			return dbus.MakeFailedError(err)
		}
	}
	return nil
}

func (e *engineObject) Disable() *dbus.Error {
	trace("Disable()")
	e.mu.Lock()
	e.active = false
	handler := e.service.Handler
	e.mu.Unlock()
	if handler != nil {
		handler.Disable()
	}
	return nil
}

// ProcessKeyEvent returns false: every key belongs to the application. Consuming
// keys would make voiceime a keyboard layout, which it is not.
func (e *engineObject) ProcessKeyEvent(keyval, keycode, state uint32) (bool, *dbus.Error) {
	trace("ProcessKeyEvent(%d,%d,%d)", keyval, keycode, state)
	e.mu.Lock()
	handler := e.service.Handler
	e.mu.Unlock()
	if handler == nil {
		return false, nil
	}
	return handler.KeyEvent(keyval, keycode, state), nil
}

func (e *engineObject) FocusIn() *dbus.Error  { return nil }
func (e *engineObject) FocusOut() *dbus.Error { return nil }
func (e *engineObject) Reset() *dbus.Error    { return nil }
func (e *engineObject) Disable_() *dbus.Error { return nil }

func (e *engineObject) FocusInId(objectPath, client string) *dbus.Error           { return nil }
func (e *engineObject) FocusOutId(objectPath string) *dbus.Error                  { return nil }
func (e *engineObject) SetCapabilities(caps uint32) *dbus.Error                   { return nil }
func (e *engineObject) PropertyActivate(name string, state uint32) *dbus.Error    { return nil }
func (e *engineObject) PropertyShow(name string) *dbus.Error                      { return nil }
func (e *engineObject) PropertyHide(name string) *dbus.Error                      { return nil }
func (e *engineObject) CandidateClicked(index, button, state uint32) *dbus.Error  { return nil }
func (e *engineObject) PageUp() *dbus.Error                                       { return nil }
func (e *engineObject) PageDown() *dbus.Error                                     { return nil }
func (e *engineObject) CursorUp() *dbus.Error                                     { return nil }
func (e *engineObject) CursorDown() *dbus.Error                                   { return nil }
func (e *engineObject) CancelHandWriting(strokes uint32) *dbus.Error              { return nil }
func (e *engineObject) ProcessHandWritingEvent(coordinates []float64) *dbus.Error { return nil }
func (e *engineObject) PanelExtensionReceived(event dbus.Variant) *dbus.Error     { return nil }
func (e *engineObject) PanelExtensionRegisterKeys(data dbus.Variant) *dbus.Error  { return nil }

func (e *engineObject) SetCursorLocation(x, y, w, h int32) *dbus.Error {
	trace("SetCursorLocation(%d,%d,%d,%d)", x, y, w, h)
	return nil
}

func (e *engineObject) SetSurroundingText(text dbus.Variant, cursorPos, anchorPos uint32) *dbus.Error {
	return nil
}

// Destroy releases this instance, which the daemon does when an input context
// goes away.
func (e *engineObject) Destroy() *dbus.Error {
	e.service.drop(e.path)
	return nil
}

func (e *engineObject) shutdown() {
	e.mu.Lock()
	active, handler := e.active, e.service.Handler
	e.active = false
	e.mu.Unlock()
	if active && handler != nil {
		handler.Disable()
	}
}

// propertiesObject serves the three properties IBus declares on an engine.
// ContentType is written by the daemon and never read; the two read-only
// properties report capabilities voiceime does not have, which is correct:
// answering "no" is better than claiming surrounding-text support and failing.
type propertiesObject struct{ engine *engineObject }

func (p *propertiesObject) Get(iface, name string) (dbus.Variant, *dbus.Error) {
	trace("Properties.Get(%s.%s)", iface, name)
	switch name {
	case "FocusId":
		return dbus.MakeVariant(true), nil
	case "ActiveSurroundingText":
		return dbus.MakeVariant(false), nil
	case "ContentType":
		return dbus.MakeVariant([]uint32{0, 0}), nil
	}
	return dbus.MakeVariant(""), dbus.MakeFailedError(fmt.Errorf("unknown property %s", name))
}

func (p *propertiesObject) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	trace("Properties.GetAll(%s)", iface)
	return map[string]dbus.Variant{
		"FocusId":               dbus.MakeVariant(true),
		"ActiveSurroundingText": dbus.MakeVariant(false),
		"ContentType":           dbus.MakeVariant([]uint32{0, 0}),
	}, nil
}

func (p *propertiesObject) Set(iface, name string, value dbus.Variant) *dbus.Error {
	trace("Properties.Set(%s.%s)", iface, name)
	// ContentType is the only writable property and voiceime does not vary its
	// behaviour by input purpose, so accepting and ignoring is correct.
	return nil
}
