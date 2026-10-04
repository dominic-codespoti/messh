//go:build windows

package recipes

import "os"

// Windows stores access control in the file's ACL, not in POSIX permission
// bits. State files inherit their access policy from the state directory, so
// FileMode.Perm cannot be used to validate their Windows access boundary.
func checkRegistryPermissions(os.FileInfo) error { return nil }
