package main

import "testing"

func TestTokenCommand(t *testing.T) {
	for _, c := range []struct {
		bearer bool
		state  string
		want   string
	}{
		{false, "", "messh agent token omp"},
		{true, "", "messh agent token omp --bearer"},
		{true, `C:\Users\Example\AppData\Local\Temp\x\B`, "messh agent token omp --bearer --state C:/Users/Example/AppData/Local/Temp/x/B"},
		{false, "/srv/messh", "messh agent token omp --state /srv/messh"},
		{false, `C:\Users\Example\messh state`, "messh agent token omp --state 'C:/Users/Example/messh state'"},
		{false, `/home/test'user/messh`, `messh agent token omp --state '/home/test'\''user/messh'`},
	} {
		if got := tokenCommand("omp", c.bearer, c.state); got != c.want {
			t.Errorf("tokenCommand(%v, %q) = %q, want %q", c.bearer, c.state, got, c.want)
		}
	}
}
