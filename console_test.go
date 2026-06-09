// End-to-end tests for the OpenVirtioConsole driver path + the raw
// byte-stream Write / Read paths. Uses a fakeConsoleDevice transport
// that:
//
//   - Publishes a valid virtio-console PCI config-space cap chain
//     (CommonCfg + extended NotifyCfg, no DeviceCfg — this driver does
//     not negotiate VIRTIO_CONSOLE_F_SIZE so it never reads device-config).
//   - Tracks COMMON_CFG register state: the device-status progression,
//     feature-select index, and the two queues' address publication.
//   - Simulates the device side of TX completion: on a doorbell write to
//     the transmitq it publishes the just-added descriptor in the used
//     ring (gated by txCompletes for the timeout test).
//   - Injects console input on demand via deliver (the device side of an
//     RX completion).
//
// injectTransport wraps the fake to force a transport-level error (or a
// zero physical address) on the Nth call to a chosen method, which lets
// the error-return branches of OpenVirtioConsole / setupQueue / Write be
// exercised deterministically.

package console

import (
	"encoding/binary"
	"errors"
	"sync"
	"testing"

	"github.com/go-virtio/common"
)

var le = binary.LittleEndian

// fakeConsoleDevice is a minimal in-memory virtio-console device for
// driver tests.
type fakeConsoleDevice struct {
	mu sync.Mutex

	// PCI config-space contents.
	cfg []byte

	// COMMON_CFG state.
	deviceFeatureSelect uint32
	deviceFeatures      uint64 // what the device offers
	driverFeatures      uint64 // what the driver acked
	deviceStatus        uint8
	currentQueue        uint16
	// Per-queue state. Key: queue idx.
	qsize      map[uint16]uint16
	qenable    map[uint16]uint16
	qdesc      map[uint16]uint64
	qdriver    map[uint16]uint64
	qdevice    map[uint16]uint64
	qnotifyOff map[uint16]uint16

	// BAR memory store (other reads/writes).
	bar map[uint64]uint64 // key = (bar<<48 | offset)

	// FEATURES_OK gate override: when true the device always clears the
	// FEATURES_OK bit, simulating a device that rejects our feature set.
	clearFeaturesOK bool

	// txCompletes: when true a transmitq doorbell publishes a used-ring
	// entry for the most-recently-added descriptor.
	txCompletes bool

	// rxConsumed is the device's running index into the rx avail ring
	// (the next rx buffer deliver will fill).
	rxConsumed uint16

	// heldPages pins references to allocated pages so the GC does not
	// reclaim them — the driver retains addresses via uintptr which the
	// GC doesn't trace.
	heldPages [][]byte
	allocFail bool
}

func newFakeConsoleDevice(deviceFeats uint64) *fakeConsoleDevice {
	d := &fakeConsoleDevice{
		deviceFeatures: deviceFeats,
		qsize:          map[uint16]uint16{0: 32, 1: 32},
		qenable:        map[uint16]uint16{},
		qdesc:          map[uint16]uint64{},
		qdriver:        map[uint16]uint64{},
		qdevice:        map[uint16]uint64{},
		qnotifyOff:     map[uint16]uint16{0: 0, 1: 1},
		bar:            map[uint64]uint64{},
		txCompletes:    true,
	}
	d.cfg = buildVirtioConsoleCfgSpace()
	return d
}

func barKey(bar uint8, off uint64) uint64 { return uint64(bar)<<48 | off }

// PCIConfigReader.
func (d *fakeConsoleDevice) ReadConfig8(off uint8) (uint8, error) {
	if int(off) >= len(d.cfg) {
		return 0, errors.New("read past cfg")
	}
	return d.cfg[off], nil
}
func (d *fakeConsoleDevice) ReadConfig16(off uint8) (uint16, error) {
	if int(off)+2 > len(d.cfg) {
		return 0, errors.New("read past cfg")
	}
	return le.Uint16(d.cfg[off : off+2]), nil
}
func (d *fakeConsoleDevice) ReadConfig32(off uint8) (uint32, error) {
	if int(off)+4 > len(d.cfg) {
		return 0, errors.New("read past cfg")
	}
	return le.Uint32(d.cfg[off : off+4]), nil
}

// PageAllocator.
func (d *fakeConsoleDevice) AllocatePages(count int) (uint64, []byte, error) {
	if d.allocFail {
		return 0, nil, errors.New("alloc fail")
	}
	mem := make([]byte, count*int(common.PageSize))
	addr := uintptr(0)
	if len(mem) > 0 {
		d.heldPages = append(d.heldPages, mem)
		addr = uintptrFromSlice(mem)
	}
	return uint64(addr), mem, nil
}

func (d *fakeConsoleDevice) commonCfgBAR() uint8     { return 0 }
func (d *fakeConsoleDevice) commonCfgOffset() uint64 { return 0 }

// BARMemoryAccessor.
func (d *fakeConsoleDevice) Read8(bar uint8, off uint64) (uint8, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgDeviceStatus:
			return d.deviceStatus, nil
		case common.CfgConfigGeneration:
			return 0, nil
		}
	}
	return uint8(d.bar[barKey(bar, off)] & 0xFF), nil
}

func (d *fakeConsoleDevice) Read16(bar uint8, off uint64) (uint16, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgNumQueues:
			return 2, nil
		case common.CfgQueueSelect:
			return d.currentQueue, nil
		case common.CfgQueueSize:
			return d.qsize[d.currentQueue], nil
		case common.CfgQueueEnable:
			return d.qenable[d.currentQueue], nil
		case common.CfgQueueNotifyOff:
			return d.qnotifyOff[d.currentQueue], nil
		}
	}
	return uint16(d.bar[barKey(bar, off)] & 0xFFFF), nil
}

func (d *fakeConsoleDevice) Read32(bar uint8, off uint64) (uint32, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgDeviceFeatureSelect:
			return d.deviceFeatureSelect, nil
		case common.CfgDeviceFeature:
			if d.deviceFeatureSelect == 0 {
				return uint32(d.deviceFeatures & 0xFFFFFFFF), nil
			}
			return uint32(d.deviceFeatures >> 32), nil
		}
	}
	return uint32(d.bar[barKey(bar, off)] & 0xFFFFFFFF), nil
}

func (d *fakeConsoleDevice) Read64(bar uint8, off uint64) (uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgQueueDesc:
			return d.qdesc[d.currentQueue], nil
		case common.CfgQueueDriver:
			return d.qdriver[d.currentQueue], nil
		case common.CfgQueueDevice:
			return d.qdevice[d.currentQueue], nil
		}
	}
	return d.bar[barKey(bar, off)], nil
}

func (d *fakeConsoleDevice) Write8(bar uint8, off uint64, v uint8) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() && off-d.commonCfgOffset() == common.CfgDeviceStatus {
		// Simulate the FEATURES_OK handshake. virtio-console requires no
		// device-specific bits for the single-port baseline, so the only
		// requirement is VERSION_1 (which the driver always acks) — unless
		// the test forces a rejection via clearFeaturesOK.
		if v&common.StatusFeaturesOK != 0 {
			if d.clearFeaturesOK || d.driverFeatures&common.FeatureVersion1 == 0 {
				v &^= common.StatusFeaturesOK
			}
		}
		d.deviceStatus = v
		return nil
	}
	d.bar[barKey(bar, off)] = uint64(v)
	return nil
}

func (d *fakeConsoleDevice) Write16(bar uint8, off uint64, v uint16) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgQueueSelect:
			d.currentQueue = v
			return nil
		case common.CfgQueueSize:
			d.qsize[d.currentQueue] = v
			return nil
		case common.CfgQueueEnable:
			d.qenable[d.currentQueue] = v
			return nil
		}
	}
	if off >= 0x1000 && off < 0x2000 {
		d.handleNotify(v)
	}
	d.bar[barKey(bar, off)] = uint64(v)
	return nil
}

func (d *fakeConsoleDevice) Write32(bar uint8, off uint64, v uint32) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgDeviceFeatureSelect:
			d.deviceFeatureSelect = v
			return nil
		case common.CfgDriverFeatureSelect:
			d.bar[barKey(bar, off)] = uint64(v)
			return nil
		case common.CfgDriverFeature:
			sel := d.bar[barKey(bar, common.CfgDriverFeatureSelect)]
			if sel == 0 {
				d.driverFeatures = (d.driverFeatures &^ 0xFFFFFFFF) | uint64(v)
			} else {
				d.driverFeatures = (d.driverFeatures & 0xFFFFFFFF) | (uint64(v) << 32)
			}
			return nil
		}
	}
	// virtio-console's notify_off_multiplier is 4, so the doorbell is a
	// uint32 MMIO write (common.NotifyQueue widens it).
	if off >= 0x1000 && off < 0x2000 {
		d.handleNotify(uint16((off - 0x1000) / 4))
	}
	d.bar[barKey(bar, off)] = uint64(v)
	return nil
}

func (d *fakeConsoleDevice) Write64(bar uint8, off uint64, v uint64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgQueueDesc:
			d.qdesc[d.currentQueue] = v
			return nil
		case common.CfgQueueDriver:
			d.qdriver[d.currentQueue] = v
			return nil
		case common.CfgQueueDevice:
			d.qdevice[d.currentQueue] = v
			return nil
		}
	}
	d.bar[barKey(bar, off)] = v
	return nil
}

// handleNotify simulates the device-side reaction to a transmitq
// doorbell: complete the most-recently-published descriptor by writing a
// used-ring entry. The receiveq doorbell is a no-op; rx delivery is
// driven explicitly via deliver.
func (d *fakeConsoleDevice) handleNotify(qIdx uint16) {
	if !d.txCompletes || qIdx != TransmitQueueIdx {
		return
	}
	availAddr := d.qdriver[qIdx]
	usedAddr := d.qdevice[qIdx]
	if availAddr == 0 || usedAddr == 0 {
		return
	}
	size := d.qsize[qIdx]
	availSlice := readBufferBytes(uintptr(availAddr), 4+2*int(size))
	if availSlice == nil {
		return
	}
	availIdx := le.Uint16(availSlice[2:4])
	if availIdx == 0 {
		return
	}
	lastSlot := (availIdx - 1) % size
	descIdx := le.Uint16(availSlice[4+lastSlot*2 : 4+lastSlot*2+2])
	usedSlice := readBufferBytes(uintptr(usedAddr), 4+8*int(size))
	if usedSlice == nil {
		return
	}
	usedIdx := le.Uint16(usedSlice[2:4])
	slot := usedIdx % size
	uo := 4 + int(slot)*8
	le.PutUint32(usedSlice[uo:uo+4], uint32(descIdx))
	le.PutUint32(usedSlice[uo+4:uo+8], 0)
	le.PutUint16(usedSlice[2:4], usedIdx+1)
}

// deliver injects raw bytes into the next available receiveq descriptor
// and posts a used-ring entry reporting len(raw). Returns false if the
// driver has not posted a receiveq buffer to consume.
func (d *fakeConsoleDevice) deliver(raw []byte) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	const q = ReceiveQueueIdx
	availAddr := d.qdriver[q]
	usedAddr := d.qdevice[q]
	descAddr := d.qdesc[q]
	if availAddr == 0 || usedAddr == 0 || descAddr == 0 {
		return false
	}
	size := d.qsize[q]
	availSlice := readBufferBytes(uintptr(availAddr), 4+2*int(size))
	if availSlice == nil {
		return false
	}
	availIdx := le.Uint16(availSlice[2:4])
	if d.rxConsumed >= availIdx {
		return false
	}
	slot := d.rxConsumed % size
	descIdx := le.Uint16(availSlice[4+slot*2 : 4+slot*2+2])
	descSlice := readBufferBytes(uintptr(descAddr), 16*int(size))
	o := int(descIdx) * 16
	bufAddr := le.Uint64(descSlice[o : o+8])
	bufLen := le.Uint32(descSlice[o+8 : o+12])
	n := len(raw)
	if uint32(n) > bufLen {
		n = int(bufLen)
	}
	copy(readBufferBytes(uintptr(bufAddr), n), raw[:n])
	usedSlice := readBufferBytes(uintptr(usedAddr), 4+8*int(size))
	usedIdx := le.Uint16(usedSlice[2:4])
	uslot := usedIdx % size
	uo := 4 + int(uslot)*8
	le.PutUint32(usedSlice[uo:uo+4], uint32(descIdx))
	le.PutUint32(usedSlice[uo+4:uo+8], uint32(n))
	le.PutUint16(usedSlice[2:4], usedIdx+1)
	d.rxConsumed++
	return true
}

// buildVirtioConsoleCfgSpace builds a 256-byte PCI config-space buffer
// with a virtio-console cap chain:
//
//	0x00 VID=0x1AF4 DID=0x1043
//	0x06 Status[CapList]=1
//	0x34 CapPtr=0x40
//	0x40 CommonCfg cap (16 bytes) BAR=0 offset=0 length=0x38
//	0x50 NotifyCfg ext cap (20 bytes) BAR=0 offset=0x1000 length=0x100
//	     [+16..+20] = 4 (notify_off_multiplier); next = end
//
// No DeviceCfg cap — this driver does not negotiate F_SIZE and so never
// reads a device-config region.
func buildVirtioConsoleCfgSpace() []byte {
	cfg := make([]byte, 256)
	le.PutUint16(cfg[0:], common.PCIVendorID)
	le.PutUint16(cfg[2:], common.PCIDeviceIDModernConsole)
	le.PutUint16(cfg[6:], common.PCIStatusCapabilityList)
	cfg[0x34] = 0x40

	// CommonCfg cap at 0x40.
	cfg[0x40] = common.PCICapIDVendorSpecific
	cfg[0x41] = 0x50 // next
	cfg[0x42] = 16   // cap_len
	cfg[0x43] = common.PCICapCommonCfg
	cfg[0x44] = 0                  // bar
	cfg[0x45] = 0                  // id
	le.PutUint32(cfg[0x48:], 0)    // offset
	le.PutUint32(cfg[0x4C:], 0x38) // length

	// NotifyCfg ext cap at 0x50, 20 bytes, next = end.
	cfg[0x50] = common.PCICapIDVendorSpecific
	cfg[0x51] = 0x00 // next = end
	cfg[0x52] = 20   // cap_len (extended)
	cfg[0x53] = common.PCICapNotifyCfg
	cfg[0x54] = 0
	cfg[0x55] = 0
	le.PutUint32(cfg[0x58:], 0x1000) // offset
	le.PutUint32(cfg[0x5C:], 0x100)  // length
	le.PutUint32(cfg[0x60:], 4)      // notify_off_multiplier

	return cfg
}

// --- happy-path + semantic tests --------------------------------------

func TestOpenVirtioConsole_Success(t *testing.T) {
	d := newFakeConsoleDevice(common.FeatureVersion1)
	v, err := OpenVirtioConsole(d)
	if err != nil {
		t.Fatalf("OpenVirtioConsole: %v", err)
	}
	if v.NegotiatedFeatures != common.FeatureVersion1 {
		t.Errorf("Negotiated: got 0x%x, want 0x%x", v.NegotiatedFeatures, common.FeatureVersion1)
	}
	if v.ReceiveQueue() == nil {
		t.Error("ReceiveQueue nil")
	}
	if v.TransmitQueue() == nil {
		t.Error("TransmitQueue nil")
	}
}

func TestOpenVirtioConsole_IgnoresExtraDeviceBits(t *testing.T) {
	// Device offers spurious high feature bits + F_SIZE(0) + F_MULTIPORT(1);
	// the driver must mask them all out and negotiate only VERSION_1.
	d := newFakeConsoleDevice(common.FeatureVersion1 | (1 << 40) | (1 << 0) | (1 << 1))
	v, err := OpenVirtioConsole(d)
	if err != nil {
		t.Fatalf("OpenVirtioConsole: %v", err)
	}
	if v.NegotiatedFeatures != common.FeatureVersion1 {
		t.Errorf("Negotiated: got 0x%x, want 0x%x", v.NegotiatedFeatures, common.FeatureVersion1)
	}
}

func TestAcceptFeatures(t *testing.T) {
	if got, err := AcceptFeatures(common.FeatureVersion1 | (1 << 7)); err != nil || got != common.FeatureVersion1 {
		t.Errorf("AcceptFeatures(modern): got 0x%x, %v", got, err)
	}
	if _, err := AcceptFeatures(1 << 7); !errors.Is(err, ErrNotModernDevice) {
		t.Errorf("AcceptFeatures(legacy): got %v, want ErrNotModernDevice", err)
	}
}

func TestOpenVirtioConsole_WrongDeviceID(t *testing.T) {
	d := newFakeConsoleDevice(common.FeatureVersion1)
	le.PutUint16(d.cfg[2:], common.PCIDeviceIDModernNet) // pretend to be virtio-net
	if _, err := OpenVirtioConsole(d); !errors.Is(err, ErrInitWrongDeviceID) {
		t.Errorf("got %v, want ErrInitWrongDeviceID", err)
	}
}

func TestOpenVirtioConsole_LegacyDevice(t *testing.T) {
	d := newFakeConsoleDevice(1 << 7) // no VERSION_1
	if _, err := OpenVirtioConsole(d); !errors.Is(err, ErrNotModernDevice) {
		t.Errorf("got %v, want ErrNotModernDevice", err)
	}
}

func TestOpenVirtioConsole_FeaturesNotOK(t *testing.T) {
	d := newFakeConsoleDevice(common.FeatureVersion1)
	d.clearFeaturesOK = true
	if _, err := OpenVirtioConsole(d); !errors.Is(err, ErrFeaturesNotOK) {
		t.Errorf("got %v, want ErrFeaturesNotOK", err)
	}
}

func TestOpenVirtioConsole_QueueZeroSize(t *testing.T) {
	d := newFakeConsoleDevice(common.FeatureVersion1)
	d.qsize[0] = 0
	if _, err := OpenVirtioConsole(d); !errors.Is(err, ErrQueueNotAvailable) {
		t.Errorf("got %v, want ErrQueueNotAvailable", err)
	}
}

func TestOpenVirtioConsole_QueueSizeClampAndRound(t *testing.T) {
	// maxSize=6 is below the desired 16 (exercises the clamp) and is not a
	// power of two (exercises the round-down loop): 6 → 4.
	d := newFakeConsoleDevice(common.FeatureVersion1)
	d.qsize[0] = 6
	d.qsize[1] = 6
	v, err := OpenVirtioConsole(d)
	if err != nil {
		t.Fatalf("OpenVirtioConsole: %v", err)
	}
	if got := v.ReceiveQueue().Layout.Size; got != 4 {
		t.Errorf("queue size: got %d, want 4 (clamped 16→6, rounded 6→4)", got)
	}
}

func TestOpenVirtioConsole_AllocFail(t *testing.T) {
	d := newFakeConsoleDevice(common.FeatureVersion1)
	d.allocFail = true
	if _, err := OpenVirtioConsole(d); err == nil {
		t.Error("expected alloc error")
	}
}

func TestSentinelError(t *testing.T) {
	// The sentinel type's Error() method must round-trip its message.
	if got := ErrReceiveTimeout.Error(); got != string(ErrReceiveTimeout) {
		t.Errorf("Error(): got %q", got)
	}
}

func TestReadBufferBytes_NilGuard(t *testing.T) {
	if readBufferBytes(0, 8) != nil {
		t.Error("addr==0 should return nil")
	}
	if readBufferBytes(1234, 0) != nil {
		t.Error("length<=0 should return nil")
	}
}

// --- Write path -------------------------------------------------------

func TestWrite_RoundTrip(t *testing.T) {
	d := newFakeConsoleDevice(common.FeatureVersion1)
	v, err := OpenVirtioConsole(d)
	if err != nil {
		t.Fatalf("OpenVirtioConsole: %v", err)
	}
	n, err := v.Write([]byte("hello console"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len("hello console") {
		t.Errorf("n: got %d, want %d", n, len("hello console"))
	}
}

func TestWrite_MultiPage(t *testing.T) {
	// Larger than one page: exercises the chunk>pageSize clamp and the
	// multi-chunk loop.
	d := newFakeConsoleDevice(common.FeatureVersion1)
	v, err := OpenVirtioConsole(d)
	if err != nil {
		t.Fatalf("OpenVirtioConsole: %v", err)
	}
	p := make([]byte, int(common.PageSize)+100)
	for i := range p {
		p[i] = byte(i)
	}
	n, err := v.Write(p)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(p) {
		t.Errorf("n: got %d, want %d", n, len(p))
	}
}

func TestWrite_ZeroLen(t *testing.T) {
	d := newFakeConsoleDevice(common.FeatureVersion1)
	v, err := OpenVirtioConsole(d)
	if err != nil {
		t.Fatalf("OpenVirtioConsole: %v", err)
	}
	n, err := v.Write(nil)
	if err != nil || n != 0 {
		t.Errorf("Write(nil): got (%d, %v), want (0, nil)", n, err)
	}
}

func TestWrite_Timeout(t *testing.T) {
	d := newFakeConsoleDevice(common.FeatureVersion1)
	v, err := OpenVirtioConsole(d)
	if err != nil {
		t.Fatalf("OpenVirtioConsole: %v", err)
	}
	d.txCompletes = false // device never completes the transmit
	if _, err := v.Write([]byte("stuck")); !errors.Is(err, ErrTransmitTimeout) {
		t.Errorf("got %v, want ErrTransmitTimeout", err)
	}
}

func TestWrite_AllocFail(t *testing.T) {
	d := newFakeConsoleDevice(common.FeatureVersion1)
	v, err := OpenVirtioConsole(d)
	if err != nil {
		t.Fatalf("OpenVirtioConsole: %v", err)
	}
	d.allocFail = true
	if _, err := v.Write([]byte("x")); err == nil {
		t.Error("expected alloc error")
	}
}

func TestWrite_AllocZeroPhys(t *testing.T) {
	d := newFakeConsoleDevice(common.FeatureVersion1)
	it := newInject(d, false)
	v, err := OpenVirtioConsole(it)
	if err != nil {
		t.Fatalf("OpenVirtioConsole: %v", err)
	}
	it.enable = true
	it.zeroPhys = true
	if _, err := v.Write([]byte("x")); !errors.Is(err, common.ErrAllocReturnedZero) {
		t.Errorf("got %v, want ErrAllocReturnedZero", err)
	}
}

func TestWrite_BufferTooSmall(t *testing.T) {
	d := newFakeConsoleDevice(common.FeatureVersion1)
	it := newInject(d, false)
	v, err := OpenVirtioConsole(it)
	if err != nil {
		t.Fatalf("OpenVirtioConsole: %v", err)
	}
	it.enable = true
	it.shortAllocBytes = 4 // truncate the next AllocatePages mem
	if _, err := v.Write(make([]byte, 16)); !errors.Is(err, ErrBufferTooSmall) {
		t.Errorf("got %v, want ErrBufferTooSmall", err)
	}
}

func TestWrite_QueueFull(t *testing.T) {
	d := newFakeConsoleDevice(common.FeatureVersion1)
	v, err := OpenVirtioConsole(d)
	if err != nil {
		t.Fatalf("OpenVirtioConsole: %v", err)
	}
	// Saturate the transmitq descriptor slots so AddBuffer returns
	// ErrQueueFull.
	for i := range v.txq.Buffers {
		v.txq.Buffers[i].InUse = true
	}
	if _, err := v.Write([]byte("x")); !errors.Is(err, common.ErrQueueFull) {
		t.Errorf("got %v, want ErrQueueFull", err)
	}
}

func TestWrite_NotifyFail(t *testing.T) {
	d := newFakeConsoleDevice(common.FeatureVersion1)
	it := newInject(d, false)
	v, err := OpenVirtioConsole(it)
	if err != nil {
		t.Fatalf("OpenVirtioConsole: %v", err)
	}
	it.enable = true
	// TX notify offset is 0x1000 + TransmitQueueIdx*4 = 0x1004. Arm a
	// one-shot Write32 failure there.
	it.fp = failPoint{"Write32@0x1004", 1}
	if _, err := v.Write([]byte("x")); err == nil {
		t.Error("expected notify error")
	}
}

// --- Read path --------------------------------------------------------

func TestRead_RoundTrip(t *testing.T) {
	d := newFakeConsoleDevice(common.FeatureVersion1)
	v, err := OpenVirtioConsole(d)
	if err != nil {
		t.Fatalf("OpenVirtioConsole: %v", err)
	}
	want := []byte("guest input\n")
	if !d.deliver(want) {
		t.Fatal("deliver failed")
	}
	got, err := v.Read(10000)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("Read: got %q, want %q", got, want)
	}
}

func TestRead_Timeout(t *testing.T) {
	d := newFakeConsoleDevice(common.FeatureVersion1)
	v, err := OpenVirtioConsole(d)
	if err != nil {
		t.Fatalf("OpenVirtioConsole: %v", err)
	}
	if _, err := v.Read(100); !errors.Is(err, ErrReceiveTimeout) {
		t.Errorf("got %v, want ErrReceiveTimeout", err)
	}
}

func TestRead_RePostThenReadAgain(t *testing.T) {
	// A second deliver after the first Read re-posts the buffer must be
	// readable too — exercises the re-post + notify path's success branch.
	d := newFakeConsoleDevice(common.FeatureVersion1)
	v, err := OpenVirtioConsole(d)
	if err != nil {
		t.Fatalf("OpenVirtioConsole: %v", err)
	}
	if !d.deliver([]byte("one")) {
		t.Fatal("deliver one failed")
	}
	if _, err := v.Read(10000); err != nil {
		t.Fatalf("Read one: %v", err)
	}
	if !d.deliver([]byte("two")) {
		t.Fatal("deliver two failed")
	}
	got, err := v.Read(10000)
	if err != nil {
		t.Fatalf("Read two: %v", err)
	}
	if string(got) != "two" {
		t.Errorf("Read two: got %q, want %q", got, "two")
	}
}

// TestRead_RePostAddBufferFails covers Read's swallowed AddBuffer
// re-post error: the captured bytes are still returned. Strategy mirrors
// net's: publish a used entry with a descIdx out of the shrunk range,
// saturate all buffers, shrink Layout.Size to 1 so Reclaim(descIdx) fails
// and AddBuffer's only slot is InUse → ErrQueueFull (swallowed).
func TestRead_RePostAddBufferFails(t *testing.T) {
	d := newFakeConsoleDevice(common.FeatureVersion1)
	v, err := OpenVirtioConsole(d)
	if err != nil {
		t.Fatalf("OpenVirtioConsole: %v", err)
	}
	for i := range v.rxq.Buffers {
		v.rxq.Buffers[i].InUse = true
	}
	usedSlice := readBufferBytes(uintptr(v.rxq.BasePhys+uint64(v.rxq.Layout.UsedRingOffset)), 4+8)
	if usedSlice == nil {
		t.Fatal("could not get usedSlice")
	}
	le.PutUint32(usedSlice[4:8], 5)  // descIdx
	le.PutUint32(usedSlice[8:12], 4) // length
	le.PutUint16(usedSlice[2:4], 1)  // bump usedIdx
	v.rxq.Layout.Size = 1
	out, err := v.Read(10)
	if err != nil {
		t.Errorf("Read should swallow re-post AddBuffer error: got %v", err)
	}
	if out == nil {
		t.Error("expected non-nil bytes even with swallowed error")
	}
}

// TestRead_RePostNotifyFails covers Read's swallowed NotifyQueue re-post
// error: the captured bytes are still returned.
func TestRead_RePostNotifyFails(t *testing.T) {
	d := newFakeConsoleDevice(common.FeatureVersion1)
	w := &notifyFailRx{fakeConsoleDevice: d, failOff: 0x1000}
	v, err := OpenVirtioConsole(w)
	if err != nil {
		t.Fatalf("OpenVirtioConsole: %v", err)
	}
	if !d.deliver([]byte("data")) {
		t.Fatal("deliver failed")
	}
	// Arm the failure AFTER bring-up so the next NotifyQueue (inside Read)
	// is what fails.
	w.armed = true
	out, err := v.Read(10000)
	if err != nil {
		t.Errorf("Read should swallow re-post NotifyQueue error: got %v", err)
	}
	if string(out) != "data" {
		t.Errorf("bytes: got %q, want %q", out, "data")
	}
}

// notifyFailRx wraps fakeConsoleDevice and returns an error from Write32
// on the targeted notify offset once `armed` is set. Used to cover Read's
// swallowed-NotifyQueue branch.
type notifyFailRx struct {
	*fakeConsoleDevice
	failOff uint64
	armed   bool
}

func (n *notifyFailRx) Write32(bar uint8, off uint64, v uint32) error {
	if n.armed && bar == 0 && off == n.failOff {
		n.armed = false
		return errInjected
	}
	return n.fakeConsoleDevice.Write32(bar, off, v)
}

// --- injection harness + transport-error coverage ---------------------

var errInjected = errors.New("injected transport failure")

type failPoint struct {
	method string
	nth    int // fail on this 1-based call count to method; 0 = never
}

// injectTransport wraps a fakeConsoleDevice and fails the nth call to a
// chosen method once `enable` is set. ReadConfig8/ReadConfig32/Read32/
// Read64 are never targeted, so they stay promoted from the embedded
// device.
//
// zeroPhys forces AllocatePages to return phys==0 (counted only while
// enabled). shortAllocBytes truncates the returned mem to that many bytes
// (counted only while enabled).
type injectTransport struct {
	*fakeConsoleDevice
	fp              failPoint
	counts          map[string]int
	enable          bool
	zeroPhys        bool
	zeroPhysAfter   int
	shortAllocBytes int
}

func newInject(d *fakeConsoleDevice, enable bool) *injectTransport {
	return &injectTransport{fakeConsoleDevice: d, counts: map[string]int{}, enable: enable}
}

func (t *injectTransport) fail(m string) bool {
	if !t.enable || t.fp.method != m {
		return false
	}
	t.counts[m]++
	return t.counts[m] == t.fp.nth
}

func (t *injectTransport) ReadConfig16(o uint8) (uint16, error) {
	if t.fail("ReadConfig16") {
		return 0, errInjected
	}
	return t.fakeConsoleDevice.ReadConfig16(o)
}
func (t *injectTransport) Read8(b uint8, o uint64) (uint8, error) {
	if t.fail("Read8") {
		return 0, errInjected
	}
	return t.fakeConsoleDevice.Read8(b, o)
}
func (t *injectTransport) Read16(b uint8, o uint64) (uint16, error) {
	if t.fail("Read16") {
		return 0, errInjected
	}
	return t.fakeConsoleDevice.Read16(b, o)
}
func (t *injectTransport) Write8(b uint8, o uint64, v uint8) error {
	if t.fail("Write8") {
		return errInjected
	}
	return t.fakeConsoleDevice.Write8(b, o, v)
}
func (t *injectTransport) Write16(b uint8, o uint64, v uint16) error {
	if t.fail("Write16") {
		return errInjected
	}
	return t.fakeConsoleDevice.Write16(b, o, v)
}
func (t *injectTransport) Write32(b uint8, o uint64, v uint32) error {
	if t.fail("Write32") {
		return errInjected
	}
	// Offset-specific notify-doorbell targets (RX at 0x1000, TX at 0x1004).
	for _, target := range []struct {
		key string
		off uint64
	}{{"Write32@0x1000", 0x1000}, {"Write32@0x1004", 0x1004}} {
		if t.enable && t.fp.method == target.key && o == target.off {
			t.counts[target.key]++
			if t.counts[target.key] == t.fp.nth {
				return errInjected
			}
		}
	}
	return t.fakeConsoleDevice.Write32(b, o, v)
}
func (t *injectTransport) Write64(b uint8, o uint64, v uint64) error {
	if t.fail("Write64") {
		return errInjected
	}
	return t.fakeConsoleDevice.Write64(b, o, v)
}
func (t *injectTransport) AllocatePages(c int) (uint64, []byte, error) {
	if t.fail("AllocatePages") {
		return 0, nil, errInjected
	}
	phys, mem, err := t.fakeConsoleDevice.AllocatePages(c)
	if err != nil {
		return phys, mem, err
	}
	if t.enable && t.zeroPhys {
		t.zeroPhysAfter++
		return 0, mem, nil
	}
	if t.enable && t.shortAllocBytes > 0 && t.shortAllocBytes < len(mem) {
		mem = mem[:t.shortAllocBytes]
	}
	return phys, mem, err
}

// TestOpenVirtioConsole_TransportErrors drives every `if err != nil`
// return inside OpenVirtioConsole + setupQueue by failing the
// corresponding transport call. The (method, nth) coordinates follow the
// fixed call order of the bring-up sequence (mirrors rng/net: receiveq is
// queue setup #1, transmitq is queue setup #2).
func TestOpenVirtioConsole_TransportErrors(t *testing.T) {
	cases := []struct {
		name string
		fp   failPoint
	}{
		{"DIDRead", failPoint{"ReadConfig16", 1}},
		{"InitModernConfig", failPoint{"ReadConfig16", 2}}, // PCI status read
		{"ResetStatus", failPoint{"Write8", 1}},
		{"PostResetStatusRead", failPoint{"Read8", 1}},
		{"AckStatus", failPoint{"Write8", 2}},
		{"DriverStatus", failPoint{"Write8", 3}},
		{"DeviceFeatures", failPoint{"Write32", 1}}, // WriteDeviceFeatureSelect(0)
		{"DriverFeatures", failPoint{"Write32", 3}}, // WriteDriverFeatureSelect(0)
		{"FeaturesOKStatus", failPoint{"Write8", 4}},
		{"PostFeaturesStatusRead", failPoint{"Read8", 2}},
		// receiveq (queue setup #1).
		{"RxSelectQueue", failPoint{"Write16", 1}},
		{"RxQueueSize", failPoint{"Read16", 1}},
		{"RxSetQueueSize", failPoint{"Write16", 2}},
		{"RxQueueNotifyOff", failPoint{"Read16", 2}},
		{"RxAllocVirtqueue", failPoint{"AllocatePages", 1}},
		{"RxSetQueueDesc", failPoint{"Write64", 1}},
		{"RxSetQueueDriver", failPoint{"Write64", 2}},
		{"RxSetQueueDevice", failPoint{"Write64", 3}},
		{"RxSetQueueEnable", failPoint{"Write16", 3}},
		// transmitq (queue setup #2).
		{"TxSelectQueue", failPoint{"Write16", 4}},
		{"TxSetQueueEnable", failPoint{"Write16", 6}},
		{"TxAllocVirtqueue", failPoint{"AllocatePages", 2}},
		// DRIVER_OK status write.
		{"DriverOKStatus", failPoint{"Write8", 5}},
		// fillRxRing AllocatePages (3rd alloc: rxq backing, txq backing,
		// then the first rx buffer).
		{"FillRxAlloc", failPoint{"AllocatePages", 3}},
		// RX notify after fillRxRing (RX doorbell at offset 0x1000).
		{"RxNotify", failPoint{"Write32@0x1000", 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeConsoleDevice(common.FeatureVersion1)
			it := newInject(d, true)
			it.fp = tc.fp
			if _, err := OpenVirtioConsole(it); err == nil {
				t.Fatalf("%s: expected error injected at %+v", tc.name, tc.fp)
			}
		})
	}
}

// TestOpenVirtioConsole_FillRxBufferTooSmall covers fillRxRing's
// ErrBufferTooSmall branch: the rx-buffer AllocatePages returns a
// truncated page.
func TestOpenVirtioConsole_FillRxBufferTooSmall(t *testing.T) {
	d := newFakeConsoleDevice(common.FeatureVersion1)
	it := newInject(d, false)
	// Enable AFTER the two virtqueue-backing allocations so the truncation
	// hits the first rx-buffer allocation in fillRxRing. We can't easily
	// gate by call index here, so use a dedicated wrapper.
	w := &fillShortAlloc{injectTransport: it, shortAfter: 2, shortBytes: 8}
	if _, err := OpenVirtioConsole(w); !errors.Is(err, ErrBufferTooSmall) {
		t.Errorf("got %v, want ErrBufferTooSmall", err)
	}
}

// TestOpenVirtioConsole_FillRxZeroPhys covers fillRxRing's
// ErrAllocReturnedZero branch.
func TestOpenVirtioConsole_FillRxZeroPhys(t *testing.T) {
	d := newFakeConsoleDevice(common.FeatureVersion1)
	it := newInject(d, false)
	w := &fillZeroPhys{injectTransport: it, zeroAfter: 2}
	if _, err := OpenVirtioConsole(w); !errors.Is(err, common.ErrAllocReturnedZero) {
		t.Errorf("got %v, want ErrAllocReturnedZero", err)
	}
}

// TestOpenVirtioConsole_FillRxQueueFull covers fillRxRing's AddBuffer
// error branch. Drive it directly: saturate the rxq, then re-run
// fillRxRing — every AddBuffer returns ErrQueueFull.
func TestOpenVirtioConsole_FillRxQueueFull(t *testing.T) {
	d := newFakeConsoleDevice(common.FeatureVersion1)
	v, err := OpenVirtioConsole(d)
	if err != nil {
		t.Fatalf("OpenVirtioConsole: %v", err)
	}
	for i := range v.rxq.Buffers {
		v.rxq.Buffers[i].InUse = true
	}
	if err := v.fillRxRing(); !errors.Is(err, common.ErrQueueFull) {
		t.Errorf("got %v, want ErrQueueFull", err)
	}
}

// fillShortAlloc truncates AllocatePages' mem to shortBytes only after
// `shortAfter` successful allocations — used to target the first
// rx-buffer allocation inside fillRxRing.
type fillShortAlloc struct {
	*injectTransport
	shortAfter int
	shortBytes int
	count      int
}

func (f *fillShortAlloc) AllocatePages(c int) (uint64, []byte, error) {
	phys, mem, err := f.injectTransport.fakeConsoleDevice.AllocatePages(c)
	if err != nil {
		return phys, mem, err
	}
	f.count++
	if f.count > f.shortAfter && f.shortBytes < len(mem) {
		mem = mem[:f.shortBytes]
	}
	return phys, mem, err
}

// fillZeroPhys forces AllocatePages to return phys==0 only after
// `zeroAfter` allocations — targets the first rx-buffer allocation.
type fillZeroPhys struct {
	*injectTransport
	zeroAfter int
	count     int
}

func (f *fillZeroPhys) AllocatePages(c int) (uint64, []byte, error) {
	phys, mem, err := f.injectTransport.fakeConsoleDevice.AllocatePages(c)
	if err != nil {
		return phys, mem, err
	}
	f.count++
	if f.count > f.zeroAfter {
		return 0, mem, nil
	}
	return phys, mem, err
}
