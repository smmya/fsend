package main

// These regression checks were written for the security review. No WorkBuddy
// test files, fixtures, logs, or selftest script are included in this repository.
import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRejectUnsafeNames(t *testing.T) {
	for _, name := range []string{"", ".", "..", "../x", "a/../x", "/x", `\x`, `C:\x`, `x:ads`, "x\x1b[2J", "x\n", "NUL.txt", "a/COM1", "COM¹", "LPT².txt", "CON .txt", strings.Repeat("a/", 129) + "x"} {
		if _, err := safeRelPath(name); err == nil {
			t.Errorf("accepted unsafe name %q", name)
		}
	}
	for _, name := range []string{"hello world.txt", "目录/文件.txt", "a/b.txt", "x.", "x ", "dir. /file. "} {
		if _, err := safeRelPath(name); err != nil {
			t.Errorf("rejected %q: %v", name, err)
		}
	}
}

func TestExclusiveCreationAndSymlinkConfinement(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(original, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	names := make(chan string, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := openReceiveStore(dir)
			if err != nil {
				t.Error(err)
				return
			}
			defer s.close()
			s.committed = true
			f, n, err := s.create("file.txt")
			if err != nil {
				t.Error(err)
				return
			}
			_ = f.Close()
			names <- n
		}()
	}
	wg.Wait()
	close(names)
	seen := map[string]bool{}
	for n := range names {
		if seen[n] {
			t.Errorf("duplicate reservation: %s", n)
		}
		seen[n] = true
	}
	if len(seen) != 16 {
		t.Fatalf("got %d names", len(seen))
	}
	data, err := os.ReadFile(original)
	if err != nil || string(data) != "original" {
		t.Fatal("existing file overwritten", err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Logf("symlink creation unavailable: %v", err)
		return
	}
	s, err := openReceiveStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if f, _, err := s.create("escape/payload"); err == nil {
		f.Close()
		t.Fatal("escaped through existing symlink")
	}
	if _, err := os.Stat(filepath.Join(outside, "payload")); !os.IsNotExist(err) {
		t.Fatal("outside file created")
	}
}

func TestTarRejectsLinksAndRollsBack(t *testing.T) {
	dir := t.TempDir()
	s, err := openReceiveStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	f, n, err := s.create("partial")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := extractTarEntry(tar.NewReader(bytes.NewReader(nil)), &tar.Header{Name: "escape", Typeflag: tar.TypeSymlink, Linkname: "../../outside"}, s); err == nil {
		t.Fatal("accepted symlink")
	}
	s.close()
	if _, err := os.Stat(filepath.Join(dir, n)); !os.IsNotExist(err) {
		t.Fatal("failed output was not removed")
	}
}

func pair(t *testing.T, reverse bool, wrongPSK bool) (*SecureSession, *SecureSession) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	a.SetDeadline(time.Now().Add(5 * time.Second))
	b.SetDeadline(time.Now().Add(5 * time.Second))
	key := bytes.Repeat([]byte{7}, 15)
	other := append([]byte(nil), key...)
	if wrongPSK {
		other[0]++
	}
	type result struct {
		s   *SecureSession
		err error
	}
	ch := make(chan result, 1)
	go func() {
		s, err := handshake(b, other, sessionRole{tcpClient: reverse, sender: false})
		if err != nil {
			b.Close()
		}
		ch <- result{s, err}
	}()
	sender, err := handshake(a, key, sessionRole{tcpClient: !reverse, sender: true})
	if err != nil {
		a.Close()
	}
	receiver := <-ch
	if wrongPSK {
		if err == nil || receiver.err == nil {
			t.Fatal("wrong PSK authenticated")
		}
		return nil, nil
	}
	if err != nil || receiver.err != nil {
		t.Fatalf("handshake: %v / %v", err, receiver.err)
	}
	a.SetDeadline(time.Now().Add(10 * time.Second))
	b.SetDeadline(time.Now().Add(10 * time.Second))
	return sender, receiver.s
}

func TestHandshakeRejectsWrongSecret(t *testing.T) { pair(t, false, true); pair(t, true, true) }

func TestTransfersBothDirectionsAndModes(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		for _, mode := range []string{"tar", "serial"} {
			t.Run(fmt.Sprintf("%v/%s", reverse, mode), func(t *testing.T) {
				src := t.TempDir()
				dst := t.TempDir()
				if err := os.MkdirAll(filepath.Join(src, "empty"), 0o700); err != nil {
					t.Fatal(err)
				}
				content := bytes.Repeat([]byte("abc123"), 400000)
				if err := os.WriteFile(filepath.Join(src, "large.bin"), content, 0o640); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(src, "zero"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if runtime.GOOS != "windows" {
					if err := os.Mkdir(filepath.Join(src, "trailing. "), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(src, "trailing. ", "file. "), content, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				entry, err := scanEntry(src)
				if err != nil {
					t.Fatal(err)
				}
				sender, receiver := pair(t, reverse, false)
				done := make(chan error, 1)
				go func() {
					var err error
					if mode == "tar" {
						var ok bool
						ok, err = receiveTarStream(receiver, dst, newProgressBar(entry.Bytes))
						if err == nil && !ok {
							err = fmt.Errorf("tar checksum failed")
						}
					} else {
						_, _, err = receiveSerialStream(receiver, dst, newProgressBar(entry.Bytes))
					}
					done <- err
				}()
				var sum string
				if mode == "tar" {
					sum, err = sendTarStream(sender, []*queueEntry{entry}, newProgressBar(entry.Bytes))
				} else {
					var files []fileItem
					files, err = flattenEntries([]*queueEntry{entry})
					if err == nil {
						var failures int
						sum, failures, err = sendSerialStream(sender, files, newProgressBar(entry.Bytes))
						if failures != 0 {
							t.Errorf("%d failures", failures)
						}
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				if err = sender.WriteJSON(doneMsg{T: "done", SHA256: sum}); err != nil {
					t.Fatal(err)
				}
				raw, err := sender.ReadCtrl()
				if err != nil {
					t.Fatal(err)
				}
				var ack doneAckMsg
				if err = json.Unmarshal(raw, &ack); err != nil || !ack.OK || ack.SHA256 != sum {
					t.Fatal("bad done ack", err)
				}
				if err = <-done; err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(filepath.Join(dst, entry.Name, "large.bin"))
				if err != nil || !bytes.Equal(got, content) {
					t.Fatal("content mismatch", err)
				}
				if st, err := os.Stat(filepath.Join(dst, entry.Name, "empty")); err != nil || !st.IsDir() {
					t.Fatal("empty directory lost", err)
				}
				if st, err := os.Stat(filepath.Join(dst, entry.Name, "zero")); err != nil || st.Size() != 0 {
					t.Fatal("empty file lost", err)
				}
				if runtime.GOOS != "windows" {
					got, err := os.ReadFile(filepath.Join(dst, entry.Name, "trailing. ", "file. "))
					if err != nil || !bytes.Equal(got, content) {
						t.Fatal("trailing dot/space filename was not preserved", err)
					}
				}
			})
		}
	}
}

func TestSerialRejectsHashAndOversize(t *testing.T) {
	for _, oversize := range []bool{false, true} {
		t.Run(fmt.Sprint(oversize), func(t *testing.T) {
			sender, receiver := pair(t, false, false)
			dir := t.TempDir()
			store, err := openReceiveStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			store.committed = true
			defer store.close()
			done := make(chan error, 1)
			go func() {
				_, ok, err := recvOneFile(receiver, store, fileMetaMsg{Name: "bad", Size: 3, Mode: 0o600}, nil)
				if ok {
					err = fmt.Errorf("bad file accepted")
				}
				receiver.conn.Close()
				done <- err
			}()
			payload := []byte("bad")
			if oversize {
				payload = []byte("toolong")
			}
			_ = sender.WriteFrame(frameData, payload)
			if !oversize {
				_ = sender.WriteFrame(frameStreamEnd, nil)
				_ = sender.WriteJSON(fileHashMsg{T: "filehash", SHA256: strings.Repeat("0", 64)})
				raw, err := sender.ReadCtrl()
				if err != nil {
					t.Fatal(err)
				}
				var ack fileAckMsg
				json.Unmarshal(raw, &ack)
				if ack.OK {
					t.Fatal("bad hash accepted")
				}
			}
			err = <-done
			if oversize && err == nil {
				t.Fatal("oversize data accepted")
			}
			if _, err := os.Stat(filepath.Join(dir, "bad")); !os.IsNotExist(err) {
				t.Fatal("bad output remains")
			}
		})
	}
}

func TestTarBadChecksumRemovesCreatedFiles(t *testing.T) {
	sender, receiver := pair(t, false, false)
	dir := t.TempDir()
	done := make(chan error, 1)
	go func() {
		ok, err := receiveTarStream(receiver, dir, nil)
		if ok {
			err = fmt.Errorf("bad archive accepted")
		}
		done <- err
	}()
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	tw.WriteHeader(&tar.Header{Name: "file", Mode: 0o600, Size: 3})
	tw.Write([]byte("abc"))
	tw.Close()
	if err := sender.WriteFrame(frameData, archive.Bytes()); err != nil {
		t.Fatal(err)
	}
	sender.WriteFrame(frameStreamEnd, nil)
	sender.WriteJSON(doneMsg{T: "done", SHA256: strings.Repeat("0", 64)})
	raw, err := sender.ReadCtrl()
	if err != nil {
		t.Fatal(err)
	}
	var ack doneAckMsg
	json.Unmarshal(raw, &ack)
	if ack.OK {
		t.Fatal("checksum accepted")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "file")); !os.IsNotExist(err) {
		t.Fatal("archive output remains")
	}
}

func TestQuotaAndFrameAuthentication(t *testing.T) {
	s, err := openReceiveStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	s.bytesLeft = 3
	s.entriesLeft = 2
	if err = s.consume(3); err != nil {
		t.Fatal(err)
	}
	if s.consume(1) == nil {
		t.Fatal("byte quota ignored")
	}
	key := sha256.Sum256([]byte("frame key"))
	enc, _ := newDirSession(key[:], "C2S")
	dec, _ := newDirSession(key[:], "C2S")
	encrypted := enc.seal([]byte("payload"))
	plain, err := dec.open(encrypted)
	if err != nil || string(plain) != "payload" {
		t.Fatal("frame decrypt failed")
	}
	if _, err = dec.open(encrypted); err == nil {
		t.Fatal("replay accepted")
	}
	dec, _ = newDirSession(key[:], "C2S")
	encrypted[0] ^= 1
	if _, err = dec.open(encrypted); err == nil {
		t.Fatal("tampering accepted")
	}
	prk := hkdfExtract([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}, bytes.Repeat([]byte{11}, 22))
	okm := hkdfExpand(prk, []byte{240, 241, 242, 243, 244, 245, 246, 247, 248, 249}, 42)
	if hex.EncodeToString(okm) != "3cb25f25faacd57a90434f64d0362f2a2d2d0a90cf1a5a4c5db02d56ecc4c5bf34007208d5b887185865" {
		t.Fatal("HKDF RFC5869 vector mismatch")
	}
}

type shortWriter struct{ bytes.Buffer }

func (w *shortWriter) Write(p []byte) (int, error) {
	if len(p) > 3 {
		p = p[:3]
	}
	return w.Buffer.Write(p)
}

type stalledWriter struct{}

func (stalledWriter) Write([]byte) (int, error) { return 0, nil }
func TestWriteAll(t *testing.T) {
	w := &shortWriter{}
	if err := writeAll(w, []byte("abcdefgh")); err != nil || w.String() != "abcdefgh" {
		t.Fatal("short write lost bytes", err)
	}
	if writeAll(stalledWriter{}, []byte("a")) != io.ErrShortWrite {
		t.Fatal("zero write not rejected")
	}
}
