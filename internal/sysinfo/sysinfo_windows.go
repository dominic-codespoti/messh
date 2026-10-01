//go:build windows

package sysinfo

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

var (
	modkernel32 = syscall.NewLazyDLL("kernel32.dll")
	moduser32   = syscall.NewLazyDLL("user32.dll")
	modwtsapi32 = syscall.NewLazyDLL("wtsapi32.dll")

	procGlobalMemoryStatusEx        = modkernel32.NewProc("GlobalMemoryStatusEx")
	procGetTickCount64              = modkernel32.NewProc("GetTickCount64")
	procGetLastInputInfo            = moduser32.NewProc("GetLastInputInfo")
	procWTSQuerySessionInformationW = modwtsapi32.NewProc("WTSQuerySessionInformationW")
	procWTSFreeMemory               = modwtsapi32.NewProc("WTSFreeMemory")
)

const createNoWindow = 0x08000000

func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

// --- minimal registry helpers over syscall ---

type regKey syscall.Handle

func openKey(root syscall.Handle, path string) (regKey, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var h syscall.Handle
	if err := syscall.RegOpenKeyEx(root, p, 0, syscall.KEY_READ, &h); err != nil {
		return 0, err
	}
	return regKey(h), nil
}

func (k regKey) close() { syscall.RegCloseKey(syscall.Handle(k)) }

// value returns the raw data and type of a value ("" = default value).
func (k regKey) value(name string) ([]byte, uint32, error) {
	var np *uint16
	if name != "" {
		var err error
		if np, err = syscall.UTF16PtrFromString(name); err != nil {
			return nil, 0, err
		}
	} else {
		np = &[]uint16{0}[0]
	}
	var typ, n uint32
	if err := syscall.RegQueryValueEx(syscall.Handle(k), np, nil, &typ, nil, &n); err != nil {
		return nil, 0, err
	}
	if n == 0 {
		return nil, typ, nil
	}
	buf := make([]byte, n)
	if err := syscall.RegQueryValueEx(syscall.Handle(k), np, nil, &typ, &buf[0], &n); err != nil {
		return nil, 0, err
	}
	return buf[:n], typ, nil
}

func (k regKey) str(name string) string {
	b, typ, err := k.value(name)
	if err != nil || (typ != syscall.REG_SZ && typ != syscall.REG_EXPAND_SZ) || len(b) < 2 {
		return ""
	}
	u := unsafe.Slice((*uint16)(unsafe.Pointer(&b[0])), len(b)/2)
	return strings.TrimSpace(syscall.UTF16ToString(u))
}

// uint64Val reads REG_DWORD, REG_QWORD, or REG_BINARY (little-endian) values.
func (k regKey) uint64Val(name string) (uint64, bool) {
	b, typ, err := k.value(name)
	if err != nil {
		return 0, false
	}
	switch {
	case typ == syscall.REG_DWORD && len(b) >= 4:
		return uint64(binary.LittleEndian.Uint32(b)), true
	case len(b) >= 8 && (typ == 11 /* REG_QWORD */ || typ == syscall.REG_BINARY):
		return binary.LittleEndian.Uint64(b), true
	case len(b) >= 4 && typ == syscall.REG_BINARY:
		return uint64(binary.LittleEndian.Uint32(b)), true
	}
	return 0, false
}

func (k regKey) subkeys() []string {
	var out []string
	for i := uint32(0); ; i++ {
		buf := make([]uint16, 256)
		n := uint32(len(buf))
		if err := syscall.RegEnumKeyEx(syscall.Handle(k), i, &buf[0], &n, nil, nil, nil, nil); err != nil {
			return out
		}
		out = append(out, syscall.UTF16ToString(buf[:n]))
	}
}

func regString(root syscall.Handle, path, name string) string {
	k, err := openKey(root, path)
	if err != nil {
		return ""
	}
	defer k.close()
	return k.str(name)
}

// --- platform ---

func collectPlatform(_ context.Context, es *errSink) platformInfo {
	var p platformInfo
	hklm := syscall.Handle(syscall.HKEY_LOCAL_MACHINE)

	if k, err := openKey(hklm, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`); err == nil {
		name, disp, build := k.str("ProductName"), k.str("DisplayVersion"), k.str("CurrentBuild")
		k.close()
		if b, err := strconv.Atoi(build); err == nil && b >= 22000 && strings.HasPrefix(name, "Windows 10") {
			name = "Windows 11" + strings.TrimPrefix(name, "Windows 10")
		}
		v := name
		if disp != "" {
			v += " " + disp
		}
		if build != "" {
			v += " (build " + build + ")"
		}
		p.osVersion = strings.TrimSpace(v)
	} else {
		es.add("os_version", err)
	}

	if k, err := openKey(hklm, `HARDWARE\DESCRIPTION\System\BIOS`); err == nil {
		var parts []string
		for _, v := range []string{k.str("SystemManufacturer"), k.str("SystemProductName")} {
			if v != "" && !isPlaceholder(v) {
				parts = append(parts, v)
			}
		}
		k.close()
		p.machine = strings.Join(parts, " ")
	}

	p.cpuModel = regString(hklm, `HARDWARE\DESCRIPTION\System\CentralProcessor\0`, "ProcessorNameString")

	if m, err := memoryStatus(); err == nil {
		p.memory = m
	} else {
		es.add("memory", err)
	}

	p.gpus = registryGPUs()
	return p
}

func isPlaceholder(s string) bool {
	l := strings.ToLower(s)
	for _, ph := range []string{"system manufacturer", "system product name", "to be filled by o.e.m.", "default string", "not applicable", "o.e.m."} {
		if l == ph {
			return true
		}
	}
	return false
}

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func memoryStatus() (Memory, error) {
	var ms memoryStatusEx
	ms.Length = uint32(unsafe.Sizeof(ms))
	r, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms)))
	if r == 0 {
		return Memory{}, err
	}
	return Memory{TotalBytes: ms.TotalPhys, AvailableBytes: ms.AvailPhys}, nil
}

const displayClass = `SYSTEM\CurrentControlSet\Control\Class\{4d36e968-e325-11ce-bfc1-08002be10318}`

func registryGPUs() []GPU {
	k, err := openKey(syscall.Handle(syscall.HKEY_LOCAL_MACHINE), displayClass)
	if err != nil {
		return nil
	}
	defer k.close()
	var gpus []GPU
	seen := map[string]bool{}
	for _, sub := range k.subkeys() {
		if len(sub) != 4 || strings.Trim(sub, "0123456789") != "" {
			continue
		}
		sk, err := openKey(syscall.Handle(k), sub)
		if err != nil {
			continue
		}
		name := sk.str("DriverDesc")
		l := strings.ToLower(name)
		if name == "" || strings.Contains(l, "basic display") || strings.Contains(l, "basic render") ||
			strings.Contains(l, "remote") || strings.Contains(l, "virtual display") || seen[name] {
			sk.close()
			continue
		}
		seen[name] = true
		mem, ok := sk.uint64Val("HardwareInformation.qwMemorySize")
		if !ok {
			mem, _ = sk.uint64Val("HardwareInformation.MemorySize")
		}
		driver := sk.str("DriverVersion")
		sk.close()
		gpus = append(gpus, GPU{Name: name, Vendor: vendorOf(name), MemoryTotalMiB: int(mem >> 20), Driver: driver})
	}
	return gpus
}

func vendorOf(name string) string {
	l := strings.ToLower(name)
	switch {
	case strings.Contains(l, "nvidia"), strings.Contains(l, "geforce"), strings.Contains(l, "quadro"):
		return "nvidia"
	case strings.Contains(l, "amd"), strings.Contains(l, "radeon"):
		return "amd"
	case strings.Contains(l, "intel"):
		return "intel"
	}
	return "unknown"
}

// --- browsers ---

func probeBrowsers(_ context.Context, _ *errSink) []Browser {
	var out []Browser
	seen := map[string]bool{}
	for _, root := range []syscall.Handle{syscall.HKEY_LOCAL_MACHINE, syscall.HKEY_CURRENT_USER} {
		k, err := openKey(root, `SOFTWARE\Clients\StartMenuInternet`)
		if err != nil {
			continue
		}
		for _, sub := range k.subkeys() {
			if strings.EqualFold(sub, "IEXPLORE.EXE") {
				continue
			}
			sk, err := openKey(syscall.Handle(k), sub)
			if err != nil {
				continue
			}
			name := sk.str("")
			sk.close()
			cmd := regString(syscall.Handle(k), sub+`\shell\open\command`, "")
			exe := exeFromCommand(cmd)
			if name == "" || exe == "" {
				continue
			}
			name = canonicalBrowser(name)
			if seen[name] {
				continue
			}
			seen[name] = true
			b := Browser{Name: name, Engine: "chromium", Path: exe}
			if strings.Contains(strings.ToLower(name), "firefox") {
				b.Engine = "gecko"
			}
			switch name {
			case "Google Chrome":
				b.Version = regString(syscall.HKEY_CURRENT_USER, `Software\Google\Chrome\BLBeacon`, "version")
			case "Microsoft Edge":
				b.Version = regString(syscall.HKEY_CURRENT_USER, `Software\Microsoft\Edge\BLBeacon`, "version")
			}
			out = append(out, b)
		}
		k.close()
	}
	return out
}

func canonicalBrowser(name string) string {
	l := strings.ToLower(name)
	switch {
	case strings.Contains(l, "google chrome"):
		return "Google Chrome"
	case strings.Contains(l, "microsoft edge"):
		return "Microsoft Edge"
	case strings.Contains(l, "brave"):
		return "Brave"
	case strings.Contains(l, "firefox"):
		return "Firefox"
	case strings.Contains(l, "chromium"):
		return "Chromium"
	}
	return name
}

func exeFromCommand(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if strings.HasPrefix(cmd, `"`) {
		if end := strings.Index(cmd[1:], `"`); end >= 0 {
			return filepath.Clean(cmd[1 : end+1])
		}
		return ""
	}
	if i := strings.Index(strings.ToLower(cmd), ".exe"); i >= 0 {
		return filepath.Clean(cmd[:i+4])
	}
	if f := strings.Fields(cmd); len(f) > 0 {
		return f[0]
	}
	return ""
}

// --- presence ---

type lastInputInfo struct {
	Size uint32
	Time uint32
}

const (
	wtsCurrentSession = ^uint32(0) // (DWORD)-1
	wtsSessionInfoEx  = 25
)

func probePresence(_ context.Context, es *errSink) Presence {
	p := Presence{Source: "win32"}
	lii := lastInputInfo{Size: uint32(unsafe.Sizeof(lastInputInfo{}))}
	if r, _, err := procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&lii))); r != 0 {
		now, _, _ := procGetTickCount64.Call()
		// dwTime is a 32-bit tick count; compare modulo 2^32.
		idleMs := uint32(uint64(now)) - lii.Time
		s := int64(idleMs / 1000)
		p.IdleSeconds = &s
	} else {
		es.add("presence idle", err)
	}
	if locked, err := sessionLocked(); err != nil {
		es.add("presence locked", err)
	} else {
		p.Locked = locked
	}
	return p
}

func sessionLocked() (*bool, error) {
	if err := procWTSQuerySessionInformationW.Find(); err != nil {
		return nil, err
	}
	var buf *byte
	var n uint32
	r, _, err := procWTSQuerySessionInformationW.Call(0, uintptr(wtsCurrentSession), wtsSessionInfoEx,
		uintptr(unsafe.Pointer(&buf)), uintptr(unsafe.Pointer(&n)))
	if r == 0 {
		return nil, err
	}
	defer procWTSFreeMemory.Call(uintptr(unsafe.Pointer(buf)))
	// WTSINFOEXW: DWORD Level; then union (8-byte aligned on amd64 due to LARGE_INTEGERs in LEVEL1).
	// WTSINFOEX_LEVEL1_W: ULONG SessionId; WTS_CONNECTSTATE_CLASS SessionState; LONG SessionFlags.
	const off = 8
	if n < off+12 {
		return nil, fmt.Errorf("WTSINFOEXW too small (%d bytes)", n)
	}
	if lvl := *(*uint32)(unsafe.Pointer(buf)); lvl != 1 {
		return nil, errors.New("unexpected WTSINFOEX level " + strconv.Itoa(int(lvl)))
	}
	flags := *(*int32)(unsafe.Add(unsafe.Pointer(buf), off+8))
	var v bool
	switch flags {
	case 0:
		v = true
	case 1:
		v = false
	default:
		return nil, nil
	}
	return &v, nil
}
