package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

func selectionFixture(t *testing.T) (string, string, string, string) {
	t.Helper()
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(root, "A")
	b := filepath.Join(root, "B")
	for _, dir := range []string{a, b} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	af, bf := filepath.Join(a, "alpha.txt"), filepath.Join(b, "beta.txt")
	if err := os.WriteFile(af, []byte("alpha"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bf, []byte("beta"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, a, af, bf
}

func TestSelectionParentChildDeduplication(t *testing.T) {
	root, parent, child, other := selectionFixture(t)
	for _, paths := range [][]string{{child, parent, other}, {parent, child, other}, {child, filepath.Join(parent, ".", "alpha.txt"), other, parent}} {
		q, err := buildSelectedQueue(context.Background(), paths)
		if err != nil {
			t.Fatal(err)
		}
		if len(q) != 2 {
			t.Fatalf("duplicate paths retained: %v", paths)
		}
		var bytes int64
		var files int
		for _, e := range q {
			bytes += e.Bytes
			files += e.Files
		}
		if bytes != 9 || files != 2 {
			t.Fatalf("files counted twice: %d bytes, %d files", bytes, files)
		}
	}
	if pathWithin(parent, filepath.Join(root, "A-sibling", "file")) {
		t.Fatal("prefix sibling treated as child")
	}
	link := filepath.Join(root, "hardlink.txt")
	if err := os.Link(child, link); err == nil {
		q, err := buildSelectedQueue(context.Background(), []string{child, link})
		if err != nil || len(q) != 2 {
			t.Fatal("distinct hardlink names lost", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := buildSelectedQueue(ctx, []string{parent}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancel ignored", err)
	}
}

func TestSelectionManualFallbackAndSymlinks(t *testing.T) {
	root, parent, child, _ := selectionFixture(t)
	q, code := inputQueueWithSelection(newInputScanner(strings.NewReader(parent+"\n\n")), []string{child})
	if code != 0 || len(q) != 1 || q[0].Path != parent {
		t.Fatal("manual fallback lost selection/dedup", code, q)
	}
	link := filepath.Join(root, "alias")
	if err := os.Symlink(parent, link); err == nil {
		var selection pathSelection
		if _, err := selection.add(link); err == nil {
			t.Fatal("symlink selected")
		}
		if _, err := selection.add(filepath.Join(link, "alpha.txt")); err != nil {
			t.Fatal(err)
		}
		if _, err := selection.add(child); err != nil || len(selection.items) != 1 {
			t.Fatal("ancestor alias not deduplicated", err)
		}
	}
}

type frameScreen struct {
	tcell.SimulationScreen
	frames chan string
}

func (s *frameScreen) Show() {
	s.SimulationScreen.Show()
	w, h := s.Size()
	var out strings.Builder
	for y := 0; y < h; y++ {
		for x := 0; x < w; {
			text, _, width := s.Get(x, y)
			if text == "" {
				text = " "
			}
			out.WriteString(text)
			x += max(1, width)
		}
		out.WriteByte('\n')
	}
	select {
	case s.frames <- out.String():
	default:
	}
}

type browserOutcome struct {
	result browserResult
	err    error
}
type browserHarness struct {
	screen   *frameScreen
	out      chan browserOutcome
	finished chan struct{}
}

func startBrowserTest(t *testing.T, dir string) *browserHarness {
	t.Helper()
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatal(err)
	}
	sim.SetSize(110, 26)
	s := &frameScreen{SimulationScreen: sim, frames: make(chan string, 128)}
	h := &browserHarness{screen: s, out: make(chan browserOutcome, 1), finished: make(chan struct{})}
	go func() {
		defer close(h.finished)
		result, err := runFileBrowser(s, dir, nil)
		h.out <- browserOutcome{result, err}
	}()
	t.Cleanup(func() {
		select {
		case <-h.finished:
		default:
			_ = s.PostEvent(tcell.NewEventKey(tcell.KeyCtrlC, 0, tcell.ModNone))
			select {
			case <-h.finished:
			case <-time.After(3 * time.Second):
				s.Fini()
				t.Error("browser failed to cancel")
			}
		}
		s.Fini()
	})
	h.wait(t, func(frame string) bool { return strings.Contains(frame, "空格勾选，Enter") })
	return h
}
func (h *browserHarness) wait(t *testing.T, match func(string) bool) string {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	last := ""
	for {
		select {
		case frame := <-h.screen.frames:
			last = frame
			if match(frame) {
				return frame
			}
		case <-deadline.C:
			t.Fatalf("browser frame timed out:\n%s", last)
			return ""
		}
	}
}
func (h *browserHarness) key(key tcell.Key, r rune) { h.screen.InjectKey(key, r, tcell.ModNone) }
func (h *browserHarness) result(t *testing.T) browserOutcome {
	t.Helper()
	select {
	case result := <-h.out:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("browser did not finish")
		return browserOutcome{}
	}
}

func TestBrowserCrossDirectorySelectionQueueAndConfirmation(t *testing.T) {
	root, parent, _, other := selectionFixture(t)
	h := startBrowserTest(t, root)
	h.key(tcell.KeyHome, 0)
	h.key(tcell.KeyDown, 0)
	h.key(tcell.KeyEnter, 0)
	h.wait(t, func(frame string) bool {
		return strings.Contains(frame, "alpha.txt") && strings.Contains(frame, "空格勾选，Enter")
	})
	h.key(tcell.KeyEnd, 0)
	h.key(tcell.KeyRune, ' ')
	h.wait(t, func(frame string) bool {
		return strings.Contains(frame, "已选 1 项") && strings.Contains(frame, "[x]")
	})
	h.key(tcell.KeyLeft, 0)
	h.wait(t, func(frame string) bool {
		return strings.Contains(frame, `"B"`) && strings.Contains(frame, "空格勾选，Enter")
	})
	h.key(tcell.KeyHome, 0)
	h.key(tcell.KeyDown, 0)
	h.key(tcell.KeyDown, 0)
	h.key(tcell.KeyEnter, 0)
	h.wait(t, func(frame string) bool {
		return strings.Contains(frame, "beta.txt") && strings.Contains(frame, "空格勾选，Enter")
	})
	h.key(tcell.KeyEnd, 0)
	h.key(tcell.KeyRune, ' ')
	h.wait(t, func(frame string) bool { return strings.Contains(frame, "已选 2 项") })
	h.key(tcell.KeyLeft, 0)
	h.wait(t, func(frame string) bool {
		return strings.Contains(frame, `"A"`) && strings.Contains(frame, "空格勾选，Enter")
	})
	h.key(tcell.KeyHome, 0)
	h.key(tcell.KeyDown, 0)
	h.key(tcell.KeyRune, ' ')
	h.wait(t, func(frame string) bool {
		return strings.Contains(frame, "已选 2 项") && strings.Contains(frame, `[x] <DIR> "A"`)
	})
	h.key(tcell.KeyTab, 0)
	h.wait(t, func(frame string) bool {
		return strings.Contains(frame, "待发送队列") && strings.Contains(frame, "beta.txt")
	})
	h.key(tcell.KeyEnd, 0)
	h.key(tcell.KeyDelete, 0)
	h.wait(t, func(frame string) bool { return strings.Contains(frame, "已选 1 项") })
	h.key(tcell.KeyTab, 0)
	h.key(tcell.KeyRune, ' ')
	h.wait(t, func(frame string) bool { return strings.Contains(frame, "已选 2 项") })
	h.key(tcell.KeyF10, 0)
	result := h.result(t)
	if result.err != nil || result.result.manual || len(result.result.queue) != 2 {
		t.Fatal("confirm failed", result.err)
	}
	want := map[string]bool{parent: true, other: true}
	for _, path := range result.result.paths {
		if !want[path] {
			t.Errorf("unexpected selected path %s", path)
		}
		delete(want, path)
	}
	if len(want) != 0 {
		t.Fatal("cross-directory selection lost", want)
	}
}

func TestBrowserJumpManualAndPasteSafety(t *testing.T) {
	root, parent, _, _ := selectionFixture(t)
	h := startBrowserTest(t, root)
	_ = h.screen.PostEvent(tcell.NewEventPaste(true))
	h.key(tcell.KeyRune, 'a')
	h.key(tcell.KeyF10, 0)
	_ = h.screen.PostEvent(tcell.NewEventPaste(false))
	h.key(tcell.KeyRune, 'g')
	h.wait(t, func(frame string) bool { return strings.Contains(frame, "跳转目录:") })
	for _, r := range parent {
		h.key(tcell.KeyRune, r)
	}
	h.key(tcell.KeyEnter, 0)
	h.wait(t, func(frame string) bool {
		return strings.Contains(frame, "alpha.txt") && strings.Contains(frame, "空格勾选，Enter")
	})
	h.key(tcell.KeyRune, 'a')
	h.wait(t, func(frame string) bool { return strings.Contains(frame, "已选 1 项") })
	h.key(tcell.KeyRune, 'm')
	result := h.result(t)
	if result.err != nil || !result.result.manual || !reflect.DeepEqual(result.result.paths, []string{parent}) {
		t.Fatal("manual fallback/paste safety failed", result)
	}
}

func TestBrowserCancelAndSmallWindow(t *testing.T) {
	root, _, _, _ := selectionFixture(t)
	h := startBrowserTest(t, root)
	h.key(tcell.KeyRune, 'q')
	if result := h.result(t); !errors.Is(result.err, errBrowserCancelled) {
		t.Fatal("cancel failed", result.err)
	}
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	b := &fileBrowser{dir: root, status: "准备选择"}
	for _, size := range [][2]int{{110, 26}, {40, 12}, {20, 4}, {80, 24}} {
		s.SetSize(size[0], size[1])
		b.draw(s)
	}
}

func TestBrowserEscNavigationAndYSelection(t *testing.T) {
	root, _, child, _ := selectionFixture(t)
	h := startBrowserTest(t, root)
	enterA := func() {
		h.key(tcell.KeyHome, 0)
		h.key(tcell.KeyDown, 0)
		h.key(tcell.KeyEnter, 0)
		h.wait(t, func(frame string) bool {
			return strings.Contains(frame, "alpha.txt") && strings.Contains(frame, "空格勾选，Enter")
		})
	}
	enterA()
	h.key(tcell.KeyEnd, 0)
	h.key(tcell.KeyRune, 'Y')
	h.wait(t, func(frame string) bool { return strings.Contains(frame, "已选 1 项") })
	h.key(tcell.KeyRune, 'y')
	h.wait(t, func(frame string) bool { return strings.Contains(frame, "已选 0 项") })
	h.key(tcell.KeyRune, ' ')
	h.wait(t, func(frame string) bool { return strings.Contains(frame, "已选 1 项") })
	h.key(tcell.KeyEsc, 0)
	h.wait(t, func(frame string) bool {
		return strings.Contains(frame, `"A"`) && strings.Contains(frame, `"B"`) && strings.Contains(frame, "空格勾选，Enter")
	})
	enterA()
	h.key(tcell.KeyLeft, 0)
	h.wait(t, func(frame string) bool {
		return strings.Contains(frame, `"B"`) && strings.Contains(frame, "空格勾选，Enter")
	})
	h.key(tcell.KeyTab, 0)
	h.wait(t, func(frame string) bool { return strings.Contains(frame, "待发送队列") })
	h.key(tcell.KeyEsc, 0)
	h.wait(t, func(frame string) bool { return strings.Contains(frame, "已返回目录浏览") })
	h.key(tcell.KeyRune, 'g')
	h.wait(t, func(frame string) bool { return strings.Contains(frame, "跳转目录:") })
	h.key(tcell.KeyRune, 'y')
	h.key(tcell.KeyEsc, 0)
	h.wait(t, func(frame string) bool {
		return strings.Contains(frame, "G 跳转路径") && strings.Contains(frame, "已选 1 项")
	})
	h.key(tcell.KeyCtrlS, 0)
	result := h.result(t)
	if result.err != nil || len(result.result.paths) != 1 || result.result.paths[0] != child {
		t.Fatal("Esc changed the queue or Y confirmed instead of selecting", result)
	}
}
