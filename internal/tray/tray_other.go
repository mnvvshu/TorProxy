//go:build !windows

package tray

// Tray is a no-op placeholder on non-Windows platforms.
type Tray struct{}

// New always fails with ErrUnsupported on non-Windows platforms.
func New(tooltip string, cb Callbacks) (*Tray, error) { return nil, ErrUnsupported }

func (t *Tray) SetStatus(s Status, tooltip string)   {}
func (t *Tray) SetMenuState(ms MenuState)            {}
func (t *Tray) Notify(title, text string, level int) {}
func (t *Tray) Close()                               {}
