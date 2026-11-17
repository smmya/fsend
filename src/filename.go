package main

import (
	"path/filepath"
	"strings"
)

func isWindowsDeviceName(part string) bool {
	base := strings.ToUpper(strings.TrimRight(strings.SplitN(part, ".", 2)[0], " "))
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT") {
		suffix := []rune(base[3:])
		return len(suffix) == 1 && strings.ContainsRune("123456789¹²³", suffix[0])
	}
	return false
}

// Validation and platform spelling are separate. POSIX receivers preserve
// trailing dots and spaces. Windows spells them explicitly instead of silently
// stripping them or merging distinct source directories.
func receiveRelPath(name, platform string) (string, error) {
	rel, err := safeRelPath(name)
	if err != nil {
		return "", err
	}
	if platform != "windows" {
		return rel, nil
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i, part := range parts {
		end := len(strings.TrimRight(part, ". "))
		var out strings.Builder
		for j := 0; j < len(part); j++ {
			switch {
			case part[j] == '%':
				out.WriteString("%25")
			case j >= end && part[j] == '.':
				out.WriteString("%2E")
			case j >= end && part[j] == ' ':
				out.WriteString("%20")
			default:
				out.WriteByte(part[j])
			}
		}
		parts[i] = out.String()
	}
	return filepath.FromSlash(strings.Join(parts, "/")), nil
}
