package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"time"
)

// 帧类型
const (
	frameCtrl      byte = 0x01
	frameData      byte = 0x02
	frameStreamEnd byte = 0x03
)

const (
	maxPlainLen = 1 << 20 // 1 MiB
	minFrameLen = 17      // 1B type + 16B GCM tag
	maxFrameLen = maxPlainLen + 17
)

// ---------- 控制消息（字段名逐字对应工作书 §5.4） ----------

type authMsg struct {
	Role string `json:"role"`
	V    int    `json:"v"`
}

type offerEntry struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Bytes int64  `json:"bytes"`
	Files int    `json:"files,omitempty"`
}

type offerMsg struct {
	T          string       `json:"t"`
	Mode       string       `json:"mode"`
	TotalBytes int64        `json:"totalBytes"`
	TotalFiles int          `json:"totalFiles"`
	Entries    []offerEntry `json:"entries"`
}

type acceptMsg struct {
	T         string `json:"t"`
	Dir       string `json:"dir"`
	Overwrite string `json:"overwrite"`
}

type fileMetaMsg struct {
	T     string `json:"t"`
	Kind  string `json:"kind,omitempty"`
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	Mode  int64  `json:"mode"`
	Mtime int64  `json:"mtime"`
}

type fileHashMsg struct {
	T      string `json:"t"`
	SHA256 string `json:"sha256"`
}

type fileAckMsg struct {
	T      string `json:"t"`
	OK     bool   `json:"ok"`
	SHA256 string `json:"sha256"`
	Err    string `json:"err,omitempty"`
}

type doneMsg struct {
	T      string `json:"t"`
	SHA256 string `json:"sha256"`
}

type doneAckMsg struct {
	T      string `json:"t"`
	OK     bool   `json:"ok"`
	SHA256 string `json:"sha256"`
	Err    string `json:"err,omitempty"`
}

type errorMsg struct {
	T   string `json:"t"`
	Msg string `json:"msg"`
}

// ---------- 帧读写 ----------

// WriteFrame 写入一个加密帧：明文 = [1B type][payload]
func (s *SecureSession) WriteFrame(typ byte, payload []byte) error {
	if len(payload) > maxPlainLen {
		return fmt.Errorf("协议错误: 单帧明文超限 (%d > %d)", len(payload), maxPlainLen)
	}
	plain := make([]byte, 1+len(payload))
	plain[0] = typ
	copy(plain[1:], payload)
	if s.enc.ctr == ^uint64(0) {
		return fmt.Errorf("会话帧数超限")
	}
	if s.transfer {
		_ = s.conn.SetWriteDeadline(time.Now().Add(10 * time.Minute))
	}
	ct := s.enc.seal(plain)
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(ct)))
	if err := writeAll(s.conn, hdr[:]); err != nil {
		return err
	}
	if err := writeAll(s.conn, ct); err != nil {
		return err
	}
	return nil
}

// ReadFrame 读取并解密一个帧，返回类型与 payload
func (s *SecureSession) ReadFrame() (byte, []byte, error) {
	if s.dec.ctr == ^uint64(0) {
		return 0, nil, fmt.Errorf("会话帧数超限")
	}
	if s.transfer {
		_ = s.conn.SetReadDeadline(time.Now().Add(10 * time.Minute))
	}
	var hdr [4]byte
	if _, err := io.ReadFull(s.br, hdr[:]); err != nil {
		return 0, nil, err
	}
	n32 := binary.BigEndian.Uint32(hdr[:])
	if n32 < minFrameLen || n32 > maxFrameLen {
		return 0, nil, fmt.Errorf("协议错误: 帧长度越界 (%d)", n32)
	}
	ct := make([]byte, int(n32))
	if _, err := io.ReadFull(s.br, ct); err != nil {
		return 0, nil, err
	}
	plain, err := s.dec.open(ct)
	if err != nil {
		return 0, nil, fmt.Errorf("解密失败: %w", err)
	}
	if len(plain) < 1 {
		return 0, nil, fmt.Errorf("协议错误: 空帧")
	}
	return plain[0], plain[1:], nil
}

// WriteJSON 以 ctrl 帧发送 JSON 控制消息
func (s *SecureSession) WriteJSON(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("编码控制消息失败: %w", err)
	}
	return s.WriteFrame(frameCtrl, raw)
}

// ReadCtrl 读取一个控制帧，返回 JSON 原文
func (s *SecureSession) ReadCtrl() ([]byte, error) {
	typ, payload, err := s.ReadFrame()
	if err != nil {
		return nil, err
	}
	if typ != frameCtrl {
		return nil, fmt.Errorf("协议错误: 期望控制帧, 收到帧类型 0x%02x", typ)
	}
	return payload, nil
}

// ctrlType 提取控制消息的 t 字段
func ctrlType(raw []byte) string {
	var x struct {
		T string `json:"t"`
	}
	if err := json.Unmarshal(raw, &x); err != nil {
		return ""
	}
	return x.T
}

// ---------- 数据流（把连续的 data 帧拼成 io.Reader，止于 stream-end） ----------

type dataStream struct {
	sess *SecureSession
	cur  []byte
	end  bool
	h    hash.Hash
	bar  *progressBar
}

func newDataStream(sess *SecureSession, bar *progressBar) *dataStream {
	return &dataStream{sess: sess, h: sha256.New(), bar: bar}
}

func (d *dataStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(d.cur) == 0 {
		if d.end {
			return 0, io.EOF
		}
		typ, payload, err := d.sess.ReadFrame()
		if err != nil {
			return 0, err
		}
		switch typ {
		case frameData:
			d.cur = payload
			d.h.Write(payload)
			if d.bar != nil {
				d.bar.add(int64(len(payload)))
			}
		case frameStreamEnd:
			if len(payload) != 0 {
				return 0, fmt.Errorf("非空 stream-end 帧")
			}
			d.end = true
			return 0, io.EOF
		default:
			return 0, fmt.Errorf("协议错误: 数据流中收到帧类型 0x%02x", typ)
		}
	}
	n := copy(p, d.cur)
	d.cur = d.cur[n:]
	return n, nil
}

// sum 返回本次流内经解密明文的 sha256 hex
func (d *dataStream) sum() string {
	return hex.EncodeToString(d.h.Sum(nil))
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(data) {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
