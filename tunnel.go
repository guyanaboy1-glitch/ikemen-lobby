package main

// Internet link for stream sessions, via a free Cloudflare quick tunnel.
//
// Guests on the internet can't reach a home PC directly, and browsers want
// https before they'll share a controller with a page. cloudflared solves
// both: it gives a temporary https://….trycloudflare.com address that
// forwards to Ikemen Party on this PC. It's downloaded once, on first use.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"time"
)

var tunnelURLRe = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

type TunnelState struct {
	State string `json:"state"` // off | downloading | starting | ready | error
	URL   string `json:"url,omitempty"`
	Error string `json:"error,omitempty"`
}

type Tunnel struct {
	mu      sync.Mutex
	port    int
	want    bool
	state   TunnelState
	cmd     *exec.Cmd
	dir     string
	onReady func()
}

func NewTunnel(port int, dir string) *Tunnel {
	return &Tunnel{port: port, dir: dir, state: TunnelState{State: "off"}}
}

func (t *Tunnel) State() TunnelState {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}

func (t *Tunnel) set(s TunnelState) {
	t.mu.Lock()
	t.state = s
	cb := t.onReady
	t.mu.Unlock()
	if s.State == "ready" && cb != nil {
		cb()
	}
}

func cloudflaredAsset() string {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "windows/amd64":
		return "cloudflared-windows-amd64.exe"
	case "windows/386":
		return "cloudflared-windows-386.exe"
	case "linux/amd64":
		return "cloudflared-linux-amd64"
	case "linux/arm64":
		return "cloudflared-linux-arm64"
	}
	return ""
}

func (t *Tunnel) binPath() string {
	name := "cloudflared"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	local := filepath.Join(t.dir, name)
	if _, err := os.Stat(local); err == nil {
		return local
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return local // not present yet; download target
}

// Start brings the tunnel up (downloading cloudflared if needed). Safe to call repeatedly.
func (t *Tunnel) Start() {
	t.mu.Lock()
	if t.want {
		t.mu.Unlock()
		return
	}
	t.want = true
	t.mu.Unlock()
	go t.run()
}

func (t *Tunnel) Stop() {
	t.mu.Lock()
	t.want = false
	cmd := t.cmd
	t.cmd = nil
	t.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	t.set(TunnelState{State: "off"})
}

func (t *Tunnel) wanted() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.want
}

func (t *Tunnel) run() {
	for t.wanted() {
		bin := t.binPath()
		if _, err := os.Stat(bin); err != nil {
			if err := t.download(bin); err != nil {
				t.set(TunnelState{State: "error", Error: "couldn't download cloudflared: " + err.Error()})
				log.Printf("Tunnel: %v", err)
				time.Sleep(30 * time.Second)
				continue
			}
		}
		t.set(TunnelState{State: "starting"})
		err := t.runOnce(bin)
		if !t.wanted() {
			return
		}
		msg := "the internet link closed"
		if err != nil {
			msg = err.Error()
		}
		log.Printf("Tunnel: %s. Retrying in 10 s.", msg)
		t.set(TunnelState{State: "error", Error: msg})
		time.Sleep(10 * time.Second)
	}
}

func (t *Tunnel) runOnce(bin string) error {
	cmd := exec.Command(bin, "tunnel", "--no-autoupdate", "--url", fmt.Sprintf("http://localhost:%d", t.port))
	hideWindow(cmd)
	out, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	cmd.Stdout = io.Discard
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("couldn't start cloudflared: %w", err)
	}
	t.mu.Lock()
	t.cmd = cmd
	t.mu.Unlock()
	sc := bufio.NewScanner(out)
	found := false
	for sc.Scan() {
		if !found {
			if u := tunnelURLRe.FindString(sc.Text()); u != "" {
				found = true
				log.Printf("Internet link ready: %s", u)
				t.set(TunnelState{State: "ready", URL: u})
			}
		}
	}
	return cmd.Wait()
}

func (t *Tunnel) download(dst string) error {
	asset := cloudflaredAsset()
	if asset == "" {
		return fmt.Errorf("no cloudflared build for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	t.set(TunnelState{State: "downloading"})
	log.Printf("Downloading cloudflared (one time, about 60 MB)...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	url := "https://github.com/cloudflare/cloudflared/releases/latest/download/" + asset
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download failed (%s)", resp.Status)
	}
	tmp := dst + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, resp.Body)
	f.Close()
	if err != nil || n < 1<<20 {
		os.Remove(tmp)
		if err == nil {
			err = fmt.Errorf("download incomplete")
		}
		return err
	}
	return os.Rename(tmp, dst)
}
