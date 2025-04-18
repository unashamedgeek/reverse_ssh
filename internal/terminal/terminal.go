// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package terminal

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/NHAS/reverse_ssh/internal"
	"github.com/NHAS/reverse_ssh/internal/server/users"
	"github.com/NHAS/reverse_ssh/internal/terminal/autocomplete"
	"github.com/NHAS/reverse_ssh/pkg/trie"
)

var (
	ErrCtrlD = errors.New("ctrl + D")
)

type TerminalFunctionCallback func(term *Terminal, args ...string) error

// EscapeCodes contains escape sequences that can be written to the terminal in
// order to achieve different styles of text.
type EscapeCodes struct {
	// Foreground colors
	Black, Red, Green, Yellow, Blue, Magenta, Cyan, White []byte

	// Reset all attributes
	Reset []byte
}

var vt100EscapeCodes = EscapeCodes{
	Black:   []byte{keyEscape, '[', '3', '0', 'm'},
	Red:     []byte{keyEscape, '[', '3', '1', 'm'},
	Green:   []byte{keyEscape, '[', '3', '2', 'm'},
	Yellow:  []byte{keyEscape, '[', '3', '3', 'm'},
	Blue:    []byte{keyEscape, '[', '3', '4', 'm'},
	Magenta: []byte{keyEscape, '[', '3', '5', 'm'},
	Cyan:    []byte{keyEscape, '[', '3', '6', 'm'},
	White:   []byte{keyEscape, '[', '3', '7', 'm'},

	Reset: []byte{keyEscape, '[', '0', 'm'},
}

// Terminal contains the state for running a VT100 terminal that is capable of
// reading lines of input.
type Terminal struct {
	session *users.Connection
	user    *users.User
	cancel  chan bool

	// AutoCompleteCallback, if non-null, is called for each keypress with
	// the full input line and the current position of the cursor (in
	// bytes, as an index into |line|). If it returns ok=false, the key
	// press is processed normally. Otherwise it returns a replacement line
	// and the new cursor position.
	AutoCompleteCallback func(term *Terminal, line string, pos int, key rune) (newLine string, newPos int, ok bool)

	// Escape contains a pointer to the escape codes for this terminal.
	// It's always a valid pointer, although the escape codes themselves
	// may be empty if the terminal doesn't support them.
	Escape *EscapeCodes

	// lock protects the terminal and the state in this object from
	// concurrent processing of a key press and a Write() call.
	lock sync.Mutex

	c      io.ReadWriter
	prompt []rune

	// line is the current line being entered.
	line []rune
	// pos is the logical position of the cursor in line
	pos int
	// echo is true if local echo is enabled
	echo bool
	// pasteActive is true iff there is a bracketed paste operation in
	// progress.
	pasteActive bool

	// cursorX contains the current X value of the cursor where the left
	// edge is 0. cursorY contains the row number where the first row of
	// the current line is 0.
	cursorX, cursorY int
	// maxLine is the greatest value of cursorY so far.
	maxLine int

	termWidth, termHeight int

	// outBuf contains the terminal data to be sent.
	outBuf []byte
	// remainder contains the remainder of any partial key sequences after
	// a read. It aliases into inBuf.
	remainder []byte
	inBuf     [256]byte

	// history contains previously entered commands so that they can be
	// accessed with the up and down keys.
	history stRingBuffer
	// historyIndex stores the currently accessed history entry, where zero
	// means the immediately previous entry.
	historyIndex int
	// When navigating up and down the history it's possible to return to
	// the incomplete, initial line. That value is stored in
	// historyPending.
	historyPending string

	autoCompleteIndex, autoCompletePos int
	autoCompletePendng                 string
	autoCompleting                     bool

	functions             map[string]Command
	functionsAutoComplete *trie.Trie

	autoCompleteValues map[string][]*trie.Trie

	raw bool

	rawOverflow chan []byte
}

func (t *Terminal) EnableRaw() {
	t.lock.Lock()
	defer t.lock.Unlock()

	if !t.raw {
		t.cancel <- true
		t.raw = true
	}
}

func (t *Terminal) DisableRaw(captureOverflow bool) {
	t.lock.Lock()
	defer t.lock.Unlock()

	if t.raw {
		if captureOverflow {
			t.rawOverflow = make(chan []byte, 1)
		}

		t.raw = false

		t.handleWindowSize()
	}
}

// NewTerminal runs a VT100 terminal on the given ReadWriter. If the ReadWriter is
// a local terminal, that terminal must first have been put into raw mode.
// prompt is a string that is written at the start of each input line (i.e.
// "> ").
func NewTerminal(c io.ReadWriter, prompt string) *Terminal {
	return &Terminal{
		Escape:       &vt100EscapeCodes,
		c:            c,
		prompt:       []rune(prompt),
		termWidth:    80,
		termHeight:   24,
		echo:         true,
		historyIndex: -1,
	}
}

func NewAdvancedTerminal(c io.ReadWriter, user *users.User, session *users.Connection, prompt string) *Terminal {
	t := &Terminal{
		session:               session,
		user:                  user,
		cancel:                make(chan bool),
		Escape:                &vt100EscapeCodes,
		c:                     c,
		prompt:                []rune(prompt),
		termWidth:             80,
		termHeight:            24,
		echo:                  true,
		historyIndex:          -1,
		AutoCompleteCallback:  defaultAutoComplete,
		functionsAutoComplete: trie.NewTrie(),
		functions:             make(map[string]Command),
		autoCompleteValues:    make(map[string][]*trie.Trie),
	}

	t.AddValueAutoComplete(autocomplete.Functions, t.functionsAutoComplete)

	t.handleWindowSize()

	return t
}

func (t *Terminal) handleWindowSize() {
	go func() {
		for {
			select {
			case <-t.cancel:

				return
			case req := <-t.session.ShellRequests:
				if req == nil { // Channel has closed, so therefore end this default handler

					return
				}

				switch req.Type {

				case "window-change":
					w, h := internal.ParseDims(req.Payload)
					t.SetSize(int(w), int(h))

					t.session.Pty.Columns = w
					t.session.Pty.Rows = h

				default:
					log.Println("Handled unknown request type in default handler: ", req.Type)
					if req.WantReply {
						req.Reply(false, nil)
					}
				}
			}

		}
	}()
}

func (t *Terminal) GetWidth() int {
	return int(t.termWidth)
}

func (t *Terminal) AddValueAutoComplete(placement string, trie ...*trie.Trie) error {
	t.lock.Lock()
	defer t.lock.Unlock()

	if _, ok := t.autoCompleteValues[placement]; ok {
		return errors.New("trampling function value, ignoring")
	}

	t.autoCompleteValues[placement] = trie

	return nil
}

func defaultAutoComplete(term *Terminal, line string, pos int, key rune) (newLine string, newPos int, ok bool) {

	if key == '\t' {

		if !term.autoCompleting {
			term.startAutoComplete(line, pos)
		}
		parsedLine := ParseLine(term.autoCompletePendng, term.autoCompletePos)

		var matches []string
		if parsedLine.Command == nil {
			matches = term.functionsAutoComplete.PrefixMatch("")
		} else {
			if parsedLine.Focus != nil && parsedLine.Focus.Start() == 0 {
				matches = term.functionsAutoComplete.PrefixMatch(parsedLine.Focus.Value())
			} else {
				if function, ok := term.functions[parsedLine.Command.Value()]; ok {
					expected := function.Expect(parsedLine)

					if expected != nil {

						//If the output of expected isnt a autocomplete tag, then its our match
						matches = expected

						if len(expected) == 1 && len(expected[0]) > 1 {
							if expected[0][0] == '<' && expected[0][len(expected[0])-1] == '>' {
								if trie, ok := term.autoCompleteValues[expected[0]]; ok {

									searchString := ""

									if parsedLine.Focus != nil && (parsedLine.Section == nil || parsedLine.Focus.Start() != parsedLine.Section.Start()) {
										searchString = parsedLine.Focus.Value()
									}

									matches = []string{}
									for _, t := range trie {
										matches = append(matches, t.PrefixMatch(searchString)...)
									}

								}
							}
						}

					}
				}
			}
		}

		sort.Strings(matches)

		parsedLine = ParseLine(line, pos)

		if len(matches) == 1 {
			term.resetAutoComplete()

			output, newPos := buildDisplayLine(parsedLine.Focus, line, matches[0], pos)
			if parsedLine.Focus != nil && parsedLine.Focus.Type() == (Cmd{}.Type()) {
				output += " "
				newPos += 1
			}

			return output, newPos, true
		}

		if len(matches) > 1 {

			currentMatch := matches[term.autoCompleteIndex]

			term.autoCompleteIndex = (term.autoCompleteIndex + 1) % len(matches)

			output, newPos := buildDisplayLine(parsedLine.Focus, line, currentMatch, pos)
			return output, newPos, true
		}

	} else {
		term.resetAutoComplete()
	}

	return "", 0, false
}

func buildDisplayLine(focus Node, line string, match string, currentPos int) (output string, newPos int) {
	if focus == nil {
		output = line[:currentPos]
		output += match
		newPos = len(output)
		output += line[currentPos:]

		return output, newPos
	}

	switch focus.Type() {
	case Cmd{}.Type():
	case Argument{}.Type():
		output = line[:focus.Start()]
	case Flag{}.Type():
		output = line[:focus.End()] + " "
	default:
		panic("Unknown type: " + focus.Type())
	}

	output += match
	newPos = len(output)
	output += line[focus.End():]

	return
}

const (
	keyCtrlC     = 3
	keyCtrlD     = 4
	keyCtrlU     = 21
	keyEnter     = '\r'
	keyEscape    = 27
	keyBackspace = 127
	keyUnknown   = 0xd800 /* UTF-16 surrogate area */ + iota
	keyUp
	keyDown
	keyLeft
	keyRight
	keyAltLeft
	keyAltRight
	keyHome
	keyDel
	keyEnd
	keyDeleteWord
	keyDeleteLine
	keyClearScreen
	keyPasteStart
	keyPasteEnd
)

var (
	crlf       = []byte{'\r', '\n'}
	pasteStart = []byte{keyEscape, '[', '2', '0', '0', '~'}
	pasteEnd   = []byte{keyEscape, '[', '2', '0', '1', '~'}
)

// bytesToKey tries to parse a key sequence from b. If successful, it returns
// the key and the remainder of the input. Otherwise it returns utf8.RuneError.
func bytesToKey(b []byte, pasteActive bool) (rune, []byte) {
	if len(b) == 0 {
		return utf8.RuneError, nil
	}

	if !pasteActive {
		switch b[0] {
		case 1: // ^A
			return keyHome, b[1:]
		case 2: // ^B
			return keyLeft, b[1:]
		case 5: // ^E
			return keyEnd, b[1:]
		case 6: // ^F
			return keyRight, b[1:]
		case 8: // ^H
			return keyBackspace, b[1:]
		case 11: // ^K
			return keyDeleteLine, b[1:]
		case 12: // ^L
			return keyClearScreen, b[1:]
		case 23: // ^W
			return keyDeleteWord, b[1:]
		case 14: // ^N
			return keyDown, b[1:]
		case 16: // ^P
			return keyUp, b[1:]
		}
	}

	if b[0] != keyEscape {
		if !utf8.FullRune(b) {
			return utf8.RuneError, b
		}
		r, l := utf8.DecodeRune(b)
		return r, b[l:]
	}

	if !pasteActive && len(b) >= 3 && b[0] == keyEscape && b[1] == '[' {
		switch b[2] {
		case 'A':
			return keyUp, b[3:]
		case 'B':
			return keyDown, b[3:]
		case 'C':
			return keyRight, b[3:]
		case 'D':
			return keyLeft, b[3:]
		case 'H':
			return keyHome, b[3:]
		case 'F':
			return keyEnd, b[3:]
		case 51:
			return keyDel, b[4:]
		}
	}

	if !pasteActive && len(b) >= 6 && b[0] == keyEscape && b[1] == '[' && b[2] == '1' && b[3] == ';' && b[4] == '3' {
		switch b[5] {
		case 'C':
			return keyAltRight, b[6:]
		case 'D':
			return keyAltLeft, b[6:]
		}
	}

	if !pasteActive && len(b) >= 6 && bytes.Equal(b[:6], pasteStart) {
		return keyPasteStart, b[6:]
	}

	if pasteActive && len(b) >= 6 && bytes.Equal(b[:6], pasteEnd) {
		return keyPasteEnd, b[6:]
	}

	// If we get here then we have a key that we don't recognise, or a
	// partial sequence. It's not clear how one should find the end of a
	// sequence without knowing them all, but it seems that [a-zA-Z~] only
	// appears at the end of a sequence.
	for i, c := range b[0:] {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '~' {
			return keyUnknown, b[i+1:]
		}
	}

	return utf8.RuneError, b
}

func (t *Terminal) AddCommands(m map[string]Command) error {
	t.lock.Lock()
	defer t.lock.Unlock()

	t.functions = m

	for k := range t.functions {
		t.functionsAutoComplete.Add(k)
	}

	return nil
}

func (t *Terminal) removeDuplicates(stringsSlice []string) []string {
	allKeys := make(map[string]bool)
	list := []string{}
	for _, item := range stringsSlice {
		if _, value := allKeys[item]; !value {
			allKeys[item] = true
			list = append(list, item)
		}
	}

	sort.Strings(list)
	return list
}

func (t *Terminal) Run() error {
	for {
		line, err := t.ReadLine()
		if err != nil {
			return err
		}

		parsedLine := ParseLine(line, t.pos)

		if parsedLine.Command != nil {
			f, ok := t.functions[parsedLine.Command.Value()]
			if !ok {
				fmt.Fprintf(t, "Unknown command: %s\n", parsedLine.Command.Value())
				continue
			}

			_, isSmallHelp := parsedLine.Flags["h"]
			_, isBigHelp := parsedLine.Flags["help"]

			if isSmallHelp || isBigHelp {
				fmt.Fprint(t, f.Help(false))
				continue
			}

			validFlags := f.ValidArgs()

			failed := []string{}
			for flag := range parsedLine.Flags {
				_, ok := validFlags[flag]
				if !ok && !(flag == "h" || flag == "help") {
					failed = append(failed, flag)
				}
			}

			if len(failed) > 0 {
				failed = t.removeDuplicates(failed)
				suffix := ""
				if len(failed) > 1 {
					suffix = "s"
				}

				fmt.Fprintf(t, "invalid flag%s: %q\n\n", suffix, strings.Join(failed, ", "))
				fmt.Fprint(t, f.Help(false))
				continue
			}

			err = f.Run(t.user, t, parsedLine)
			if err != nil {
				if err == io.EOF {
					return err
				}

				fmt.Fprintf(t, "%s\n", err)
			}
		}
	}
}

// queue appends data to the end of t.outBuf
func (t *Terminal) queue(data []rune) {
	t.outBuf = append(t.outBuf, []byte(string(data))...)
}

var space = []rune{' '}

func isPrintable(key rune) bool {
	isInSurrogateArea := key >= 0xd800 && key <= 0xdbff
	return key >= 32 && !isInSurrogateArea
}

// moveCursorToPos appends data to t.outBuf which will move the cursor to the
// given, logical position in the text.
func (t *Terminal) moveCursorToPos(pos int) {
	if !t.echo {
		return
	}

	x := visualLength(t.prompt) + pos
	y := x / t.termWidth
	x = x % t.termWidth

	up := 0
	if y < t.cursorY {
		up = t.cursorY - y
	}

	down := 0
	if y > t.cursorY {
		down = y - t.cursorY
	}

	left := 0
	if x < t.cursorX {
		left = t.cursorX - x
	}

	right := 0
	if x > t.cursorX {
		right = x - t.cursorX
	}

	t.cursorX = x
	t.cursorY = y
	t.move(up, down, left, right)
}

func (t *Terminal) move(up, down, left, right int) {
	m := []rune{}

	// 1 unit up can be expressed as ^[[A or ^[A
	// 5 units up can be expressed as ^[[5A

	if up == 1 {
		m = append(m, keyEscape, '[', 'A')
	} else if up > 1 {
		m = append(m, keyEscape, '[')
		m = append(m, []rune(strconv.Itoa(up))...)
		m = append(m, 'A')
	}

	if down == 1 {
		m = append(m, keyEscape, '[', 'B')
	} else if down > 1 {
		m = append(m, keyEscape, '[')
		m = append(m, []rune(strconv.Itoa(down))...)
		m = append(m, 'B')
	}

	if right == 1 {
		m = append(m, keyEscape, '[', 'C')
	} else if right > 1 {
		m = append(m, keyEscape, '[')
		m = append(m, []rune(strconv.Itoa(right))...)
		m = append(m, 'C')
	}

	if left == 1 {
		m = append(m, keyEscape, '[', 'D')
	} else if left > 1 {
		m = append(m, keyEscape, '[')
		m = append(m, []rune(strconv.Itoa(left))...)
		m = append(m, 'D')
	}

	t.queue(m)
}

func (t *Terminal) Read(b []byte) (n int, err error) {

	//Intentional racecondition, essentially due to how all the blocking reads do their thing (and the fact we cant cancel them without closing the connection)
	//We have to jank shit like this

	if t.raw {
		n, err := t.c.Read(b)
		//This is a patch due to blocking reads
		if !t.raw && t.rawOverflow != nil {

			c := make([]byte, n)
			copy(c, b[:n])
			t.rawOverflow <- c
		}

		return n, err
	}

	return 0, io.EOF
}

func (t *Terminal) clearLineToRight() {
	op := []rune{keyEscape, '[', 'K'}
	t.queue(op)
}

const maxLineLength = 4096

func (t *Terminal) setLine(newLine []rune, newPos int) {
	if t.echo {
		t.moveCursorToPos(0)
		t.writeLine(newLine)
		for i := len(newLine); i < len(t.line); i++ {
			t.writeLine(space)
		}
		t.moveCursorToPos(newPos)
	}
	t.line = newLine
	t.pos = newPos
}

func (t *Terminal) addCharacterToInput(characters []byte) {
	t.lock.Lock()
	defer t.lock.Unlock()

	log.Println("adding: ", characters)

	n := copy(t.inBuf[:], characters[:min(len(characters)-1):256])
	t.remainder = t.inBuf[:n]
}

func (t *Terminal) advanceCursor(places int) {
	t.cursorX += places
	t.cursorY += t.cursorX / t.termWidth
	if t.cursorY > t.maxLine {
		t.maxLine = t.cursorY
	}
	t.cursorX = t.cursorX % t.termWidth

	if places > 0 && t.cursorX == 0 {
		// Normally terminals will advance the current position
		// when writing a character. But that doesn't happen
		// for the last character in a line. However, when
		// writing a character (except a new line) that causes
		// a line wrap, the position will be advanced two
		// places.
		//
		// So, if we are stopping at the end of a line, we
		// need to write a newline so that our cursor can be
		// advanced to the next line.
		t.outBuf = append(t.outBuf, '\r', '\n')
	}
}

func (t *Terminal) eraseNPreviousChars(n int) {
	if n == 0 {
		return
	}

	if t.pos < n {
		n = t.pos
	}
	t.pos -= n
	t.moveCursorToPos(t.pos)

	copy(t.line[t.pos:], t.line[n+t.pos:])
	t.line = t.line[:len(t.line)-n]
	if t.echo {
		t.writeLine(t.line[t.pos:])
		for i := 0; i < n; i++ {
			t.queue(space)
		}
		t.advanceCursor(n)
		t.moveCursorToPos(t.pos)
	}
}

// countToLeftWord returns then number of characters from the cursor to the
// start of the previous word.
func (t *Terminal) countToLeftWord() int {
	if t.pos == 0 {
		return 0
	}

	pos := t.pos - 1
	for pos > 0 {
		if t.line[pos] != ' ' {
			break
		}
		pos--
	}
	for pos > 0 {
		if t.line[pos] == ' ' {
			pos++
			break
		}
		pos--
	}

	return t.pos - pos
}

// countToRightWord returns then number of characters from the cursor to the
// start of the next word.
func (t *Terminal) countToRightWord() int {
	pos := t.pos
	for pos < len(t.line) {
		if t.line[pos] == ' ' {
			break
		}
		pos++
	}
	for pos < len(t.line) {
		if t.line[pos] != ' ' {
			break
		}
		pos++
	}
	return pos - t.pos
}

// visualLength returns the number of visible glyphs in s.
func visualLength(runes []rune) int {
	inEscapeSeq := false
	length := 0

	for _, r := range runes {
		switch {
		case inEscapeSeq:
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscapeSeq = false
			}
		case r == '\x1b':
			inEscapeSeq = true
		default:
			length++
		}
	}

	return length
}

// handleKey processes the given key and, optionally, returns a line of text
// that the user has entered.
func (t *Terminal) handleKey(key rune) (line string, ok bool) {
	if t.pasteActive && key != keyEnter {
		t.resetAutoComplete()
		t.addKeyToLine(key)
		return
	}

	switch key {
	case keyBackspace, keyAltLeft, keyAltRight, keyLeft, keyRight, keyHome, keyEnd, keyDel, keyUp, keyDown, keyEnter, keyDeleteWord, keyDeleteLine, keyCtrlD, keyCtrlU, keyClearScreen:
		t.resetAutoComplete()
	}

	switch key {
	case keyDel:
		if t.pos >= len(t.line) || len(t.line) == 0 {
			return
		}
		t.pos += 1
		t.eraseNPreviousChars(1)
	case keyBackspace:
		if t.pos == 0 {
			return
		}
		t.eraseNPreviousChars(1)
	case keyAltLeft:
		// move left by a word.
		t.pos -= t.countToLeftWord()
		t.moveCursorToPos(t.pos)
	case keyAltRight:
		// move right by a word.
		t.pos += t.countToRightWord()
		t.moveCursorToPos(t.pos)
	case keyLeft:
		if t.pos == 0 {
			return
		}
		t.pos--
		t.moveCursorToPos(t.pos)
	case keyRight:
		if t.pos == len(t.line) {
			return
		}
		t.pos++
		t.moveCursorToPos(t.pos)
	case keyHome:
		if t.pos == 0 {
			return
		}
		t.pos = 0
		t.moveCursorToPos(t.pos)
	case keyEnd:
		if t.pos == len(t.line) {
			return
		}
		t.pos = len(t.line)
		t.moveCursorToPos(t.pos)
	case keyUp:
		entry, ok := t.history.NthPreviousEntry(t.historyIndex + 1)
		if !ok {
			return "", false
		}
		if t.historyIndex == -1 {
			t.historyPending = string(t.line)
		}
		t.historyIndex++
		runes := []rune(entry)
		t.setLine(runes, len(runes))
	case keyDown:
		switch t.historyIndex {
		case -1:
			return
		case 0:
			runes := []rune(t.historyPending)
			t.setLine(runes, len(runes))
			t.historyIndex--
		default:
			entry, ok := t.history.NthPreviousEntry(t.historyIndex - 1)
			if ok {
				t.historyIndex--
				runes := []rune(entry)
				t.setLine(runes, len(runes))
			}
		}
	case keyEnter:
		t.moveCursorToPos(len(t.line))
		t.queue([]rune("\r\n"))
		line = string(t.line)
		ok = true
		t.line = t.line[:0]
		t.pos = 0
		t.cursorX = 0
		t.cursorY = 0
		t.maxLine = 0
	case keyDeleteWord:
		// Delete zero or more spaces and then one or more characters.
		t.eraseNPreviousChars(t.countToLeftWord())
	case keyDeleteLine:
		// Delete everything from the current cursor position to the
		// end of line.
		for i := t.pos; i < len(t.line); i++ {
			t.queue(space)
			t.advanceCursor(1)
		}
		t.line = t.line[:t.pos]
		t.moveCursorToPos(t.pos)
	case keyCtrlD:
		// Erase the character under the current position.
		// The EOF case when the line is empty is handled in
		// readLine().
		if t.pos < len(t.line) {
			t.pos++
			t.eraseNPreviousChars(1)
		}
	case keyCtrlU:
		t.eraseNPreviousChars(t.pos)
	case keyClearScreen:
		// Erases the screen and moves the cursor to the home position.
		t.queue([]rune("\x1b[2J\x1b[H"))
		t.queue(t.prompt)
		t.cursorX, t.cursorY = 0, 0
		t.advanceCursor(visualLength(t.prompt))
		t.setLine(t.line, t.pos)
	case keyCtrlC:
		t.queue([]rune("^C\r\n"))
		t.queue(t.prompt)
		t.cursorX = 0
		t.advanceCursor(visualLength(t.prompt))
		t.setLine([]rune{}, 0)
	default:
		if t.AutoCompleteCallback != nil {
			prefix := string(t.line[:t.pos])
			suffix := string(t.line[t.pos:])

			t.lock.Unlock()
			newLine, newPos, completeOk := t.AutoCompleteCallback(t, prefix+suffix, len(prefix), key)
			t.lock.Lock()

			if completeOk {
				t.setLine([]rune(newLine), utf8.RuneCount([]byte(newLine)[:newPos]))
				return
			}
		}
		if !isPrintable(key) {
			return
		}
		if len(t.line) == maxLineLength {
			return
		}
		t.addKeyToLine(key)
	}
	return
}

func (t *Terminal) Clear() {

	t.lock.Lock()
	defer t.lock.Unlock()

	// Erases the screen and moves the cursor to the home position.
	t.queue([]rune("\x1b[2J\x1b[H"))
	t.queue(t.prompt)
	t.cursorX, t.cursorY = 0, 0
	t.advanceCursor(visualLength(t.prompt))
	t.setLine(t.line, t.pos)
}

// addKeyToLine inserts the given key at the current position in the current
// line.
func (t *Terminal) addKeyToLine(key rune) {
	if len(t.line) == cap(t.line) {
		newLine := make([]rune, len(t.line), 2*(1+len(t.line)))
		copy(newLine, t.line)
		t.line = newLine
	}
	t.line = t.line[:len(t.line)+1]
	copy(t.line[t.pos+1:], t.line[t.pos:])
	t.line[t.pos] = key
	if t.echo {
		t.writeLine(t.line[t.pos:])
	}
	t.pos++
	t.moveCursorToPos(t.pos)
}

func (t *Terminal) writeLine(line []rune) {
	for len(line) != 0 {
		remainingOnLine := t.termWidth - t.cursorX
		todo := len(line)
		if todo > remainingOnLine {
			todo = remainingOnLine
		}
		t.queue(line[:todo])
		t.advanceCursor(visualLength(line[:todo]))
		line = line[todo:]
	}
}

// writeWithCRLF writes buf to w but replaces all occurrences of \n with \r\n.
func writeWithCRLF(w io.Writer, buf []byte) (n int, err error) {
	for len(buf) > 0 {
		i := bytes.IndexByte(buf, '\n')
		todo := len(buf)
		if i >= 0 {
			todo = i
		}

		var nn int
		nn, err = w.Write(buf[:todo])
		n += nn
		if err != nil {
			return n, err
		}
		buf = buf[todo:]

		if i >= 0 {
			if _, err = w.Write(crlf); err != nil {
				return n, err
			}
			n++
			buf = buf[1:]
		}
	}

	return n, nil
}

func (t *Terminal) Write(buf []byte) (n int, err error) {
	if t.raw {
		return t.c.Write(buf)
	}

	t.lock.Lock()
	defer t.lock.Unlock()

	if t.cursorX == 0 && t.cursorY == 0 {
		// This is the easy case: there's nothing on the screen that we
		// have to move out of the way.
		return writeWithCRLF(t.c, buf)
	}

	// We have a prompt and possibly user input on the screen. We
	// have to clear it first.
	t.move(0 /* up */, 0 /* down */, t.cursorX /* left */, 0 /* right */)
	t.cursorX = 0
	t.clearLineToRight()

	for t.cursorY > 0 {
		t.move(1 /* up */, 0, 0, 0)
		t.cursorY--
		t.clearLineToRight()
	}

	if _, err = t.c.Write(t.outBuf); err != nil {
		return
	}
	t.outBuf = t.outBuf[:0]

	if n, err = writeWithCRLF(t.c, buf); err != nil {
		return
	}

	t.writeLine(t.prompt)
	if t.echo {
		t.writeLine(t.line)
	}

	t.moveCursorToPos(t.pos)

	if _, err = t.c.Write(t.outBuf); err != nil {
		return
	}
	t.outBuf = t.outBuf[:0]
	return
}

// ReadPassword temporarily changes the prompt and reads a password, without
// echo, from the terminal.
func (t *Terminal) ReadPassword(prompt string) (line string, err error) {
	t.lock.Lock()
	defer t.lock.Unlock()

	oldPrompt := t.prompt
	t.prompt = []rune(prompt)
	t.echo = false

	line, err = t.readLine()

	t.prompt = oldPrompt
	t.echo = true

	return
}

// ReadLine returns a line of input from the terminal.
func (t *Terminal) ReadLine() (line string, err error) {
	t.lock.Lock()
	defer t.lock.Unlock()

	return t.readLine()
}

func (t *Terminal) readLine() (line string, err error) {
	// t.lock must be held at this point

	if t.cursorX == 0 && t.cursorY == 0 {
		t.writeLine(t.prompt)
		t.c.Write(t.outBuf)
		t.outBuf = t.outBuf[:0]
	}

	lineIsPasted := t.pasteActive
	if t.rawOverflow != nil {
		data, ok := <-t.rawOverflow
		if ok {
			n := copy(t.inBuf[:], data)
			t.remainder = t.inBuf[:n]
			close(t.rawOverflow)
		}
		t.rawOverflow = nil
	}

	for {

		rest := t.remainder
		lineOk := false
		for !lineOk {

			var key rune
			key, rest = bytesToKey(rest, t.pasteActive)

			if key == utf8.RuneError {
				break
			}

			if !t.pasteActive {
				if key == keyCtrlD {
					if len(t.line) == 0 {
						return "", ErrCtrlD
					}
				}
				if key == keyPasteStart {
					t.pasteActive = true
					if len(t.line) == 0 {
						lineIsPasted = true
					}
					continue
				}
			} else if key == keyPasteEnd {
				t.pasteActive = false
				continue
			}
			if !t.pasteActive {
				lineIsPasted = false
			}
			line, lineOk = t.handleKey(key)
		}

		if len(rest) > 0 {
			n := copy(t.inBuf[:], rest)
			t.remainder = t.inBuf[:n]
		} else {
			t.remainder = nil
		}

		t.c.Write(t.outBuf)

		t.outBuf = t.outBuf[:0]
		if lineOk {
			if t.echo {
				t.historyIndex = -1
				line2 := strings.TrimSpace(line)
				if line2 != "" {
					t.history.Add(line2)
				}
			}
			if lineIsPasted {
				err = ErrPasteIndicator
			}
			return
		}

		// t.remainder is a slice at the beginning of t.inBuf
		// containing a partial key sequence
		readBuf := t.inBuf[len(t.remainder):]
		var n int

		t.lock.Unlock()

		n, err = t.c.Read(readBuf)

		t.lock.Lock()

		if err != nil {
			return
		}

		t.remainder = t.inBuf[:n+len(t.remainder)]
	}
}

// SetPrompt sets the prompt to be used when reading subsequent lines.
func (t *Terminal) SetPrompt(prompt string) {
	t.lock.Lock()
	defer t.lock.Unlock()

	t.prompt = []rune(prompt)
}

func (t *Terminal) clearAndRepaintLinePlusNPrevious(numPrevLines int) {
	// Move cursor to column zero at the start of the line.
	t.move(t.cursorY, 0, t.cursorX, 0)
	t.cursorX, t.cursorY = 0, 0
	t.clearLineToRight()
	for t.cursorY < numPrevLines {
		// Move down a line
		t.move(0, 1, 0, 0)
		t.cursorY++
		t.clearLineToRight()
	}
	// Move back to beginning.
	t.move(t.cursorY, 0, 0, 0)
	t.cursorX, t.cursorY = 0, 0

	t.queue(t.prompt)
	t.advanceCursor(visualLength(t.prompt))
	t.writeLine(t.line)
	t.moveCursorToPos(t.pos)
}

func (t *Terminal) SetSize(width, height int) error {
	t.lock.Lock()
	defer t.lock.Unlock()

	if width == 0 {
		width = 1
	}

	oldWidth := t.termWidth
	t.termWidth, t.termHeight = width, height

	switch {
	case width == oldWidth:
		// If the width didn't change then nothing else needs to be
		// done.
		return nil
	case len(t.line) == 0 && t.cursorX == 0 && t.cursorY == 0:
		// If there is nothing on current line and no prompt printed,
		// just do nothing
		return nil
	case width < oldWidth:
		// Some terminals (e.g. xterm) will truncate lines that were
		// too long when shinking. Others, (e.g. gnome-terminal) will
		// attempt to wrap them. For the former, repainting t.maxLine
		// works great, but that behaviour goes badly wrong in the case
		// of the latter because they have doubled every full line.

		// We assume that we are working on a terminal that wraps lines
		// and adjust the cursor position based on every previous line
		// wrapping and turning into two. This causes the prompt on
		// xterms to move upwards, which isn't great, but it avoids a
		// huge mess with gnome-terminal.
		if t.cursorX >= t.termWidth {
			t.cursorX = t.termWidth - 1
		}
		t.cursorY *= 2
		t.clearAndRepaintLinePlusNPrevious(t.maxLine * 2)
	case width > oldWidth:
		// If the terminal expands then our position calculations will
		// be wrong in the future because we think the cursor is
		// |t.pos| chars into the string, but there will be a gap at
		// the end of any wrapped line.
		//
		// But the position will actually be correct until we move, so
		// we can move back to the beginning and repaint everything.
		t.clearAndRepaintLinePlusNPrevious(t.maxLine)
	}

	_, err := t.c.Write(t.outBuf)
	t.outBuf = t.outBuf[:0]
	return err
}

type pasteIndicatorError struct{}

func (pasteIndicatorError) Error() string {
	return "terminal: ErrPasteIndicator not correctly handled"
}

// ErrPasteIndicator may be returned from ReadLine as the error, in addition
// to valid line data. It indicates that bracketed paste mode is enabled and
// that the returned line consists only of pasted data. Programs may wish to
// interpret pasted data more literally than typed data.
var ErrPasteIndicator = pasteIndicatorError{}

// SetBracketedPasteMode requests that the terminal bracket paste operations
// with markers. Not all terminals support this but, if it is supported, then
// enabling this mode will stop any autocomplete callback from running due to
// pastes. Additionally, any lines that are completely pasted will be returned
// from ReadLine with the error set to ErrPasteIndicator.
func (t *Terminal) SetBracketedPasteMode(on bool) {
	if on {
		io.WriteString(t.c, "\x1b[?2004h")
	} else {
		io.WriteString(t.c, "\x1b[?2004l")
	}
}

// stRingBuffer is a ring buffer of strings.
type stRingBuffer struct {
	// entries contains max elements.
	entries []string
	max     int
	// head contains the index of the element most recently added to the ring.
	head int
	// size contains the number of elements in the ring.
	size int
}

func (s *stRingBuffer) Add(a string) {
	if s.entries == nil {
		const defaultNumEntries = 100
		s.entries = make([]string, defaultNumEntries)
		s.max = defaultNumEntries
	}

	s.head = (s.head + 1) % s.max
	s.entries[s.head] = a
	if s.size < s.max {
		s.size++
	}
}

// NthPreviousEntry returns the value passed to the nth previous call to Add.
// If n is zero then the immediately prior value is returned, if one, then the
// next most recent, and so on. If such an element doesn't exist then ok is
// false.
func (s *stRingBuffer) NthPreviousEntry(n int) (value string, ok bool) {
	if n >= s.size {
		return "", false
	}
	index := s.head - n
	if index < 0 {
		index += s.max
	}
	return s.entries[index], true
}

func (t *Terminal) resetAutoComplete() {
	t.autoCompleteIndex = 0
	t.autoCompletePendng = ""
	t.autoCompleting = false
	t.autoCompletePos = 0
}

func (t *Terminal) startAutoComplete(lineFragment string, pos int) {
	t.autoCompleteIndex = 0
	t.autoCompletePendng = lineFragment
	t.autoCompleting = true
	t.autoCompletePos = pos
}
