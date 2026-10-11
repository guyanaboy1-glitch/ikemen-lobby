// Ikemen Party - host app.
//
// Run on the PC that runs Ikemen GO. It serves the host page
// (http://localhost:PORT/host) and the guest page (/), relays WebRTC setup
// between them, keeps the session roster (viewers, players, controller slots,
// chat) and drives one virtual controller per player slot.
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const Version = "0.3.0"

//go:embed web
var embeddedWeb embed.FS

type Config struct {
	Help               string           `json:"_help,omitempty"`
	Port               int              `json:"port"`
	SessionName        string           `json:"session_name"`
	GameName           string           `json:"game_name"`
	PlayerSlotsHelp    string           `json:"_player_slots,omitempty"`
	PlayerSlots        int              `json:"player_slots"`
	MaxViewers         int              `json:"max_viewers"`
	PadTypeHelp        string           `json:"_pad_type,omitempty"`
	PadType            string           `json:"pad_type"`
	VideoHelp          string           `json:"_video,omitempty"`
	VideoMaxKbps       int              `json:"video_max_kbps"`
	VideoMaxFPS        int              `json:"video_max_fps"`
	IdleReleaseSeconds float64          `json:"idle_release_seconds"`
	ICEServers         []map[string]any `json:"ice_servers"`
	LobbyHelp          string           `json:"_lobby,omitempty"`
	LobbyURL           string           `json:"lobby_url"`
	HostName           string           `json:"host_name"`
	TunnelHelp         string           `json:"_tunnel,omitempty"`
	Tunnel             string           `json:"tunnel"`
	PublicURL          string           `json:"public_url"`
}

func defaultConfig() *Config {
	return &Config{
		Help:               "Ikemen Party settings. Edit, save, then restart Ikemen Party. Keys starting with _ are notes.",
		Port:               8787,
		SessionName:        "Ikemen Party",
		GameName:           "Ikemen GO",
		PlayerSlotsHelp:    "Virtual controllers for guests (1-7). Host + 7 = a full 4v4.",
		PlayerSlots:        4,
		MaxViewers:         16,
		PadTypeHelp:        "ds4 avoids Windows' 4-controller XInput limit. Use x360 if Ikemen won't see the pads.",
		PadType:            "ds4",
		VideoHelp:          "Lower video_max_kbps if guests stutter. Each guest gets their own copy of the stream.",
		VideoMaxKbps:       6000,
		VideoMaxFPS:        60,
		IdleReleaseSeconds: 1.5,
		ICEServers:         []map[string]any{{"urls": "stun:stun.l.google.com:19302"}},
		LobbyHelp:          "Online Lobby server address, e.g. https://your-lobby.onrender.com. host_name is shown to everyone.",
		LobbyURL:           "",
		HostName:           "Host",
		TunnelHelp:         "tunnel: auto = free Cloudflare https link for internet guests (downloads cloudflared once), off = none. public_url overrides it.",
		Tunnel:             "auto",
		PublicURL:          "",
	}
}

// loadConfig reads the settings file, creating it with defaults on first run.
func loadConfig(path string) *Config {
	cfg := defaultConfig()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if b, err := json.MarshalIndent(cfg, "", "  "); err == nil {
			_ = os.WriteFile(path, append(b, '\n'), 0o644)
		}
	} else if err != nil {
		log.Printf("Could not read %s: %v (using defaults)", path, err)
	} else if err := json.Unmarshal(data, cfg); err != nil {
		log.Printf("%s has a mistake: %v (using defaults)", filepath.Base(path), err)
		cfg = defaultConfig()
	}
	if cfg.PlayerSlots < 1 {
		cfg.PlayerSlots = 1
	}
	if cfg.PlayerSlots > 7 {
		cfg.PlayerSlots = 7
	}
	if cfg.MaxViewers < 1 {
		cfg.MaxViewers = 1
	}
	if cfg.PadType != "x360" {
		cfg.PadType = "ds4"
	}
	if cfg.Port <= 0 || cfg.Port > 65535 {
		cfg.Port = 8787
	}
	if cfg.IdleReleaseSeconds <= 0 {
		cfg.IdleReleaseSeconds = 1.5
	}
	cfg.LobbyURL = strings.TrimRight(strings.TrimSpace(cfg.LobbyURL), "/")
	cfg.PublicURL = strings.TrimSpace(cfg.PublicURL)
	if cfg.PublicURL != "" && !strings.HasSuffix(cfg.PublicURL, "/") {
		cfg.PublicURL += "/"
	}
	if cfg.HostName = cleanText(cfg.HostName, 24); cfg.HostName == "" {
		cfg.HostName = "Host"
	}
	if cfg.Tunnel != "off" {
		cfg.Tunnel = "auto"
	}
	return cfg
}

func (c *Config) public() map[string]any {
	return map[string]any{
		"player_slots": c.PlayerSlots, "video_max_kbps": c.VideoMaxKbps,
		"video_max_fps": c.VideoMaxFPS, "ice_servers": c.ICEServers, "max_viewers": c.MaxViewers,
	}
}

// isLocal is true only for requests made on this PC. A tunnel (e.g.
// cloudflared) also connects from 127.0.0.1 but adds forwarding headers.
func isLocal(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return false
	}
	for _, h := range []string{"X-Forwarded-For", "Cf-Connecting-Ip", "Forwarded"} {
		if r.Header.Get(h) != "" {
			return false
		}
	}
	return true
}

func newMux(sess *Session, web fs.FS) *http.ServeMux {
	mux := http.NewServeMux()
	noStore := func(w http.ResponseWriter) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
	}
	serveFile := func(w http.ResponseWriter, r *http.Request, name string) {
		data, err := fs.ReadFile(web, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		noStore(w)
		switch {
		case strings.HasSuffix(name, ".html"):
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		case strings.HasSuffix(name, ".js"):
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		case strings.HasSuffix(name, ".css"):
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		}
		_, _ = w.Write(data)
	}

	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		local := isLocal(r)
		ws, err := wsUpgrade(w, r)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		sess.Serve(ws, local)
	})
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		_ = json.NewEncoder(w).Encode(sess.Info())
	})
	mux.HandleFunc("/thumb.jpg", func(w http.ResponseWriter, r *http.Request) {
		t := sess.Thumb()
		if t == nil {
			http.NotFound(w, r)
			return
		}
		noStore(w)
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		_, _ = w.Write(t)
	})
	mux.HandleFunc("/host", func(w http.ResponseWriter, r *http.Request) {
		if !isLocal(r) {
			http.Error(w, "The host page only opens on the host PC.", http.StatusForbidden)
			return
		}
		serveFile(w, r, "host.html")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			name = "guest.html"
		}
		if name == "host.html" || !fs.ValidPath(name) || strings.Contains(name, "/") {
			http.NotFound(w, r)
			return
		}
		serveFile(w, r, name)
	})
	return mux
}

func localIPs() []string {
	var out []string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			out = append(out, n.IP.String())
		}
	}
	return out
}

func exeDir() string {
	if p, err := os.Executable(); err == nil {
		return filepath.Dir(p)
	}
	return "."
}

func main() {
	dir := exeDir()
	cfgPath := flag.String("config", filepath.Join(dir, "IkemenParty_config.json"), "settings file")
	port := flag.Int("port", 0, "web port (overrides config)")
	bind := flag.String("bind", "0.0.0.0", "address to listen on")
	dry := flag.Bool("dry-run", false, "don't create real virtual controllers")
	verbose := flag.Bool("v", false, "verbose logging")
	noBrowser := flag.Bool("no-browser", false, "don't open the host page automatically")
	companion := flag.Bool("companion", false, "started by the Ikemen mod: stay in the background, exit with Ikemen")
	ikemenFlag := flag.String("ikemen", "", "Ikemen GO folder (default: next to this app)")
	flag.Parse()
	log.SetFlags(log.Ltime)

	fmt.Printf("\n  Ikemen Party v%s\n\n", Version)
	cfg := loadConfig(*cfgPath)
	if *port > 0 {
		cfg.Port = *port
	}

	// A "web" folder next to the exe overrides the built-in pages (for modding).
	var web fs.FS
	if st, err := os.Stat(filepath.Join(dir, "web")); err == nil && st.IsDir() {
		web = os.DirFS(filepath.Join(dir, "web"))
		log.Printf("Using pages from the web folder next to the app.")
	} else {
		web, _ = fs.Sub(embeddedWeb, "web")
	}

	pads := NewPadBank(cfg.PlayerSlots, cfg.PadType, *dry, *verbose)
	defer pads.Close()
	sess := NewSession(cfg, pads)
	ikemenDir := findIkemenDir(*ikemenFlag)
	app := NewApp(cfg, sess, ikemenDir, *companion)

	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", *bind, cfg.Port))
	if err != nil {
		fmt.Printf("\n  Could not start on port %d: %v\n", cfg.Port, err)
		fmt.Println("  Is Ikemen Party already running? Otherwise change \"port\" in IkemenParty_config.json.")
		if !*companion { // the mod may start us while one is already open: that's fine
			waitForEnter()
		}
		return
	}
	srv := &http.Server{Handler: newMux(sess, web), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	app.Start()

	go func() {
		t := time.NewTicker(250 * time.Millisecond)
		for range t.C {
			pads.ReleaseIdle(time.Duration(cfg.IdleReleaseSeconds * float64(time.Second)))
		}
	}()

	hostURL := fmt.Sprintf("http://localhost:%d/host", cfg.Port)
	mode := strings.ToUpper(cfg.PadType)
	if pads.DryRun {
		mode += ", DRY RUN - guests can watch but not play"
	}
	fmt.Println()
	fmt.Println("  Ikemen Party is running. Keep this window open.")
	fmt.Printf("  1) Host page (this PC):   %s\n", hostURL)
	for _, ip := range localIPs() {
		fmt.Printf("  2) Friends on your Wi-Fi: http://%s:%d/\n", ip, cfg.Port)
	}
	fmt.Println("     Friends over the internet: share your tunnel link (see README).")
	fmt.Printf("  Controller slots: %d (%s)\n", pads.Len(), mode)
	if cfg.LobbyURL != "" {
		fmt.Printf("  Online Lobby: %s\n", cfg.LobbyURL)
	} else {
		fmt.Println("  Online Lobby: not set (add lobby_url to IkemenParty_config.json)")
	}
	if ikemenDir != "" {
		fmt.Printf("  Ikemen GO folder: %s (Online Lobby mod enabled)\n", ikemenDir)
	} else {
		fmt.Println("  Ikemen GO folder: not found. Put this app next to Ikemen_GO.exe for the in-game lobby.")
	}
	fmt.Println("  Close this window or press Ctrl+C to stop.")
	fmt.Println()
	if !*noBrowser && !*companion {
		openBrowser(hostURL)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	fmt.Println("\n  Stopping. Unplugging virtual controllers...")
	app.Shutdown()
	_ = srv.Close()
}

func waitForEnter() {
	fmt.Print("  Press Enter to close.")
	var b [1]byte
	_, _ = os.Stdin.Read(b[:])
}
