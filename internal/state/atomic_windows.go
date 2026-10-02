//go:build windows

package state

import "golang.org/x/sys/windows"

func replaceFile(tmp, path string) error {
	from, err := windows.UTF16PtrFromString(tmp)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

// MoveFileEx with WRITE_THROUGH flushes the replacement on Windows. Windows
// does not provide a portable directory-handle flush through os.File.Sync.
func syncDirectory(string) error { return nil }
