//go:build !windows

package recipes

import (
	"fmt"
	"os"
)

func checkRegistryPermissions(st os.FileInfo) error {
	if st.Mode().Perm() != 0o600 {
		return fmt.Errorf("recipe registry permissions must be 0600, got %04o", st.Mode().Perm())
	}
	return nil
}
