//go:build windows

package main

// Direct calls into ViGEmClient.dll (bundled inside the exe) so no Python,
// compiler or extra runtime is needed on the host PC.

import (
	"bufio"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

//go:embed assets/ViGEmClient.dll
var vigemDLL []byte

//go:embed assets/ViGEmBusSetup_x64.msi
var vigemBusMSI []byte

const (
	vigemErrorNone        = 0x20000000
	vigemErrorBusNotFound = 0xE0000001
)

type vigemAPI struct {
	alloc, free, connect, disconnect               *syscall.Proc
	x360Alloc, ds4Alloc, targetFree                *syscall.Proc
	targetAdd, targetRemove, x360Update, ds4Update *syscall.Proc
}

func loadVigem() (*vigemAPI, error) {
	dir := filepath.Join(os.TempDir(), "IkemenParty")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "ViGEmClient.dll")
	if old, err := os.ReadFile(path); err != nil || len(old) != len(vigemDLL) {
		if err := os.WriteFile(path, vigemDLL, 0o644); err != nil {
			return nil, fmt.Errorf("could not unpack ViGEmClient.dll: %w", err)
		}
	}
	dll, err := syscall.LoadDLL(path)
	if err != nil {
		return nil, fmt.Errorf("could not load ViGEmClient.dll: %w", err)
	}
	api := &vigemAPI{}
	procs := map[string]**syscall.Proc{
		"vigem_alloc": &api.alloc, "vigem_free": &api.free,
		"vigem_connect": &api.connect, "vigem_disconnect": &api.disconnect,
		"vigem_target_x360_alloc": &api.x360Alloc, "vigem_target_ds4_alloc": &api.ds4Alloc,
		"vigem_target_free": &api.targetFree, "vigem_target_add": &api.targetAdd,
		"vigem_target_remove":      &api.targetRemove,
		"vigem_target_x360_update": &api.x360Update, "vigem_target_ds4_update": &api.ds4Update,
	}
	for name, dst := range procs {
		p, err := dll.FindProc(name)
		if err != nil {
			return nil, fmt.Errorf("ViGEmClient.dll is missing %s", name)
		}
		*dst = p
	}
	return api, nil
}

type vigemPad struct {
	api    *vigemAPI
	client uintptr
	target uintptr
	ds4    bool
}

// The report structs are larger than 8 bytes, so the Windows x64 calling
// convention passes them by pointer. We hand over a fresh copy each call.
func (p *vigemPad) apply(s PadState) error {
	var r1 uintptr
	if p.ds4 {
		rep := toDS4(s)
		r1, _, _ = p.api.ds4Update.Call(p.client, p.target, uintptr(unsafe.Pointer(&rep)))
	} else {
		rep := toXUSB(s)
		r1, _, _ = p.api.x360Update.Call(p.client, p.target, uintptr(unsafe.Pointer(&rep)))
	}
	if uint32(r1) != vigemErrorNone {
		return fmt.Errorf("ViGEm error 0x%08X", uint32(r1))
	}
	return nil
}

func (p *vigemPad) close() {
	p.api.targetRemove.Call(p.client, p.target)
	p.api.targetFree.Call(p.target)
}

func openVirtualPads(count int, kind string) ([]padDevice, func(), error) {
	api, err := loadVigem()
	if err != nil {
		return nil, nil, err
	}
	client, _, _ := api.alloc.Call()
	if client == 0 {
		return nil, nil, errors.New("vigem_alloc failed")
	}
	r1, _, _ := api.connect.Call(client)
	if uint32(r1) == vigemErrorBusNotFound {
		if offerDriverInstall() {
			r1, _, _ = api.connect.Call(client)
		}
	}
	if uint32(r1) != vigemErrorNone {
		api.free.Call(client)
		if uint32(r1) == vigemErrorBusNotFound {
			return nil, nil, errors.New("the ViGEmBus driver isn't installed (restart the app to install it)")
		}
		return nil, nil, fmt.Errorf("could not connect to ViGEmBus (0x%08X)", uint32(r1))
	}

	var pads []padDevice
	for i := 0; i < count; i++ {
		var target uintptr
		if kind == "x360" {
			target, _, _ = api.x360Alloc.Call()
		} else {
			target, _, _ = api.ds4Alloc.Call()
		}
		if target == 0 {
			return nil, nil, errors.New("could not allocate a virtual pad")
		}
		r, _, _ := api.targetAdd.Call(client, target)
		if uint32(r) != vigemErrorNone {
			api.targetFree.Call(target)
			for _, p := range pads {
				p.close()
			}
			api.disconnect.Call(client)
			api.free.Call(client)
			return nil, nil, fmt.Errorf("could not plug in virtual pad %d (0x%08X)", i+1, uint32(r))
		}
		pad := &vigemPad{api: api, client: client, target: target, ds4: kind != "x360"}
		_ = pad.apply(PadState{})
		pads = append(pads, pad)
		time.Sleep(150 * time.Millisecond) // let Windows enumerate them in order
	}
	log.Printf("%d virtual %s controller(s) plugged in.", count, strings.ToUpper(kind))
	closer := func() {
		api.disconnect.Call(client)
		api.free.Call(client)
	}
	return pads, closer, nil
}

// offerDriverInstall runs the bundled ViGEmBus installer after asking.
func offerDriverInstall() bool {
	fmt.Println()
	fmt.Println("  Ikemen Party needs the free ViGEmBus driver to create virtual controllers.")
	fmt.Println("  It's a one-time install. Windows will ask for permission.")
	fmt.Print("  Install it now? [Y/n] ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if a := strings.TrimSpace(strings.ToLower(line)); a == "n" || a == "no" {
		return false
	}
	path := filepath.Join(os.TempDir(), "IkemenParty", "ViGEmBusSetup_x64.msi")
	if err := os.WriteFile(path, vigemBusMSI, 0o644); err != nil {
		log.Printf("Could not unpack the driver installer: %v", err)
		return false
	}
	fmt.Println("  Starting the driver installer... finish it, then come back here.")
	if err := exec.Command("msiexec", "/i", path).Run(); err != nil {
		log.Printf("Driver installer did not finish: %v", err)
		return false
	}
	time.Sleep(time.Second)
	return true
}

func openBrowser(url string) {
	_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}

// ikemenRunning reports whether Ikemen GO is still open.
func ikemenRunning() bool {
	cmd := exec.Command("tasklist", "/FI", "IMAGENAME eq Ikemen_GO.exe", "/NH")
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return true // can't tell: assume yes
	}
	return strings.Contains(strings.ToLower(string(out)), "ikemen_go.exe")
}
