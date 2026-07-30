// Package ui 的 editor.go 实现基于 raw mode 的字符级行编辑器。
//
// 在 utils.EnableRaw() 开启的原始输入模式下，按字节读取 stdin 并解析 ANSI
// 转义序列，提供现代 TUI 所需的行编辑能力：
//   - 多行编辑（Enter 发送，Alt+Enter / Shift+Enter / Ctrl+J 换行）
//   - 光标移动（←→ Home/End Ctrl+A/E/B/F）
//   - 删除（Backspace Delete Ctrl+W/U/H/D）
//   - 历史翻页（↑↓ Ctrl+P/N）
//   - Tab 补全（命令前缀匹配，单候选直接补全 + 空格，多候选补公共前缀）
//
// 纯 stdlib + ANSI 实现，无第三方 TUI 库依赖。
package ui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

const escTimeout = 10 * time.Millisecond // ESC 后等待后续字节的超时

// errInterrupt 表示用户按下 Ctrl+C 要求退出。
var errInterrupt = errors.New("interrupted")

// stdinReader 在单独 goroutine 中持续读取 stdin，按字节发出，支持超时读取。
// 这样可以在 ESC 后做短超时 peek，以区分单独 ESC 与转义序列。
type stdinReader struct {
	ch  chan byte
	err chan error
}

func newStdinReader() *stdinReader {
	s := &stdinReader{ch: make(chan byte, 512), err: make(chan error, 1)}
	go func() {
		buf := make([]byte, 1)
		for {
			n, e := os.Stdin.Read(buf)
			if e != nil {
				s.err <- e
				return
			}
			if n > 0 {
				s.ch <- buf[0]
			}
		}
	}()
	return s
}

func (s *stdinReader) readByte() (byte, error) {
	select {
	case b := <-s.ch:
		return b, nil
	case e := <-s.err:
		return 0, e
	}
}

func (s *stdinReader) readByteTimeout(d time.Duration) (byte, bool) {
	select {
	case b := <-s.ch:
		return b, true
	case <-time.After(d):
		return 0, false
	case <-s.err:
		return 0, false
	}
}

// keyKind 按键类别。
type keyKind int

const (
	keyChar keyKind = iota
	keyEnter      // 发送
	keyNewline    // 换行
	keyBackspace
	keyDelete
	keyTab
	keyUp
	keyDown
	keyLeft
	keyRight
	keyHome
	keyEnd
	keyEsc    // 单独 ESC
	keyCtrlC  // Ctrl+C
	keyEOF    // Ctrl+D
	keyCtrl   // Ctrl+letter，ch 存小写字母
)

type keyEvent struct {
	kind keyKind
	ch   rune
}

// readKey 读取一个按键事件。
func (s *stdinReader) readKey() (keyEvent, error) {
	b, err := s.readByte()
	if err != nil {
		return keyEvent{}, err
	}
	switch {
	case b == 0x1b: // ESC —— 转义序列或单独 ESC
		return s.readEsc()
	case b == 0x0d: // CR —— Enter 发送
		return keyEvent{kind: keyEnter}, nil
	case b == 0x0a: // LF —— 视作换行（Ctrl+J 风格）
		return keyEvent{kind: keyNewline}, nil
	case b == 0x09:
		return keyEvent{kind: keyTab}, nil
	case b == 0x7f, b == 0x08: // DEL / BS
		return keyEvent{kind: keyBackspace}, nil
	case b == 0x03: // Ctrl+C
		return keyEvent{kind: keyCtrlC}, nil
	case b == 0x04: // Ctrl+D
		return keyEvent{kind: keyEOF}, nil
	case b < 0x20: // 其他控制字符 → Ctrl+letter
		return keyEvent{kind: keyCtrl, ch: rune(b + 0x60)}, nil
	case b < 0x80: // ASCII 可打印
		return keyEvent{kind: keyChar, ch: rune(b)}, nil
	default: // UTF-8 多字节：按首字节范围判定后续字节数
		var size int
		switch {
		case b < 0xC0:
			return keyEvent{kind: keyChar, ch: '?'}, nil // 无效首字节
		case b < 0xE0:
			size = 2
		case b < 0xF0:
			size = 3
		default:
			size = 4
		}
		var buf [4]byte
		buf[0] = b
		for i := 1; i < size; i++ {
			nb, ok := s.readByteTimeout(50 * time.Millisecond)
			if !ok {
				break
			}
			buf[i] = nb
		}
		r, _ := utf8.DecodeRune(buf[:size])
		if r == utf8.RuneError {
			return keyEvent{kind: keyChar, ch: '?'}, nil
		}
		return keyEvent{kind: keyChar, ch: r}, nil
	}
}

// readEsc 解析 ESC 开头的序列。
func (s *stdinReader) readEsc() (keyEvent, error) {
	b, ok := s.readByteTimeout(escTimeout)
	if !ok {
		return keyEvent{kind: keyEsc}, nil // 单独 ESC
	}
	switch b {
	case 0x0d, 0x0a: // Alt+Enter —— 换行
		return keyEvent{kind: keyNewline}, nil
	case '[': // CSI
		return s.readCSI()
	case 'O': // SS3（部分终端的功能键）
		fb, ok := s.readByteTimeout(escTimeout)
		if !ok {
			return keyEvent{kind: keyEsc}, nil
		}
		switch fb {
		case 'H':
			return keyEvent{kind: keyHome}, nil
		case 'F':
			return keyEvent{kind: keyEnd}, nil
		}
		return keyEvent{kind: keyEsc}, nil
	default: // Alt+key —— 简化为该字符
		if b < 0x80 {
			return keyEvent{kind: keyChar, ch: rune(b)}, nil
		}
		return keyEvent{kind: keyEsc}, nil
	}
}

// readCSI 解析 CSI 序列：\x1b[<params><final>。
func (s *stdinReader) readCSI() (keyEvent, error) {
	var params []byte
	for {
		b, ok := s.readByteTimeout(escTimeout)
		if !ok {
			return keyEvent{kind: keyEsc}, nil
		}
		if (b >= '0' && b <= '9') || b == ';' || b == '?' || b == '>' {
			params = append(params, b)
			continue
		}
		switch b {
		case 'A':
			return keyEvent{kind: keyUp}, nil
		case 'B':
			return keyEvent{kind: keyDown}, nil
		case 'C':
			return keyEvent{kind: keyRight}, nil
		case 'D':
			return keyEvent{kind: keyLeft}, nil
		case 'H':
			return keyEvent{kind: keyHome}, nil
		case 'F':
			return keyEvent{kind: keyEnd}, nil
		case '~':
			switch parseParam(params, 0) {
			case 3:
				return keyEvent{kind: keyDelete}, nil
			case 1, 7:
				return keyEvent{kind: keyHome}, nil
			case 4, 8:
				return keyEvent{kind: keyEnd}, nil
			}
			return keyEvent{kind: keyEsc}, nil
		case 'u': // CSI u 格式（modifyOtherKeys）：Shift/Ctrl+Enter = CSI 13;Nu
			if parseParam(params, 0) == 13 {
				return keyEvent{kind: keyNewline}, nil
			}
			return keyEvent{kind: keyEsc}, nil
		default:
			return keyEvent{kind: keyEsc}, nil
		}
	}
}

// parseParam 解析 CSI 参数的第 idx 项（; 分隔），非数字或越界返回 0。
func parseParam(params []byte, idx int) int {
	parts := strings.Split(string(params), ";")
	if idx >= len(parts) {
		return 0
	}
	n := 0
	for _, c := range parts[idx] {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}

// keyAction 表示一次按键的处理结果。
type keyAction int

const (
	actionNone keyAction = iota
	actionDone // 提交当前输入
	actionQuit // Ctrl+C 退出
	actionEOF  // Ctrl+D 空行 EOF
)

// Editor 行编辑器：维护多行 buffer、光标、历史与补全状态，负责自身的 ANSI 渲染。
type Editor struct {
	in       *stdinReader
	width    int
	complete func(prefix string) []string
	// OnAppKey 应用级快捷键钩子。返回 true 表示应用已处理（如 Ctrl+K/L），
	// 编辑器将重置渲染状态并重画当前 buffer。
	OnAppKey func(ch rune) bool

	// Prompt 显示文本（如 "❯ "），PromptStyle 为其 ANSI 样式码。
	Prompt      string
	PromptStyle string

	buf      []rune
	pos      int
	history  []string
	histIdx  int // -1 = 新输入
	saved    string
	rendered int // 已渲染行数（用于下次渲染前上移清除）
}

// NewEditor 创建行编辑器。
func NewEditor(in *stdinReader, width int) *Editor {
	return &Editor{in: in, width: width, histIdx: -1, Prompt: "❯ "}
}

// SetComplete 设置补全函数（输入 / 开头时触发命令补全）。
func (e *Editor) SetComplete(f func(prefix string) []string) { e.complete = f }

// AddHistory 追加一条历史（空串、与上一条重复则忽略）。
func (e *Editor) AddHistory(s string) {
	if strings.TrimSpace(s) == "" {
		return
	}
	if len(e.history) > 0 && e.history[len(e.history)-1] == s {
		return
	}
	e.history = append(e.history, s)
	if len(e.history) > 200 {
		e.history = e.history[len(e.history)-200:]
	}
}

// ReadLine 读取一行输入，返回提交文本（多行含 \n）。
// io.EOF 表示 Ctrl+D 空行；errInterrupt 表示 Ctrl+C。
func (e *Editor) ReadLine() (string, error) {
	e.buf = e.buf[:0]
	e.pos = 0
	e.histIdx = -1
	e.saved = ""
	e.rendered = 0
	e.render()

	for {
		ev, err := e.in.readKey()
		if err != nil {
			e.clearRendered()
			return "", err
		}
		switch e.handleKey(ev) {
		case actionDone:
			line := string(e.buf)
			e.pos = len(e.buf)
			e.render()
			fmt.Print("\r\x1b[K\n") // 提交：保留显示，换行
			e.rendered = 0
			return line, nil
		case actionQuit:
			e.clearRendered()
			return "", errInterrupt
		case actionEOF:
			e.clearRendered()
			return "", io.EOF
		}
		e.render()
	}
}

// handleKey 处理一个按键，返回处理结果。
func (e *Editor) handleKey(ev keyEvent) keyAction {
	switch ev.kind {
	case keyEnter:
		return actionDone
	case keyNewline:
		e.insert('\n')
	case keyChar:
		e.insert(ev.ch)
	case keyBackspace:
		e.backspace()
	case keyDelete:
		e.delete()
	case keyTab:
		e.doComplete()
	case keyLeft:
		if e.pos > 0 {
			e.pos--
		}
	case keyRight:
		if e.pos < len(e.buf) {
			e.pos++
		}
	case keyHome:
		e.pos = 0
	case keyEnd:
		e.pos = len(e.buf)
	case keyUp:
		e.historyPrev()
	case keyDown:
		e.historyNext()
	case keyEsc:
		// ESC：清空当前输入（若有）
		if len(e.buf) > 0 {
			e.buf = e.buf[:0]
			e.pos = 0
		}
	case keyCtrlC:
		if len(e.buf) > 0 {
			e.buf = e.buf[:0]
			e.pos = 0
		} else {
			return actionQuit
		}
	case keyEOF:
		if len(e.buf) == 0 {
			return actionEOF
		}
		e.delete()
	case keyCtrl:
		// 应用级快捷键优先交给上层
		if e.OnAppKey != nil && e.OnAppKey(ev.ch) {
			e.rendered = 0 // 应用已输出内容，重置渲染状态
			return actionNone
		}
		switch ev.ch {
		case 'a':
			e.pos = 0
		case 'e':
			e.pos = len(e.buf)
		case 'b':
			if e.pos > 0 {
				e.pos--
			}
		case 'f':
			if e.pos < len(e.buf) {
				e.pos++
			}
		case 'u': // 删到行首
			e.buf = e.buf[e.pos:]
			e.pos = 0
		case 'w': // 删一个词
			e.deleteWord()
		case 'h': // 同 Backspace
			e.backspace()
		case 'p':
			e.historyPrev()
		case 'n':
			e.historyNext()
		}
	}
	return actionNone
}

// ---------- 编辑操作 ----------

func (e *Editor) insert(ch rune) {
	e.buf = append(e.buf, 0)
	copy(e.buf[e.pos+1:], e.buf[e.pos:])
	e.buf[e.pos] = ch
	e.pos++
}

func (e *Editor) backspace() {
	if e.pos == 0 {
		return
	}
	e.buf = append(e.buf[:e.pos-1], e.buf[e.pos:]...)
	e.pos--
}

func (e *Editor) delete() {
	if e.pos >= len(e.buf) {
		return
	}
	e.buf = append(e.buf[:e.pos], e.buf[e.pos+1:]...)
}

func (e *Editor) deleteWord() {
	if e.pos == 0 {
		return
	}
	end := e.pos
	for end > 0 && (e.buf[end-1] == ' ' || e.buf[end-1] == '\n') {
		end--
	}
	for end > 0 && e.buf[end-1] != ' ' && e.buf[end-1] != '\n' {
		end--
	}
	e.buf = append(e.buf[:end], e.buf[e.pos:]...)
	e.pos = end
}

// ---------- 历史 ----------

func (e *Editor) historyPrev() {
	if len(e.history) == 0 || e.histIdx <= 0 {
		return
	}
	if e.histIdx == -1 {
		e.saved = string(e.buf)
		e.histIdx = len(e.history)
	}
	e.histIdx--
	e.buf = []rune(e.history[e.histIdx])
	e.pos = len(e.buf)
}

func (e *Editor) historyNext() {
	if e.histIdx == -1 {
		return
	}
	e.histIdx++
	if e.histIdx >= len(e.history) {
		e.histIdx = -1
		e.buf = []rune(e.saved)
		e.pos = len(e.buf)
		return
	}
	e.buf = []rune(e.history[e.histIdx])
	e.pos = len(e.buf)
}

// ---------- Tab 补全 ----------

func (e *Editor) doComplete() {
	if e.complete == nil {
		return
	}
	start := e.pos
	for start > 0 && e.buf[start-1] != ' ' && e.buf[start-1] != '\n' {
		start--
	}
	word := string(e.buf[start:e.pos])
	if word == "" {
		return
	}
	cands := e.complete(word)
	if len(cands) == 0 {
		return
	}
	var comp string
	if len(cands) == 1 {
		comp = cands[0]
	} else {
		comp = commonPrefix(cands)
		if comp == word {
			return // 无法继续补全
		}
	}
	rs := []rune(comp)
	newBuf := make([]rune, 0, len(e.buf)-e.pos+start+len(rs))
	newBuf = append(newBuf, e.buf[:start]...)
	newBuf = append(newBuf, rs...)
	newBuf = append(newBuf, e.buf[e.pos:]...)
	e.buf = newBuf
	e.pos = start + len(rs)
	// 单候选命令补全后补一个空格，便于继续输入参数
	if len(cands) == 1 && strings.HasPrefix(comp, "/") {
		e.insert(' ')
	}
}

func commonPrefix(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	p := ss[0]
	for _, s := range ss[1:] {
		for !strings.HasPrefix(s, p) {
			p = p[:len(p)-1]
			if p == "" {
				return ""
			}
		}
	}
	return p
}

// ---------- 渲染 ----------

// render 重画当前 buffer。先上移已渲染的行数并清到屏底，再输出 prompt+buffer，
// 最后将光标定位到 buffer 中 pos 位置。
func (e *Editor) render() {
	if e.rendered > 1 {
		fmt.Printf("\x1b[%dF", e.rendered-1)
	}
	fmt.Print("\r\x1b[J")
	fmt.Print(e.PromptStyle + e.Prompt + reset)
	text := string(e.buf)
	fmt.Print(text)
	e.rendered = strings.Count(text, "\n") + 1

	// 光标定位：计算目标行列（相对渲染起始行）
	before := string(e.buf[:e.pos])
	curRow := strings.Count(before, "\n")
	lastSeg := before[strings.LastIndex(before, "\n")+1:]
	curCol := utf8.RuneCountInString(lastSeg)
	if curRow == 0 {
		curCol += utf8.RuneCountInString(e.Prompt)
	}
	endRow := e.rendered - 1
	if curRow < endRow {
		fmt.Printf("\x1b[%dA", endRow-curRow)
	} else if curRow > endRow {
		fmt.Printf("\x1b[%dB", curRow-endRow)
	}
	fmt.Print("\r")
	if curCol > 0 {
		fmt.Printf("\x1b[%dC", curCol)
	}
}

// clearRendered 清除已渲染的 buffer 显示，光标回到渲染起始行。
func (e *Editor) clearRendered() {
	if e.rendered > 1 {
		fmt.Printf("\x1b[%dF", e.rendered-1)
	}
	fmt.Print("\r\x1b[J")
	e.rendered = 0
}
