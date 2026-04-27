//go:build darwin

package memory

func init() {
	// Darwin has no working huge page support for user code.
	HugepageSize = 0
}
