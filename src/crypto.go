package main

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
)

// ---------- HKDF-SHA256（RFC 5869，自实现以避开 x/crypto 依赖） ----------

func hkdfExtract(salt, ikm []byte) []byte {
	if len(salt) == 0 {
		salt = make([]byte, sha256.Size)
	}
	mac := hmac.New(sha256.New, salt)
	mac.Write(ikm)
	return mac.Sum(nil)
}

func hkdfExpand(prk, info []byte, length int) []byte {
	var out, t []byte
	for i := byte(1); len(out) < length; i++ {
		mac := hmac.New(sha256.New, prk)
		mac.Write(t)
		mac.Write(info)
		mac.Write([]byte{i})
		t = mac.Sum(nil)
		out = append(out, t...)
	}
	return out[:length]
}

// ---------- 单向会话（每个方向独立 AEAD + 计数器） ----------

type dirSession struct {
	aead  cipher.AEAD
	label [4]byte
	ctr   uint64
}

func newDirSession(key []byte, label string) (*dirSession, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("初始化 AES 失败: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("初始化 GCM 失败: %w", err)
	}
	var l [4]byte
	copy(l[:], label)
	return &dirSession{aead: aead, label: l}, nil
}

// nonce: [4B 方向标签][8B 大端计数器]
func (d *dirSession) nonce() []byte {
	n := make([]byte, 12)
	copy(n, d.label[:])
	binary.BigEndian.PutUint64(n[4:], d.ctr)
	d.ctr++
	return n
}

func (d *dirSession) seal(plain []byte) []byte {
	return d.aead.Seal(nil, d.nonce(), plain, nil)
}

func (d *dirSession) open(ct []byte) ([]byte, error) {
	return d.aead.Open(nil, d.nonce(), ct, nil)
}

// ---------- 加密会话 ----------

type SecureSession struct {
	transfer bool
	conn     net.Conn
	br       *bufio.Reader
	enc      *dirSession // 本端加密方向
	dec      *dirSession // 本端解密方向
}

// newSecureSession 由已派生的双向密钥构造会话（客户端：写 c2s / 读 s2c）
func newSecureSession(conn net.Conn, kc2s, ks2c []byte, isClient bool) (*SecureSession, error) {
	var encKey, decKey, encLabel, decLabel []byte
	if isClient {
		encKey, encLabel = kc2s, []byte("C2S\x00")
		decKey, decLabel = ks2c, []byte("S2C\x00")
	} else {
		encKey, encLabel = ks2c, []byte("S2C\x00")
		decKey, decLabel = kc2s, []byte("C2S\x00")
	}
	enc, err := newDirSession(encKey, string(encLabel))
	if err != nil {
		return nil, err
	}
	dec, err := newDirSession(decKey, string(decLabel))
	if err != nil {
		return nil, err
	}
	return &SecureSession{
		conn: conn,
		br:   bufio.NewReaderSize(conn, 64*1024),
		enc:  enc,
		dec:  dec,
	}, nil
}

// ---------- 握手 ----------

const (
	hsLen     = 51
	hsMagic0  = 0x46 // 'F'
	hsMagic1  = 0x53 // 'S'
	hsVersion = 0x01
)

// sessionRole 把"网络方向"与"数据角色"解耦：
// 正向模式（默认）接收端监听、发送端拨号；反向模式（-R）发送端监听、接收端拨号。
// 数据的组织方式（offer / data / done）始终由发送方发起，与谁监听无关。
type sessionRole struct {
	tcpClient bool // 本端是否为 TCP 发起方（决定握手包与认证帧的先后顺序）
	sender    bool // 本端是否为数据发送方（决定认证帧 role 字段）
}

// handshake 执行 51 字节明文握手 + 密钥派生 + 双向认证帧
func handshake(conn net.Conn, psk []byte, role sessionRole) (*SecureSession, error) {
	if len(psk) != 15 {
		return nil, errors.New("PSK 长度错误")
	}
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("生成 X25519 密钥失败: %w", err)
	}
	mine := make([]byte, hsLen)
	mine[0], mine[1], mine[2] = hsMagic0, hsMagic1, hsVersion
	copy(mine[3:35], priv.PublicKey().Bytes())
	if _, err := rand.Read(mine[35:51]); err != nil {
		return nil, fmt.Errorf("生成随机数失败: %w", err)
	}

	peer := make([]byte, hsLen)
	if role.tcpClient {
		if err := writeAll(conn, mine); err != nil {
			return nil, fmt.Errorf("发送握手包失败: %w", err)
		}
		if _, err := io.ReadFull(conn, peer); err != nil {
			return nil, fmt.Errorf("读取握手包失败: %w", err)
		}
	} else {
		if _, err := io.ReadFull(conn, peer); err != nil {
			return nil, fmt.Errorf("读取握手包失败: %w", err)
		}
		if err := writeAll(conn, mine); err != nil {
			return nil, fmt.Errorf("发送握手包失败: %w", err)
		}
	}

	if peer[0] != hsMagic0 || peer[1] != hsMagic1 {
		return nil, errors.New("握手失败: 魔数不匹配")
	}
	if peer[2] != hsVersion {
		return nil, fmt.Errorf("握手失败: 不支持的协议版本 %d", peer[2])
	}

	peerPub, err := ecdh.X25519().NewPublicKey(peer[3:35])
	if err != nil {
		return nil, fmt.Errorf("握手失败: 对端公钥非法: %w", err)
	}
	shared, err := priv.ECDH(peerPub)
	if err != nil {
		return nil, fmt.Errorf("握手失败: 密钥协商失败: %w", err)
	}

	// nc 为 TCP 发起方随机数，ns 为 TCP 接受方随机数（与数据角色无关）
	var nc, ns []byte
	if role.tcpClient {
		nc, ns = mine[35:51], peer[35:51]
	} else {
		nc, ns = peer[35:51], mine[35:51]
	}

	ikm := make([]byte, 0, len(shared)+len(psk))
	ikm = append(ikm, shared...)
	ikm = append(ikm, psk...)
	salt := make([]byte, 0, 32)
	salt = append(salt, nc...)
	salt = append(salt, ns...)

	prk := hkdfExtract(salt, ikm)
	kc2s := hkdfExpand(prk, []byte("fastsend/c2s"), 32)
	ks2c := hkdfExpand(prk, []byte("fastsend/s2c"), 32)

	sess, err := newSecureSession(conn, kc2s, ks2c, role.tcpClient)
	if err != nil {
		return nil, err
	}

	// 认证帧：TCP 发起方先写后读，接受方先读后写
	myRole, peerRole := "receiver", "sender"
	if role.sender {
		myRole, peerRole = "sender", "receiver"
	}
	writeAuth := func() error {
		raw, err := json.Marshal(authMsg{Role: myRole, V: 1})
		if err != nil {
			return err
		}
		return sess.WriteFrame(frameCtrl, raw)
	}
	readAuth := func() error {
		raw, err := sess.ReadCtrl()
		if err != nil {
			return err
		}
		var am authMsg
		if err := json.Unmarshal(raw, &am); err != nil {
			return fmt.Errorf("认证帧解析失败: %w", err)
		}
		if am.Role != peerRole {
			return fmt.Errorf("对端角色异常: %q", am.Role)
		}
		if am.V != 1 {
			return fmt.Errorf("对端协议版本异常: %d", am.V)
		}
		return nil
	}

	if role.tcpClient {
		if err := writeAuth(); err != nil {
			return nil, err
		}
		if err := readAuth(); err != nil {
			return nil, err
		}
	} else {
		if err := readAuth(); err != nil {
			return nil, err
		}
		if err := writeAuth(); err != nil {
			return nil, err
		}
	}
	sess.transfer = true
	return sess, nil
}
