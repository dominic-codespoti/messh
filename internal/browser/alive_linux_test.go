//go:build linux

package browser

import (
	"os"
	"strconv"
	"strings"
)

// processAlive reports whether a live (not zombie) process has this PID.
func processAlive(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i >= 0 && len(s) > i+2 && s[i+2] != 'Z'
}
