// Buffer helper — turns a uintptr buffer address (returned by the
// PageAllocator and stored in the virtqueue's per-descriptor
// bookkeeping) back into a Go byte slice. Lives in its own file so the
// `unsafe` import is contained.

package console

import "unsafe"

// readBufferBytes returns a Go byte view of `length` bytes starting at
// host-virtual address `addr`. The address is whatever the
// PageAllocator stored in the virtqueue's per-descriptor bookkeeping
// — on identity-mapped UEFI hosts this is the same as the physical
// address; on hosts with a separate kernel-virtual mapping the
// allocator's implementation has translated already.
//
// The returned slice aliases the underlying memory — DO NOT retain it
// past the receiver re-posting / freeing the descriptor. The
// Read path copies bytes out before re-posting so callers see
// a stable slice.
func readBufferBytes(addr uintptr, length int) []byte {
	if addr == 0 || length <= 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(addr)), length)
}
