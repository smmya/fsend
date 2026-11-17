package main

import (
	"errors"
	"os"
	"testing"
)

func TestTerminalLocalePrecedence(t *testing.T) {
	cases := []struct {
		all, ctype, lang string
		ascii            bool
	}{
		{"C", "en_US.UTF-8", "en_US.UTF-8", true},
		{"POSIX", "", "en_US.UTF-8", true},
		{"", "C", "en_US.UTF-8", true},
		{"", "", "C", true},
		{"", "", "en_US.US-ASCII", true},
		{"en_US.ANSI_X3.4-1968", "", "", true},
		{"C.UTF-8", "C", "C", false},
		{"", "UTF-8", "C", false},
		{"", "", "en_US.UTF8", false},
		{"zh_CN.GBK", "C", "C", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		values := map[string]string{"LC_ALL": tc.all, "LC_CTYPE": tc.ctype, "LANG": tc.lang}
		if got := asciiTerminalLocale(func(key string) string { return values[key] }); got != tc.ascii {
			t.Errorf("%v: got %v", values, got)
		}
	}
}

func TestBrowserLocaleInitializationRestoresEnvironment(t *testing.T) {
	for _, initial := range []string{"C", "POSIX", ""} {
		for _, fail := range []bool{false, true} {
			t.Run(initial+map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
				t.Setenv("LC_ALL", initial)
				t.Setenv("LC_CTYPE", "C")
				t.Setenv("LANG", "en_US.UTF-8")
				if initial == "" {
					if err := os.Unsetenv("LC_ALL"); err != nil {
						t.Fatal(err)
					}
				}
				before, existed := os.LookupEnv("LC_ALL")
				sentinel := errors.New("init failure")
				err := initializeBrowserEncoding(func() error {
					if os.Getenv("LC_ALL") != "C.UTF-8" {
						t.Error("UTF-8 not selected during initialization")
					}
					if fail {
						return sentinel
					}
					return nil
				}, "linux")
				if fail && !errors.Is(err, sentinel) {
					t.Fatal("init error lost", err)
				}
				after, exists := os.LookupEnv("LC_ALL")
				if after != before || exists != existed || os.Getenv("LC_CTYPE") != "C" || os.Getenv("LANG") != "en_US.UTF-8" {
					t.Fatal("environment not restored")
				}
			})
		}
	}
	for _, platform := range []string{"linux", "windows"} {
		t.Setenv("LC_ALL", "C.UTF-8")
		t.Setenv("LC_CTYPE", "C")
		if err := initializeBrowserEncoding(func() error {
			if os.Getenv("LC_ALL") != "C.UTF-8" {
				t.Fatal("valid locale changed")
			}
			return nil
		}, platform); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("LC_ALL", "C")
	if err := initializeBrowserEncoding(func() error {
		if os.Getenv("LC_ALL") != "C" {
			t.Fatal("Windows console locale changed")
		}
		return nil
	}, "windows"); err != nil {
		t.Fatal(err)
	}
}
