// uintptrFromSlice is a tiny helper used by console_test.go to convert a
// Go []byte slice's first-byte address to a uintptr. Lives in its own
// _test.go file so the `unsafe` use is contained to the test build.

package console

import "unsafe"

func uintptrFromSlice(b []byte) uintptr {
	if len(b) == 0 {
		return 0
	}
	return uintptr(unsafe.Pointer(&b[0]))
}
