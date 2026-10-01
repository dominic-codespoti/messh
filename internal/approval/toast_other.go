//go:build !windows

package approval

// UnregisterNotifications has nothing to remove off Windows: notifications
// there go through the desktop's notification daemon and register nothing.
func UnregisterNotifications() (string, error) {
	return "Nothing to remove: messh registers nothing on this platform.", nil
}
