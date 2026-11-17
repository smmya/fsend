package main

import (
	"bufio"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// logf 统一前缀日志（stdout）
func logf(format string, a ...any) {
	fmt.Fprintf(os.Stdout, "[fastsend] "+format+"\n", a...)
}

// errf 错误日志（stderr，同样带前缀）
func errf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "[fastsend] "+format+"\n", a...)
}

// ---------- 人类可读体积 ----------

func humanSize(n int64) string {
	const (
		kib = int64(1024)
		mib = 1024 * kib
		gib = 1024 * mib
	)
	switch {
	case n < kib:
		return fmt.Sprintf("%d B", n)
	case n < mib:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(kib))
	case n < gib:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(mib))
	default:
		return fmt.Sprintf("%.1f GiB", float64(n)/float64(gib))
	}
}

// mib1 以 MiB 为单位保留 1 位小数
func mib1(n int64) string {
	return fmt.Sprintf("%.1f", float64(n)/(1024*1024))
}

// ---------- 混合密钥 ----------

// newPSK 生成 15 字节密钥材料
func newPSK() ([]byte, error) {
	b := make([]byte, 15)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("生成密钥材料失败: %w", err)
	}
	return b, nil
}

// encodeCode 15 字节 → base32(无 padding) 24 字符 → 6 组 4 字符，用 - 分隔
func encodeCode(psk []byte) string {
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(psk)
	parts := make([]string, 0, 6)
	for i := 0; i < len(enc); i += 4 {
		end := i + 4
		if end > len(enc) {
			end = len(enc)
		}
		parts = append(parts, enc[i:end])
	}
	return strings.Join(parts, "-")
}

// decodeCode 密钥码 → 15 字节；容忍 - 与大小写
func decodeCode(code string) ([]byte, error) {
	s := strings.ToUpper(strings.TrimSpace(code))
	s = strings.ReplaceAll(s, "-", "")
	s = strings.TrimRight(s, "=")
	if s == "" {
		return nil, errors.New("密钥码为空")
	}
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("密钥码解码失败: %v", err)
	}
	if len(raw) != 15 {
		return nil, fmt.Errorf("密钥长度错误（期望 15 字节, 实际 %d 字节）", len(raw))
	}
	return raw, nil
}

// buildKey 拼装 fs://IP:PORT#CODE
func buildKey(ip string, port int, code string) string {
	return "fs://" + net.JoinHostPort(ip, strconv.Itoa(port)) + "#" + code
}

// parseKey 解析混合密钥，返回 host:port 与 PSK
func parseKey(s string) (string, []byte, error) {
	t := strings.TrimSpace(s)
	if len(t) < 5 || !strings.EqualFold(t[:5], "fs://") {
		return "", nil, errors.New("缺少 fs:// 前缀")
	}
	rest := t[5:]
	i := strings.LastIndex(rest, "#")
	if i < 0 {
		return "", nil, errors.New("缺少 # 后的密钥码")
	}
	hostPort := strings.TrimSpace(rest[:i])
	code := strings.TrimSpace(rest[i+1:])
	host, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		return "", nil, fmt.Errorf("地址格式错误: %v", err)
	}
	if host == "" {
		return "", nil, errors.New("地址为空")
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return "", nil, fmt.Errorf("端口非法: %s", port)
	}
	psk, err := decodeCode(code)
	if err != nil {
		return "", nil, err
	}
	return net.JoinHostPort(host, port), psk, nil
}

// ---------- IP 候选（网卡枚举 + 出口探测 + 手动覆盖） ----------

func localIPv4s() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			v4 := ip.To4()
			if v4 == nil {
				continue
			}
			if v4[0] == 169 && v4[1] == 254 { // 链路本地
				continue
			}
			if v4[0] == 127 { // 回环
				continue
			}
			out = append(out, v4.String())
		}
	}
	return out
}

// egressIP 通过 UDP 拨号（不实际发包）取得 NAT/公网出口 IP
func egressIP() string {
	for _, probe := range []string{"223.5.5.5:53", "119.29.29.29:53", "8.8.8.8:53"} {
		conn, err := net.DialTimeout("udp", probe, 2*time.Second)
		if err != nil {
			continue
		}
		addr := conn.LocalAddr()
		conn.Close()
		ua, ok := addr.(*net.UDPAddr)
		if !ok || ua.IP == nil {
			continue
		}
		v4 := ua.IP.To4()
		if v4 == nil || v4.IsUnspecified() || v4.IsLoopback() {
			continue
		}
		return v4.String()
	}
	return ""
}

// mergeCandidates 纯函数：出口 IP 优先，去重保序
func mergeCandidates(egress string, ifaceIPs []string) []string {
	seen := make(map[string]bool, len(ifaceIPs)+1)
	out := make([]string, 0, len(ifaceIPs)+1)
	add := func(ip string) {
		ip = strings.TrimSpace(ip)
		if ip == "" || seen[ip] {
			return
		}
		seen[ip] = true
		out = append(out, ip)
	}
	add(egress)
	for _, ip := range ifaceIPs {
		add(ip)
	}
	return out
}

// candidateIPs 生成候选列表；override 非空时只返回它
func candidateIPs(override string) []string {
	if s := strings.TrimSpace(override); s != "" {
		return []string{s}
	}
	return mergeCandidates(egressIP(), localIPv4s())
}

// ---------- 进度条 ----------

type progressBar struct {
	mu        sync.Mutex
	total     int64
	done      int64
	start     time.Time
	tty       bool
	step      int
	lastWidth int
}

func newProgressBar(total int64) *progressBar {
	return &progressBar{total: total, start: time.Now(), tty: isTTY(os.Stderr), step: -1}
}

func isTTY(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

func (p *progressBar) add(n int64) {
	if n <= 0 {
		return
	}
	p.mu.Lock()
	p.done += n
	p.renderLocked(false)
	p.mu.Unlock()
}

func (p *progressBar) ratio() float64 {
	if p.total <= 0 {
		return 1
	}
	r := float64(p.done) / float64(p.total)
	if r > 1 {
		r = 1
	}
	if r < 0 {
		r = 0
	}
	return r
}

func (p *progressBar) renderLocked(force bool) {
	r := p.ratio()
	pct := r * 100
	filled := int(r*20 + 0.5)
	if filled > 20 {
		filled = 20
	}
	if filled < 0 {
		filled = 0
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", 20-filled)

	elapsed := time.Since(p.start).Seconds()
	rate := 0.0
	if elapsed > 0 {
		rate = float64(p.done) / elapsed
	}
	eta := "--"
	if rate > 0 {
		remain := float64(p.total-p.done) / rate
		if remain < 0 {
			remain = 0
		}
		eta = fmt.Sprintf("%ds", int(remain+0.5))
	}
	line := fmt.Sprintf("[ %s ] %.1f%%  %s/%s MiB  %.1f MiB/s  剩余 %s",
		bar, pct, mib1(p.done), mib1(p.total), rate/(1024*1024), eta)

	if p.tty {
		padding := ""
		if p.lastWidth > len(line) {
			padding = strings.Repeat(" ", p.lastWidth-len(line))
		}
		fmt.Fprintf(os.Stderr, "\r%s%s", line, padding)
		p.lastWidth = len(line)
		return
	}
	cur := int(pct / 10)
	if force || cur > p.step {
		p.step = cur
		fmt.Fprintln(os.Stderr, line)
	}
}

// finish 输出收尾行（非 tty 下若已打印过 100% 则不重复）
func (p *progressBar) finish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tty {
		p.renderLocked(true)
		fmt.Fprint(os.Stderr, "\n")
		return
	}
	if p.step < 10 {
		p.renderLocked(true)
	}
}

// ---------- 统计 ----------

type xferStats struct {
	total int64
	files int
	start time.Time
}

func newStats(total int64, files int) *xferStats {
	return &xferStats{total: total, files: files, start: time.Now()}
}

func (s *xferStats) line() string {
	el := time.Since(s.start).Seconds()
	rate := "--"
	if el > 0 {
		rate = humanSize(int64(float64(s.total)/el)) + "/s"
	}
	return fmt.Sprintf("%s, %d 个文件, 耗时 %.1fs, 均速 %s", humanSize(s.total), s.files, el, rate)
}

// ---------- 交互输入 ----------

// readLine 打印提示（无前缀，以 ": " 结尾）并读取一行
func readLine(sc *bufio.Scanner, prompt string) (string, error) {
	fmt.Fprint(os.Stdout, prompt)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return "", err
		}
		return "", io.EOF
	}
	return strings.TrimSpace(sc.Text()), nil
}

// defaultStoreDir 接收端默认存储目录
func defaultStoreDir() string { return filepath.Join(".", "fsend-received") }

// applyTCPOptions 统一 TCP 选项：禁用 Nagle + KeepAlive 15s
func applyTCPOptions(conn net.Conn) {
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(15 * time.Second)
	}
}

// explicitlySet 返回命令行中显式出现的 flag 名（统一加 - 前缀，用于反向模式的参数互斥校验）
func explicitlySet(fs *flag.FlagSet, names ...string) []string {
	set := make(map[string]bool, len(names))
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	var out []string
	for _, n := range names {
		if set[n] {
			out = append(out, "-"+n)
		}
	}
	return out
}
