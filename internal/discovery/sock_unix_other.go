//go:build unix && !linux

package discovery

func setMulticastAllOff(int) error { return nil }
