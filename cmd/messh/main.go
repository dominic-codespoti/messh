// Command messh runs a messh node and manages pairing, agents, and tools.
//
// Commands are declared with add (see spec.go) by the file that implements
// them; usage text, -h and `messh describe` are generated from those
// declarations.
package main

import "os"

func main() {
	tty := false
	if st, err := os.Stdin.Stat(); err == nil {
		tty = st.Mode()&os.ModeCharDevice != 0
	}
	os.Exit(run(os.Args[1:], os.Stdin, tty, os.Stdout, os.Stderr))
}
