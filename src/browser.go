package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/uniseg"
	"golang.org/x/term"
)

var errBrowserCancelled = errors.New("已取消文件选择")

type browserResult struct {
	paths  []string
	queue  []*queueEntry
	manual bool
}

type pickerTerminal struct {
	screen tcell.Screen
	once   sync.Once
}

func (t *pickerTerminal) close() { t.once.Do(t.screen.Fini) }

var activePicker atomic.Pointer[pickerTerminal]

func closeActivePicker() {
	if t := activePicker.Load(); t != nil {
		t.close()
	}
}

func terminalInputAvailable() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) && os.Getenv("TERM") != "dumb"
}

func pickTransferFiles(initial []string) (browserResult, error) {
	screen, err := tcell.NewScreen()
	if err != nil {
		return browserResult{}, err
	}
	if err = screen.Init(); err != nil {
		screen.Fini()
		return browserResult{}, err
	}
	t := &pickerTerminal{screen: screen}
	activePicker.Store(t)
	defer func() { t.close(); activePicker.CompareAndSwap(t, nil) }()
	dir, err := os.Getwd()
	if err != nil {
		return browserResult{}, err
	}
	return runFileBrowser(screen, dir, initial)
}

type browserEntry struct {
	name, path            string
	dir, disabled, parent bool
}
type browserUpdate struct {
	dir     string
	entries []browserEntry
	queue   []*queueEntry
	err     error
	confirm bool
}
type fileBrowser struct {
	dir                               string
	entries                           []browserEntry
	selection                         pathSelection
	cursor, queueCursor               int
	showQueue, busy, editing, pasting bool
	input                             []rune
	status                            string
}

func readBrowserDir(dir string) ([]browserEntry, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	entries := make([]browserEntry, 0, len(files)+1)
	parent := filepath.Dir(dir)
	if parent != dir {
		entries = append(entries, browserEntry{name: "..", path: parent, dir: true, parent: true})
	}
	for _, file := range files {
		entries = append(entries, browserEntry{name: file.Name(), path: filepath.Join(dir, file.Name()), dir: file.IsDir(), disabled: !file.IsDir() && !file.Type().IsRegular()})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].parent != entries[j].parent {
			return entries[i].parent
		}
		if entries[i].dir != entries[j].dir {
			return entries[i].dir
		}
		return entries[i].name < entries[j].name
	})
	return entries, nil
}

func (b *fileBrowser) selectionMark(name string) string {
	for _, item := range b.selection.items {
		if item.path == name {
			return "[x]"
		}
		if item.dir && pathWithin(item.path, name) {
			return "[-]"
		}
	}
	return "[ ]"
}

func (b *fileBrowser) toggle(name string) {
	item, err := canonicalSelectionPath(name)
	if err != nil {
		b.status = err.Error()
		return
	}
	for i, other := range b.selection.items {
		if sameSelectionPath(item.path, other.path) {
			b.selection.items = append(b.selection.items[:i], b.selection.items[i+1:]...)
			b.status = "已移除；不会修改磁盘上的文件。"
			return
		}
	}
	changed, err := b.selection.add(name)
	if err != nil {
		b.status = err.Error()
		return
	}
	if !changed {
		b.status = "此项已包含在选中的父目录内；如需单独选择，请先从队列移除父目录。"
		return
	}
	b.status = "已添加；父子目录已自动去重。可继续进入其他目录选择。"
}

func runFileBrowser(screen tcell.Screen, start string, initial []string) (browserResult, error) {
	screen.EnablePaste()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &fileBrowser{dir: start}
	for _, p := range initial {
		if _, err := b.selection.add(p); err != nil {
			return browserResult{}, err
		}
	}
	updates := make(chan browserUpdate, 1)
	post := func(update browserUpdate) {
		select {
		case updates <- update:
			_ = screen.PostEvent(tcell.NewEventInterrupt(nil))
		case <-ctx.Done():
		}
	}
	load := func(dir string) {
		b.busy = true
		b.status = "正在读取当前目录…（Esc 取消）"
		go func() {
			abs, err := filepath.Abs(dir)
			if err == nil {
				abs, err = filepath.EvalSymlinks(abs)
			}
			var entries []browserEntry
			if err == nil {
				entries, err = readBrowserDir(abs)
			}
			post(browserUpdate{dir: abs, entries: entries, err: err})
		}()
	}
	load(start)
	for {
		select {
		case update := <-updates:
			b.busy = false
			if update.err != nil {
				b.status = "操作失败: " + update.err.Error()
			} else if update.confirm {
				return browserResult{paths: b.selection.paths(), queue: update.queue}, nil
			} else {
				b.dir = update.dir
				b.entries = update.entries
				b.cursor = 0
				b.status = "空格勾选，Enter 进入目录；可跨目录连续添加。"
			}
		default:
		}
		b.draw(screen)
		event := screen.PollEvent()
		if event == nil {
			return browserResult{}, errBrowserCancelled
		}
		switch event := event.(type) {
		case *tcell.EventResize:
			screen.Sync()
		case *tcell.EventPaste:
			// Pasted path data is accepted only in the jump field; it never
			// becomes shortcut keys or an accidental confirmation.
			b.pasting = event.Start()
		case *tcell.EventKey:
			if b.pasting {
				if b.editing && event.Key() == tcell.KeyRune && !unicode.IsControl(event.Rune()) && len(b.input) < 4096 {
					b.input = append(b.input, event.Rune())
				}
				continue
			}
			if event.Key() == tcell.KeyCtrlC {
				return browserResult{}, errBrowserCancelled
			}
			if b.editing {
				switch event.Key() {
				case tcell.KeyEsc:
					b.editing = false
				case tcell.KeyBackspace, tcell.KeyBackspace2:
					if len(b.input) > 0 {
						b.input = b.input[:len(b.input)-1]
					}
				case tcell.KeyEnter:
					name := unquotePath(string(b.input))
					b.editing = false
					if name == "~" || strings.HasPrefix(name, "~/") || strings.HasPrefix(name, `~\`) {
						if home, err := os.UserHomeDir(); err == nil {
							name = filepath.Join(home, strings.TrimLeft(name[1:], `/\`))
						}
					}
					if !filepath.IsAbs(name) {
						name = filepath.Join(b.dir, name)
					}
					load(name)
				case tcell.KeyRune:
					if len(b.input) < 4096 && !unicode.IsControl(event.Rune()) {
						b.input = append(b.input, event.Rune())
					}
				}
				continue
			}
			if event.Key() == tcell.KeyEsc {
				return browserResult{}, errBrowserCancelled
			}
			if b.busy {
				continue
			}
			count := len(b.entries)
			cursor := &b.cursor
			if b.showQueue {
				count = len(b.selection.items)
				cursor = &b.queueCursor
			}
			_, height := screen.Size()
			page := max(1, height-9)
			switch event.Key() {
			case tcell.KeyTab, tcell.KeyBacktab:
				b.showQueue = !b.showQueue
			case tcell.KeyUp:
				*cursor = max(0, *cursor-1)
			case tcell.KeyDown:
				*cursor = min(max(0, count-1), *cursor+1)
			case tcell.KeyPgUp:
				*cursor = max(0, *cursor-page)
			case tcell.KeyPgDn:
				*cursor = min(max(0, count-1), *cursor+page)
			case tcell.KeyHome:
				*cursor = 0
			case tcell.KeyEnd:
				*cursor = max(0, count-1)
			case tcell.KeyLeft, tcell.KeyBackspace, tcell.KeyBackspace2:
				if !b.showQueue {
					load(filepath.Dir(b.dir))
				}
			case tcell.KeyEnter, tcell.KeyRight:
				if !b.showQueue && b.cursor < len(b.entries) {
					e := b.entries[b.cursor]
					if e.dir && !e.disabled {
						load(e.path)
					}
				}
			case tcell.KeyDelete:
				if b.showQueue && count > 0 {
					b.selection.items = append(b.selection.items[:*cursor], b.selection.items[*cursor+1:]...)
					*cursor = min(*cursor, max(0, count-2))
				}
			case tcell.KeyF10, tcell.KeyCtrlS:
				if len(b.selection.items) == 0 {
					b.status = "请先用空格选择文件/目录，或按 A 选择当前目录。"
					continue
				}
				paths := b.selection.paths()
				b.busy = true
				b.status = "正在检查所选目录和文件…（Esc 取消）"
				go func() {
					queue, err := buildSelectedQueue(ctx, paths)
					post(browserUpdate{queue: queue, err: err, confirm: true})
				}()
			case tcell.KeyRune:
				switch event.Rune() {
				case ' ':
					if b.showQueue {
						if count > 0 {
							b.toggle(b.selection.items[*cursor].path)
							*cursor = min(*cursor, max(0, len(b.selection.items)-1))
						}
					} else if b.cursor < len(b.entries) {
						e := b.entries[b.cursor]
						if !e.parent && !e.disabled {
							b.toggle(e.path)
						} else {
							b.status = "上级目录入口、软链接及特殊文件不能勾选。"
						}
					}
				case 'a', 'A':
					if !b.showQueue {
						b.toggle(b.dir)
					}
				case 'g', 'G':
					b.editing = true
					b.input = nil
				case 'h', 'H':
					if home, err := os.UserHomeDir(); err == nil {
						load(home)
					}
				case 'r', 'R':
					load(b.dir)
				case 'm', 'M':
					return browserResult{paths: b.selection.paths(), manual: true}, nil
				case 'q', 'Q':
					return browserResult{}, errBrowserCancelled
				}
			}
		}
	}
}

func browserText(screen tcell.Screen, x, y, width int, text string, style tcell.Style) {
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '\uFFFD'
		}
		return r
	}, text)
	g := uniseg.NewGraphemes(text)
	for g.Next() {
		w := g.Width()
		if w < 1 {
			continue
		}
		if width < w {
			break
		}
		screen.Put(x, y, g.Str(), style)
		x += w
		width -= w
	}
}

func browserTail(text string, width int) string {
	remaining := uniseg.StringWidth(text)
	if remaining <= width {
		return text
	}
	if width < 4 {
		return strings.Repeat(".", max(0, width))
	}
	g := uniseg.NewGraphemes(text)
	for g.Next() {
		remaining -= g.Width()
		_, end := g.Positions()
		if remaining <= width-3 {
			return "..." + text[end:]
		}
	}
	return ""
}

func (b *fileBrowser) draw(screen tcell.Screen) {
	w, h := screen.Size()
	base := tcell.StyleDefault.Foreground(tcell.ColorWhite).Background(tcell.ColorBlack)
	screen.SetStyle(base)
	screen.Clear()
	screen.HideCursor()
	if w < 32 || h < 9 {
		browserText(screen, 0, 0, w, "终端太小，请放大；Esc 取消。", base)
		screen.Show()
		return
	}
	browserText(screen, 0, 0, w, fmt.Sprintf("fsend 文件浏览器   已选 %d 项（父子自动去重）", len(b.selection.items)), base.Bold(true).Foreground(tcell.ColorAqua))
	browserText(screen, 0, 1, w, "当前位置: "+browserTail(fmt.Sprintf("%q", b.dir), w-10), base)
	label := "目录内容"
	if b.showQueue {
		label = "待发送队列（空格 / Delete 移除，Tab 返回浏览）"
	}
	browserText(screen, 0, 3, w, label, base.Bold(true))
	count := len(b.entries)
	cursor := b.cursor
	if b.showQueue {
		count = len(b.selection.items)
		cursor = b.queueCursor
	}
	rows := h - 9
	start := max(0, cursor-rows+1)
	for row := 0; row < rows && start+row < count; row++ {
		i := start + row
		style := base
		var text string
		if b.showQueue {
			e := b.selection.items[i]
			kind := "FILE"
			if e.dir {
				kind = "DIR"
			}
			text = fmt.Sprintf("[x] <%s> %s", kind, browserTail(fmt.Sprintf("%q", e.path), w-12))
		} else {
			e := b.entries[i]
			mark := b.selectionMark(e.path)
			kind := "FILE"
			if e.dir {
				kind = "DIR"
			}
			if e.parent {
				mark = "   "
			}
			if e.disabled {
				mark = "[!]"
				kind = "跳过"
				style = style.Foreground(tcell.ColorGray)
			}
			text = fmt.Sprintf("%s <%s> %q", mark, kind, e.name)
		}
		if i == cursor {
			style = style.Background(tcell.ColorNavy).Foreground(tcell.ColorWhite)
		}
		browserText(screen, 0, 4+row, w, text, style)
	}
	browserText(screen, 0, h-4, w, b.status, base.Foreground(tcell.ColorYellow))
	browserText(screen, 0, h-3, w, "↑↓ 移动  Enter 进入  ← 上级  空格 勾选  A 当前目录  Tab 队列", base)
	if b.editing {
		input := browserTail(string(b.input), w-12)
		browserText(screen, 0, h-2, w, "跳转目录: "+input, base)
		screen.ShowCursor(min(w-1, 10+uniseg.StringWidth(input)), h-2)
		browserText(screen, 0, h-1, w, "Enter 跳转（支持 ~ / Windows 盘符）  Esc 取消输入", base)
	} else {
		browserText(screen, 0, h-2, w, "G 跳转目录/盘符  H 用户目录  R 刷新  M 手动输入", base)
		browserText(screen, 0, h-1, w, "F10 / Ctrl+S 确认选择并检查  Esc / Q 取消发送", base.Bold(true))
	}
	screen.Show()
}
