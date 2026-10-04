//go:build clients && windows

package clients

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"
)

const (
	terminalReadChunkLimit = 32 * 1024
	terminalEvidenceLimit  = 16 * 1024 * 1024
	terminalTextLimit      = 4 * 1024 * 1024
	terminalSequenceLimit  = 4096
	terminalCellLimit      = 96_000

	terminalWaitObject0 = 0
	terminalWaitTimeout = 258
	terminalWaitFailed  = 0xffffffff

	terminalCreateSuspended          = 0x00000004
	terminalCreateUnicodeEnvironment = 0x00000400
	terminalExtendedStartupInfo      = 0x00080000
	terminalStartfUseStdHandles      = 0x00000100
	terminalProcThreadAttrPseudo     = 0x00020016
	terminalJobObjectExtendedInfo    = 9
	terminalJobKillOnClose           = 0x00002000
)

var (
	terminalKernel32                  = syscall.NewLazyDLL("kernel32.dll")
	terminalCreatePipe                = terminalKernel32.NewProc("CreatePipe")
	terminalCreatePseudoConsole       = terminalKernel32.NewProc("CreatePseudoConsole")
	terminalResizePseudoConsole       = terminalKernel32.NewProc("ResizePseudoConsole")
	terminalClosePseudoConsole        = terminalKernel32.NewProc("ClosePseudoConsole")
	terminalInitializeProcThreadAttrs = terminalKernel32.NewProc("InitializeProcThreadAttributeList")
	terminalUpdateProcThreadAttr      = terminalKernel32.NewProc("UpdateProcThreadAttribute")
	terminalDeleteProcThreadAttr      = terminalKernel32.NewProc("DeleteProcThreadAttributeList")
	terminalCreateProcessW            = terminalKernel32.NewProc("CreateProcessW")
	terminalResumeThread              = terminalKernel32.NewProc("ResumeThread")
	terminalGetExitCodeProcess        = terminalKernel32.NewProc("GetExitCodeProcess")
	terminalCreateJobObjectW          = terminalKernel32.NewProc("CreateJobObjectW")
	terminalSetInformationJobObject   = terminalKernel32.NewProc("SetInformationJobObject")
	terminalAssignProcessToJobObject  = terminalKernel32.NewProc("AssignProcessToJobObject")
	terminalTerminateJobObject        = terminalKernel32.NewProc("TerminateJobObject")
	terminalTerminateProcess          = terminalKernel32.NewProc("TerminateProcess")
)

type terminalStartupInfoEx struct {
	StartupInfo   syscall.StartupInfo
	AttributeList unsafe.Pointer
}

type terminalProcessInformation struct {
	Process   syscall.Handle
	Thread    syscall.Handle
	ProcessID uint32
	ThreadID  uint32
}

type terminalBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type terminalIOCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type terminalExtendedLimitInformation struct {
	BasicLimitInformation terminalBasicLimitInformation
	IOInfo                terminalIOCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// helixTerminal is a bounded Windows ConPTY session. Output is streamed into
// an ANSI screen model; raw output and frame history are never accumulated.
// The root and descendants belong to a kill-on-close job object and are also
// identified by the acceptance process-tree tracker.
type helixTerminal struct {
	pty      uintptr
	input    *os.File
	output   *os.File
	process  syscall.Handle
	thread   syscall.Handle
	job      syscall.Handle
	pid      uint32
	identity editorProcessIdentity
	tree     *editorProcessTracker
	screen   *terminalScreen

	readDone    chan struct{}
	readMu      sync.Mutex
	readRev     uint64
	ioMu        sync.RWMutex
	closed      atomic.Bool
	exited      atomic.Bool
	killOnce    sync.Once
	stopCtx     func() bool
	cancelDone  chan struct{}
	inputBytes  atomic.Uint64
	outputBytes atomic.Uint64
}

type terminalExit struct {
	Identity        editorProcessIdentity
	ExitCode        uint32
	ProcessTreeGone bool
}

type terminalCell struct {
	text         string
	foreground   uint32
	background   uint32
	attributes   uint16
	continuation bool
}

type terminalBuffer struct {
	cells        []terminalCell
	columns      int
	rows         int
	cursorX      int
	cursorY      int
	savedX       int
	savedY       int
	scrollTop    int
	scrollBottom int
	wrapPending  bool
	autoWrap     bool
	originMode   bool
	insertMode   bool
	style        terminalCell
}

type terminalFrame struct {
	Revision        uint64
	Columns         int
	Rows            int
	Lines           []string
	CursorColumn    int
	CursorRow       int
	CursorVisible   bool
	AlternateScreen bool
	Title           string
	Error           string
}

func (f terminalFrame) Text() string { return strings.Join(f.Lines, "\n") }

type terminalParserState uint8

const (
	terminalGround terminalParserState = iota
	terminalEscape
	terminalCSI
	terminalOSC
	terminalOSCEscape
	terminalCharset
)

type terminalScreen struct {
	mu             sync.Mutex
	primary        terminalBuffer
	alternate      terminalBuffer
	useAlternate   bool
	cursorVisible  bool
	title          string
	state          terminalParserState
	sequence       []byte
	utf8Pending    []byte
	style          terminalCell
	revision       uint64
	changed        chan struct{}
	err            error
	totalTextBytes int
	respond        func([]byte) error
}

func newTerminalScreen(columns, rows int, respond func([]byte) error) *terminalScreen {
	screen := &terminalScreen{
		primary:       newTerminalBuffer(columns, rows),
		alternate:     newTerminalBuffer(columns, rows),
		cursorVisible: true,
		changed:       make(chan struct{}),
		respond:       respond,
	}
	return screen
}

func newTerminalBuffer(columns, rows int) terminalBuffer {
	return terminalBuffer{
		cells: make([]terminalCell, columns*rows), columns: columns, rows: rows,
		scrollBottom: rows - 1, autoWrap: true,
	}
}

func (s *terminalScreen) active() *terminalBuffer {
	if s.useAlternate {
		return &s.alternate
	}
	return &s.primary
}

func (s *terminalScreen) signalLocked() {
	s.revision++
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *terminalScreen) fail(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	if s.err == nil {
		s.err = err
		s.signalLocked()
	}
	s.mu.Unlock()
}

func (s *terminalScreen) finish() {
	s.mu.Lock()
	if s.err == nil && (s.state != terminalGround || len(s.utf8Pending) != 0) {
		s.err = fmt.Errorf("terminal output ended inside an ANSI/UTF-8 sequence (state=%d)", s.state)
		s.signalLocked()
	} else {
		s.signalLocked()
	}
	s.mu.Unlock()
}

func (s *terminalScreen) feed(data []byte) error {
	s.mu.Lock()
	if s.err != nil {
		err := s.err
		s.mu.Unlock()
		return err
	}
	if err := s.feedLocked(data); err != nil {
		s.err = err
		s.signalLocked()
		s.mu.Unlock()
		return err
	}
	s.signalLocked()
	s.mu.Unlock()
	return nil
}

func (s *terminalScreen) feedLocked(data []byte) error {
	if len(s.utf8Pending) != 0 {
		data = append(append([]byte(nil), s.utf8Pending...), data...)
		s.utf8Pending = s.utf8Pending[:0]
	}
	var responses [][]byte
	for index := 0; index < len(data); {
		value := data[index]
		index++
		switch s.state {
		case terminalGround:
			if value == 0x1b {
				s.state = terminalEscape
				continue
			}
			if value < 0x20 || value == 0x7f {
				if err := s.control(value); err != nil {
					return err
				}
				continue
			}
			if value < utf8.RuneSelf {
				s.printRune(rune(value))
				continue
			}
			index--
			r, size := utf8.DecodeRune(data[index:])
			if r == utf8.RuneError && size == 1 {
				if !utf8.FullRune(data[index:]) {
					s.utf8Pending = append(s.utf8Pending, data[index:]...)
					if len(s.utf8Pending) > utf8.UTFMax {
						return errors.New("terminal UTF-8 carry exceeds four bytes")
					}
					return s.sendResponses(responses)
				}
				return fmt.Errorf("terminal output contains invalid UTF-8 at byte 0x%02x", value)
			}
			s.printRune(r)
			index += size
		case terminalEscape:
			s.state = terminalGround
			switch value {
			case '[':
				s.state = terminalCSI
				s.sequence = s.sequence[:0]
			case ']':
				s.state = terminalOSC
				s.sequence = s.sequence[:0]
			case '(':
				s.state = terminalCharset
			case '7':
				buffer := s.active()
				buffer.savedX, buffer.savedY = buffer.cursorX, buffer.cursorY
			case '8':
				buffer := s.active()
				buffer.cursorX, buffer.cursorY = buffer.savedX, buffer.savedY
			case 'c':
				s.reset()
			case 'D':
				s.lineFeed(false)
			case 'E':
				s.active().cursorX = 0
				s.lineFeed(false)
			case 'M':
				s.reverseIndex()
			case 'H':
				// Set one-based horizontal tab stops are not used by the fixture views.
			case '=', '>':
				// Application keypad mode changes input interpretation only.
			case '\\':
				return errors.New("standalone ANSI string terminator is unsupported")
			default:
				return fmt.Errorf("unsupported ANSI ESC sequence ESC %q", value)
			}
		case terminalCSI:
			if value >= 0x40 && value <= 0x7e {
				response, err := s.executeCSI(s.sequence, value)
				s.state = terminalGround
				s.sequence = s.sequence[:0]
				if err != nil {
					return err
				}
				if len(response) != 0 {
					responses = append(responses, response)
				}
				continue
			}
			if value < 0x20 || value > 0x3f {
				return fmt.Errorf("invalid byte 0x%02x in ANSI CSI sequence", value)
			}
			if len(s.sequence) >= terminalSequenceLimit {
				return errors.New("ANSI CSI sequence exceeds the 4096-byte limit")
			}
			s.sequence = append(s.sequence, value)
		case terminalOSC:
			switch value {
			case 0x07:
				if err := s.executeOSC(s.sequence); err != nil {
					return err
				}
				s.state = terminalGround
				s.sequence = s.sequence[:0]
			case 0x1b:
				s.state = terminalOSCEscape
			default:
				if len(s.sequence) >= terminalSequenceLimit {
					return errors.New("ANSI OSC sequence exceeds the 4096-byte limit")
				}
				s.sequence = append(s.sequence, value)
			}
		case terminalOSCEscape:
			if value != '\\' {
				return fmt.Errorf("unsupported ANSI OSC escape terminator ESC %q", value)
			}
			if err := s.executeOSC(s.sequence); err != nil {
				return err
			}
			s.state = terminalGround
			s.sequence = s.sequence[:0]
		case terminalCharset:
			s.state = terminalGround
			if value != 'B' && value != '0' {
				return fmt.Errorf("unsupported ANSI character set selector %q", value)
			}
		default:
			return fmt.Errorf("invalid terminal parser state %d", s.state)
		}
	}
	return s.sendResponses(responses)
}

func (s *terminalScreen) sendResponses(responses [][]byte) error {
	if s.respond == nil {
		if len(responses) != 0 {
			return errors.New("terminal requested a response but no response writer is configured")
		}
		return nil
	}
	for _, response := range responses {
		if err := s.respond(response); err != nil {
			return fmt.Errorf("respond to terminal query: %w", err)
		}
	}
	return nil
}

func (s *terminalScreen) control(value byte) error {
	buffer := s.active()
	switch value {
	case 0x00, 0x07:
		return nil
	case 0x08:
		buffer.cursorX = max(0, buffer.cursorX-1)
		buffer.wrapPending = false
	case 0x09:
		buffer.cursorX = min(buffer.columns-1, ((buffer.cursorX/8)+1)*8)
		buffer.wrapPending = false
	case 0x0a, 0x0b, 0x0c:
		s.lineFeed(true)
	case 0x0d:
		buffer.cursorX = 0
		buffer.wrapPending = false
	default:
		return fmt.Errorf("unsupported terminal control byte 0x%02x", value)
	}
	return nil
}

func (s *terminalScreen) printRune(char rune) {
	buffer := s.active()
	width := terminalRuneWidth(char)
	if width == 0 {
		if buffer.cursorX == 0 && buffer.cursorY == 0 {
			s.failLocked(errors.New("combining terminal rune has no preceding screen cell"))
			return
		}
		x := buffer.cursorX - 1
		y := buffer.cursorY
		if buffer.wrapPending {
			x = buffer.cursorX
		}
		if x > 0 && buffer.cells[y*buffer.columns+x].continuation {
			x--
		}
		cell := &buffer.cells[y*buffer.columns+x]
		cell.text += string(char)
		s.totalTextBytes += len(string(char))
		if s.totalTextBytes > terminalTextLimit {
			s.failLocked(errors.New("terminal screen text exceeded the bounded 4 MiB limit"))
		}
		return
	}
	if buffer.wrapPending || (width == 2 && buffer.cursorX == buffer.columns-1) {
		if buffer.autoWrap {
			buffer.cursorX = 0
			s.lineFeed(false)
		}
		buffer.wrapPending = false
	}
	if width == 2 && buffer.cursorX == buffer.columns-1 {
		return
	}
	if buffer.insertMode {
		s.insertCharacters(1)
	}
	s.setCell(buffer, buffer.cursorX, buffer.cursorY, string(char), false)
	if width == 2 {
		s.setCell(buffer, buffer.cursorX+1, buffer.cursorY, "", true)
	}
	if buffer.cursorX+width >= buffer.columns {
		buffer.cursorX = buffer.columns - 1
		buffer.wrapPending = true
	} else {
		buffer.cursorX += width
	}
}

func terminalRuneWidth(char rune) int {
	if unicode.Is(unicode.Mn, char) || unicode.Is(unicode.Me, char) || char == 0x200d || char == 0xfe0f {
		return 0
	}
	if char >= 0x1100 && (char <= 0x115f || char == 0x2329 || char == 0x232a ||
		(char >= 0x2e80 && char <= 0xa4cf && char != 0x303f) ||
		(char >= 0xac00 && char <= 0xd7a3) || (char >= 0xf900 && char <= 0xfaff) ||
		(char >= 0xfe10 && char <= 0xfe19) || (char >= 0xfe30 && char <= 0xfe6f) ||
		(char >= 0xff00 && char <= 0xff60) || (char >= 0xffe0 && char <= 0xffe6) ||
		(char >= 0x1f300 && char <= 0x1faff) || (char >= 0x20000 && char <= 0x3fffd)) {
		return 2
	}
	return 1
}

func (s *terminalScreen) setCell(buffer *terminalBuffer, x, y int, text string, continuation bool) {
	if x < 0 || x >= buffer.columns || y < 0 || y >= buffer.rows {
		return
	}
	cell := &buffer.cells[y*buffer.columns+x]
	s.totalTextBytes -= len(cell.text)
	*cell = terminalCell{text: text, foreground: buffer.style.foreground, background: buffer.style.background,
		attributes: buffer.style.attributes, continuation: continuation}
	s.totalTextBytes += len(text)
	if s.totalTextBytes > terminalTextLimit {
		s.failLocked(errors.New("terminal screen text exceeded the bounded 4 MiB limit"))
	}
}

func (s *terminalScreen) failLocked(err error) {
	if s.err == nil {
		s.err = err
		s.signalLocked()
	}
}

func (s *terminalScreen) lineFeed(carriage bool) {
	buffer := s.active()
	if carriage {
		buffer.cursorX = 0
	}
	buffer.wrapPending = false
	if buffer.cursorY == buffer.scrollBottom {
		s.scrollUp(1)
		return
	}
	buffer.cursorY = min(buffer.rows-1, buffer.cursorY+1)
}

func (s *terminalScreen) reverseIndex() {
	buffer := s.active()
	if buffer.cursorY == buffer.scrollTop {
		s.scrollDown(1)
		return
	}
	buffer.cursorY = max(0, buffer.cursorY-1)
}

func (s *terminalScreen) reset() {
	s.primary = newTerminalBuffer(s.primary.columns, s.primary.rows)
	s.alternate = newTerminalBuffer(s.alternate.columns, s.alternate.rows)
	s.useAlternate = false
	s.cursorVisible = true
	s.title = ""
	s.style = terminalCell{}
	s.totalTextBytes = 0
}

func (s *terminalScreen) executeCSI(raw []byte, final byte) ([]byte, error) {
	private, params, intermediate, err := parseTerminalCSI(raw)
	if err != nil {
		return nil, err
	}
	buffer := s.active()
	param := func(index, fallback int) int {
		if index >= len(params) || params[index] == 0 {
			return fallback
		}
		return params[index]
	}
	count := min(max(1, param(0, 1)), 100_000)
	moveY := func(delta int) {
		minY, maxY := 0, buffer.rows-1
		if buffer.originMode {
			minY, maxY = buffer.scrollTop, buffer.scrollBottom
		}
		buffer.cursorY = min(maxY, max(minY, buffer.cursorY+delta))
		buffer.wrapPending = false
	}
	switch final {
	case 'A':
		moveY(-count)
	case 'B', 'e':
		moveY(count)
	case 'C', 'a':
		buffer.cursorX = min(buffer.columns-1, buffer.cursorX+count)
		buffer.wrapPending = false
	case 'D':
		buffer.cursorX = max(0, buffer.cursorX-count)
		buffer.wrapPending = false
	case 'E':
		moveY(count)
		buffer.cursorX = 0
	case 'F':
		moveY(-count)
		buffer.cursorX = 0
	case 'G', '`':
		buffer.cursorX = min(buffer.columns-1, max(0, param(0, 1)-1))
		buffer.wrapPending = false
	case 'd':
		base := 0
		if buffer.originMode {
			base = buffer.scrollTop
		}
		buffer.cursorY = min(buffer.rows-1, max(0, base+param(0, 1)-1))
		buffer.wrapPending = false
	case 'H', 'f':
		x, y := param(1, 1)-1, param(0, 1)-1
		if buffer.originMode {
			y += buffer.scrollTop
		}
		buffer.cursorX = min(buffer.columns-1, max(0, x))
		buffer.cursorY = min(buffer.rows-1, max(0, y))
		buffer.wrapPending = false
	case 'J':
		mode := param(0, 0)
		switch mode {
		case 0:
			s.eraseRange(buffer, buffer.cursorY*buffer.columns+buffer.cursorX, len(buffer.cells))
		case 1:
			s.eraseRange(buffer, 0, buffer.cursorY*buffer.columns+buffer.cursorX+1)
		case 2, 3:
			s.eraseRange(buffer, 0, len(buffer.cells))
		default:
			return nil, fmt.Errorf("unsupported CSI J erase mode %d", mode)
		}
	case 'K':
		line := buffer.cursorY * buffer.columns
		mode := param(0, 0)
		switch mode {
		case 0:
			s.eraseRange(buffer, line+buffer.cursorX, line+buffer.columns)
		case 1:
			s.eraseRange(buffer, line, line+buffer.cursorX+1)
		case 2:
			s.eraseRange(buffer, line, line+buffer.columns)
		default:
			return nil, fmt.Errorf("unsupported CSI K erase mode %d", mode)
		}
	case 'X':
		start := buffer.cursorY*buffer.columns + buffer.cursorX
		s.eraseRange(buffer, start, min(len(buffer.cells), start+count))
	case 'S':
		s.scrollUp(count)
	case 'T':
		s.scrollDown(count)
	case 'L':
		s.insertLines(count)
	case 'M':
		s.deleteLines(count)
	case '@':
		s.insertCharacters(count)
	case 'P':
		s.deleteCharacters(count)
	case 'r':
		top, bottom := param(0, 1)-1, param(1, buffer.rows)-1
		if top < 0 || bottom >= buffer.rows || top >= bottom {
			return nil, fmt.Errorf("invalid CSI scroll region %d..%d for %d rows", top+1, bottom+1, buffer.rows)
		}
		buffer.scrollTop, buffer.scrollBottom = top, bottom
		buffer.cursorX, buffer.cursorY, buffer.wrapPending = 0, top, false
	case 's':
		buffer.savedX, buffer.savedY = buffer.cursorX, buffer.cursorY
	case 'u':
		if private != 0 {
			if private == '?' { // Kitty keyboard protocol query.
				return []byte("\x1b[?0u"), nil
			}
			return nil, fmt.Errorf("unsupported CSI %cu sequence", private)
		}
		buffer.cursorX = min(buffer.columns-1, max(0, buffer.savedX))
		buffer.cursorY = min(buffer.rows-1, max(0, buffer.savedY))
		buffer.wrapPending = false
	case 'm':
		if private != 0 {
			return nil, fmt.Errorf("unsupported private SGR CSI %c m", private)
		}
		if err := s.applySGR(params); err != nil {
			return nil, err
		}
	case 'h', 'l':
		set := final == 'h'
		for _, mode := range params {
			if private == '?' {
				if err := s.setPrivateMode(mode, set); err != nil {
					return nil, err
				}
				continue
			}
			switch mode {
			case 4:
				buffer.insertMode = set
			case 20:
				// Newline mode is harmless; explicit CR/LF controls are modeled.
			default:
				return nil, fmt.Errorf("unsupported CSI mode %d%c", mode, final)
			}
		}
	case 'n':
		if private != 0 {
			return nil, fmt.Errorf("unsupported private terminal status query CSI %c n", private)
		}
		switch param(0, 0) {
		case 5:
			return []byte("\x1b[0n"), nil
		case 6:
			return []byte(fmt.Sprintf("\x1b[%d;%dR", buffer.cursorY+1, buffer.cursorX+1)), nil
		default:
			return nil, fmt.Errorf("unsupported CSI terminal status query %d n", param(0, 0))
		}
	case 'c':
		if private == '>' {
			return []byte("\x1b[>0;1;0c"), nil
		}
		if private != 0 && private != '?' {
			return nil, fmt.Errorf("unsupported device attributes query CSI %c c", private)
		}
		return []byte("\x1b[?62;1;6c"), nil
	case 't':
		if private != 0 {
			return nil, fmt.Errorf("unsupported terminal window operation CSI %q t", raw)
		}
		switch param(0, 0) {
		case 18:
			return []byte(fmt.Sprintf("\x1b[8;%d;%dt", buffer.rows, buffer.columns)), nil
		case 8:
			if len(params) != 3 || params[1] != buffer.rows || params[2] != buffer.columns {
				return nil, fmt.Errorf("terminal resize report CSI %q t does not match modeled size %dx%d", raw, buffer.columns, buffer.rows)
			}
		default:
			return nil, fmt.Errorf("unsupported terminal window operation CSI %q t", raw)
		}
	case 'q':
		if intermediate != " " {
			return nil, fmt.Errorf("unsupported CSI cursor-style sequence %q", raw)
		}
		switch param(0, 0) {
		case 0, 1, 2, 3, 4, 5, 6:
			// Cursor shape is terminal chrome, not a screen-cell operation.
		default:
			return nil, fmt.Errorf("unsupported cursor style %d", param(0, 0))
		}
	case 'g':
		if private != 0 || (param(0, 0) != 0 && param(0, 0) != 3) {
			return nil, fmt.Errorf("unsupported tab-stop operation CSI %q", raw)
		}
	case 'p':
		if private != '?' || intermediate != "$" || param(0, 0) != 2026 {
			return nil, fmt.Errorf("unsupported ANSI mode report CSI %q", raw)
		}
		// Synchronized-output mode does not change the final rendered frame.
	default:
		return nil, fmt.Errorf("unsupported ANSI CSI sequence %q%c", raw, final)
	}
	return nil, nil
}

func parseTerminalCSI(raw []byte) (byte, []int, string, error) {
	if len(raw) > terminalSequenceLimit {
		return 0, nil, "", errors.New("ANSI CSI sequence exceeds the 4096-byte limit")
	}
	private := byte(0)
	index := 0
	if len(raw) != 0 && (raw[0] == '?' || raw[0] == '>' || raw[0] == '<' || raw[0] == '=') {
		private = raw[0]
		index++
	}
	var params []int
	value, hasDigit := 0, false
	intermediate := false
	var intermediateBytes strings.Builder
	for ; index < len(raw); index++ {
		char := raw[index]
		switch {
		case char >= '0' && char <= '9' && !intermediate:
			if value > 1_000_000 {
				return 0, nil, "", errors.New("ANSI CSI parameter exceeds one million")
			}
			value = value*10 + int(char-'0')
			hasDigit = true
		case char == ';' && !intermediate:
			if hasDigit {
				params = append(params, value)
			} else {
				params = append(params, 0)
			}
			value, hasDigit = 0, false
		case char >= 0x20 && char <= 0x2f:
			intermediate = true
			intermediateBytes.WriteByte(char)
		case char == ':' && !intermediate:
			return 0, nil, "", errors.New("colon-separated ANSI CSI parameters are unsupported")
		default:
			return 0, nil, "", fmt.Errorf("unsupported ANSI CSI parameter byte 0x%02x", char)
		}
	}
	if hasDigit || len(raw) != 0 {
		params = append(params, value)
	}
	return private, params, intermediateBytes.String(), nil
}

func (s *terminalScreen) setPrivateMode(mode int, set bool) error {
	buffer := s.active()
	switch mode {
	case 1, 1000, 1002, 1003, 1004, 1006, 1015, 2004, 2026, 2027, 9001:
		// Input modes, mouse tracking and synchronized output do not change text.
	case 6:
		buffer.originMode = set
		buffer.cursorX, buffer.cursorY, buffer.wrapPending = 0, buffer.scrollTop, false
	case 7:
		buffer.autoWrap = set
	case 25:
		s.cursorVisible = set
	case 47, 1047, 1049:
		if set {
			if mode == 1049 && !s.useAlternate {
				s.primary.savedX, s.primary.savedY = s.primary.cursorX, s.primary.cursorY
			}
			if mode == 1047 || mode == 1049 {
				s.clearBuffer(&s.alternate)
			}
			s.useAlternate = true
		} else {
			s.useAlternate = false
			if mode == 1049 {
				s.primary.cursorX = min(s.primary.columns-1, max(0, s.primary.savedX))
				s.primary.cursorY = min(s.primary.rows-1, max(0, s.primary.savedY))
			}
		}
	default:
		return fmt.Errorf("unsupported DEC private mode %d", mode)
	}
	return nil
}

func (s *terminalScreen) applySGR(params []int) error {
	buffer := s.active()
	if len(params) == 0 {
		params = []int{0}
	}
	for index := 0; index < len(params); index++ {
		code := params[index]
		switch {
		case code == 0:
			buffer.style = terminalCell{}
		case code == 1:
			buffer.style.attributes |= 1 << 0
		case code == 2:
			buffer.style.attributes |= 1 << 1
		case code == 3:
			buffer.style.attributes |= 1 << 2
		case code == 4:
			buffer.style.attributes |= 1 << 3
		case code == 5 || code == 6:
			buffer.style.attributes |= 1 << 4
		case code == 7:
			buffer.style.attributes |= 1 << 5
		case code == 8:
			buffer.style.attributes |= 1 << 6
		case code == 9:
			buffer.style.attributes |= 1 << 7
		case code == 22:
			buffer.style.attributes &^= 1<<0 | 1<<1
		case code == 23:
			buffer.style.attributes &^= 1 << 2
		case code == 24:
			buffer.style.attributes &^= 1 << 3
		case code == 25:
			buffer.style.attributes &^= 1 << 4
		case code == 27:
			buffer.style.attributes &^= 1 << 5
		case code == 28:
			buffer.style.attributes &^= 1 << 6
		case code == 29:
			buffer.style.attributes &^= 1 << 7
		case code >= 30 && code <= 37:
			buffer.style.foreground = uint32(code-30) + 1
		case code == 39:
			buffer.style.foreground = 0
		case code >= 40 && code <= 47:
			buffer.style.background = uint32(code-40) + 1
		case code == 49:
			buffer.style.background = 0
		case code >= 90 && code <= 97:
			buffer.style.foreground = uint32(code-90+8) + 1
		case code >= 100 && code <= 107:
			buffer.style.background = uint32(code-100+8) + 1
		case code == 38 || code == 48:
			if index+2 >= len(params) {
				return fmt.Errorf("truncated ANSI extended color SGR at %d", index)
			}
			colorType := params[index+1]
			var color uint32
			switch colorType {
			case 5:
				if params[index+2] < 0 || params[index+2] > 255 {
					return fmt.Errorf("invalid ANSI indexed color %d", params[index+2])
				}
				color = 0x01000000 | uint32(params[index+2])
				index += 2
			case 2:
				if index+4 >= len(params) {
					return fmt.Errorf("truncated ANSI RGB color SGR at %d", index)
				}
				r, g, b := params[index+2], params[index+3], params[index+4]
				if r < 0 || r > 255 || g < 0 || g > 255 || b < 0 || b > 255 {
					return errors.New("ANSI RGB color channel is outside 0..255")
				}
				color = 0x02000000 | uint32(r)<<16 | uint32(g)<<8 | uint32(b)
				index += 4
			default:
				return fmt.Errorf("unsupported ANSI extended color type %d", colorType)
			}
			if code == 38 {
				buffer.style.foreground = color
			} else {
				buffer.style.background = color
			}
		case code == 51:
			buffer.style.attributes |= 1 << 8
		case code == 52:
			buffer.style.attributes |= 1 << 9
		case code == 53:
			buffer.style.attributes |= 1 << 10
		case code == 54:
			buffer.style.attributes &^= 1<<8 | 1<<9
		case code == 55:
			buffer.style.attributes &^= 1 << 10
		case code == 59:
			// Underline color resets to the default terminal color.
		default:
			return fmt.Errorf("unsupported ANSI SGR attribute %d", code)
		}
	}
	return nil
}

func (s *terminalScreen) executeOSC(raw []byte) error {
	if !utf8.Valid(raw) {
		return errors.New("terminal OSC contains invalid UTF-8")
	}
	value := string(raw)
	command, rest, found := strings.Cut(value, ";")
	if !found {
		return fmt.Errorf("unsupported ANSI OSC sequence %q", value)
	}
	switch command {
	case "0", "2":
		if len(rest) > 512 {
			return errors.New("terminal title exceeds the 512-byte limit")
		}
		s.title = rest
		return nil
	case "8":
		// OSC 8 hyperlink state decorates rendered cells; URLs are intentionally
		// not executed or exposed by this acceptance screen model.
		_, _, ok := strings.Cut(rest, ";")
		if !ok {
			return fmt.Errorf("malformed OSC 8 hyperlink sequence %q", value)
		}
		return nil
	default:
		return fmt.Errorf("unsupported ANSI OSC command %q", command)
	}
}

func (s *terminalScreen) eraseRange(buffer *terminalBuffer, start, end int) {
	start = max(0, min(len(buffer.cells), start))
	end = max(start, min(len(buffer.cells), end))
	for index := start; index < end; index++ {
		s.totalTextBytes -= len(buffer.cells[index].text)
		buffer.cells[index] = terminalCell{foreground: buffer.style.foreground, background: buffer.style.background, attributes: buffer.style.attributes}
	}
}

func (s *terminalScreen) clearBuffer(buffer *terminalBuffer) {
	for index := range buffer.cells {
		s.totalTextBytes -= len(buffer.cells[index].text)
		buffer.cells[index] = terminalCell{foreground: buffer.style.foreground, background: buffer.style.background, attributes: buffer.style.attributes}
	}
	buffer.cursorX, buffer.cursorY, buffer.wrapPending = 0, 0, false
}

func (s *terminalScreen) scrollUp(count int) {
	buffer := s.active()
	count = min(count, buffer.scrollBottom-buffer.scrollTop+1)
	for step := 0; step < count; step++ {
		start := buffer.scrollTop * buffer.columns
		end := (buffer.scrollBottom + 1) * buffer.columns
		for _, cell := range buffer.cells[start : start+buffer.columns] {
			s.totalTextBytes -= len(cell.text)
		}
		copy(buffer.cells[start:end-buffer.columns], buffer.cells[start+buffer.columns:end])
		for index := end - buffer.columns; index < end; index++ {
			buffer.cells[index] = terminalCell{foreground: buffer.style.foreground, background: buffer.style.background, attributes: buffer.style.attributes}
		}
	}
}

func (s *terminalScreen) scrollDown(count int) {
	buffer := s.active()
	count = min(count, buffer.scrollBottom-buffer.scrollTop+1)
	for step := 0; step < count; step++ {
		start := buffer.scrollTop * buffer.columns
		end := (buffer.scrollBottom + 1) * buffer.columns
		for _, cell := range buffer.cells[end-buffer.columns : end] {
			s.totalTextBytes -= len(cell.text)
		}
		copy(buffer.cells[start+buffer.columns:end], buffer.cells[start:end-buffer.columns])
		for index := start; index < start+buffer.columns; index++ {
			buffer.cells[index] = terminalCell{foreground: buffer.style.foreground, background: buffer.style.background, attributes: buffer.style.attributes}
		}
	}
}

func (s *terminalScreen) insertLines(count int) {
	buffer := s.active()
	if buffer.cursorY < buffer.scrollTop || buffer.cursorY > buffer.scrollBottom {
		return
	}
	count = min(count, buffer.scrollBottom-buffer.cursorY+1)
	start, end := buffer.cursorY*buffer.columns, (buffer.scrollBottom+1)*buffer.columns
	for _, cell := range buffer.cells[end-count*buffer.columns : end] {
		s.totalTextBytes -= len(cell.text)
	}
	copy(buffer.cells[start+count*buffer.columns:end], buffer.cells[start:end-count*buffer.columns])
	for index := start; index < start+count*buffer.columns; index++ {
		s.totalTextBytes -= len(buffer.cells[index].text)
		buffer.cells[index] = terminalCell{foreground: buffer.style.foreground, background: buffer.style.background, attributes: buffer.style.attributes}
	}
}

func (s *terminalScreen) deleteLines(count int) {
	buffer := s.active()
	if buffer.cursorY < buffer.scrollTop || buffer.cursorY > buffer.scrollBottom {
		return
	}
	count = min(count, buffer.scrollBottom-buffer.cursorY+1)
	start, end := buffer.cursorY*buffer.columns, (buffer.scrollBottom+1)*buffer.columns
	for _, cell := range buffer.cells[start : start+count*buffer.columns] {
		s.totalTextBytes -= len(cell.text)
	}
	copy(buffer.cells[start:end-count*buffer.columns], buffer.cells[start+count*buffer.columns:end])
	for index := end - count*buffer.columns; index < end; index++ {
		buffer.cells[index] = terminalCell{foreground: buffer.style.foreground, background: buffer.style.background, attributes: buffer.style.attributes}
	}
}

func (s *terminalScreen) insertCharacters(count int) {
	buffer := s.active()
	row := buffer.cursorY * buffer.columns
	count = min(count, buffer.columns-buffer.cursorX)
	start, end := row+buffer.cursorX, row+buffer.columns
	for _, cell := range buffer.cells[end-count : end] {
		s.totalTextBytes -= len(cell.text)
	}
	copy(buffer.cells[start+count:end], buffer.cells[start:end-count])
	for index := start; index < start+count; index++ {
		s.totalTextBytes -= len(buffer.cells[index].text)
		buffer.cells[index] = terminalCell{foreground: buffer.style.foreground, background: buffer.style.background, attributes: buffer.style.attributes}
	}
}

func (s *terminalScreen) deleteCharacters(count int) {
	buffer := s.active()
	row := buffer.cursorY * buffer.columns
	count = min(count, buffer.columns-buffer.cursorX)
	start, end := row+buffer.cursorX, row+buffer.columns
	for _, cell := range buffer.cells[start : start+count] {
		s.totalTextBytes -= len(cell.text)
	}
	copy(buffer.cells[start:end-count], buffer.cells[start+count:end])
	for index := end - count; index < end; index++ {
		buffer.cells[index] = terminalCell{foreground: buffer.style.foreground, background: buffer.style.background, attributes: buffer.style.attributes}
	}
}

func (s *terminalScreen) resize(columns, rows int) error {
	if err := validateTerminalSize(columns, rows); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	resizeBuffer := func(buffer *terminalBuffer) {
		cells := make([]terminalCell, columns*rows)
		copyRows, copyColumns := min(rows, buffer.rows), min(columns, buffer.columns)
		for row := 0; row < copyRows; row++ {
			copy(cells[row*columns:row*columns+copyColumns], buffer.cells[row*buffer.columns:row*buffer.columns+copyColumns])
		}
		buffer.cells, buffer.columns, buffer.rows = cells, columns, rows
		buffer.cursorX, buffer.cursorY = min(buffer.cursorX, columns-1), min(buffer.cursorY, rows-1)
		buffer.scrollTop, buffer.scrollBottom = 0, rows-1
		buffer.wrapPending = false
	}
	resizeBuffer(&s.primary)
	resizeBuffer(&s.alternate)
	s.recountTextLocked()
	s.signalLocked()
	return nil
}

func (s *terminalScreen) recountTextLocked() {
	total := 0
	for _, buffer := range []*terminalBuffer{&s.primary, &s.alternate} {
		for _, cell := range buffer.cells {
			total += len(cell.text)
		}
	}
	s.totalTextBytes = total
	if total > terminalTextLimit && s.err == nil {
		s.err = errors.New("terminal screen text exceeded the bounded 4 MiB limit")
	}
}

func (s *terminalScreen) snapshotLocked() terminalFrame {
	buffer := s.active()
	lines := make([]string, buffer.rows)
	for row := 0; row < buffer.rows; row++ {
		var line strings.Builder
		for column := 0; column < buffer.columns; column++ {
			cell := buffer.cells[row*buffer.columns+column]
			if cell.continuation || cell.attributes&(1<<6) != 0 {
				continue
			}
			line.WriteString(cell.text)
		}
		lines[row] = strings.TrimRight(line.String(), " ")
	}
	frame := terminalFrame{
		Revision: s.revision, Columns: buffer.columns, Rows: buffer.rows, Lines: lines,
		CursorColumn: buffer.cursorX, CursorRow: buffer.cursorY, CursorVisible: s.cursorVisible,
		AlternateScreen: s.useAlternate, Title: s.title,
	}
	if s.err != nil {
		frame.Error = s.err.Error()
	}
	return frame
}

func (s *terminalScreen) snapshot() terminalFrame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

func (s *terminalScreen) readAfter(ctx context.Context, revision uint64) (terminalFrame, error) {
	if ctx == nil {
		return terminalFrame{}, errors.New("terminal read requires a context")
	}
	for {
		s.mu.Lock()
		if s.err != nil {
			frame, err := s.snapshotLocked(), s.err
			s.mu.Unlock()
			return frame, err
		}
		if s.revision > revision {
			frame := s.snapshotLocked()
			s.mu.Unlock()
			return frame, nil
		}
		changed := s.changed
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return terminalFrame{}, ctx.Err()
		case <-changed:
		}
	}
}

func (s *terminalScreen) waitForText(ctx context.Context, text string) (terminalFrame, error) {
	if text == "" {
		return terminalFrame{}, errors.New("terminal text query must not be empty")
	}
	revision := uint64(0)
	for {
		frame, err := s.readAfter(ctx, revision)
		if err != nil {
			return frame, err
		}
		if strings.Contains(frame.Text(), text) {
			return frame, nil
		}
		revision = frame.Revision
	}
}

func TestHelixTerminalDuplexProbe(t *testing.T) {
	if os.Getenv("OMNILSP_HELIX_CONPTY_CHILD") == "1" {
		runHelixTerminalDuplexChild(t)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := startHelixTerminal(ctx, executable, []string{"-test.run=^TestHelixTerminalDuplexProbe$", "-test.v"}, t.TempDir(), []string{
		"OMNILSP_HELIX_CONPTY_CHILD=1",
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
	}, 120, 40)
	if err != nil {
		t.Fatalf("start native pseudoconsole child: %v", err)
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer closeCancel()
		if err := terminal.Close(closeCtx); err != nil {
			t.Errorf("close pseudoconsole child: %v", err)
		}
	}()
	identity := terminal.Identity()
	if identity.PID != int(terminal.pid) || identity.CreationTime == 0 || identity.Image == "" {
		t.Fatalf("root process identity is incomplete: %#v", identity)
	}

	frame, err := terminal.WaitForText(ctx, "HELIX-READY 雪")
	if err != nil {
		t.Fatalf("wait for child TUI output: %v\n%s", err, terminal.Snapshot().Text())
	}
	if !frame.AlternateScreen || frame.CursorVisible {
		t.Fatalf("terminal modes were not captured: alternate=%t cursorVisible=%t", frame.AlternateScreen, frame.CursorVisible)
	}
	if got := frame.Lines[1]; got != "" {
		t.Fatalf("CSI erase-line did not remove the overwritten text: row 2 = %q", got)
	}
	if got := frame.Lines[3]; !strings.Contains(got, "HELIX-READY 雪") {
		t.Fatalf("CSI cursor movement or Unicode rendering is wrong: row 4 = %q", got)
	}
	if err := terminal.Resize(100, 30); err != nil {
		t.Fatalf("resize pseudoconsole: %v", err)
	}
	if resized := terminal.Snapshot(); resized.Columns != 100 || resized.Rows != 30 {
		t.Fatalf("resize frame = %dx%d, want 100x30", resized.Columns, resized.Rows)
	}
	if err := terminal.Send(ctx, []byte("probe-line\r")); err != nil {
		t.Fatalf("send terminal input: %v", err)
	}
	frame, err = terminal.WaitForText(ctx, "HELIX-ECHO:probe-line")
	if err != nil {
		t.Fatalf("wait for duplex echo: %v\n%s", err, terminal.Snapshot().Text())
	}
	if !strings.Contains(frame.Text(), "HELIX-ECHO:probe-line") {
		t.Fatalf("duplex echo was not present in rendered frame: %q", frame.Text())
	}
	if err := terminal.Send(ctx, []byte("finish\r")); err != nil {
		t.Fatalf("send graceful child completion: %v", err)
	}
	exit, err := terminal.Wait(ctx)
	if err != nil {
		t.Fatalf("wait for pseudoconsole process tree: %v", err)
	}
	if exit.ExitCode != 0 || !exit.ProcessTreeGone || exit.Identity != identity {
		t.Fatalf("terminal process outcome = %#v, want same identity, exit 0, and empty process tree", exit)
	}
}

func runHelixTerminalDuplexChild(t *testing.T) {
	writer := os.Stdout
	if _, err := fmt.Fprint(writer, "\x1b[?1049h\x1b[2J\x1b[2;1HWRONG-LINE\x1b[2;1H\x1b[2K\x1b[4;7HHELIX-READY 雪\x1b[?25l"); err != nil {
		t.Fatalf("write initial terminal frame: %v", err)
	}
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read terminal input: %v", err)
	}
	if _, err := fmt.Fprintf(writer, "\x1b[5;3HHELIX-ECHO:%s", strings.TrimSpace(line)); err != nil {
		t.Fatalf("write terminal echo: %v", err)
	}
	line, err = reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read terminal completion input: %v", err)
	}
	if strings.TrimSpace(line) != "finish" {
		t.Fatalf("terminal completion input = %q", strings.TrimSpace(line))
	}
	if _, err := fmt.Fprint(writer, "\x1b[?25h\x1b[?1049l"); err != nil {
		t.Fatalf("restore primary screen: %v", err)
	}
}

func TestHelixTerminalScreenParserRejectsUnknownSequences(t *testing.T) {
	screen := newTerminalScreen(24, 6, nil)
	if err := screen.feed([]byte("\x1b[?1049h\x1b[2J\x1b[3;4H雪\x1b[?25l")); err != nil {
		t.Fatalf("parse supported ANSI screen sequence: %v", err)
	}
	frame := screen.snapshot()
	if !frame.AlternateScreen || frame.CursorVisible || !strings.Contains(frame.Lines[2], "雪") {
		t.Fatalf("supported ANSI sequence was not represented: %#v", frame)
	}
	if err := screen.feed([]byte("\x1b[999z")); err == nil {
		t.Fatal("unsupported meaningful CSI sequence was silently discarded")
	}
	if got := screen.snapshot().Error; !strings.Contains(got, "unsupported ANSI CSI") {
		t.Fatalf("unsupported sequence failure was not retained: %q", got)
	}
}

// startHelixTerminal launches a console client under a native pseudoconsole.
// The API is intentionally in a clients test file; it is acceptance plumbing,
// not an application runtime dependency.
func startHelixTerminal(ctx context.Context, executable string, args []string, workingDirectory string, extraEnv []string, columns, rows int) (*helixTerminal, error) {
	if ctx == nil {
		return nil, errors.New("terminal start requires a context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateTerminalSize(columns, rows); err != nil {
		return nil, err
	}
	absoluteExecutable, err := filepath.Abs(executable)
	if err != nil {
		return nil, fmt.Errorf("resolve terminal executable: %w", err)
	}
	info, err := os.Stat(absoluteExecutable)
	if err != nil || info.IsDir() {
		if err == nil {
			err = errors.New("path is a directory")
		}
		return nil, fmt.Errorf("terminal executable %q is unavailable: %w", absoluteExecutable, err)
	}
	absoluteDirectory := workingDirectory
	if absoluteDirectory != "" {
		absoluteDirectory, err = filepath.Abs(workingDirectory)
		if err != nil {
			return nil, fmt.Errorf("resolve terminal working directory: %w", err)
		}
		if stat, statErr := os.Stat(absoluteDirectory); statErr != nil || !stat.IsDir() {
			if statErr == nil {
				statErr = errors.New("path is not a directory")
			}
			return nil, fmt.Errorf("terminal working directory %q is unavailable: %w", absoluteDirectory, statErr)
		}
	}
	if err := resolveTerminalAPIs(); err != nil {
		return nil, err
	}

	var inputRead, inputWrite, outputRead, outputWrite syscall.Handle
	defer func() {
		for _, handle := range []syscall.Handle{inputRead, inputWrite, outputRead, outputWrite} {
			if handle != 0 {
				_ = syscall.CloseHandle(handle)
			}
		}
	}()
	if err := terminalCreatePipePair(&inputRead, &inputWrite); err != nil {
		return nil, fmt.Errorf("create pseudoconsole input pipe: %w", err)
	}
	if err := terminalCreatePipePair(&outputRead, &outputWrite); err != nil {
		return nil, fmt.Errorf("create pseudoconsole output pipe: %w", err)
	}

	var pty uintptr
	coord := uintptr(uint16(columns)) | uintptr(uint16(rows))<<16
	hr, _, _ := terminalCreatePseudoConsole.Call(coord, uintptr(inputRead), uintptr(outputWrite), 0, uintptr(unsafe.Pointer(&pty)))
	if hr != 0 {
		return nil, fmt.Errorf("CreatePseudoConsole failed with HRESULT 0x%08x", uint32(hr))
	}
	ptyOpen := true
	defer func() {
		if ptyOpen {
			terminalClosePseudoConsole.Call(pty)
		}
	}()

	attributeList, attributeBytes, err := terminalCreatePseudoConsoleAttribute(pty)
	if err != nil {
		return nil, err
	}
	defer terminalDeleteProcThreadAttr.Call(uintptr(attributeList))
	_ = attributeBytes // Retains the Go backing allocation through CreateProcessW.

	job, _, callErr := terminalCreateJobObjectW.Call(0, 0)
	if job == 0 {
		return nil, fmt.Errorf("CreateJobObjectW: %w", callErr)
	}
	jobHandle := syscall.Handle(job)
	defer func() {
		if jobHandle != 0 {
			_ = syscall.CloseHandle(jobHandle)
		}
	}()
	jobInfo := terminalExtendedLimitInformation{}
	jobInfo.BasicLimitInformation.LimitFlags = terminalJobKillOnClose
	ret, _, callErr := terminalSetInformationJobObject.Call(job, terminalJobObjectExtendedInfo, uintptr(unsafe.Pointer(&jobInfo)), unsafe.Sizeof(jobInfo))
	if ret == 0 {
		return nil, fmt.Errorf("SetInformationJobObject(KILL_ON_JOB_CLOSE): %w", callErr)
	}

	startup := terminalStartupInfoEx{}
	startup.StartupInfo.Cb = uint32(unsafe.Sizeof(startup))
	startup.StartupInfo.Flags = terminalStartfUseStdHandles
	startup.AttributeList = attributeList
	startupProcessInfo := terminalProcessInformation{}
	commandLine := windowsCommandLine(append([]string{absoluteExecutable}, args...))
	commandLineUTF16, err := syscall.UTF16FromString(commandLine)
	if err != nil {
		return nil, fmt.Errorf("encode terminal command line: %w", err)
	}
	var directoryUTF16 *uint16
	if absoluteDirectory != "" {
		directoryUTF16, err = syscall.UTF16PtrFromString(absoluteDirectory)
		if err != nil {
			return nil, fmt.Errorf("encode terminal working directory: %w", err)
		}
	}
	environmentUTF16, err := terminalEnvironmentBlock(extraEnv)
	if err != nil {
		return nil, err
	}
	executableUTF16, err := syscall.UTF16PtrFromString(absoluteExecutable)
	if err != nil {
		return nil, fmt.Errorf("encode terminal executable path: %w", err)
	}
	ret, _, callErr = terminalCreateProcessW.Call(
		uintptr(unsafe.Pointer(executableUTF16)),
		uintptr(unsafe.Pointer(&commandLineUTF16[0])),
		0, 0, 0,
		terminalCreateSuspended|terminalCreateUnicodeEnvironment|terminalExtendedStartupInfo,
		uintptr(unsafe.Pointer(&environmentUTF16[0])),
		uintptr(unsafe.Pointer(directoryUTF16)),
		uintptr(unsafe.Pointer(&startup)),
		uintptr(unsafe.Pointer(&startupProcessInfo)),
	)
	runtime.KeepAlive(executableUTF16)
	runtime.KeepAlive(commandLineUTF16)
	runtime.KeepAlive(environmentUTF16)
	runtime.KeepAlive(directoryUTF16)
	runtime.KeepAlive(attributeBytes)
	runtime.KeepAlive(&startup)
	runtime.KeepAlive(&startupProcessInfo)
	if ret == 0 {
		return nil, fmt.Errorf("CreateProcessW for terminal client: %w", callErr)
	}
	// The pseudoconsole owns these pipe endpoints after CreatePseudoConsole,
	// but Microsoft's ConPTY lifecycle requires keeping the handles alive until
	// CreateProcessW has consumed the PSEUDOCONSOLE startup attribute.
	_ = syscall.CloseHandle(inputRead)
	inputRead = 0
	_ = syscall.CloseHandle(outputWrite)
	outputWrite = 0
	process := startupProcessInfo.Process
	thread := startupProcessInfo.Thread
	pid := startupProcessInfo.ProcessID
	defer func() {
		if process != 0 {
			_ = syscall.CloseHandle(process)
		}
		if thread != 0 {
			_ = syscall.CloseHandle(thread)
		}
	}()

	ret, _, callErr = terminalAssignProcessToJobObject.Call(job, uintptr(process))
	if ret == 0 {
		assignErr := fmt.Errorf("AssignProcessToJobObject: %w", callErr)
		terminateErr := terminateSuspendedTerminalProcess(process, 1)
		return nil, errors.Join(assignErr, terminateErr)
	}
	tree, err := startEditorProcessTracker(int(pid))
	if err != nil {
		_ = terminateTerminalJob(jobHandle, 1)
		return nil, fmt.Errorf("track terminal process identity: %w", err)
	}
	if err := ctx.Err(); err != nil {
		tree.stopMonitor()
		return nil, errors.Join(err, terminateTerminalJob(jobHandle, 1))
	}
	ret, _, callErr = terminalResumeThread.Call(uintptr(thread))
	if uint32(ret) == 0xffffffff {
		tree.stopMonitor()
		_ = terminateTerminalJob(jobHandle, 1)
		return nil, fmt.Errorf("ResumeThread for terminal client: %w", callErr)
	}

	inputFile := os.NewFile(uintptr(inputWrite), "helix-conpty-input")
	outputFile := os.NewFile(uintptr(outputRead), "helix-conpty-output")
	inputWrite = 0
	outputRead = 0
	process = 0
	thread = 0
	jobHandle = 0
	ptyOpen = false
	terminal := &helixTerminal{
		pty: pty, input: inputFile, output: outputFile, process: startupProcessInfo.Process,
		thread: startupProcessInfo.Thread, job: syscall.Handle(job), pid: pid, tree: tree,
		readDone: make(chan struct{}), cancelDone: make(chan struct{}),
	}
	terminal.identity = tree.root
	terminal.screen = newTerminalScreen(columns, rows, terminal.sendTerminalResponse)
	terminal.stopCtx = context.AfterFunc(ctx, func() {
		defer close(terminal.cancelDone)
		_ = terminal.terminate()
	})
	go terminal.readOutput()
	return terminal, nil
}

func terminalCreatePipePair(readHandle, writeHandle *syscall.Handle) error {
	ret, _, callErr := terminalCreatePipe.Call(
		uintptr(unsafe.Pointer(readHandle)), uintptr(unsafe.Pointer(writeHandle)), 0, 0,
	)
	if ret == 0 {
		return callErr
	}
	return nil
}

func terminalCreatePseudoConsoleAttribute(pty uintptr) (unsafe.Pointer, []uintptr, error) {
	var size uintptr
	terminalInitializeProcThreadAttrs.Call(0, 1, 0, uintptr(unsafe.Pointer(&size)))
	if size == 0 {
		return nil, nil, errors.New("InitializeProcThreadAttributeList returned no storage size")
	}
	storage := make([]uintptr, (size+unsafe.Sizeof(uintptr(0))-1)/unsafe.Sizeof(uintptr(0)))
	list := unsafe.Pointer(&storage[0])
	ret, _, callErr := terminalInitializeProcThreadAttrs.Call(uintptr(list), 1, 0, uintptr(unsafe.Pointer(&size)))
	if ret == 0 {
		return nil, nil, fmt.Errorf("InitializeProcThreadAttributeList: %w", callErr)
	}
	ret, _, callErr = terminalUpdateProcThreadAttr.Call(uintptr(list), 0, terminalProcThreadAttrPseudo, pty, unsafe.Sizeof(uintptr(0)), 0, 0)
	if ret == 0 {
		terminalDeleteProcThreadAttr.Call(uintptr(list))
		return nil, nil, fmt.Errorf("UpdateProcThreadAttribute(PSEUDOCONSOLE): %w", callErr)
	}
	return list, storage, nil
}

func resolveTerminalAPIs() error {
	procs := map[string]*syscall.LazyProc{
		"CreatePipe":                        terminalCreatePipe,
		"CreatePseudoConsole":               terminalCreatePseudoConsole,
		"ResizePseudoConsole":               terminalResizePseudoConsole,
		"ClosePseudoConsole":                terminalClosePseudoConsole,
		"InitializeProcThreadAttributeList": terminalInitializeProcThreadAttrs,
		"UpdateProcThreadAttribute":         terminalUpdateProcThreadAttr,
		"DeleteProcThreadAttributeList":     terminalDeleteProcThreadAttr,
		"CreateProcessW":                    terminalCreateProcessW,
		"ResumeThread":                      terminalResumeThread,
		"GetExitCodeProcess":                terminalGetExitCodeProcess,
		"CreateJobObjectW":                  terminalCreateJobObjectW,
		"SetInformationJobObject":           terminalSetInformationJobObject,
		"AssignProcessToJobObject":          terminalAssignProcessToJobObject,
		"TerminateJobObject":                terminalTerminateJobObject,
		"TerminateProcess":                  terminalTerminateProcess,
	}
	for name, proc := range procs {
		if err := proc.Find(); err != nil {
			return fmt.Errorf("resolve terminal API %s: %w", name, err)
		}
	}
	return nil
}

func windowsCommandLine(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = quoteWindowsArgument(arg)
	}
	return strings.Join(quoted, " ")
}

func quoteWindowsArgument(value string) string {
	if value != "" && !strings.ContainsAny(value, " \t\n\v\"") {
		return value
	}
	var output strings.Builder
	output.Grow(len(value) + 2)
	output.WriteByte('"')
	backslashes := 0
	for _, char := range value {
		if char == '\\' {
			backslashes++
			continue
		}
		if char == '"' {
			for i := 0; i < backslashes*2+1; i++ {
				output.WriteByte('\\')
			}
			output.WriteRune(char)
			backslashes = 0
			continue
		}
		for i := 0; i < backslashes; i++ {
			output.WriteByte('\\')
		}
		backslashes = 0
		output.WriteRune(char)
	}
	for i := 0; i < backslashes*2; i++ {
		output.WriteByte('\\')
	}
	output.WriteByte('"')
	return output.String()
}

func terminalEnvironmentBlock(overrides []string) ([]uint16, error) {
	values := make(map[string]string, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		values[strings.ToUpper(key)] = entry
	}
	for _, entry := range overrides {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" || strings.ContainsRune(key, '\x00') || strings.ContainsRune(entry, '\x00') {
			return nil, fmt.Errorf("invalid terminal environment entry %q", entry)
		}
		values[strings.ToUpper(key)] = entry
	}
	entries := make([]string, 0, len(values))
	for _, value := range values {
		entries = append(entries, value)
	}
	sort.Slice(entries, func(i, j int) bool { return strings.ToUpper(entries[i]) < strings.ToUpper(entries[j]) })
	block := strings.Join(entries, "\x00") + "\x00\x00"
	return utf16.Encode([]rune(block)), nil
}

func validateTerminalSize(columns, rows int) error {
	if columns <= 0 || rows <= 0 {
		return fmt.Errorf("terminal dimensions must be positive, got %dx%d", columns, rows)
	}
	if columns > 4000 || rows > 4000 || columns*rows > terminalCellLimit {
		return fmt.Errorf("terminal dimensions %dx%d exceed the bounded screen area", columns, rows)
	}
	return nil
}

func terminateTerminalJob(job syscall.Handle, exitCode uint32) error {
	if job == 0 {
		return nil
	}
	ret, _, callErr := terminalTerminateJobObject.Call(uintptr(job), uintptr(exitCode))
	if ret == 0 {
		return fmt.Errorf("TerminateJobObject: %w", callErr)
	}
	return nil
}

func terminateSuspendedTerminalProcess(process syscall.Handle, exitCode uint32) error {
	ret, _, callErr := terminalTerminateProcess.Call(uintptr(process), uintptr(exitCode))
	if ret == 0 {
		return fmt.Errorf("TerminateProcess for unassigned suspended client: %w", callErr)
	}
	result, waitErr := syscall.WaitForSingleObject(process, 5000)
	if result != terminalWaitObject0 {
		return fmt.Errorf("wait for unassigned suspended client termination returned 0x%x: %w", result, waitErr)
	}
	return nil
}

// Identity returns the immutable root process identity captured by the same
// PPID/creation-time tracker used by the editor acceptance harness.
func (t *helixTerminal) Identity() editorProcessIdentity { return t.identity }

// Send writes raw terminal input bytes. Callers send key sequences as they
// would arrive from a terminal, for example []byte("gd") or []byte("\x1b").
func (t *helixTerminal) Send(ctx context.Context, input []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if t.closed.Load() {
		return errors.New("terminal session is closed")
	}
	if len(input) > terminalEvidenceLimit {
		return fmt.Errorf("terminal input of %d bytes exceeds the %d-byte limit", len(input), terminalEvidenceLimit)
	}
	if t.inputBytes.Add(uint64(len(input))) > terminalEvidenceLimit {
		return errors.New("terminal input exceeded the 16 MiB session limit")
	}
	t.ioMu.RLock()
	defer t.ioMu.RUnlock()
	if t.closed.Load() || t.input == nil {
		return errors.New("terminal session is closed")
	}
	for len(input) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk := input
		if len(chunk) > terminalReadChunkLimit {
			chunk = chunk[:terminalReadChunkLimit]
		}
		written, err := t.input.Write(chunk)
		if err != nil {
			return fmt.Errorf("write terminal input: %w", err)
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		input = input[written:]
	}
	return ctx.Err()
}

// Resize changes both the native pseudoconsole and the bounded screen model.
func (t *helixTerminal) Resize(columns, rows int) error {
	if err := validateTerminalSize(columns, rows); err != nil {
		return err
	}
	if t.closed.Load() {
		return errors.New("terminal session is closed")
	}
	coord := uintptr(uint16(columns)) | uintptr(uint16(rows))<<16
	hr, _, _ := terminalResizePseudoConsole.Call(t.pty, coord)
	if hr != 0 {
		return fmt.Errorf("ResizePseudoConsole(%dx%d) failed with HRESULT 0x%08x", columns, rows, uint32(hr))
	}
	return t.screen.resize(columns, rows)
}

// Read waits for the next rendered screen revision. The returned frame is a
// single bounded snapshot; the terminal retains no transcript or frame list.
func (t *helixTerminal) Read(ctx context.Context) (terminalFrame, error) {
	t.readMu.Lock()
	defer t.readMu.Unlock()
	frame, err := t.screen.readAfter(ctx, t.readRev)
	if err == nil {
		t.readRev = frame.Revision
	}
	return frame, err
}

func (t *helixTerminal) Snapshot() terminalFrame { return t.screen.snapshot() }

// WaitForText polls rendered screen contents and fails on unsupported ANSI or
// transport errors. It is a convenience for bounded acceptance interactions.
func (t *helixTerminal) WaitForText(ctx context.Context, text string) (terminalFrame, error) {
	return t.screen.waitForText(ctx, text)
}

func (t *helixTerminal) readOutput() {
	defer close(t.readDone)
	buffer := make([]byte, terminalReadChunkLimit)
	for {
		count, err := t.output.Read(buffer)
		if count > 0 {
			if t.outputBytes.Add(uint64(count)) > terminalEvidenceLimit {
				t.screen.fail(fmt.Errorf("terminal output exceeded the %d-byte session limit", terminalEvidenceLimit))
				_ = t.terminate()
				return
			}
			if feedErr := t.screen.feed(buffer[:count]); feedErr != nil {
				t.screen.fail(feedErr)
				_ = t.terminate()
				return
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !t.closed.Load() {
				t.screen.fail(fmt.Errorf("read terminal output: %w", err))
			}
			t.screen.finish()
			return
		}
		if count == 0 {
			t.screen.fail(errors.New("terminal output reader made no progress"))
			return
		}
	}
}

func (t *helixTerminal) sendTerminalResponse(response []byte) error {
	if len(response) == 0 || t.closed.Load() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return t.Send(ctx, response)
}

func (t *helixTerminal) terminate() error {
	var terminateErr error
	t.killOnce.Do(func() { terminateErr = terminateTerminalJob(t.job, 1) })
	return terminateErr
}

// Wait requires both the terminal client and all tracked descendants to exit.
func (t *helixTerminal) Wait(ctx context.Context) (terminalExit, error) {
	if ctx == nil {
		return terminalExit{}, errors.New("terminal wait requires a context")
	}
	var exitCode uint32
	for {
		result, callErr := syscall.WaitForSingleObject(t.process, 50)
		switch result {
		case terminalWaitObject0:
			ret, _, callErr := terminalGetExitCodeProcess.Call(uintptr(t.process), uintptr(unsafe.Pointer(&exitCode)))
			if ret == 0 {
				return terminalExit{}, fmt.Errorf("GetExitCodeProcess: %w", callErr)
			}
			t.exited.Store(true)
			goto waitTree
		case terminalWaitTimeout:
			if err := ctx.Err(); err != nil {
				return terminalExit{}, err
			}
		case terminalWaitFailed:
			return terminalExit{}, fmt.Errorf("WaitForSingleObject(terminal process): %w", callErr)
		default:
			return terminalExit{}, fmt.Errorf("WaitForSingleObject returned unexpected value 0x%x", result)
		}
	}

waitTree:
	for {
		if err := t.tree.scanOnce(); err != nil {
			return terminalExit{}, fmt.Errorf("refresh terminal process tree: %w", err)
		}
		t.tree.mu.Lock()
		liveCount := len(t.tree.live)
		treeErr := t.tree.err
		t.tree.mu.Unlock()
		if treeErr != nil {
			return terminalExit{}, treeErr
		}
		if liveCount == 0 {
			t.tree.stopMonitor()
			return terminalExit{Identity: t.identity, ExitCode: exitCode, ProcessTreeGone: true}, nil
		}
		select {
		case <-ctx.Done():
			return terminalExit{}, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Close releases all native resources. If the root is still running, closing
// the kill-on-close job also terminates any language-server descendants.
func (t *helixTerminal) Close(ctx context.Context) error {
	if t == nil || !t.closed.CompareAndSwap(false, true) {
		return nil
	}
	if t.stopCtx != nil && !t.stopCtx() {
		<-t.cancelDone
	}
	var closeErrors []error
	if !t.exited.Load() {
		if err := terminateTerminalJob(t.job, 1); err != nil {
			closeErrors = append(closeErrors, err)
		}
		waitCtx := ctx
		if waitCtx == nil {
			waitCtx = context.Background()
		}
		if _, err := t.Wait(waitCtx); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("wait after terminating terminal process tree: %w", err))
		}
	}
	if t.pty != 0 {
		terminalClosePseudoConsole.Call(t.pty)
		t.pty = 0
	}
	t.ioMu.Lock()
	if t.input != nil {
		if err := t.input.Close(); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("close terminal input: %w", err))
		}
		t.input = nil
	}
	t.ioMu.Unlock()
	if t.output != nil {
		if err := t.output.Close(); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("close terminal output: %w", err))
		}
	}
	select {
	case <-t.readDone:
	case <-time.After(2 * time.Second):
		closeErrors = append(closeErrors, errors.New("terminal output reader did not stop after pseudoconsole close"))
	}
	for name, handle := range map[string]syscall.Handle{"thread": t.thread, "process": t.process, "job": t.job} {
		if handle != 0 {
			if err := syscall.CloseHandle(handle); err != nil {
				closeErrors = append(closeErrors, fmt.Errorf("close terminal %s handle: %w", name, err))
			}
		}
	}
	t.thread, t.process, t.job = 0, 0, 0
	t.output = nil
	return errors.Join(closeErrors...)
}
