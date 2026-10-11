package main

// Online Lobby glue: publishes this PC's session to the lobby server, keeps
// the session list fresh for the Ikemen mod, and runs the file "bridge"
// the mod uses to talk to Ikemen Party (Ikemen's Lua has no networking).

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"ikemenparty/lobbyapi"
)

type NetplayHost struct {
	Name         string
	Port         int
	RollbackPort int
	Rollback     bool
	Status       string // open | in_match
}

type JoinResult struct {
	CmdID        float64 `json:"cmd_id"`
	Done         bool    `json:"done"`
	OK           bool    `json:"ok"`
	Error        string  `json:"error,omitempty"`
	Mode         string  `json:"mode,omitempty"`
	Address      string  `json:"address,omitempty"`
	NetplayPort  int     `json:"netplay_port,omitempty"`
	RollbackPort int     `json:"rollback_port,omitempty"`
}

type App struct {
	cfg       *Config
	sess      *Session
	tunnel    *Tunnel
	lobby     *lobbyapi.Client
	ikemenDir string
	hostURL   string
	companion bool

	mu           sync.Mutex
	fingerprint  string
	hostMode     string // "" | stream | netplay
	streamPublic bool
	netplay      NetplayHost
	// publishing
	pubID, pubSecret, publicIP string
	pubMode                    string
	pubLast                    lobbyapi.SessionInfo
	pubSentAt, pubRetryAt      time.Time
	thumbSum                   [32]byte
	thumbSentAt                time.Time
	lobbyState                 string
	// browsing
	list     []lobbyapi.Session
	listErr  string
	listAt   time.Time
	pingAt   time.Time
	listKick chan struct{}
	// bridge
	lastCmd float64
	join    JoinResult
	kick    chan struct{} // write status now
}

func NewApp(cfg *Config, sess *Session, ikemenDir string, companion bool) *App {
	a := &App{
		cfg: cfg, sess: sess, ikemenDir: ikemenDir, companion: companion,
		tunnel:   NewTunnel(cfg.Port, exeDir()),
		hostURL:  fmt.Sprintf("http://localhost:%d/host", cfg.Port),
		kick:     make(chan struct{}, 1),
		listKick: make(chan struct{}, 1),
	}
	if cfg.LobbyURL != "" {
		a.lobby = lobbyapi.NewClient(cfg.LobbyURL)
	}
	a.tunnel.onReady = func() { a.sess.PushRoster() }
	sess.app = a
	return a
}

func (a *App) Start() {
	if a.ikemenDir != "" {
		go func() {
			fp, err := computeFingerprint(a.ikemenDir)
			if err != nil {
				log.Printf("Version tag failed: %v", err)
				return
			}
			a.mu.Lock()
			a.fingerprint = fp
			a.mu.Unlock()
			log.Printf("Ikemen version tag: %s", fp)
			a.poke()
		}()
		go a.bridgeLoop()
	}
	go a.publishLoop()
	go a.browseLoop()
	if a.companion {
		go a.companionWatch()
	}
}

func (a *App) poke() {
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

// Shutdown removes our listing so it doesn't linger for 45 s.
func (a *App) Shutdown() {
	a.mu.Lock()
	id, secret := a.pubID, a.pubSecret
	a.pubID = ""
	a.mu.Unlock()
	if id != "" && a.lobby != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = a.lobby.Unregister(ctx, id, secret)
		cancel()
	}
	a.tunnel.Stop()
	if a.ikemenDir != "" {
		a.mu.Lock()
		st := a.statusL()
		a.mu.Unlock()
		st.Alive = 0
		a.writeStatus(st)
	}
}

// ---------------------------------------------------------- stream public --

// SetStreamPublic lists (or unlists) the stream session in the Online Lobby.
func (a *App) SetStreamPublic(on bool) {
	a.mu.Lock()
	a.streamPublic = on
	if on {
		a.hostMode = lobbyapi.ModeStream
	} else if a.hostMode == lobbyapi.ModeStream {
		a.hostMode = ""
	}
	a.mu.Unlock()
	if on && a.cfg.PublicURL == "" && a.cfg.Tunnel != "off" {
		a.tunnel.Start()
	}
	a.poke()
}

func (a *App) streamJoinURL() string {
	if a.cfg.PublicURL != "" {
		return a.cfg.PublicURL
	}
	if t := a.tunnel.State(); t.State == "ready" {
		return t.URL + "/"
	}
	return ""
}

// LobbyView is what the host page shows about the Online Lobby.
type LobbyView struct {
	Configured bool   `json:"configured"`
	Public     bool   `json:"public"`
	State      string `json:"state"`
	JoinURL    string `json:"join_url"`
	Tunnel     string `json:"tunnel"`
}

func (a *App) lobbyView() LobbyView {
	a.mu.Lock()
	v := LobbyView{Configured: a.lobby != nil, Public: a.streamPublic, State: a.lobbyState}
	a.mu.Unlock()
	v.JoinURL = a.streamJoinURL()
	t := a.tunnel.State()
	v.Tunnel = t.State
	if t.Error != "" {
		v.Tunnel += ": " + t.Error
	}
	return v
}

// -------------------------------------------------------------- publishing --

func (a *App) desiredListing() (*lobbyapi.SessionInfo, string) {
	a.mu.Lock()
	mode, public, np, fp := a.hostMode, a.streamPublic, a.netplay, a.fingerprint
	a.mu.Unlock()
	base := lobbyapi.SessionInfo{
		HostName: a.cfg.HostName, Fingerprint: fp, AppVersion: Version,
	}
	switch {
	case mode == lobbyapi.ModeNetplay:
		info := base
		info.Mode = lobbyapi.ModeNetplay
		info.Name = np.Name
		info.Game = a.cfg.GameName
		info.NetplayPort, info.RollbackPort, info.Rollback = np.Port, np.RollbackPort, np.Rollback
		info.Status = np.Status
		info.Counts = lobbyapi.Counts{Playing: 1, PlayerCapacity: 2, InLobby: 1}
		if np.Status == "in_match" {
			info.Counts.Playing, info.Counts.InLobby = 2, 2
		}
		if info.Name == "" {
			info.Name = a.cfg.SessionName
		}
		return &info, "listed"
	case mode == lobbyapi.ModeStream && public:
		snap := a.sess.Snapshot()
		if !snap.HostOnline {
			return nil, "waiting: open the host page"
		}
		if !snap.Sharing {
			return nil, "waiting: click Share Ikemen window"
		}
		url := a.streamJoinURL()
		if url == "" {
			t := a.tunnel.State()
			if t.State == "error" {
				return nil, "error: " + t.Error
			}
			return nil, "waiting: getting an internet link…"
		}
		info := base
		info.Mode = lobbyapi.ModeStream
		info.Name, info.Game = snap.SessionName, snap.GameName
		info.JoinURL = url
		info.Locked = snap.Locked
		info.Status = "live"
		c := snap.Counts
		info.Counts = lobbyapi.Counts{Watching: c.Watching, Playing: c.Playing, PlayerCapacity: c.PlayerCapacity, InLobby: c.InLobby}
		if c.InLobby-1 >= a.cfg.MaxViewers {
			info.Status = "full"
		}
		return &info, "listed"
	}
	return nil, "off"
}

func (a *App) setLobbyState(s string) {
	a.mu.Lock()
	changed := a.lobbyState != s
	a.lobbyState = s
	a.mu.Unlock()
	if changed {
		a.sess.PushRoster()
		a.poke()
	}
}

func (a *App) publishLoop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for range t.C {
		a.publishTick()
	}
}

func (a *App) publishTick() {
	want, state := a.desiredListing()
	a.mu.Lock()
	id, secret, mode := a.pubID, a.pubSecret, a.pubMode
	last, sentAt, retryAt := a.pubLast, a.pubSentAt, a.pubRetryAt
	a.mu.Unlock()

	if want != nil && a.lobby == nil {
		a.setLobbyState("error: no lobby server set (lobby_url in IkemenParty_config.json)")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	// Unlist when no longer wanted (or the mode changed).
	if id != "" && (want == nil || want.Mode != mode) {
		_ = a.lobby.Unregister(ctx, id, secret)
		a.mu.Lock()
		a.pubID, a.pubSecret, a.publicIP = "", "", ""
		a.mu.Unlock()
		log.Printf("Removed from the Online Lobby.")
		id = ""
	}
	if want == nil {
		a.setLobbyState(state)
		return
	}
	if err := want.Validate(); err != nil {
		a.setLobbyState("error: " + err.Error())
		return
	}
	now := time.Now()
	if id == "" {
		if now.Before(retryAt) {
			return
		}
		r, err := a.lobby.Register(ctx, *want)
		if err != nil {
			a.mu.Lock()
			a.pubRetryAt = now.Add(10 * time.Second)
			a.mu.Unlock()
			a.setLobbyState("error: " + friendlyNetErr(err))
			return
		}
		a.mu.Lock()
		a.pubID, a.pubSecret, a.publicIP, a.pubMode = r.ID, r.Secret, r.PublicIP, want.Mode
		a.pubLast, a.pubSentAt = *want, now
		a.thumbSum = [32]byte{}
		a.mu.Unlock()
		log.Printf("Listed in the Online Lobby as %q (%s).", want.Name, want.Mode)
		a.setLobbyState("listed")
		return
	}
	// Heartbeat: every 15 s, or sooner (max every 3 s) when something changed.
	changed := *want != last
	if now.Sub(sentAt) >= lobbyapi.HeartbeatInterval || (changed && now.Sub(sentAt) >= 3*time.Second) {
		err := a.lobby.Update(ctx, id, secret, *want)
		var he *lobbyapi.HTTPError
		if errors.As(err, &he) && he.Code == 404 {
			a.mu.Lock()
			a.pubID = "" // listing expired; re-register next tick
			a.mu.Unlock()
			return
		}
		if err != nil {
			a.setLobbyState("error: " + friendlyNetErr(err))
			return
		}
		a.mu.Lock()
		a.pubLast, a.pubSentAt = *want, now
		a.mu.Unlock()
		a.setLobbyState("listed")
	}
	// Thumbnail (stream only): at most every 15 s.
	if want.Mode == lobbyapi.ModeStream {
		if th := a.sess.Thumb(); th != nil {
			sum := sha256.Sum256(th)
			a.mu.Lock()
			due := sum != a.thumbSum && now.Sub(a.thumbSentAt) >= 15*time.Second
			a.mu.Unlock()
			if due && a.lobby.PutThumb(ctx, id, secret, th) == nil {
				a.mu.Lock()
				a.thumbSum, a.thumbSentAt = sum, now
				a.mu.Unlock()
			}
		}
	}
}

func friendlyNetErr(err error) string {
	var he *lobbyapi.HTTPError
	if errors.As(err, &he) {
		return he.Msg
	}
	return "can't reach the lobby server"
}

// ---------------------------------------------------------------- browsing --

func (a *App) browseLoop() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		a.mu.Lock()
		active := time.Since(a.pingAt) < 30*time.Second
		a.mu.Unlock()
		if active && a.lobby != nil {
			a.refreshList()
		}
		select {
		case <-t.C:
		case <-a.listKick:
		}
	}
}

func (a *App) refreshList() {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	r, err := a.lobby.List(ctx)
	a.mu.Lock()
	if err != nil {
		a.listErr = friendlyNetErr(err)
	} else {
		a.list, a.listErr = r.Sessions, ""
	}
	a.listAt = time.Now()
	a.mu.Unlock()
	a.poke()
}

// ------------------------------------------------------------------ bridge --

// Files in <Ikemen>/save/party/:
//   party_status.json  written here every second, read by the mod
//   party_cmd.json     written by the mod: {"id": n, "cmd": "...", ...}

type modSession struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Game        string          `json:"game"`
	HostName    string          `json:"host_name"`
	Mode        string          `json:"mode"`
	Status      string          `json:"status"`
	Locked      bool            `json:"locked"`
	Rollback    bool            `json:"rollback"`
	Match       bool            `json:"match"`
	Fingerprint string          `json:"fingerprint"`
	Counts      lobbyapi.Counts `json:"counts"`
	Mine        bool            `json:"mine"`
}

type modHosting struct {
	Mode     string `json:"mode"`
	State    string `json:"state"`
	JoinURL  string `json:"join_url"`
	PublicIP string `json:"public_ip"`
	Sharing  bool   `json:"sharing"`
	Watching int    `json:"watching"`
	Playing  int    `json:"playing"`
}

type modStatus struct {
	Alive        int64        `json:"alive"`
	AppVersion   string       `json:"app_version"`
	Fingerprint  string       `json:"fingerprint"`
	LobbyURL     string       `json:"lobby_url"`
	LobbyOK      bool         `json:"lobby_ok"`
	LobbyError   string       `json:"lobby_error"`
	ListAge      int64        `json:"list_age"`
	Sessions     []modSession `json:"sessions"`
	Hosting      modHosting   `json:"hosting"`
	Tunnel       TunnelState  `json:"tunnel"`
	LastCmd      float64      `json:"last_cmd"`
	Join         JoinResult   `json:"join"`
	PadSlots     int          `json:"pad_slots"`
	DryRun       bool         `json:"dry_run"`
	SessionCount int          `json:"session_count"`
}

func (a *App) statusL() modStatus {
	st := modStatus{
		Alive: time.Now().Unix(), AppVersion: Version, Fingerprint: a.fingerprint,
		LobbyURL: a.cfg.LobbyURL, LobbyOK: a.lobby != nil && a.listErr == "" && !a.listAt.IsZero(),
		LobbyError: a.listErr, LastCmd: a.lastCmd, Join: a.join,
		PadSlots: a.sess.pads.Len(), DryRun: a.sess.pads.DryRun, Sessions: []modSession{},
	}
	if a.lobby == nil {
		st.LobbyError = "No lobby server set. Add lobby_url to IkemenParty_config.json."
	}
	if !a.listAt.IsZero() {
		st.ListAge = int64(time.Since(a.listAt).Seconds())
	}
	for _, s := range a.list {
		st.Sessions = append(st.Sessions, modSession{
			ID: s.ID, Name: s.Name, Game: s.Game, HostName: s.HostName, Mode: s.Mode,
			Status: s.Status, Locked: s.Locked, Rollback: s.Rollback, Fingerprint: s.Fingerprint,
			Match: a.fingerprint != "" && s.Fingerprint == a.fingerprint, Counts: s.Counts,
			Mine: s.ID == a.pubID,
		})
	}
	st.SessionCount = len(st.Sessions)
	st.Hosting = modHosting{Mode: a.hostMode, State: a.lobbyState, PublicIP: a.publicIP}
	return st
}

func (a *App) bridgeDir() string { return filepath.Join(a.ikemenDir, "save", "party") }

func (a *App) writeStatus(st modStatus) {
	data, err := json.MarshalIndent(st, "", " ")
	if err != nil {
		return
	}
	dir := a.bridgeDir()
	tmp := filepath.Join(dir, "party_status.tmp")
	if os.WriteFile(tmp, data, 0o644) == nil {
		// May fail for a moment if Ikemen is reading it; next tick retries.
		_ = os.Rename(tmp, filepath.Join(dir, "party_status.json"))
	}
}

func (a *App) bridgeLoop() {
	if err := os.MkdirAll(a.bridgeDir(), 0o755); err != nil {
		log.Printf("Can't create %s: %v (the Ikemen mod won't see Ikemen Party)", a.bridgeDir(), err)
		return
	}
	// Ignore a command file left over from a previous run.
	if c, err := a.readCmd(); err == nil {
		a.lastCmd = c.ID
	}
	statusT := time.NewTicker(time.Second)
	cmdT := time.NewTicker(150 * time.Millisecond)
	for {
		select {
		case <-cmdT.C:
			if c, err := a.readCmd(); err == nil && c.ID != a.lastCmd {
				a.lastCmd = c.ID
				a.handleCmd(c)
				a.poke()
			}
			continue
		case <-statusT.C:
		case <-a.kick:
		}
		snap := a.sess.Snapshot()
		a.mu.Lock()
		st := a.statusL()
		a.mu.Unlock()
		st.Hosting.JoinURL = a.streamJoinURL()
		st.Hosting.Sharing = snap.Sharing
		st.Hosting.Watching = snap.Counts.Watching
		st.Hosting.Playing = snap.Counts.Playing
		st.Tunnel = a.tunnel.State()
		a.writeStatus(st)
	}
}

type modCmd struct {
	ID           float64 `json:"id"`
	Cmd          string  `json:"cmd"`
	SessionID    string  `json:"session_id"`
	Name         string  `json:"name"`
	Port         int     `json:"port"`
	RollbackPort int     `json:"rollback_port"`
	Rollback     bool    `json:"rollback"`
	Status       string  `json:"status"`
}

func (a *App) readCmd() (modCmd, error) {
	var c modCmd
	data, err := os.ReadFile(filepath.Join(a.bridgeDir(), "party_cmd.json"))
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(data, &c)
}

func (a *App) handleCmd(c modCmd) {
	a.mu.Lock()
	a.pingAt = time.Now()
	a.mu.Unlock()
	switch c.Cmd {
	case "ping":
	case "refresh":
		select {
		case a.listKick <- struct{}{}:
		default:
		}
	case "join":
		a.mu.Lock()
		a.join = JoinResult{CmdID: c.ID}
		a.mu.Unlock()
		go a.doJoin(c)
	case "host_stream":
		if n := cleanText(c.Name, 48); n != "" {
			a.sess.SetName(n)
		}
		a.SetStreamPublic(true)
		if !a.sess.Snapshot().HostOnline {
			openBrowser(a.hostURL)
		}
	case "open_host_page":
		openBrowser(a.hostURL)
	case "host_netplay":
		a.mu.Lock()
		a.streamPublic = false
		a.hostMode = lobbyapi.ModeNetplay
		a.netplay = NetplayHost{Name: cleanText(c.Name, 48), Port: c.Port, RollbackPort: c.RollbackPort,
			Rollback: c.Rollback, Status: "open"}
		a.mu.Unlock()
		log.Printf("Hosting a netplay session on port %d.", c.Port)
	case "netplay_status":
		a.mu.Lock()
		if a.hostMode == lobbyapi.ModeNetplay && (c.Status == "open" || c.Status == "in_match") {
			a.netplay.Status = c.Status
		}
		a.mu.Unlock()
	case "stop_hosting":
		a.mu.Lock()
		a.hostMode, a.streamPublic = "", false
		a.mu.Unlock()
		a.sess.PushRoster()
	}
}

func (a *App) doJoin(c modCmd) {
	res := JoinResult{CmdID: c.ID, Done: true}
	if a.lobby == nil {
		res.Error = "No lobby server set."
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		r, err := a.lobby.Join(ctx, c.SessionID)
		cancel()
		if err != nil {
			res.Error = friendlyNetErr(err)
		} else {
			res.OK, res.Mode = true, r.Mode
			res.Address, res.NetplayPort, res.RollbackPort = r.Address, r.NetplayPort, r.RollbackPort
			if r.Mode == lobbyapi.ModeStream {
				openBrowser(r.JoinURL)
			}
		}
	}
	a.mu.Lock()
	if a.join.CmdID == c.ID {
		a.join = res
	}
	a.mu.Unlock()
	a.poke()
}

// companionWatch closes Ikemen Party shortly after Ikemen GO closes, when
// the mod started it in the background.
func (a *App) companionWatch() {
	misses := 0
	for range time.Tick(10 * time.Second) {
		if ikemenRunning() {
			misses = 0
			continue
		}
		misses++
		if misses >= 2 {
			log.Printf("Ikemen GO closed. Shutting down.")
			a.Shutdown()
			os.Exit(0)
		}
	}
}
