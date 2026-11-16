package companionui

// Run stays in the interactive user's session until ctx ends. Opening/hiding
// the panel never starts privileged processes or changes service lifetime.
// Backend must honour contexts; all calls are serialised, bounded to 10s.
// Nil Requests is allowed. Bindings defaults must be supplied explicitly.
type Options struct {
	InitiallyOpen bool
	Requests      <-chan OpenRequest
	Shortcuts     [4]Shortcut
	SaveShortcuts func([4]Shortcut) error
}
