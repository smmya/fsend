package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

func newInputScanner(r io.Reader) *bufio.Scanner {
	// A canonical-mode terminal may receive several pasted lines at once.
	// Do not let the line menu steal keystrokes intended for the raw-mode picker.
	if f, ok := r.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		r = oneByteReader{r}
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024)
	return sc
}

type oneByteReader struct{ io.Reader }

func (r oneByteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return r.Reader.Read(p)
}

// Pass this same scanner into the selected command: a second scanner would
// lose input already buffered from pipes or pasted multi-line input.
func interactiveArgs(sc *bufio.Scanner) ([]string, error) {
	fmt.Printf("fsend %s\n  [1] 发送文件\n  [2] 接收文件\n  [0] 退出\n", appVersion)
	role, err := menuChoice(sc, "选择操作: ", "", "1", "2", "0")
	if err != nil || role == "0" {
		return nil, err
	}
	sender := role == "1"
	preferred := "1"
	if !sender {
		preferred = "2"
	}
	fmt.Println("连接方式:\n  [1] 输入对方给你的连接码\n  [2] 生成连接码，发给对方并等待连接\n  [0] 退出")
	connection, err := menuChoice(sc, fmt.Sprintf("选择连接方式（回车选 %s）: ", preferred), preferred, "1", "2", "0")
	if err != nil || connection == "0" {
		return nil, err
	}
	args := []string{"receive"}
	if sender {
		args = []string{"send", "--mode", "tar"}
	}
	if !sender {
		dir, err := readPathLine(sc, fmt.Sprintf("保存目录（回车用 %s）: ", defaultStoreDir()))
		if err != nil {
			return nil, err
		}
		if dir != "" {
			args = append(args, "-o", dir)
		}
	}
	listening := connection == "2"
	if sender == listening {
		args = append(args, "-R")
	}
	if listening {
		host, err := readLine(sc, "对方可访问的 IP/域名（回车自动检测）: ")
		if err != nil {
			return nil, err
		}
		if host != "" {
			args = append(args, "--ip", host)
		}
		// Leave enough time to share the code and start the other side.
		args = append(args, "--wait", "600")
	}
	if sender {
		fmt.Println("终端中可多选文件/目录；选择后再连接。M 可切换手动输入；默认打包模式。")
	}
	return args, nil
}

func menuChoice(sc *bufio.Scanner, prompt, fallback string, choices ...string) (string, error) {
	for {
		value, err := readLine(sc, prompt)
		if err != nil {
			return "", err
		}
		if value == "" {
			value = fallback
		}
		for _, choice := range choices {
			if value == choice {
				return value, nil
			}
		}
		fmt.Printf("请输入 %s。\n", strings.Join(choices, " / "))
	}
}

// File paths are not trimmed: a trailing space can be part of a POSIX name.
// Paired quotes are accepted for paths pasted or dragged from a file manager.
func readPathLine(sc *bufio.Scanner, prompt string) (string, error) {
	fmt.Print(prompt)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return "", err
		}
		return "", io.EOF
	}
	return unquotePath(sc.Text()), nil
}

func unquotePath(value string) string {
	if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
		return value[1 : len(value)-1]
	}
	return value
}

type pathFlags []string

func (p *pathFlags) String() string         { return strings.Join(*p, ", ") }
func (p *pathFlags) Set(value string) error { *p = append(*p, value); return nil }
