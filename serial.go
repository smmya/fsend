package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// ---------- 发送侧：串行逐文件 ----------

// sendSerialStream 逐文件发送：filemeta → data* → stream-end → filehash → 等 fileack
// 返回总校验（成功文件 sha256 hex 拼接后再 sha256）、失败数
func sendSerialStream(sess *SecureSession, files []fileItem, bar *progressBar) (string, int, error) {
	var hexList []string
	failed := 0

	for _, it := range files {
		kind := "file"
		if it.IsDir {
			kind = "dir"
		}
		meta := fileMetaMsg{Kind: kind, T: "filemeta", Name: it.Name, Size: it.Size, Mode: it.Mode, Mtime: it.Mtime}
		if err := sess.WriteJSON(meta); err != nil {
			return "", failed, err
		}

		h := sha256.New()
		if it.IsDir {
		} else if it.IsLink {
			data := []byte(it.Link)
			h.Write(data)
			if len(data) > 0 {
				if err := sess.WriteFrame(frameData, data); err != nil {
					return "", failed, err
				}
				bar.add(int64(len(data)))
			}
		} else {
			f, err := os.Open(it.Path)
			if err != nil {
				return "", failed, fmt.Errorf("打开文件失败 %s: %w", it.Path, err)
			}
			buf := make([]byte, maxPlainLen)
			for {
				n, rerr := f.Read(buf)
				if n > 0 {
					h.Write(buf[:n])
					if err := sess.WriteFrame(frameData, buf[:n]); err != nil {
						f.Close()
						return "", failed, err
					}
					bar.add(int64(n))
				}
				if rerr == io.EOF {
					break
				}
				if rerr != nil {
					f.Close()
					return "", failed, fmt.Errorf("读取文件失败 %s: %w", it.Path, rerr)
				}
			}
			f.Close()
		}

		if err := sess.WriteFrame(frameStreamEnd, nil); err != nil {
			return "", failed, err
		}
		sum := hex.EncodeToString(h.Sum(nil))
		if err := sess.WriteJSON(fileHashMsg{T: "filehash", SHA256: sum}); err != nil {
			return "", failed, err
		}

		raw, err := sess.ReadCtrl()
		if err != nil {
			return "", failed, err
		}
		var ack fileAckMsg
		if err := json.Unmarshal(raw, &ack); err != nil {
			return "", failed, fmt.Errorf("fileack 解析失败: %w", err)
		}
		if ack.T != "fileack" {
			return "", failed, fmt.Errorf("协议错误: 期望 fileack, 收到 %q", ack.T)
		}
		if !ack.OK {
			failed++
			reason := ack.Err
			if reason == "" {
				reason = "接收端校验失败"
			}
			errf("文件校验失败: %s（%s）", it.Name, reason)
			continue
		}
		if !strings.EqualFold(ack.SHA256, sum) {
			failed++
			errf("文件校验失败: %s（对端回执哈希不一致）", it.Name)
			continue
		}
		hexList = append(hexList, sum)
	}

	total := sha256.Sum256([]byte(strings.Join(hexList, "")))
	return hex.EncodeToString(total[:]), failed, nil
}

// ---------- 接收侧：串行逐文件 ----------

// receiveSerialStream 接收串行文件流直到 done；返回成功数、失败数
func receiveSerialStream(sess *SecureSession, storeDir string, bar *progressBar) (int, int, error) {
	store, err := openReceiveStore(storeDir)
	if err != nil {
		return 0, 0, err
	}
	// Successfully verified serial files remain if a later file fails.
	store.committed = true
	defer store.close()
	var hexList []string
	okCount, failed := 0, 0

	for {
		raw, err := sess.ReadCtrl()
		if err != nil {
			return okCount, failed, err
		}
		switch ctrlType(raw) {
		case "filemeta":
			var fm fileMetaMsg
			if err := json.Unmarshal(raw, &fm); err != nil {
				return okCount, failed, fmt.Errorf("filemeta 解析失败: %w", err)
			}
			sum, ok, err := recvOneFile(sess, store, fm, bar)
			if err != nil {
				return okCount, failed, err
			}
			if ok {
				okCount++
				hexList = append(hexList, sum)
			} else {
				failed++
			}
		case "done":
			var dm doneMsg
			if err := json.Unmarshal(raw, &dm); err != nil {
				return okCount, failed, fmt.Errorf("done 解析失败: %w", err)
			}
			total := sha256.Sum256([]byte(strings.Join(hexList, "")))
			local := hex.EncodeToString(total[:])
			ok := strings.EqualFold(dm.SHA256, local)
			ack := doneAckMsg{T: "doneack", OK: ok, SHA256: local}
			if !ok {
				ack.Err = "总校验不一致"
			}
			if err := sess.WriteJSON(ack); err != nil {
				return okCount, failed, err
			}
			if !ok {
				return okCount, failed, fmt.Errorf("总校验不一致")
			}
			return okCount, failed, nil
		case "error":
			var em errorMsg
			_ = json.Unmarshal(raw, &em)
			return okCount, failed, fmt.Errorf("对端错误: %s", em.Msg)
		default:
			return okCount, failed, fmt.Errorf("协议错误: 意外控制消息 %q", ctrlType(raw))
		}
	}
}

// recvOneFile 接收单个文件（filemeta 之后的数据流 + filehash + fileack）
// 返回本地 sha256 hex 与是否成功
func recvOneFile(sess *SecureSession, store *receiveStore, fm fileMetaMsg, bar *progressBar) (string, bool, error) {
	if fm.Kind != "" && fm.Kind != "file" && fm.Kind != "dir" {
		return "", false, fmt.Errorf("未知文件类型")
	}
	if fm.Kind == "dir" && fm.Size != 0 {
		return "", false, fmt.Errorf("目录大小必须为零")
	}
	if err := store.consume(fm.Size); err != nil {
		return "", false, err
	}
	var f *os.File
	var final string
	var prepErr error
	if fm.Kind == "dir" {
		prepErr = store.mkdir(fm.Name)
	} else {
		f, final, prepErr = store.create(fm.Name)
	}
	keep := false
	defer func() {
		if f != nil {
			_ = f.Close()
			if !keep {
				store.remove(final)
			}
		}
	}()
	ds := newDataStream(sess, bar)
	var dst io.Writer = io.Discard
	if f != nil {
		dst = f
	}
	// The extra byte detects dishonest sizes without writing an unbounded stream.
	n, err := io.Copy(dst, io.LimitReader(ds, fm.Size+1))
	if err != nil {
		return "", false, err
	}
	if n > fm.Size {
		return "", false, fmt.Errorf("文件数据超过声明大小")
	}
	raw, err := sess.ReadCtrl()
	if err != nil {
		return "", false, err
	}
	var fh fileHashMsg
	if err = json.Unmarshal(raw, &fh); err != nil || fh.T != "filehash" {
		return "", false, fmt.Errorf("filehash 消息无效")
	}
	local := ds.sum()
	ok := prepErr == nil && n == fm.Size && strings.EqualFold(fh.SHA256, local)
	reason := ""
	if prepErr != nil {
		reason = prepErr.Error()
	} else if !ok {
		reason = "字节数或 SHA-256 不一致"
	}
	if f != nil {
		if ok {
			if err = f.Chmod(os.FileMode(fm.Mode) & 0o777); err != nil {
				ok = false
				reason = err.Error()
			}
		}
		if err = f.Close(); err != nil {
			ok = false
			reason = err.Error()
		}
	}
	if err = sess.WriteJSON(fileAckMsg{T: "fileack", OK: ok, SHA256: local, Err: reason}); err != nil {
		return "", false, err
	}
	keep = ok
	if !ok {
		errf("文件校验失败: %q（%s）", fm.Name, reason)
	}
	return local, ok, nil
}
