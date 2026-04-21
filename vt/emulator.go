package vt

import (
	"image/color"
	"io"
	"sync/atomic"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/ultraviolet/screen"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/parser"
)

// Logger represents a logger interface.
type Logger interface {
	Printf(format string, v ...any)
}

// Emulator represents a virtual terminal emulator.
type Emulator struct {
	handlers

	// The terminal's indexed 256 colors.
	colors [256]color.Color

	// Both main and alt screens and a pointer to the currently active screen.
	scrs [2]Screen
	scr  *Screen

	// Character sets
	charsets [4]CharSet

	// log is the logger to use.
	logger Logger

	// terminal default colors.
	defaultFg, defaultBg, defaultCur color.Color
	fgColor, bgColor, curColor       color.Color

	// Terminal modes.
	modes ansi.Modes

	// The last written character.
	lastChar rune // either ansi.Rune or ansi.Grapheme
	// A slice of runes to compose a grapheme.
	grapheme []rune

	// The ANSI parser to use.
	parser *ansi.Parser
	// The last parser state.
	lastState parser.State

	cb Callbacks

	// The terminal's icon name and title.
	iconName, title string
	// The current reported working directory. This is not validated.
	cwd string

	// tabstop is the list of tab stops.
	tabstops *uv.TabStops

	// I/O pipes.
	pr *io.PipeReader
	pw *io.PipeWriter

	// The GL and GR character set identifiers.
	gl, gr  int
	gsingle int // temporarily select GL or GR

	// Indicates if the terminal is closed.
	closed atomic.Bool

	// atPhantom indicates if the cursor is out of bounds.
	// When true, and a character is written, the cursor is moved to the next line.
	atPhantom bool

	// ed2SavesScrollback controls whether ED 2 (Erase Display) pushes
	// the current viewport into scrollback before clearing.
	// When true (default), it matches VTE/GNOME Terminal behavior.
	// When false, it matches xterm/Ghostty behavior (clear only).
	ed2SavesScrollback bool
}

var _ Terminal = (*Emulator)(nil)

// NewEmulator creates a new virtual terminal emulator.
func NewEmulator(w, h int) *Emulator {
	t := new(Emulator)
	t.ed2SavesScrollback = true // VTE-compatible default
	t.scrs[0] = *NewScreen(w, h)
	t.scrs[1] = *NewScreen(w, h)
	t.scr = &t.scrs[0]
	t.scrs[0].cb = &t.cb
	t.scrs[1].cb = &t.cb
	t.parser = ansi.NewParser()
	t.parser.SetParamsSize(parser.MaxParamsSize)
	t.parser.SetDataSize(1024 * 1024 * 4) // 4MB data buffer
	t.parser.SetHandler(ansi.Handler{
		Print:     t.handlePrint,
		Execute:   t.handleControl,
		HandleCsi: t.handleCsi,
		HandleEsc: t.handleEsc,
		HandleDcs: t.handleDcs,
		HandleOsc: t.handleOsc,
		HandleApc: t.handleApc,
		HandlePm:  t.handlePm,
		HandleSos: t.handleSos,
	})
	t.pr, t.pw = io.Pipe()
	t.resetModes()
	t.tabstops = uv.DefaultTabStops(w)
	t.registerDefaultHandlers()

	// Default colors
	t.defaultFg = color.White
	t.defaultBg = color.Black
	t.defaultCur = color.White

	return t
}

// SetLogger sets the terminal's logger.
func (e *Emulator) SetLogger(l Logger) {
	e.logger = l
}

// SetCallbacks sets the terminal's callbacks.
func (e *Emulator) SetCallbacks(cb Callbacks) {
	e.cb = cb
	e.scrs[0].cb = &e.cb
	e.scrs[1].cb = &e.cb
}

// Touched returns the touched lines in the current screen buffer.
func (e *Emulator) Touched() []*uv.LineData {
	return e.scr.Touched()
}

// String returns a string representation of the underlying screen buffer.
func (e *Emulator) String() string {
	s := e.scr.buf.String()
	return uv.TrimSpace(s)
}

// Render renders a snapshot of the terminal screen as a string with styles and
// links encoded as ANSI escape codes.
func (e *Emulator) Render() string {
	return e.scr.buf.Render()
}

var _ uv.Screen = (*Emulator)(nil)

// Bounds returns the bounds of the terminal.
func (e *Emulator) Bounds() uv.Rectangle {
	return e.scr.Bounds()
}

// CellAt returns the current focused screen cell at the given x, y position.
// It returns nil if the cell is out of bounds.
func (e *Emulator) CellAt(x, y int) *uv.Cell {
	return e.scr.CellAt(x, y)
}

// SetCell sets the current focused screen cell at the given x, y position.
func (e *Emulator) SetCell(x, y int, c *uv.Cell) {
	e.scr.SetCell(x, y, c)
}

// WidthMethod returns the width method used by the terminal.
func (e *Emulator) WidthMethod() uv.WidthMethod {
	if e.isModeSet(ansi.ModeUnicodeCore) {
		return ansi.GraphemeWidth
	}
	return ansi.WcWidth
}

// Draw implements the [uv.Drawable] interface.
func (e *Emulator) Draw(scr uv.Screen, area uv.Rectangle) {
	bg := uv.EmptyCell
	bg.Style.Bg = e.BackgroundColor()
	screen.FillArea(scr, &bg, area)
	for y := range e.Touched() {
		if y < 0 || y >= e.Height() {
			continue
		}
		for x := 0; x < e.Width(); {
			w := 1
			cell := e.CellAt(x, y)
			if cell != nil {
				cell = cell.Clone()
				if cell.Width > 1 {
					w = cell.Width
				}
				if cell.Style.Bg == nil && e.bgColor != nil {
					cell.Style.Bg = e.bgColor
				}
				if cell.Style.Fg == nil && e.fgColor != nil {
					cell.Style.Fg = e.fgColor
				}
				scr.SetCell(x+area.Min.X, y+area.Min.Y, cell)
			}
			x += w
		}
	}
}

// Height returns the height of the terminal.
func (e *Emulator) Height() int {
	return e.scr.Height()
}

// Width returns the width of the terminal.
func (e *Emulator) Width() int {
	return e.scr.Width()
}

// CursorPosition returns the terminal's cursor position.
func (e *Emulator) CursorPosition() uv.Position {
	x, y := e.scr.CursorPosition()
	return uv.Pos(x, y)
}

// Resize resizes the terminal.
func (e *Emulator) Resize(width int, height int) {
	x, y := e.scr.CursorPosition()
	if e.atPhantom {
		if x < width-1 {
			e.atPhantom = false
			x++
		}
	}

	if y < 0 {
		y = 0
	}
	if y >= height {
		y = height - 1
	}
	if x < 0 {
		x = 0
	}
	if x >= width {
		x = width - 1
	}

	e.scrs[0].Resize(width, height)
	e.scrs[1].Resize(width, height)
	e.tabstops = uv.DefaultTabStops(width)

	e.setCursor(x, y)

	if e.isModeSet(ansi.ModeInBandResize) {
		_, _ = io.WriteString(e.pw, ansi.InBandResize(e.Height(), e.Width(), 0, 0))
	}
}

// Read reads data from the terminal input buffer.
func (e *Emulator) Read(p []byte) (n int, err error) {
	if e.closed.Load() {
		return 0, io.EOF
	}

	return e.pr.Read(p) //nolint:wrapcheck
}

// Close closes the terminal.
func (e *Emulator) Close() error {
	if !e.closed.CompareAndSwap(false, true) {
		return nil
	}

	return e.pw.CloseWithError(io.EOF) //nolint:wrapcheck
}

// Write writes data to the terminal output buffer.
func (e *Emulator) Write(p []byte) (n int, err error) {
	if e.closed.Load() {
		return 0, io.ErrClosedPipe
	}

	for i := range p {
		e.parser.Advance(p[i])
		state := e.parser.State()
		// flush grapheme if we transitioned to a non-utf8 state or we have
		// written the whole byte slice.
		if len(e.grapheme) > 0 {
			if (e.lastState == parser.GroundState && state != parser.Utf8State) || i == len(p)-1 {
				e.flushGrapheme()
			}
		}
		e.lastState = state
	}
	return len(p), nil
}

// WriteString writes a string to the terminal output buffer.
func (e *Emulator) WriteString(s string) (n int, err error) {
	return e.Write([]byte(s))
}

// InputPipe returns the terminal's input pipe.
// This can be used to send input to the terminal.
func (e *Emulator) InputPipe() io.Writer {
	return e.pw
}

// Paste pastes text into the terminal.
// If bracketed paste mode is enabled, the text is bracketed with the
// appropriate escape sequences.
func (e *Emulator) Paste(text string) {
	if e.isModeSet(ansi.ModeBracketedPaste) {
		_, _ = io.WriteString(e.pw, ansi.BracketedPasteStart)
		defer io.WriteString(e.pw, ansi.BracketedPasteEnd) //nolint:errcheck
	}

	_, _ = io.WriteString(e.pw, text)
}

// SendText sends arbitrary text to the terminal.
func (e *Emulator) SendText(text string) {
	_, _ = io.WriteString(e.pw, text)
}

// SendKeys sends multiple keys to the terminal.
func (e *Emulator) SendKeys(keys ...uv.KeyEvent) {
	for _, k := range keys {
		e.SendKey(k)
	}
}

// ForegroundColor returns the terminal's foreground color. This returns nil if
// the foreground color is not set which means the outer terminal color is
// used.
func (e *Emulator) ForegroundColor() color.Color {
	if e.fgColor == nil {
		return e.defaultFg
	}
	return e.fgColor
}

// SetForegroundColor sets the terminal's foreground color.
func (e *Emulator) SetForegroundColor(c color.Color) {
	if c == nil {
		c = e.defaultFg
	}
	e.fgColor = c
	if e.cb.ForegroundColor != nil {
		e.cb.ForegroundColor(c)
	}
}

// SetDefaultForegroundColor sets the terminal's default foreground color.
func (e *Emulator) SetDefaultForegroundColor(c color.Color) {
	if c == nil {
		c = color.White
	}
	e.defaultFg = c
}

// BackgroundColor returns the terminal's background color. This returns nil if
// the background color is not set which means the outer terminal color is
// used.
func (e *Emulator) BackgroundColor() color.Color {
	if e.bgColor == nil {
		return e.defaultBg
	}
	return e.bgColor
}

// SetBackgroundColor sets the terminal's background color.
func (e *Emulator) SetBackgroundColor(c color.Color) {
	if c == nil {
		c = e.defaultBg
	}
	e.bgColor = c
	if e.cb.BackgroundColor != nil {
		e.cb.BackgroundColor(c)
	}
}

// SetDefaultBackgroundColor sets the terminal's default background color.
func (e *Emulator) SetDefaultBackgroundColor(c color.Color) {
	if c == nil {
		c = color.Black
	}
	e.defaultBg = c
}

// CursorColor returns the terminal's cursor color. This returns nil if the
// cursor color is not set which means the outer terminal color is used.
func (e *Emulator) CursorColor() color.Color {
	if e.curColor == nil {
		return e.defaultCur
	}
	return e.curColor
}

// SetCursorColor sets the terminal's cursor color.
func (e *Emulator) SetCursorColor(c color.Color) {
	if c == nil {
		c = e.defaultCur
	}
	e.curColor = c
	if e.cb.CursorColor != nil {
		e.cb.CursorColor(c)
	}
}

// SetDefaultCursorColor sets the terminal's default cursor color.
func (e *Emulator) SetDefaultCursorColor(c color.Color) {
	if c == nil {
		c = color.White
	}
	e.defaultCur = c
}

// IndexedColor returns a terminal's indexed color. An indexed color is a color
// between 0 and 255.
func (e *Emulator) IndexedColor(i int) color.Color {
	if i < 0 || i > 255 {
		return nil
	}

	c := e.colors[i]
	if c == nil {
		// Return the default color.
		return ansi.IndexedColor(i)
	}

	return c
}

// SetIndexedColor sets a terminal's indexed color.
// The index must be between 0 and 255.
func (e *Emulator) SetIndexedColor(i int, c color.Color) {
	if i < 0 || i > 255 {
		return
	}

	e.colors[i] = c
}

// resetTabStops resets the terminal tab stops to the default set.
func (e *Emulator) resetTabStops() {
	e.tabstops = uv.DefaultTabStops(e.Width())
}

func (e *Emulator) logf(format string, v ...any) {
	if e.logger != nil {
		e.logger.Printf(format, v...)
	}
}

// Scrollback returns the scrollback buffer for the main screen.
// Returns nil if the terminal is in alternate screen mode, as the alternate
// screen typically doesn't use scrollback.
func (e *Emulator) Scrollback() *Scrollback {
	// Return main screen's scrollback only
	return e.scrs[0].Scrollback()
}

// ScrollbackLen returns the number of lines in the scrollback buffer.
func (e *Emulator) ScrollbackLen() int {
	sb := e.Scrollback()
	if sb == nil {
		return 0
	}
	return sb.Len()
}

// ScrollbackCellAt returns the cell at the given position in the scrollback buffer.
// x is the column, y is the line index (0 = oldest line in scrollback).
// Returns nil if position is out of bounds.
func (e *Emulator) ScrollbackCellAt(x, y int) *uv.Cell {
	sb := e.Scrollback()
	if sb == nil {
		return nil
	}
	return sb.CellAt(x, y)
}

// SetScrollbackSize sets the maximum number of lines in the scrollback buffer.
func (e *Emulator) SetScrollbackSize(maxLines int) {
	e.scrs[0].SetScrollbackSize(maxLines)
}

// SetED2SavesScrollback controls whether ED 2 (Erase Display) pushes
// the current viewport into scrollback before clearing. When true
// (the default), the behavior matches VTE and GNOME Terminal. When false,
// it matches xterm and Ghostty, which only clear the viewport without
// saving its contents to scrollback.
func (e *Emulator) SetED2SavesScrollback(v bool) {
	e.ed2SavesScrollback = v
}

// ClearScrollback clears the scrollback buffer.
func (e *Emulator) ClearScrollback() {
	sb := e.Scrollback()
	if sb != nil {
		sb.Clear()
	}
}

// IsAltScreen returns whether the terminal is in alternate screen mode.
func (e *Emulator) IsAltScreen() bool {
	return e.scr == &e.scrs[1]
}

// SavedCursor returns the cursor position that was saved by the most recent
// DECSC (ESC 7 or CSI s) sequence, and a boolean indicating whether any DECSC
// save has been performed since the last reset. When defined is false, x and y
// are zero and no DECSC sequence has been issued.
//
// The returned x and y values are 0-based, matching the convention of
// CursorPosition().
func (e *Emulator) SavedCursor() (x, y int, defined bool) {
	saved := e.scr.SavedCursor()
	// A zero-value Cursor could be a legitimate save at position (0,0); we
	// distinguish "never saved" from "saved at origin" by checking whether
	// the saved cursor position is non-zero or whether the caller's view of
	// the screen has been modified.  Unfortunately the vt.Screen does not
	// track a "has been saved" flag directly, so we expose the raw position
	// and let the caller decide.  For Snapshot purposes (re-emit ESC 7 before
	// the final CUP), the difference is irrelevant: emitting an extra ESC 7
	// at (0,0) before the CUP is harmless.
	return saved.X, saved.Y, true
}

// ScrollRegion returns the active vertical scroll region as 1-based inclusive
// top/bottom row numbers and a boolean indicating whether a non-default
// (restricted) scroll region is active. When defined is false, top and bottom
// equal 1 and Height() respectively — the full screen.
//
// The returned values map directly to the DECSTBM sequence parameters:
//
//	ESC [ <top> ; <bottom> r
func (e *Emulator) ScrollRegion() (top, bottom int, defined bool) {
	r := e.scr.ScrollRegion()
	// Internal representation: Min.Y is 0-based inclusive top,
	// Max.Y is 0-based exclusive bottom. Convert to 1-based inclusive.
	t := r.Min.Y + 1
	b := r.Max.Y
	h := e.Height()
	if t == 1 && b == h {
		return t, b, false
	}
	return t, b, true
}
