package approval

// Default picks the platform's approval surface: a toast, then the native
// dialog if toasts cannot be used, and headless resolution (messh approvals)
// as the last resort.
func Default() Surface { return Chain(&Toast{}, Native{}, Headless{}) }
