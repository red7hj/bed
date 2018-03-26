package window

import (
	"bytes"
	"io"
	"strconv"
	"sync"
	"unicode/utf8"

	"github.com/itchyny/bed/buffer"
	. "github.com/itchyny/bed/common"
	"github.com/itchyny/bed/history"
	"github.com/itchyny/bed/mathutil"
)

type window struct {
	buffer      *buffer.Buffer
	changedTick uint64
	prevChanged bool
	history     *history.History
	filename    string
	name        string
	height      int64
	width       int64
	offset      int64
	cursor      int64
	length      int64
	stack       []position
	append      bool
	replaceByte bool
	extending   bool
	pending     bool
	pendingByte byte
	focusText   bool
	redrawCh    chan<- struct{}
	eventCh     chan Event
	mu          *sync.Mutex
}

type position struct {
	cursor int64
	offset int64
}

type readAtSeeker interface {
	io.ReaderAt
	io.Seeker
}

func newWindow(r readAtSeeker, filename string, name string, redrawCh chan<- struct{}) (*window, error) {
	buffer := buffer.NewBuffer(r)
	length, err := buffer.Len()
	if err != nil {
		return nil, err
	}
	history := history.NewHistory()
	history.Push(buffer, 0, 0)
	return &window{
		buffer:   buffer,
		history:  history,
		filename: filename,
		name:     name,
		length:   length,
		redrawCh: redrawCh,
		eventCh:  make(chan Event),
		mu:       new(sync.Mutex),
	}, nil
}

func (w *window) setSize(width, height int) {
	w.width, w.height = int64(width), int64(height)
	w.offset = w.offset / w.width * w.width
	if w.cursor >= w.offset+w.height*w.width {
		w.offset = (w.cursor - w.height*w.width + w.width) / w.width * w.width
	}
	w.offset = mathutil.MinInt64(
		w.offset,
		mathutil.MaxInt64(w.length-1-w.height*w.width+w.width, 0)/w.width*w.width,
	)
}

// Run the window.
func (w *window) Run() {
	for e := range w.eventCh {
		w.mu.Lock()
		offset, cursor, changedTick := w.offset, w.cursor, w.changedTick
		switch e.Type {
		case EventCursorUp:
			w.cursorUp(e.Count)
		case EventCursorDown:
			w.cursorDown(e.Count)
		case EventCursorLeft:
			w.cursorLeft(e.Count)
		case EventCursorRight:
			w.cursorRight(e.Mode, e.Count)
		case EventCursorPrev:
			w.cursorPrev(e.Count)
		case EventCursorNext:
			w.cursorNext(e.Mode, e.Count)
		case EventCursorHead:
			w.cursorHead(e.Count)
		case EventCursorEnd:
			w.cursorEnd(e.Count)
		case EventCursorGotoAbs:
			w.cursorGotoAbs(e.Count)
		case EventCursorGotoRel:
			w.cursorGotoRel(e.Count)
		case EventScrollUp:
			w.scrollUp(e.Count)
		case EventScrollDown:
			w.scrollDown(e.Count)
		case EventPageUp:
			w.pageUp()
		case EventPageDown:
			w.pageDown()
		case EventPageUpHalf:
			w.pageUpHalf()
		case EventPageDownHalf:
			w.pageDownHalf()
		case EventPageTop:
			w.pageTop()
		case EventPageEnd:
			w.pageEnd()
		case EventJumpTo:
			w.jumpTo()
		case EventJumpBack:
			w.jumpBack()

		case EventDeleteByte:
			w.deleteByte(e.Count)
		case EventDeletePrevByte:
			w.deletePrevByte(e.Count)
		case EventIncrement:
			w.increment(e.Count)
		case EventDecrement:
			w.decrement(e.Count)

		case EventStartInsert:
			w.startInsert()
		case EventStartInsertHead:
			w.startInsertHead()
		case EventStartAppend:
			w.startAppend()
		case EventStartAppendEnd:
			w.startAppendEnd()
		case EventStartReplaceByte:
			w.startReplaceByte()
		case EventStartReplace:
			w.startReplace()
		case EventExitInsert:
			w.exitInsert()
		case EventRune:
			w.insertRune(e.Mode, e.Rune)
		case EventBackspace:
			w.backspace()
		case EventDelete:
			w.deleteByte(1)
		case EventSwitchFocus:
			w.focusText = !w.focusText
			if w.pending {
				w.pending = false
				w.pendingByte = '\x00'
			}
			w.changedTick++
		case EventUndo:
			if e.Mode != ModeNormal {
				panic("EventUndo should be emitted under normal mode")
			}
			w.undo(e.Count)
		case EventRedo:
			if e.Mode != ModeNormal {
				panic("EventUndo should be emitted under normal mode")
			}
			w.redo(e.Count)
		case EventExecuteSearch:
			w.search(e.Arg, e.Rune == '/')
		case EventNextSearch:
			w.search(e.Arg, e.Rune == '/')
		case EventPreviousSearch:
			w.search(e.Arg, e.Rune != '/')
		default:
			w.mu.Unlock()
			continue
		}
		changed := changedTick != w.changedTick
		if e.Type != EventUndo && e.Type != EventRedo {
			if e.Mode == ModeNormal && changed || e.Type == EventExitInsert && w.prevChanged {
				w.history.Push(w.buffer, w.offset, w.cursor)
			} else if e.Mode != ModeNormal && w.prevChanged && !changed &&
				EventCursorUp <= e.Type && e.Type <= EventJumpBack {
				w.history.Push(w.buffer, offset, cursor)
			}
		}
		w.prevChanged = changed
		w.mu.Unlock()
		w.redrawCh <- struct{}{}
	}
}

func (w *window) readBytes(offset int64, len int) (int, []byte, error) {
	bytes := make([]byte, len)
	n, err := w.buffer.ReadAt(bytes, offset)
	if err != nil && err != io.EOF {
		return 0, bytes, err
	}
	return n, bytes, nil
}

// State returns the current state of the buffer.
func (w *window) State() (*WindowState, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, bytes, err := w.readBytes(w.offset, int(w.height*w.width))
	if err != nil {
		return nil, err
	}
	return &WindowState{
		Name:          w.name,
		Width:         int(w.width),
		Offset:        w.offset,
		Cursor:        w.cursor,
		Bytes:         bytes,
		Size:          n,
		Length:        w.length,
		Pending:       w.pending,
		PendingByte:   w.pendingByte,
		EditedIndices: w.buffer.EditedIndices(),
		FocusText:     w.focusText,
	}, nil
}

func (w *window) insert(offset int64, c byte) {
	w.buffer.Insert(offset, c)
	w.changedTick++
}

func (w *window) replace(offset int64, c byte) {
	w.buffer.Replace(offset, c)
	w.changedTick++
}

func (w *window) delete(offset int64) {
	w.buffer.Delete(offset)
	w.changedTick++
}

func (w *window) undo(count int64) {
	for i := int64(0); i < mathutil.MaxInt64(count, 1); i++ {
		buffer, _, offset, cursor := w.history.Undo()
		if buffer == nil {
			return
		}
		w.buffer, w.offset, w.cursor = buffer, offset, cursor
		w.length, _ = w.buffer.Len()
	}
}

func (w *window) redo(count int64) {
	for i := int64(0); i < mathutil.MaxInt64(count, 1); i++ {
		buffer, offset, cursor := w.history.Redo()
		if buffer == nil {
			return
		}
		w.buffer, w.offset, w.cursor = buffer, offset, cursor
		w.length, _ = w.buffer.Len()
	}
}

func (w *window) cursorUp(count int64) {
	w.cursor -= mathutil.MinInt64(mathutil.MaxInt64(count, 1), w.cursor/w.width) * w.width
	if w.cursor < w.offset {
		w.offset = w.cursor / w.width * w.width
	}
}

func (w *window) cursorDown(count int64) {
	w.cursor += mathutil.MinInt64(
		mathutil.MinInt64(
			mathutil.MaxInt64(count, 1),
			(mathutil.MaxInt64(w.length, 1)-1)/w.width-w.cursor/w.width,
		)*w.width,
		mathutil.MaxInt64(w.length, 1)-1-w.cursor)
	if w.cursor >= w.offset+w.height*w.width {
		w.offset = (w.cursor - w.height*w.width + w.width) / w.width * w.width
	}
}

func (w *window) cursorLeft(count int64) {
	w.cursor -= mathutil.MinInt64(mathutil.MaxInt64(count, 1), w.cursor%w.width)
	if w.append && w.extending && w.cursor < w.length-1 {
		w.append = false
		w.extending = false
		if w.length > 0 {
			w.length--
		}
	}
}

func (w *window) cursorRight(mode Mode, count int64) {
	if mode == ModeNormal {
		w.cursor += mathutil.MinInt64(
			mathutil.MinInt64(mathutil.MaxInt64(count, 1), w.width-1-w.cursor%w.width),
			mathutil.MaxInt64(w.length, 1)-1-w.cursor,
		)
	} else if !w.extending {
		w.cursor += mathutil.MinInt64(
			mathutil.MinInt64(mathutil.MaxInt64(count, 1), w.width-1-w.cursor%w.width),
			w.length-w.cursor,
		)
		if w.cursor == w.length {
			w.append = true
			w.extending = true
			w.length++
		}
	}
}

func (w *window) cursorPrev(count int64) {
	w.cursor -= mathutil.MinInt64(mathutil.MaxInt64(count, 1), w.cursor)
	if w.cursor < w.offset {
		w.offset = w.cursor / w.width * w.width
	}
	if w.append && w.extending && w.cursor != w.length {
		w.append = false
		w.extending = false
		if w.length > 0 {
			w.length--
		}
	}
}

func (w *window) cursorNext(mode Mode, count int64) {
	if mode == ModeNormal {
		w.cursor += mathutil.MinInt64(mathutil.MaxInt64(count, 1), mathutil.MaxInt64(w.length, 1)-1-w.cursor)
	} else if !w.extending {
		w.cursor += mathutil.MinInt64(mathutil.MaxInt64(count, 1), w.length-w.cursor)
		if w.cursor == w.length {
			w.append = true
			w.extending = true
			w.length++
		}
	}
	if w.cursor >= w.offset+w.height*w.width {
		w.offset = (w.cursor - w.height*w.width + w.width) / w.width * w.width
	}
}

func (w *window) cursorHead(_ int64) {
	w.cursor -= w.cursor % w.width
}

func (w *window) cursorEnd(count int64) {
	w.cursor = mathutil.MinInt64(
		(w.cursor/w.width+mathutil.MaxInt64(count, 1))*w.width-1,
		mathutil.MaxInt64(w.length, 1)-1,
	)
	if w.cursor >= w.offset+w.height*w.width {
		w.offset = (w.cursor - w.height*w.width + w.width) / w.width * w.width
	}
}

func (w *window) cursorGotoAbs(count int64) {
	w.cursor = mathutil.MinInt64(count, mathutil.MaxInt64(w.length, 1)-1)
	if w.cursor < w.offset {
		w.offset = (mathutil.MaxInt64(w.cursor/w.width, w.height/2) - w.height/2) * w.width
	} else if w.cursor >= w.offset+w.height*w.width {
		h := (mathutil.MaxInt64(w.length, 1)+w.width-1)/w.width - w.height
		w.offset = mathutil.MinInt64((w.cursor-w.height*w.width+w.width)/w.width+w.height/2, h) * w.width
	}
}

func (w *window) cursorGotoRel(count int64) {
	w.cursor += mathutil.MaxInt64(mathutil.MinInt64(count, mathutil.MaxInt64(w.length, 1)-1-w.cursor), -w.cursor)
	if w.cursor < w.offset {
		w.offset = (mathutil.MaxInt64(w.cursor/w.width, w.height/2) - w.height/2) * w.width
	} else if w.cursor >= w.offset+w.height*w.width {
		h := (mathutil.MaxInt64(w.length, 1)+w.width-1)/w.width - w.height
		w.offset = mathutil.MinInt64((w.cursor-w.height*w.width+w.width)/w.width+w.height/2, h) * w.width
	}
}

func (w *window) scrollUp(count int64) {
	w.offset -= mathutil.MinInt64(mathutil.MaxInt64(count, 1), w.offset/w.width) * w.width
	if w.cursor >= w.offset+w.height*w.width {
		w.cursor -= ((w.cursor-w.offset-w.height*w.width)/w.width + 1) * w.width
	}
}

func (w *window) scrollDown(count int64) {
	h := mathutil.MaxInt64((mathutil.MaxInt64(w.length, 1)+w.width-1)/w.width-w.height, 0)
	w.offset += mathutil.MinInt64(mathutil.MaxInt64(count, 1), h-w.offset/w.width) * w.width
	if w.cursor < w.offset {
		w.cursor += mathutil.MinInt64(
			(w.offset-w.cursor+w.width-1)/w.width*w.width,
			mathutil.MaxInt64(w.length, 1)-1-w.cursor,
		)
	}
}

func (w *window) pageUp() {
	w.offset = mathutil.MaxInt64(w.offset-(w.height-2)*w.width, 0)
	if w.offset == 0 {
		w.cursor = 0
	} else if w.cursor >= w.offset+w.height*w.width {
		w.cursor = w.offset + (w.height-1)*w.width
	}
}

func (w *window) pageDown() {
	offset := mathutil.MaxInt64(((w.length+w.width-1)/w.width-w.height)*w.width, 0)
	w.offset = mathutil.MinInt64(w.offset+(w.height-2)*w.width, offset)
	if w.cursor < w.offset {
		w.cursor = w.offset
	} else if w.offset == offset {
		w.cursor = ((mathutil.MaxInt64(w.length, 1)+w.width-1)/w.width - 1) * w.width
	}
}

func (w *window) pageUpHalf() {
	w.offset = mathutil.MaxInt64(w.offset-mathutil.MaxInt64(w.height/2, 1)*w.width, 0)
	if w.offset == 0 {
		w.cursor = 0
	} else if w.cursor >= w.offset+w.height*w.width {
		w.cursor = w.offset + (w.height-1)*w.width
	}
}

func (w *window) pageDownHalf() {
	offset := mathutil.MaxInt64(((w.length+w.width-1)/w.width-w.height)*w.width, 0)
	w.offset = mathutil.MinInt64(w.offset+mathutil.MaxInt64(w.height/2, 1)*w.width, offset)
	if w.cursor < w.offset {
		w.cursor = w.offset
	} else if w.offset == offset {
		w.cursor = ((mathutil.MaxInt64(w.length, 1)+w.width-1)/w.width - 1) * w.width
	}
}

func (w *window) pageTop() {
	w.offset = 0
	w.cursor = 0
}

func (w *window) pageEnd() {
	w.offset = mathutil.MaxInt64(((w.length+w.width-1)/w.width-w.height)*w.width, 0)
	w.cursor = ((mathutil.MaxInt64(w.length, 1)+w.width-1)/w.width - 1) * w.width
}

func isDigit(b byte) bool {
	return '\x30' <= b && b <= '\x39'
}

func isWhite(b byte) bool {
	return b == '\x00' || b == '\x09' || b == '\x0a' || b == '\x0d' || b == '\x20'
}

func (w *window) jumpTo() {
	s := 50
	_, bytes, err := w.readBytes(mathutil.MaxInt64(w.cursor-int64(s), 0), 2*s)
	if err != nil {
		return
	}
	var i, j int
	for i = s; i < 2*s && isWhite(bytes[i]); i++ {
	}
	if i == 2*s || !isDigit(bytes[i]) {
		return
	}
	for ; 0 < i && isDigit(bytes[i-1]); i-- {
	}
	for j = i; j < 2*s && isDigit(bytes[j]); j++ {
	}
	if j == 2*s {
		return
	}
	offset, _ := strconv.ParseInt(string(bytes[i:j]), 10, 64)
	if offset <= 0 || w.length <= offset {
		return
	}
	w.stack = append(w.stack, position{w.cursor, w.offset})
	w.cursor = offset
	w.offset = mathutil.MaxInt64(offset-offset%w.width-mathutil.MaxInt64(w.height/3, 0)*w.width, 0)
}

func (w *window) jumpBack() {
	if len(w.stack) == 0 {
		return
	}
	w.cursor = w.stack[len(w.stack)-1].cursor
	w.offset = w.stack[len(w.stack)-1].offset
	w.stack = w.stack[:len(w.stack)-1]
}

func (w *window) deleteByte(count int64) {
	if w.length == 0 {
		return
	}
	cnt := int(mathutil.MinInt64(
		mathutil.MinInt64(mathutil.MaxInt64(count, 1), w.width-w.cursor%w.width),
		w.length-w.cursor,
	))
	for i := 0; i < cnt; i++ {
		w.delete(w.cursor)
		w.length--
		if w.cursor == w.length && w.cursor > 0 {
			w.cursor--
		}
	}
}

func (w *window) deletePrevByte(count int64) {
	cnt := int(mathutil.MinInt64(mathutil.MaxInt64(count, 1), w.cursor%w.width))
	for i := 0; i < cnt; i++ {
		w.delete(w.cursor - 1)
		w.cursor--
		w.length--
	}
}

func (w *window) increment(count int64) {
	_, bytes, err := w.readBytes(w.cursor, 1)
	if err != nil {
		return
	}
	w.replace(w.cursor, bytes[0]+byte(mathutil.MaxInt64(count, 1)%256))
	if w.length == 0 {
		w.length++
	}
}

func (w *window) decrement(count int64) {
	_, bytes, err := w.readBytes(w.cursor, 1)
	if err != nil {
		return
	}
	w.replace(w.cursor, bytes[0]-byte(mathutil.MaxInt64(count, 1)%256))
	if w.length == 0 {
		w.length++
	}
}

func (w *window) startInsert() {
	w.append = false
	w.extending = false
	w.pending = false
	if w.cursor == w.length {
		w.append = true
		w.extending = true
		w.length++
	}
}

func (w *window) startInsertHead() {
	w.cursorHead(0)
	w.append = false
	w.extending = false
	w.pending = false
	if w.cursor == w.length {
		w.append = true
		w.extending = true
		w.length++
	}
}

func (w *window) startAppend() {
	w.append = true
	w.extending = false
	w.pending = false
	if w.length > 0 {
		w.cursor++
	}
	if w.cursor == w.length {
		w.extending = true
		w.length++
	}
	if w.cursor >= w.offset+w.height*w.width {
		w.offset = (w.cursor - w.height*w.width + w.width) / w.width * w.width
	}
}

func (w *window) startAppendEnd() {
	w.cursorEnd(0)
	w.startAppend()
}

func (w *window) startReplaceByte() {
	w.replaceByte = true
	w.append = false
	w.extending = false
	w.pending = false
}

func (w *window) startReplace() {
	w.replaceByte = false
	w.append = false
	w.extending = false
	w.pending = false
}

func (w *window) exitInsert() {
	w.pending = false
	if w.append {
		if w.extending && w.length > 0 {
			w.length--
		}
		if w.cursor > 0 {
			w.cursor--
		}
		w.replaceByte = false
		w.append = false
		w.extending = false
		w.pending = false
	}
}

func (w *window) insertRune(mode Mode, ch rune) {
	if mode == ModeInsert || mode == ModeReplace {
		if w.focusText {
			buf := make([]byte, 4)
			n := utf8.EncodeRune(buf, ch)
			for i := 0; i < n; i++ {
				w.insertByte(mode, byte(buf[i]>>4))
				w.insertByte(mode, byte(buf[i]&0x0f))
			}
		} else if '0' <= ch && ch <= '9' {
			w.insertByte(mode, byte(ch-'0'))
		} else if 'a' <= ch && ch <= 'f' {
			w.insertByte(mode, byte(ch-'a'+0x0a))
		}
	}
}

func (w *window) insertByte(mode Mode, b byte) {
	if w.pending {
		switch mode {
		case ModeInsert:
			w.insert(w.cursor, w.pendingByte|b)
			w.cursor++
			w.length++
		case ModeReplace:
			w.replace(w.cursor, w.pendingByte|b)
			if w.length == 0 {
				w.length++
			}
			if w.replaceByte {
				w.exitInsert()
			} else {
				w.cursor++
				if w.cursor == w.length {
					w.append = true
					w.extending = true
					w.length++
				}
			}
		}
		if w.cursor >= w.offset+w.height*w.width {
			w.offset = (w.cursor - w.height*w.width + w.width) / w.width * w.width
		}
		w.pending = false
		w.pendingByte = '\x00'
	} else {
		w.pending = true
		w.pendingByte = b << 4
	}
}

func (w *window) backspace() {
	if w.pending {
		w.pending = false
		w.pendingByte = '\x00'
	} else if w.cursor > 0 {
		w.delete(w.cursor - 1)
		w.cursor--
		w.length--
	}
}

func (w *window) search(str string, forward bool) {
	if forward {
		w.searchForward(str)
	} else {
		w.searchBackward(str)
	}
}

func (w *window) searchForward(str string) {
	target := []byte(str)
	base, size := w.cursor+1, mathutil.MaxInt(int(w.height*w.width)*50, len(target)*500)
	_, bs, err := w.readBytes(base, size)
	if err != nil {
		return
	}
	i := bytes.Index(bs, target)
	if i >= 0 {
		w.cursor = base + int64(i)
		if w.cursor >= w.offset+w.height*w.width {
			w.offset = (w.cursor - w.height*w.width + w.width + 1) / w.width * w.width
		}
	}
}

func (w *window) searchBackward(str string) {
	target := []byte(str)
	size := mathutil.MaxInt(int(w.height*w.width)*50, len(target)*500)
	base := mathutil.MaxInt64(0, w.cursor-int64(size))
	_, bs, err := w.readBytes(base, int(mathutil.MinInt64(int64(size), w.cursor)))
	if err != nil {
		return
	}
	i := bytes.LastIndex(bs, target)
	if i >= 0 {
		w.cursor = base + int64(i)
		if w.cursor < w.offset {
			w.offset = w.cursor / w.width * w.width
		}
	}
}

// Close the Window.
func (w *window) Close() {
	close(w.eventCh)
}
