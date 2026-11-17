package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// errWaitTimeout 监听等待超时（提示文本已由 acceptPeer 打印，调用方只需返回 1）
var errWaitTimeout = errors.New("等待超时")

// acceptPeer 监听并等待对端连入：打印监听行、防火墙提示、混合密钥、等待行。
// peerName 为对端称谓（正向模式 "发送端" / 反向模式 "接收端"），只影响提示文本。
func acceptPeer(bind string, port int, ipOverride string, waitSec int, peerName string, sender bool) (*SecureSession, error) {
	psk, err := newPSK()
	if err != nil {
		return nil, err
	}
	code := encodeCode(psk)

	ln, err := net.Listen("tcp", net.JoinHostPort(bind, fmt.Sprint(port)))
	if err != nil {
		return nil, fmt.Errorf("监听失败: %v", err)
	}
	defer ln.Close()
	actualPort := ln.Addr().(*net.TCPAddr).Port

	logf("监听 %s:%d，等待 %d 秒", bind, actualPort, waitSec)
	logf("防火墙提示: 若%s连不上, 请放行 %d/tcp（示例: ufw allow %d/tcp）", peerName, actualPort, actualPort)
	logf("混合密钥（任选其一发给%s）:", peerName)

	// 出口 IP 优先，--ip 给出则只打该行
	var ips []string
	egress := ""
	if s := strings.TrimSpace(ipOverride); s != "" {
		ips = []string{s}
	} else {
		egress = egressIP()
		ips = mergeCandidates(egress, localIPv4s())
	}
	warnFallback := false
	if len(ips) == 0 {
		ips = []string{"127.0.0.1"}
		warnFallback = true
	}
	for _, ip := range ips {
		line := buildKey(ip, actualPort, code)
		if ip == egress {
			fmt.Printf("  %s  （出口IP）\n", line)
		} else {
			fmt.Printf("  %s\n", line)
		}
	}
	if warnFallback {
		errf("警告: 未探测到可用 IP，已回退到 127.0.0.1（仅本机可用）")
	}
	logf("等待%s连接...", peerName)

	var expires time.Time
	if waitSec > 0 {
		expires = time.Now().Add(time.Duration(waitSec) * time.Second)
	}
	if tcpLn, ok := ln.(*net.TCPListener); ok && !expires.IsZero() {
		_ = tcpLn.SetDeadline(expires)
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				logf("等待超时（%d 秒），退出", waitSec)
				return nil, errWaitTimeout
			}
			return nil, fmt.Errorf("接受连接失败: %w", err)
		}
		applyTCPOptions(conn)
		deadline := time.Now().Add(30 * time.Second)
		if !expires.IsZero() && expires.Before(deadline) {
			deadline = expires
		}
		_ = conn.SetDeadline(deadline)
		sess, err := handshake(conn, psk, sessionRole{sender: sender})
		if err != nil {
			_ = conn.Close()
			errf("拒绝未通过密钥校验的连接")
			continue
		}
		_ = conn.SetDeadline(time.Time{})
		return sess, nil
	}

}

// runReceive 接收端主流程
func runReceive(args []string) int {
	return runReceiveWithInput(args, newInputScanner(os.Stdin))
}

func runReceiveWithInput(args []string, sc *bufio.Scanner) int {
	fs := flag.NewFlagSet("receive", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	key := fs.String("key", "", "发送方连接码（仅 -R）；跳过密钥输入")
	maxBytes := fs.Int64("max-bytes", 100<<30, "最大接收字节数，0 为不限")
	maxEntries := fs.Int("max-entries", 100000, "最大文件和目录条目数")
	outDir := fs.String("o", defaultStoreDir(), "存储目录")
	port := fs.Int("p", 8977, "监听端口（仅正向模式）")
	bind := fs.String("b", "0.0.0.0", "绑定地址（仅正向模式）")
	ipOverride := fs.String("ip", "", "对外 IP（仅正向模式）")
	waitSec := fs.Int("wait", 120, "无连接等待秒数（仅正向模式）")
	reverse := fs.Bool("R", false, "反向模式: 本端拨号连接发送端（由发送端监听并给出混合密钥）")
	fs.BoolVar(reverse, "reverse", false, "反向模式（同 -R）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *maxBytes < 0 || *maxBytes == int64(^uint64(0)>>1) || *maxEntries < 1 {
		errf("接收限额无效")
		return 2
	}
	if fs.NArg() > 0 {
		errf("多余的参数: %v", fs.Args())
		return 2
	}
	if *waitSec < 0 || *waitSec > 86400 || *port < 0 || *port > 65535 {
		errf("--wait 必须为 0..86400，端口必须为 0..65535")
		return 2
	}
	if *reverse {
		if bad := explicitlySet(fs, "p", "b", "ip", "wait"); len(bad) > 0 {
			errf("反向模式下本端为拨号方，%s 无效（监听参数属于发送端）", strings.Join(bad, ", "))
			return 2
		}
	} else if explicitlySet(fs, "key") != nil {
		errf("--key 仅在 receive -R 下使用")
		return 2
	}

	receiveMaxBytes, receiveMaxEntries = *maxBytes, *maxEntries
	// 1. 存储目录
	created, err := ensureDir(*outDir)
	if err != nil {
		errf("创建存储目录失败: %v", err)
		return 1
	}
	tag := "已存在"
	if created {
		tag = "已创建"
	}
	logf("存储目录: %s（%s）", *outDir, tag)

	// 2. 建立连接（正向：监听等待；反向：拨号）
	var (
		sess *SecureSession
		conn net.Conn
		psk  []byte
		peer string
	)
	if *reverse {
		keyStr := strings.TrimSpace(*key)
		var kerr error
		if keyStr == "" {
			keyStr, kerr = readLine(sc, "输入发送端混合密钥: ")
		}
		if kerr != nil || keyStr == "" {
			errf("未提供混合密钥")
			return 2
		}
		addr, p, perr := parseKey(keyStr)
		if perr != nil {
			errf("混合密钥格式错误: %v", perr)
			return 1
		}
		psk = p
		c, derr := net.DialTimeout("tcp", addr, 10*time.Second)
		if derr != nil {
			errf("连接失败: %v", derr)
			return 1
		}
		applyTCPOptions(c)
		conn, peer = c, addr
	} else {
		s, aerr := acceptPeer(*bind, *port, *ipOverride, *waitSec, "发送端", false)
		if aerr != nil {
			errf("%v", aerr)
			return 1
		}
		sess = s
		conn, peer = s.conn, s.conn.RemoteAddr().String()
	}
	defer conn.Close()

	// 3. 握手（30 秒上限）
	if sess == nil {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
		sess, err = handshake(conn, psk, sessionRole{tcpClient: true, sender: false})
		if err != nil {
			errf("密钥校验失败（密钥码错误或信道被篡改）")
			return 1
		}
		_ = conn.SetDeadline(time.Time{})
	}
	logf("已连接 %s，密钥校验通过（加密信道: X25519+AES-256-GCM）", peer)

	// 4. 读 offer
	raw, err := sess.ReadCtrl()
	if err != nil {
		errf("读取任务信息失败: %v", err)
		return 1
	}
	var offer offerMsg
	if err := json.Unmarshal(raw, &offer); err != nil || offer.T != "offer" {
		errf("任务信息格式错误")
		return 1
	}
	if offer.Mode != "tar" && offer.Mode != "serial" {
		errf("未知传输模式: %s", offer.Mode)
		return 1
	}

	if offer.TotalBytes < 0 || offer.TotalFiles < 0 || (*maxBytes > 0 && offer.TotalBytes > *maxBytes) || offer.TotalFiles > *maxEntries {
		errf("任务超过接收限额或数值无效")
		return 1
	}
	logf("接收任务: %d 项, %s, %d 个文件", len(offer.Entries), humanSize(offer.TotalBytes), offer.TotalFiles)
	for i, e := range offer.Entries {
		if e.Kind == "dir" {
			fmt.Printf("  %d. %-7s  [目录] %s / %d 个文件\n", i+1, fmt.Sprintf("%q", e.Name), humanSize(e.Bytes), e.Files)
		} else {
			fmt.Printf("  %d. %-7s  [文件] %s\n", i+1, fmt.Sprintf("%q", e.Name), humanSize(e.Bytes))
		}
	}
	if err := sess.WriteJSON(acceptMsg{T: "accept", Dir: "", Overwrite: "rename"}); err != nil {
		errf("回送确认失败: %v", err)
		return 1
	}
	if offer.Mode == "tar" {
		logf("模式: 打包传输")
	} else {
		logf("模式: 串行传输")
	}

	// 5. 传输
	stats := newStats(offer.TotalBytes, offer.TotalFiles)
	bar := newProgressBar(offer.TotalBytes)
	failedFiles := 0

	switch offer.Mode {
	case "tar":
		ok, err := receiveTarStream(sess, *outDir, bar)
		if err != nil {
			bar.finish()
			errf("传输失败: %v", err)
			return 1
		}
		if !ok {
			bar.finish()
			errf("校验失败: SHA-256 不一致")
			return 1
		}
	case "serial":
		_, failedFiles, err = receiveSerialStream(sess, *outDir, bar)
		if err != nil {
			bar.finish()
			errf("传输失败: %v", err)
			return 1
		}
	}
	bar.finish()

	logf("校验通过: SHA-256 一致")
	if failedFiles > 0 {
		errf("失败文件: %d 个（其余文件已成功接收）", failedFiles)
		return 1
	}
	logf("完成: %s", stats.line())
	logf("已保存至 %s", *outDir)
	return 0
}
