package main

import "golang.org/x/sys/windows"

// showError tells the person something went wrong. The helper runs without a
// console (see approval.handlerCommand), so stderr would go nowhere.
func showError(title, text string) {
	t, err1 := windows.UTF16PtrFromString(title)
	m, err2 := windows.UTF16PtrFromString(text)
	if err1 != nil || err2 != nil {
		return
	}
	const flags = windows.MB_OK | windows.MB_ICONWARNING | windows.MB_SETFOREGROUND | windows.MB_TOPMOST
	windows.MessageBox(0, m, t, flags)
}
