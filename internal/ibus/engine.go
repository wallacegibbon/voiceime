package ibus

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/godbus/dbus/v5"
)

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
type IBusText struct {
	TypeName    string                  // always "IBusText"
	Attachments map[string]dbus.Variant // empty in practice
	Text        string
	Attributes  dbus.Variant // an IBusAttrList; empty list is fine
}

func NewText(text string) IBusText {
	return IBusText{
		TypeName:    "IBusText",
		Attachments: map[string]dbus.Variant{},
		Text:        text,
		Attributes:  dbus.MakeVariant(newAttrList()),
	}
}

func NewPreeditText(text string) IBusText {
	payload := NewText(text)
	length := uint32(utf8.RuneCountInString(text))
	if length == 0 {
		return payload
	}
	list := newAttrList()
	list.Attributes = append(list.Attributes, dbus.MakeVariant(IBusAttribute{
		TypeName:    "IBusAttribute",
		Attachments: map[string]dbus.Variant{},
		Type:        attrTypeUnderline,
		Value:       attrUnderlineSingle,
		StartIndex:  0,
		EndIndex:    length,
	}))
	payload.Attributes = dbus.MakeVariant(list)
	return payload
}

type IBusAttribute struct {
	TypeName    string
	Attachments map[string]dbus.Variant
	Type        uint32 // IBUS_ATTR_TYPE_*
	Value       uint32 // IBUS_ATTR_UNDERLINE_* for an underline
	StartIndex  uint32
	EndIndex    uint32
}

const (
	attrTypeUnderline   = 1 // IBUS_ATTR_TYPE_UNDERLINE
	attrUnderlineSingle = 1 // IBUS_ATTR_UNDERLINE_SINGLE
)

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

func trace(format string, args ...interface{}) {
	path := os.Getenv("VOICEIME_IBUS_TRACE")
	if path == "" {
		return
	}
	// 0600: the session log shares this file, and it quotes what the user dictated.
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
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
	// KeyEvent is offered every key event - press and release, the latter marked by
	// ReleaseMask in the state. Returning true consumes it, which only the
	// push-to-talk trigger does: everything else belongs to the application.
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

	if err := conn.Export(&factoryObject{service: s}, FactoryPath, ifaceIBusFactory); err != nil {
		return fmt.Errorf("export the factory: %w", err)
	}
	if err := conn.Export(&serviceObject{service: s}, FactoryPath, ifaceIBusService); err != nil {
		return fmt.Errorf("export the service interface: %w", err)
	}

	// The daemon looks for the engine process under the component name, so this
	// name is not a formatting choice: it is what makes the engine reachable.
	reply, err := conn.RequestName(s.Component.Name, dbus.NameFlagDoNotQueue)
	if err != nil {
		return fmt.Errorf("request the name %s: %w", s.Component.Name, err)
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		return fmt.Errorf("the name %s is already taken (reply %v)", s.Component.Name, reply)
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
	// The payload carries an underline attribute, which is what draws the
	// composition underline in the application; see NewPreeditText.
	return c.conn.Emit(c.path, signalPreedit, dbus.MakeVariant(NewPreeditText(text)), uint32(cursor), visible, uint32(0))
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

	mu     sync.Mutex
	active bool
}

// Methods the daemon calls. Most are no-ops: voiceime does not compose text from
// keystrokes, so the interesting ones are Enable and Disable.

// Enable is called once per input context this engine becomes active in.
func (e *engineObject) Enable() *dbus.Error {
	trace("Enable()")
	e.mu.Lock()
	repeated := e.active
	e.active = true
	handler, testText := e.service.Handler, e.service.EnableText
	e.mu.Unlock()
	if repeated {
		return nil
	}

	switch {
	case handler != nil:
		handler.Enable(e.control())
	case testText != "":
		// No pipeline attached: this is a diagnostic run, whose only purpose is to
		// prove text reaches an application.
		if err := e.control().Commit(testText); err != nil {
			return dbus.MakeFailedError(err)
		}
	}
	return nil
}

func (e *engineObject) Disable() *dbus.Error {
	trace("Disable()")
	e.mu.Lock()
	wasActive := e.active
	e.active = false
	handler := e.service.Handler
	e.mu.Unlock()
	// Only a context that was enabled is one the handler is counting.
	if handler != nil && wasActive {
		handler.Disable()
	}
	return nil
}

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

// propertiesObject serves the properties IBus declares on an engine.
type propertiesObject struct{ engine *engineObject }

func (p *propertiesObject) Get(iface, name string) (dbus.Variant, *dbus.Error) {
	trace("Properties.Get(%s.%s)", iface, name)
	switch name {
	case "HasFocusId", "FocusId":
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
	// Both spellings, so a daemon of either generation finds what it looks for; a key it
	// does not ask about costs nothing.
	return map[string]dbus.Variant{
		"HasFocusId":            dbus.MakeVariant(true),
		"FocusId":               dbus.MakeVariant(true),
		"ActiveSurroundingText": dbus.MakeVariant(false),
	}, nil
}

func (p *propertiesObject) Set(iface, name string, value dbus.Variant) *dbus.Error {
	trace("Properties.Set(%s.%s)", iface, name)
	// ContentType is the only writable property and voiceime does not vary its
	// behaviour by input purpose, so accepting and ignoring is correct.
	return nil
}
