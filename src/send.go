package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// runSend 发送端主流程
//
// 网络方向有两种：
//   - 正向（默认）：本端拨号，接收端监听并给出混合密钥
//   - 反向（-R）  ：本端监听并给出混合密钥，接收端拨号连入
//
// 两种方向的协议与数据流向完全一致，只有 TCP 建立方向不同。
func runSend(args []string) int {
	// --mode 允许出现在位置参数之后：先手工摘出，再交给 FlagSet
	var modeArg string
	fargs := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--mode" || a == "-mode":
			if i+1 >= len(args) {
				errf("--mode 缺少取值")
				return 2
			}
			modeArg = args[i+1]
			i++
		case strings.HasPrefix(a, "--mode=") || strings.HasPrefix(a, "-mode="):
			modeArg = a[strings.Index(a, "=")+1:]
		default:
			fargs = append(fargs, a)
		}
	}

	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	reverse := fs.Bool("R", false, "反向模式: 本端监听并给出混合密钥（由接收端拨号连入）")
	fs.BoolVar(reverse, "reverse", false, "反向模式（同 -R）")
	port := fs.Int("p", 8977, "监听端口（仅反向模式）")
	bind := fs.String("b", "0.0.0.0", "绑定地址（仅反向模式）")
	ipOverride := fs.String("ip", "", "对外 IP（仅反向模式）")
	waitSec := fs.Int("wait", 120, "无连接等待秒数（仅反向模式）")
	if err := fs.Parse(fargs); err != nil {
		return 2
	}

	mode := strings.ToLower(strings.TrimSpace(modeArg))
	if mode != "" && mode != "tar" && mode != "serial" {
		errf("--mode 只能为 tar 或 serial")
		return 2
	}
	if *waitSec < 0 || *waitSec > 86400 || *port < 0 || *port > 65535 {
		errf("--wait 必须为 0..86400，端口必须为 0..65535")
		return 2
	}
	if *reverse {
		if len(fs.Args()) > 0 {
			errf("反向模式下由本端生成混合密钥，不接受位置参数（多余的: %v）", fs.Args())
			return 2
		}
	} else if bad := explicitlySet(fs, "p", "b", "ip", "wait"); len(bad) > 0 {
		errf("监听参数仅在 -R 反向模式下有效: %s", strings.Join(bad, ", "))
		return 2
	}

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024)

	// 1. 建立连接（正向：拨号；反向：监听等待）
	var sess *SecureSession
	if *reverse {
		s, aerr := acceptPeer(*bind, *port, *ipOverride, *waitSec, "接收端", true)
		if aerr != nil {
			errf("%v", aerr)
			return 1
		}
		defer s.conn.Close()
		sess = s
		logf("已连接 %s，密钥校验通过（加密信道: X25519+AES-256-GCM）", s.conn.RemoteAddr().String())
	} else {
		keyStr := strings.TrimSpace(strings.Join(fs.Args(), " "))
		if keyStr == "" {
			line, err := readLine(sc, "输入接收端混合密钥: ")
			if err != nil {
				errf("未提供混合密钥")
				return 2
			}
			keyStr = line
		}
		addr, psk, err := parseKey(keyStr)
		if err != nil {
			errf("混合密钥格式错误: %v", err)
			return 1
		}
		conn, derr := net.DialTimeout("tcp", addr, 10*time.Second)
		if derr != nil {
			errf("连接失败: %v", derr)
			return 1
		}
		defer conn.Close()
		applyTCPOptions(conn)
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
		s, herr := handshake(conn, psk, sessionRole{tcpClient: true, sender: true})
		if herr != nil {
			errf("密钥校验失败（密钥码错误或信道被篡改）")
			return 1
		}
		_ = conn.SetDeadline(time.Time{})
		sess = s
		logf("已连接 %s，密钥校验通过（加密信道: X25519+AES-256-GCM）", addr)
	}

	// 2. 交互式队列
	queue, rc := inputQueue(sc)
	if rc != 0 {
		return rc
	}

	totalBytes := int64(0)
	totalFiles := 0
	for _, e := range queue {
		totalBytes += e.Bytes
		totalFiles += e.Files
	}

	// 3. 模式选择
	if mode == "" {
		fmt.Printf("队列共 %d 项, %s。选择传输模式:\n", len(queue), humanSize(totalBytes))
		fmt.Println("  [1] 打包传输（推荐: 多文件/目录, 流式 tar 不落盘）")
		fmt.Println("  [2] 串行传输（逐文件 SHA-256 校验, 单文件失败不影响其余）")
		for {
			line, err := readLine(sc, "输入 1 或 2: ")
			if err != nil {
				errf("读取输入失败")
				return 1
			}
			switch line {
			case "1":
				mode = "tar"
			case "2":
				mode = "serial"
			default:
				logf("请输入 1 或 2")
				continue
			}
			break
		}
	}

	// 4. offer
	offer := offerMsg{T: "offer", Mode: mode, TotalBytes: totalBytes, TotalFiles: totalFiles}
	for _, e := range queue {
		kind := "file"
		if e.IsDir {
			kind = "dir"
		}
		offer.Entries = append(offer.Entries, offerEntry{Name: e.Name, Kind: kind, Bytes: e.Bytes, Files: e.Files})
	}
	if err := sess.WriteJSON(offer); err != nil {
		errf("发送任务信息失败: %v", err)
		return 1
	}
	raw, err := sess.ReadCtrl()
	if err != nil {
		errf("等待接收端确认失败: %v", err)
		return 1
	}
	var acc acceptMsg
	if err := json.Unmarshal(raw, &acc); err != nil || acc.T != "accept" {
		errf("接收端应答异常")
		return 1
	}

	// 5. 传输
	if mode == "tar" {
		logf("开始传输（打包模式）...")
	} else {
		logf("开始传输（串行模式）...")
	}
	bar := newProgressBar(totalBytes)
	stats := newStats(totalBytes, totalFiles)

	var sumHex string
	failedFiles := 0

	if mode == "tar" {
		sumHex, err = sendTarStream(sess, queue, bar)
		if err != nil {
			bar.finish()
			errf("传输失败: %v", err)
			return 1
		}
	} else {
		files, ferr := flattenEntries(queue)
		if ferr != nil {
			errf("扫描失败: %v", ferr)
			return 1
		}
		sumHex, failedFiles, err = sendSerialStream(sess, files, bar)
		if err != nil {
			bar.finish()
			errf("传输失败: %v", err)
			return 1
		}
	}
	bar.finish()

	// 6. done
	if err := sess.WriteJSON(doneMsg{T: "done", SHA256: sumHex}); err != nil {
		errf("发送完成消息失败: %v", err)
		return 1
	}
	raw, err = sess.ReadCtrl()
	if err != nil {
		errf("等待对端确认失败: %v", err)
		return 1
	}
	var da doneAckMsg
	if err := json.Unmarshal(raw, &da); err != nil || da.T != "doneack" {
		errf("对端应答异常")
		return 1
	}
	if !da.OK || !strings.EqualFold(da.SHA256, sumHex) {
		reason := da.Err
		if reason == "" {
			reason = "校验不一致"
		}
		errf("对端校验失败: %s", reason)
		return 1
	}

	logf("对端确认: 校验通过")
	if failedFiles > 0 {
		errf("失败文件: %d 个（其余文件已成功传输）", failedFiles)
		return 1
	}
	logf("完成: %s", stats.line())
	return 0
}

// inputQueue 交互式收集传输队列
func inputQueue(sc *bufio.Scanner) ([]*queueEntry, int) {
	var queue []*queueEntry
	seen := make(map[string]bool)

	for {
		line, err := readLine(sc, "添加传输任务路径（直接回车结束）: ")
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(queue) == 0 {
					errf("队列为空，退出")
					return nil, 1
				}
				return queue, 0
			}
			errf("读取输入失败: %v", err)
			return nil, 1
		}
		if line == "" {
			if len(queue) == 0 {
				continue
			}
			return queue, 0
		}
		abs, aerr := filepath.Abs(line)
		if aerr != nil {
			abs = filepath.Clean(line)
		}
		if seen[abs] {
			logf("队列中已存在: %s", line)
			continue
		}
		if _, serr := os.Lstat(line); serr != nil {
			logf("路径不存在: %s", line)
			continue
		}
		e, serr := scanEntry(line)
		if serr != nil {
			errf("扫描失败: %s: %v", line, serr)
			return nil, 1
		}
		seen[abs] = true
		queue = append(queue, e)

		if e.IsDir {
			fmt.Printf("  + %-7s  [目录] %s / %d 个文件\n", e.Name, humanSize(e.Bytes), e.Files)
		} else {
			fmt.Printf("  + %-7s  [文件] %s\n", e.Name, humanSize(e.Bytes))
		}
	}
}
