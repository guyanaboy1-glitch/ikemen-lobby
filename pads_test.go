package main

import (
	"testing"
	"unsafe"
)

func TestReportSizesMatchViGEm(t *testing.T) {
	if s := unsafe.Sizeof(XUSBReport{}); s != 12 {
		t.Fatalf("XUSB_REPORT size %d, want 12", s)
	}
	if s := unsafe.Sizeof(DS4Report{}); s != 10 {
		t.Fatalf("DS4_REPORT size %d, want 10", s)
	}
	if o := unsafe.Offsetof(DS4Report{}.Buttons); o != 4 {
		t.Fatalf("DS4 wButtons offset %d, want 4", o)
	}
	if o := unsafe.Offsetof(XUSBReport{}.ThumbLX); o != 4 {
		t.Fatalf("XUSB sThumbLX offset %d, want 4", o)
	}
}

func TestParseState(t *testing.T) {
	seq, st, ok := parseState([]float64{5, 1 << 20, 300, -300, 0, 0, 999, -4})
	if !ok || seq != 5 {
		t.Fatal("expected ok")
	}
	if st.Buttons != 1<<17-1 || st.LX != 127 || st.LY != -127 || st.LT != 255 || st.RT != 0 {
		t.Fatalf("clamping wrong: %+v", st)
	}
	if _, _, ok := parseState([]float64{1, 2, 3}); ok {
		t.Fatal("short input accepted")
	}
	if _, _, ok := parseState([]float64{-1, 0, 0, 0, 0, 0, 0, 0}); ok {
		t.Fatal("negative seq accepted")
	}
}

func TestXUSBMapping(t *testing.T) {
	st := PadState{Buttons: 1<<btnA | 1<<btnUp | 1<<btnLT | 1<<btnGuide, LX: 127, LY: 127, RT: 100}
	r := toXUSB(st)
	if r.Buttons != 0x1000|0x0001|0x0400 {
		t.Fatalf("buttons 0x%04X", r.Buttons)
	}
	if r.LeftTrigger != 255 || r.RightTrigger != 100 {
		t.Fatalf("triggers %d %d", r.LeftTrigger, r.RightTrigger)
	}
	// Browser +y (down) must become XInput -y.
	if r.ThumbLX != 32767 || r.ThumbLY != -32768 {
		t.Fatalf("thumbs %d %d", r.ThumbLX, r.ThumbLY)
	}
	if n := toXUSB(PadState{}); n != (XUSBReport{}) {
		t.Fatal("neutral not zero")
	}
}

func TestDS4Mapping(t *testing.T) {
	n := toDS4(PadState{})
	if n.Buttons != 8 || n.ThumbLX != 128 || n.ThumbLY != 128 {
		t.Fatalf("neutral wrong: %+v", n)
	}
	r := toDS4(PadState{Buttons: 1<<btnA | 1<<btnX | 1<<btnDown | 1<<btnRight | 1<<btnGuide, LY: 127, RT: 7})
	if r.Buttons&0xF != 3 { // south-east
		t.Fatalf("hat %d", r.Buttons&0xF)
	}
	if r.Buttons&(1<<5) == 0 || r.Buttons&(1<<4) == 0 {
		t.Fatal("cross/square missing")
	}
	if r.Special != 1 || r.TriggerR != 7 || r.ThumbLY != 255 {
		t.Fatalf("got %+v", r)
	}
	// SOCD: left+right cancels, up stays.
	if h := ds4Hat(true, false, true, true); h != 0 {
		t.Fatalf("SOCD hat %d", h)
	}
}
