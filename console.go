// go-virtio/console — driver core: feature negotiation + init sequence +
// raw byte-stream RX / TX path for the modern virtio-console device
// (Virtio 1.1 §5.3).
//
// This driver targets the single-port baseline: it negotiates only
// VIRTIO_F_VERSION_1, so VIRTIO_CONSOLE_F_MULTIPORT is NOT acknowledged
// and the device exposes exactly two virtqueues — a receiveq (port 0
// input) and a transmitq (port 0 output). There are no control queues
// and no per-message header: the console is a raw bidirectional byte
// stream, which makes the data path simpler than virtio-net (no
// virtio_net_hdr to prepend or strip).
//
// The driver pre-posts one-page device-writable buffers on the receiveq
// at bring-up so the device has somewhere to land guest input, and posts
// device-readable buffers on demand in Write.
package console

import (
	"github.com/go-virtio/common"
)

// ReceiveQueueIdx / TransmitQueueIdx are the two virtqueue indices for
// the single-port baseline (Virtio 1.1 §5.3.2: port 0 receiveq = 0,
// transmitq = 1; the control queues only exist when F_MULTIPORT is
// negotiated, which this driver does not do).
const (
	ReceiveQueueIdx  uint16 = 0
	TransmitQueueIdx uint16 = 1
)

// ReceiveQueueSize / TransmitQueueSize are the desired ring sizes for
// the two queues. Clamped down to the device's advertised maximum (and
// rounded to a power of two) during setup.
const (
	ReceiveQueueSize  uint16 = 16
	TransmitQueueSize uint16 = 16
)

// TxPollIterations is the default busy-poll budget Write spends waiting
// for the device to return a transmitted buffer. The console round-trip
// is sub-millisecond on every backend; this is a generous upper bound
// for the busy-poll model the driver uses.
const TxPollIterations = 200000

// AcceptedFeatures is the feature mask the driver negotiates ON. For the
// single-port baseline the only bit we ever accept is the non-negotiable
// VIRTIO_F_VERSION_1 (modern transport). VIRTIO_CONSOLE_F_SIZE (0) and
// VIRTIO_CONSOLE_F_MULTIPORT (1) are deliberately masked OUT — the
// former would require device-config reads we skip, the latter would add
// control queues we do not drive.
const AcceptedFeatures uint64 = common.FeatureVersion1

// AcceptFeatures returns the negotiated feature mask: the intersection
// of what the device offers and what we accept. The caller writes this
// back via DriverFeature.
//
// We require VIRTIO_F_VERSION_1 — if the device doesn't offer it, the
// device is legacy-only and we return ErrNotModernDevice.
func AcceptFeatures(deviceFeatures uint64) (uint64, error) {
	if deviceFeatures&common.FeatureVersion1 == 0 {
		return 0, ErrNotModernDevice
	}
	return deviceFeatures & AcceptedFeatures, nil
}

// VirtioConsole wraps one initialised virtio-console device. The caller
// holds this for the lifetime of the console; the underlying virtqueue
// pages live as long as the supplied PageAllocator's lifetime contract.
type VirtioConsole struct {
	// Cfg is the modern-transport handle (BARs + offsets + the
	// BARMemoryAccessor used for every register access).
	Cfg *common.ModernConfig

	// NegotiatedFeatures records what the driver-feature handshake
	// settled on. Exposed for diagnostic prints.
	NegotiatedFeatures uint64

	// transport is the underlying Transport — held so the data path can
	// route DMA-buffer allocations through the PageAllocator side.
	transport common.Transport

	// rxq / txq are the two virtqueues set up by OpenVirtioConsole.
	rxq *common.Virtqueue
	txq *common.Virtqueue
}

// OpenVirtioConsole drives the full bring-up of one virtio-console
// device:
//
//  1. Verify the PCI device ID is 0x1043 (modern console).
//  2. InitModernConfig walks PCI caps + populates the BAR locators.
//  3. Reset → ACK → DRIVER status progression.
//  4. Read DeviceFeature, require VERSION_1, mask, write DriverFeature.
//  5. Set FEATURES_OK, verify it stuck.
//  6. Allocate + publish receiveq (queue 0) + transmitq (queue 1).
//  7. DRIVER_OK status.
//  8. Pre-post receiveq buffers + notify the device.
//
// On success the device is in DRIVER_OK state, the receiveq is
// pre-posted with one-page buffers, and the transmitq is empty + ready.
// Unlike virtio-net there is no device-config region to read (F_SIZE is
// not negotiated) and no per-message header.
func OpenVirtioConsole(t common.Transport) (*VirtioConsole, error) {
	// Sanity-check this really is a modern virtio-console device.
	did, err := t.ReadConfig16(common.PCICfgDeviceID)
	if err != nil {
		return nil, err
	}
	if did != common.PCIDeviceIDModernConsole {
		return nil, ErrInitWrongDeviceID
	}

	cfg, err := common.InitModernConfig(t)
	if err != nil {
		return nil, err
	}

	// Step 1: full reset (write 0 to DeviceStatus).
	if err := cfg.SetDeviceStatus(0); err != nil {
		return nil, err
	}
	// Spec §3.1.1: after reset DeviceStatus reads back as 0. We don't
	// sleep — the read itself is a firmware-liveness check; its value is
	// discarded.
	if _, err := cfg.DeviceStatus(); err != nil {
		return nil, err
	}

	// Step 2: ACKNOWLEDGE.
	if err := cfg.SetDeviceStatus(common.StatusAcknowledge); err != nil {
		return nil, err
	}
	// Step 3: DRIVER.
	if err := cfg.SetDeviceStatus(common.StatusAcknowledge | common.StatusDriver); err != nil {
		return nil, err
	}

	// Step 4: read DeviceFeature, mask to our accepted set, write
	// DriverFeature.
	deviceFeats, err := cfg.DeviceFeatures64()
	if err != nil {
		return nil, err
	}
	if deviceFeats&common.FeatureVersion1 == 0 {
		return nil, ErrNotModernDevice
	}
	negotiated := deviceFeats & AcceptedFeatures
	if err := cfg.SetDriverFeatures64(negotiated); err != nil {
		return nil, err
	}

	// Step 5: FEATURES_OK + verify the device accepted our subset.
	if err := cfg.SetDeviceStatus(common.StatusAcknowledge | common.StatusDriver | common.StatusFeaturesOK); err != nil {
		return nil, err
	}
	status, err := cfg.DeviceStatus()
	if err != nil {
		return nil, err
	}
	if status&common.StatusFeaturesOK == 0 {
		return nil, ErrFeaturesNotOK
	}

	// Step 6: queue setup (receiveq then transmitq).
	rxq, err := setupQueue(cfg, t, ReceiveQueueIdx, ReceiveQueueSize)
	if err != nil {
		return nil, err
	}
	txq, err := setupQueue(cfg, t, TransmitQueueIdx, TransmitQueueSize)
	if err != nil {
		return nil, err
	}

	// Step 7: DRIVER_OK.
	if err := cfg.SetDeviceStatus(common.StatusAcknowledge | common.StatusDriver | common.StatusFeaturesOK | common.StatusDriverOK); err != nil {
		return nil, err
	}

	v := &VirtioConsole{
		Cfg:                cfg,
		NegotiatedFeatures: negotiated,
		transport:          t,
		rxq:                rxq,
		txq:                txq,
	}

	// Step 8: pre-post receive buffers so the device has somewhere to
	// land guest input.
	if err := v.fillRxRing(); err != nil {
		return nil, err
	}
	// Notify the device that the receiveq has buffers available.
	if err := cfg.NotifyQueue(ReceiveQueueIdx, rxq.NotifyOff); err != nil {
		return nil, err
	}

	return v, nil
}

// setupQueue performs the per-queue init: select, read max-size, write
// our size (= min(desired, max), rounded down to a power of two),
// allocate the Virtqueue, publish its descriptor/avail/used physical
// addresses, enable.
func setupQueue(cfg *common.ModernConfig, t common.Transport, queueIdx uint16, desiredSize uint16) (*common.Virtqueue, error) {
	if err := cfg.SelectQueue(queueIdx); err != nil {
		return nil, err
	}
	maxSize, err := cfg.QueueSize()
	if err != nil {
		return nil, err
	}
	if maxSize == 0 {
		// Device doesn't have this queue; spec says the driver must not
		// use it. (maxSize >= 1 from here on, so the size computed below
		// is always a non-zero power of two — no further zero-check
		// needed.)
		return nil, ErrQueueNotAvailable
	}
	size := desiredSize
	if size > maxSize {
		size = maxSize
	}
	// Round size DOWN to a power of two; some QEMU versions report
	// non-power-of-two QueueSize on legacy queues.
	for size&(size-1) != 0 {
		size &= size - 1
	}
	if err := cfg.SetQueueSize(size); err != nil {
		return nil, err
	}
	notifyOff, err := cfg.QueueNotifyOff()
	if err != nil {
		return nil, err
	}
	q, err := common.NewVirtqueue(t, size, queueIdx, notifyOff)
	if err != nil {
		return nil, err
	}
	descAddr := q.BasePhys + uint64(q.Layout.DescTableOffset)
	availAddr := q.BasePhys + uint64(q.Layout.AvailRingOffset)
	usedAddr := q.BasePhys + uint64(q.Layout.UsedRingOffset)
	if err := cfg.SetQueueDesc(descAddr); err != nil {
		return nil, err
	}
	if err := cfg.SetQueueDriver(availAddr); err != nil {
		return nil, err
	}
	if err := cfg.SetQueueDevice(usedAddr); err != nil {
		return nil, err
	}
	if err := cfg.SetQueueEnable(1); err != nil {
		return nil, err
	}
	return q, nil
}

// ReceiveQueue / TransmitQueue expose the per-direction
// *common.Virtqueue handles. Read-only accessors so callers can inspect
// ring state for diagnostic dumps; the fields themselves stay unexported.
func (v *VirtioConsole) ReceiveQueue() *common.Virtqueue { return v.rxq }

// TransmitQueue returns the transmit virtqueue handle.
func (v *VirtioConsole) TransmitQueue() *common.Virtqueue { return v.txq }

// fillRxRing posts one device-writable one-page buffer per receiveq slot
// so the device has somewhere to land guest input. The console has no
// per-message header, so the whole page is available for data.
func (v *VirtioConsole) fillRxRing() error {
	for i := uint16(0); i < v.rxq.Layout.Size; i++ {
		phys, mem, err := v.transport.AllocatePages(1)
		if err != nil {
			return err
		}
		if phys == 0 {
			return common.ErrAllocReturnedZero
		}
		bufLen := uint32(common.PageSize)
		if uint64(bufLen) > uint64(len(mem)) {
			return ErrBufferTooSmall
		}
		// writable=true ⇒ VIRTQ_DESC_F_WRITE set.
		addr := uintptr(phys) // identity-mapped on supported hosts
		if _, err := v.rxq.AddBuffer(addr, phys, bufLen, true); err != nil {
			return err
		}
	}
	return nil
}

// Write sends the raw bytes in p to the console output, chunked one page
// at a time. Each chunk is copied into a fresh device-readable DMA
// buffer, enqueued on the transmitq, notified, and busy-polled for
// completion up to TxPollIterations iterations. Returns the total number
// of bytes written, or ErrTransmitTimeout if the device stalls.
//
// Write(nil) / Write([]byte{}) is a no-op returning (0, nil).
func (v *VirtioConsole) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	pageSize := int(common.PageSize)
	total := 0
	for total < len(p) {
		chunk := len(p) - total
		if chunk > pageSize {
			chunk = pageSize
		}
		phys, mem, err := v.transport.AllocatePages(1)
		if err != nil {
			return total, err
		}
		if phys == 0 {
			return total, common.ErrAllocReturnedZero
		}
		if chunk > len(mem) {
			return total, ErrBufferTooSmall
		}
		copy(mem[:chunk], p[total:total+chunk])

		addr := uintptr(phys) // identity-mapped on supported hosts
		descIdx, err := v.txq.AddBuffer(addr, phys, uint32(chunk), false)
		if err != nil {
			return total, err
		}
		if err := v.Cfg.NotifyQueue(TransmitQueueIdx, v.txq.NotifyOff); err != nil {
			return total, err
		}
		done := false
		for spin := 0; spin < TxPollIterations; spin++ {
			gotIdx, _, ok := v.txq.PollUsed()
			if !ok {
				continue
			}
			_ = v.txq.Reclaim(gotIdx)
			done = true
			break
		}
		if !done {
			// Timed out with the descriptor still outstanding; free it so
			// a later Write can reuse the slot.
			_ = v.txq.Reclaim(descIdx)
			return total, ErrTransmitTimeout
		}
		total += chunk
	}
	return total, nil
}

// Read polls the receiveq for one buffer of console input, busy-polling
// up to pollIterations iterations. On success it copies the device's
// bytes out, reclaims + re-posts the same buffer (so the device has
// somewhere to land the next input), notifies the device, and returns
// the raw bytes. Returns ErrReceiveTimeout if no input arrives within
// the budget.
//
// The returned slice is a fresh copy of the descriptor's DMA buffer —
// safe to retain after this call returns (and after the descriptor is
// reclaimed and re-posted).
func (v *VirtioConsole) Read(pollIterations int) ([]byte, error) {
	for spin := 0; spin < pollIterations; spin++ {
		descIdx, length, ok := v.rxq.PollUsed()
		if !ok {
			continue
		}
		buf := v.rxq.Buffers[descIdx]
		raw := readBufferBytes(buf.Addr, int(length))
		out := make([]byte, len(raw))
		copy(out, raw)
		_ = v.rxq.Reclaim(descIdx)
		// Re-post the same buffer (it's still allocated) so the device
		// has somewhere to land the next input.
		if _, err := v.rxq.AddBuffer(buf.Addr, buf.Phys, buf.Len, true); err != nil {
			// Re-post failed; we're degraded but the captured bytes are
			// still good to return.
			_ = err
		}
		if err := v.Cfg.NotifyQueue(ReceiveQueueIdx, v.rxq.NotifyOff); err != nil {
			_ = err
		}
		return out, nil
	}
	return nil, ErrReceiveTimeout
}

// Sentinel errors for the virtio-console path. All exported so callers
// can branch + format them.
var (
	ErrNotModernDevice   = commonConsoleError("go-virtio/console: device doesn't offer VIRTIO_F_VERSION_1 (legacy-only)")
	ErrFeaturesNotOK     = commonConsoleError("go-virtio/console: FEATURES_OK status bit didn't stick after DriverFeature write")
	ErrInitWrongDeviceID = commonConsoleError("go-virtio/console: PCI device ID is not 0x1043 (modern console device)")
	ErrQueueNotAvailable = commonConsoleError("go-virtio/console: device reports QueueSize=0 for a required queue")
	ErrTransmitTimeout   = commonConsoleError("go-virtio/console: TX poll timeout (device did not return descriptor)")
	ErrReceiveTimeout    = commonConsoleError("go-virtio/console: RX poll timeout (no input received within budget)")
	ErrBufferTooSmall    = commonConsoleError("go-virtio/console: PageAllocator returned a chunk smaller than one page")
)

// commonConsoleError is the package's tiny sentinel-error type — same
// pattern as go-virtio/common.commonError and go-virtio/net.commonNetError.
type commonConsoleError string

// Error implements the `error` interface.
func (e commonConsoleError) Error() string { return string(e) }
