# go-virtio/console

Pure-Go virtio-console driver targeting the `go-virtio/common` transport
interfaces. Implements the modern-transport (Virtio 1.0+) init sequence
and the raw byte-stream RX / TX path for the standard PCI-bound
virtio-console device (VID 0x1AF4, DID 0x1043).

This package targets the single-port baseline (Virtio 1.1 §5.3): it
negotiates only `VIRTIO_F_VERSION_1`, so `VIRTIO_CONSOLE_F_MULTIPORT` is
not acknowledged and the device exposes exactly two virtqueues — a
receiveq (port 0 input) and a transmitq (port 0 output). There are no
control queues and no per-message header: the console is a raw
bidirectional byte stream, which makes the data path simpler than
virtio-net (no `virtio_net_hdr` to prepend or strip). `VIRTIO_CONSOLE_F_SIZE`
is also masked out, so the driver performs no device-config reads.

The driver pre-posts one-page device-writable buffers on the receiveq at
bring-up so the device has somewhere to land guest input, and posts
device-readable buffers on demand in `Write`. Every transport-level
operation is routed through `go-virtio/common`'s `Transport` interface,
so any implementation of that interface (UEFI's `EFI_PCI_IO_PROTOCOL`,
bare-metal MMIO, virtio-mmio adapter) drives the same driver code.

## Quick start

```go
import (
    virtioconsole "github.com/go-virtio/console"
)

// transport is any value that implements go-virtio/common.Transport.
vc, err := virtioconsole.OpenVirtioConsole(transport)
if err != nil {
    return err
}

// Write sends raw bytes to the console output, chunked one page at a time.
if _, err := vc.Write([]byte("hello from the guest\n")); err != nil {
    return err
}

// Read polls the receiveq for one buffer of console input. The argument
// is a busy-poll budget; ErrReceiveTimeout is returned if it is exhausted.
in, err := vc.Read(10000)
```

`OpenVirtioConsole` leaves the device in DRIVER_OK state with the
receiveq pre-posted with one-page buffers and the transmitq empty +
ready.

## Sibling packages

  - [`github.com/go-virtio/common`](https://github.com/go-virtio/common)
    — transport-agnostic infrastructure (PCI cap walker, modern config
    layout, split-virtqueue impl, transport interfaces).
  - [`github.com/go-virtio/net`](https://github.com/go-virtio/net) —
    pure-Go virtio-net driver (the reference per-device-class driver this
    package mirrors).
  - [`github.com/go-virtio/rng`](https://github.com/go-virtio/rng) —
    pure-Go virtio-rng driver.
  - [`github.com/go-virtio/vsock`](https://github.com/go-virtio/vsock) —
    pure-Go virtio-vsock driver.
  - [`github.com/go-virtio/blk`](https://github.com/go-virtio/blk) —
    placeholder for a future pure-Go virtio-blk driver.

## License

BSD-3-Clause. See [LICENSE](LICENSE).
