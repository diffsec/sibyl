//go:build !linux

package diag

// setLabel is a no-op off Linux. sibyl-diag runs on Linux only.
func setLabel(string, string) error { return nil }
