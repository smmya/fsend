package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestTrailingFilenameSpelling(t *testing.T) {
	cases := map[string]string{
		"dir. /file. ": "dir%2E%20/file%2E%20",
		"file%2E":      "file%252E",
		"file.":        "file%2E",
		"100%.txt":     "100%25.txt",
		"a.. .":        "a%2E%2E%20%2E",
	}
	for name, want := range cases {
		for _, platform := range []string{"linux", "darwin", "windows"} {
			got, err := receiveRelPath(name, platform)
			expected := name
			if platform == "windows" {
				expected = want
			}
			if err != nil || filepath.ToSlash(got) != expected {
				t.Errorf("%s %q: %q, %v", platform, name, got, err)
			}
		}
	}
}

func TestTrailingNamesTransferInBothModes(t *testing.T) {
	for _, mode := range []string{"tar", "serial"} {
		t.Run(mode, func(t *testing.T) {
			sender, receiver := pair(t, false, false)
			dst, source := t.TempDir(), t.TempDir()
			names := []string{"folder. /data.", "folder. /data%2E", "folder. /data "}
			payloads := []string{"first", "second", "third"}
			done := make(chan error, 1)
			go func() {
				if mode == "tar" {
					ok, err := receiveTarStream(receiver, dst, nil)
					if err == nil && !ok {
						err = errors.New("tar checksum mismatch")
					}
					done <- err
				} else {
					_, failures, err := receiveSerialStream(receiver, dst, nil)
					if err == nil && failures != 0 {
						err = errors.New("serial failures")
					}
					done <- err
				}
			}()
			var sum string
			if mode == "tar" {
				var archive bytes.Buffer
				tw := tar.NewWriter(&archive)
				if err := tw.WriteHeader(&tar.Header{Name: "folder. /empty ", Typeflag: tar.TypeDir, Mode: 0o700}); err != nil {
					t.Fatal(err)
				}
				for i, name := range names {
					if err := tw.WriteHeader(&tar.Header{Name: name, Size: int64(len(payloads[i])), Mode: 0o600}); err != nil {
						t.Fatal(err)
					}
					if _, err := tw.Write([]byte(payloads[i])); err != nil {
						t.Fatal(err)
					}
				}
				if err := tw.Close(); err != nil {
					t.Fatal(err)
				}
				hash := sha256.Sum256(archive.Bytes())
				sum = hex.EncodeToString(hash[:])
				if err := sender.WriteFrame(frameData, archive.Bytes()); err != nil {
					t.Fatal(err)
				}
				if err := sender.WriteFrame(frameStreamEnd, nil); err != nil {
					t.Fatal(err)
				}
			} else {
				files := []fileItem{{Name: "folder. /empty ", IsDir: true}}
				for i, name := range names {
					p := filepath.Join(source, string(rune('a'+i)))
					if err := os.WriteFile(p, []byte(payloads[i]), 0o600); err != nil {
						t.Fatal(err)
					}
					files = append(files, fileItem{Name: name, Path: p, Size: int64(len(payloads[i])), Mode: 0o600})
				}
				var failures int
				var err error
				sum, failures, err = sendSerialStream(sender, files, newProgressBar(16))
				if err != nil || failures != 0 {
					t.Fatal("serial send", failures, err)
				}
			}
			if err := sender.WriteJSON(doneMsg{T: "done", SHA256: sum}); err != nil {
				t.Fatal(err)
			}
			raw, err := sender.ReadCtrl()
			if err != nil {
				t.Fatal(err)
			}
			var ack doneAckMsg
			if err = json.Unmarshal(raw, &ack); err != nil || !ack.OK || ack.SHA256 != sum {
				t.Fatal("invalid ack", err)
			}
			if err = <-done; err != nil {
				t.Fatal(err)
			}
			for i, name := range names {
				rel, err := receiveRelPath(name, runtime.GOOS)
				if err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(filepath.Join(dst, rel))
				if err != nil || string(data) != payloads[i] {
					t.Fatalf("%q: %q, %v", name, data, err)
				}
			}
			rel, _ := receiveRelPath("folder. /empty ", runtime.GOOS)
			if st, err := os.Stat(filepath.Join(dst, rel)); err != nil || !st.IsDir() {
				t.Fatal("empty directory lost", err)
			}
		})
	}
}

func TestWizardDirectionsAndBufferedInput(t *testing.T) {
	cases := []struct {
		input string
		want  []string
	}{
		{"1\n\nNEXT\n", []string{"send", "--mode", "tar"}},
		{"1\n2\n127.0.0.1\nNEXT\n", []string{"send", "--mode", "tar", "-R", "--ip", "127.0.0.1", "--wait", "600"}},
		{"2\n\n\"saved dir\"\n127.0.0.1\nNEXT\n", []string{"receive", "-o", "saved dir", "--ip", "127.0.0.1", "--wait", "600"}},
		{"2\n1\n\nNEXT\n", []string{"receive", "-R"}},
	}
	for _, tc := range cases {
		sc := newInputScanner(strings.NewReader(tc.input))
		args, err := interactiveArgs(sc)
		if err != nil || !reflect.DeepEqual(args, tc.want) {
			t.Fatalf("%q: %v, %v", tc.input, args, err)
		}
		next, err := readPathLine(sc, "")
		if err != nil || next != "NEXT" {
			t.Fatal("wizard consumed or lost buffered input", next, err)
		}
	}
	if code := runWithInput(nil, newInputScanner(strings.NewReader("0\n"))); code != 0 {
		t.Fatal("exit failed", code)
	}
	if _, err := interactiveArgs(newInputScanner(strings.NewReader(""))); err != io.EOF {
		t.Fatal("EOF not returned", err)
	}
}

func TestPathInputPreservesSpacesAndAcceptsQuotes(t *testing.T) {
	for input, want := range map[string]string{"file. \n": "file. ", "\"a b\"\n": "a b", "'a b'\n": "a b"} {
		got, err := readPathLine(newInputScanner(strings.NewReader(input)), "")
		if err != nil || got != want {
			t.Fatalf("%q: %q, %v", input, got, err)
		}
	}
}

func TestSenderPreflightsNestedNames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture is not a Windows filename")
	}
	dir := t.TempDir()
	nested := filepath.Join(dir, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "bad:name"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := scanEntry(dir); err == nil {
		t.Fatal("incompatible nested filename was not caught during preflight")
	}
}

func TestProgressClearsShorterLine(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = original; w.Close(); r.Close() }()
	p := newProgressBar(1 << 30)
	p.tty = true
	p.start = time.Now().Add(-10000 * time.Hour)
	p.done = 1
	p.renderLocked(false)
	p.start = time.Now().Add(-time.Second)
	p.done = p.total
	p.renderLocked(false)
	w.Close()
	os.Stderr = original
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\r")
	if len(lines) != 3 || len(lines[2]) != len(lines[1]) || !strings.HasSuffix(lines[2], "   ") {
		t.Fatalf("shortened progress line did not overwrite previous tail: %q", data)
	}
}
