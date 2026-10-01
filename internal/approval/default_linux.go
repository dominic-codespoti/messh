package approval

// Default picks the usable surfaces in order: a desktop notification with
// buttons when notify-send and a notification daemon that supports actions
// exist, zenity when a display is present, a prompt on the node's terminal
// when stdin is one, and always headless last so requests can still be
// resolved with `messh approvals`.
func Default() Surface {
	var s []Surface
	// A notification daemon often starts after the node (at login), and
	// Notify.Available re-probes it at most every 30 s, so the surface joins
	// the chain whenever a desktop session exists and the chain skips it
	// while the daemon is missing.
	if desktopSession() {
		s = append(s, &Notify{})
	}
	if z := (&Zenity{}); z.Available() {
		s = append(s, z)
	}
	if t := (&Terminal{}); t.Available() {
		s = append(s, t)
	}
	if len(s) == 0 {
		return Headless{}
	}
	return Chain(append(s, Headless{})...)
}
