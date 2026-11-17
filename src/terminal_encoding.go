package main

import (
	"os"
	"runtime"
	"strings"

	"github.com/gdamore/tcell/v2"
)

// Match tcell's POSIX precedence. Empty locales are already treated as UTF-8.
// An explicit legacy encoding is left alone; C/POSIX/ASCII are the common
// minimal-server defaults that otherwise replace Chinese text with '?'.
func asciiTerminalLocale(lookup func(string) string) bool {
	locale := ""
	for _, key := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if locale = lookup(key); locale != "" {
			break
		}
	}
	if at := strings.IndexByte(locale, '@'); at >= 0 {
		locale = locale[:at]
	}
	if locale == "C" || locale == "POSIX" {
		return true
	}
	if dot := strings.IndexByte(locale, '.'); dot >= 0 {
		switch strings.ToUpper(locale[dot+1:]) {
		case "ASCII", "US-ASCII", "ISO646", "ANSI_X3.4-1968", "ANSI_X3.4-1986":
			return true
		}
	}
	return false
}

// tcell snapshots its encoding during Init. Scope the override to that call,
// including all error paths. Go menus and path data remain UTF-8 throughout.
func initializeBrowserEncoding(init func() error, platform string) error {
	if platform == "windows" || !asciiTerminalLocale(os.Getenv) {
		return init()
	}
	old, existed := os.LookupEnv("LC_ALL")
	if err := os.Setenv("LC_ALL", "C.UTF-8"); err != nil {
		return err
	}
	defer func() {
		if existed {
			_ = os.Setenv("LC_ALL", old)
		} else {
			_ = os.Unsetenv("LC_ALL")
		}
	}()
	return init()
}

func newBrowserScreen() (tcell.Screen, error) {
	var screen tcell.Screen
	err := initializeBrowserEncoding(func() error {
		var err error
		screen, err = tcell.NewScreen()
		if err != nil {
			return err
		}
		if err = screen.Init(); err != nil {
			screen.Fini()
			return err
		}
		return nil
	}, runtime.GOOS)
	return screen, err
}
