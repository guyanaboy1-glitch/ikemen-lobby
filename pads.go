package main

// Virtual controller bank.
//
// Each guest who holds a controller slot drives one virtual pad. Windows sees
// these as real, locally plugged-in controllers (via the ViGEmBus driver), so
// Ikemen GO treats remote players exactly like couch players.
//
// Guest input state (one JSON array):
//   [seq, buttons, lx, ly, rx, ry, lt, rt]
//   buttons: bitmask in the browser "standard gamepad" order
//            0 A, 1 B, 2 X, 3 Y, 4 LB, 5 RB, 6 LT, 7 RT, 8 Back, 9 Start,
//            10 L3, 11 R3, 12 Up, 13 Down, 14 Left, 15 Right, 16 Guide
//   lx..ry:  -127..127 (browser convention: +y = down)
//   lt, rt:  0..255

import (
	"fmt"
	"log"
	"math"
	"sync"
	"time"
)

const (
	btnA = iota
	btnB
	btnX
	btnY
	btnLB
	btnRB
	btnLT
	btnRT
	btnBack
	btnStart
	btnL3
	btnR3
	btnUp
	btnDown
	btnLeft
	btnRight
	btnGuide
)

// PadState is everything except the sequence number.
type PadState struct {
	Buttons        uint32
	LX, LY, RX, RY int
	LT, RT         int
}

func (s PadState) String() string {
	// Same text as the Python dry run, so logs and tests stay comparable.
	return fmt.Sprintf("(%d, %d, %d, %d, %d, %d, %d)", s.Buttons, s.LX, s.LY, s.RX, s.RY, s.LT, s.RT)
}

func (s PadState) bit(i int) bool { return s.Buttons>>uint(i)&1 == 1 }

func clampInt(v float64, lo, hi int) int {
	if math.IsNaN(v) {
		return 0
	}
	if v < float64(lo) {
		return lo
	}
	if v > float64(hi) {
		return hi
	}
	return int(v)
}

// parseState validates a guest input array.
func parseState(raw []float64) (seq int64, st PadState, ok bool) {
	if len(raw) != 8 {
		return 0, st, false
	}
	if raw[0] < 0 || raw[0] > 1<<53 || math.IsNaN(raw[0]) {
		return 0, st, false
	}
	seq = int64(raw[0])
	st = PadState{
		Buttons: uint32(clampInt(raw[1], 0, 1<<17-1)),
		LX:      clampInt(raw[2], -127, 127),
		LY:      clampInt(raw[3], -127, 127),
		RX:      clampInt(raw[4], -127, 127),
		RY:      clampInt(raw[5], -127, 127),
		LT:      clampInt(raw[6], 0, 255),
		RT:      clampInt(raw[7], 0, 255),
	}
	return seq, st, true
}

// ---------------------------------------------------------------- reports --

// XUSBReport matches ViGEm's XUSB_REPORT (12 bytes).
type XUSBReport struct {
	Buttons      uint16
	LeftTrigger  uint8
	RightTrigger uint8
	ThumbLX      int16
	ThumbLY      int16
	ThumbRX      int16
	ThumbRY      int16
}

// DS4Report matches ViGEm's DS4_REPORT (10 bytes with padding).
type DS4Report struct {
	ThumbLX  uint8
	ThumbLY  uint8
	ThumbRX  uint8
	ThumbRY  uint8
	Buttons  uint16
	Special  uint8
	TriggerL uint8
	TriggerR uint8
	_        uint8
}

var xusbMap = []struct {
	btn  int
	flag uint16
}{
	{btnUp, 0x0001}, {btnDown, 0x0002}, {btnLeft, 0x0004}, {btnRight, 0x0008},
	{btnStart, 0x0010}, {btnBack, 0x0020}, {btnL3, 0x0040}, {btnR3, 0x0080},
	{btnLB, 0x0100}, {btnRB, 0x0200}, {btnGuide, 0x0400},
	{btnA, 0x1000}, {btnB, 0x2000}, {btnX, 0x4000}, {btnY, 0x8000},
}

func trig(digital bool, analog int) uint8 {
	if digital {
		return 255
	}
	return uint8(analog)
}

// thumb16 converts -127..127 to XInput's -32768..32767 range.
func thumb16(v int) int16 {
	if v >= 127 {
		return 32767
	}
	if v <= -127 {
		return -32768
	}
	return int16(v * 258)
}

func toXUSB(s PadState) XUSBReport {
	var r XUSBReport
	for _, m := range xusbMap {
		if s.bit(m.btn) {
			r.Buttons |= m.flag
		}
	}
	r.LeftTrigger = trig(s.bit(btnLT), s.LT)
	r.RightTrigger = trig(s.bit(btnRT), s.RT)
	// XInput: +y = up, browser: +y = down.
	r.ThumbLX, r.ThumbLY = thumb16(s.LX), thumb16(-s.LY)
	r.ThumbRX, r.ThumbRY = thumb16(s.RX), thumb16(-s.RY)
	return r
}

var ds4Map = []struct {
	btn  int
	flag uint16
}{
	{btnX, 1 << 4}, {btnA, 1 << 5}, {btnB, 1 << 6}, {btnY, 1 << 7},
	{btnLB, 1 << 8}, {btnRB, 1 << 9}, {btnLT, 1 << 10}, {btnRT, 1 << 11},
	{btnBack, 1 << 12}, {btnStart, 1 << 13}, {btnL3, 1 << 14}, {btnR3, 1 << 15},
}

// DS4 hat values: N=0, NE=1, E=2, SE=3, S=4, SW=5, W=6, NW=7, none=8.
func ds4Hat(up, down, left, right bool) uint16 {
	if up && down {
		up, down = false, false // SOCD: opposite directions cancel
	}
	if left && right {
		left, right = false, false
	}
	switch {
	case up && right:
		return 1
	case down && right:
		return 3
	case down && left:
		return 5
	case up && left:
		return 7
	case up:
		return 0
	case right:
		return 2
	case down:
		return 4
	case left:
		return 6
	}
	return 8
}

func toDS4(s PadState) DS4Report {
	var r DS4Report
	for _, m := range ds4Map {
		if s.bit(m.btn) {
			r.Buttons |= m.flag
		}
	}
	r.Buttons |= ds4Hat(s.bit(btnUp), s.bit(btnDown), s.bit(btnLeft), s.bit(btnRight))
	if s.bit(btnGuide) {
		r.Special |= 1
	}
	r.TriggerL = trig(s.bit(btnLT), s.LT)
	r.TriggerR = trig(s.bit(btnRT), s.RT)
	// DS4 (HID): +y = down, same as the browser.
	r.ThumbLX, r.ThumbLY = uint8(128+s.LX), uint8(128+s.LY)
	r.ThumbRX, r.ThumbRY = uint8(128+s.RX), uint8(128+s.RY)
	return r
}

// ------------------------------------------------------------------- bank --

// padDevice is one virtual controller (real or dry run).
type padDevice interface {
	apply(PadState) error
	close()
}

type dryPad struct {
	index   int
	kind    string
	verbose bool
	last    PadState
}

func (d *dryPad) apply(s PadState) error {
	if s != d.last && d.verbose {
		log.Printf("pad %d (%s dry run) <- %s", d.index, d.kind, s)
	}
	d.last = s
	return nil
}
func (d *dryPad) close() {}

type PadBank struct {
	mu        sync.Mutex
	kind      string
	DryRun    bool
	pads      []padDevice
	last      []PadState
	lastInput map[int]time.Time
	closer    func()
}

// NewPadBank creates every pad up front so Windows' controller order stays
// stable and Ikemen's joystick numbers never shuffle as guests come and go.
func NewPadBank(count int, kind string, dryRun, verbose bool) *PadBank {
	b := &PadBank{kind: kind, DryRun: dryRun, lastInput: map[int]time.Time{}}
	if !dryRun {
		pads, closer, err := openVirtualPads(count, kind)
		if err != nil {
			log.Printf("Virtual controllers unavailable: %v", err)
			log.Printf("Running in DRY RUN mode (guests can watch and chat, but can't play).")
			b.DryRun = true
		} else {
			b.pads, b.closer = pads, closer
		}
	}
	if b.DryRun {
		for i := 0; i < count; i++ {
			b.pads = append(b.pads, &dryPad{index: i, kind: kind, verbose: verbose})
		}
	}
	b.last = make([]PadState, len(b.pads))
	return b
}

func (b *PadBank) Len() int { return len(b.pads) }

func (b *PadBank) Apply(slot int, s PadState) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if slot < 0 || slot >= len(b.pads) {
		return
	}
	b.lastInput[slot] = time.Now()
	if s == b.last[slot] {
		return
	}
	b.last[slot] = s
	if err := b.pads[slot].apply(s); err != nil {
		log.Printf("pad %d update failed: %v", slot+1, err)
	}
}

func (b *PadBank) Neutral(slot int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if slot < 0 || slot >= len(b.pads) {
		return
	}
	delete(b.lastInput, slot)
	b.last[slot] = PadState{}
	_ = b.pads[slot].apply(PadState{})
}

// ReleaseIdle neutralizes pads whose player has gone quiet.
func (b *PadBank) ReleaseIdle(timeout time.Duration) {
	b.mu.Lock()
	var stale []int
	for s, t := range b.lastInput {
		if time.Since(t) > timeout {
			stale = append(stale, s)
		}
	}
	b.mu.Unlock()
	for _, s := range stale {
		b.Neutral(s)
	}
}

func (b *PadBank) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, p := range b.pads {
		p.close()
	}
	if b.closer != nil {
		b.closer()
	}
}
