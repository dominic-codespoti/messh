//go:build linux

package jobs

import (
	"os"
	"strconv"
	"strings"
)

// pidAlive treats zombies as dead: nobody reaps a reparented grandchild until init gets to it.
func pidAlive(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i >= 0 && i+2 < len(s) && s[i+2] != 'Z'
}

func tokenFor(pid int) string { return procToken(pid) }
