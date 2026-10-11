package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
)

type Member struct {
	ID        string
	Name      string
	Role      string // "host" | "guest"
	Slot      *int
	Wants     bool
	ws        *wsConn
	lastSeq   int64
	chatTimes []time.Time
}

type memberJSON struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Role  string `json:"role"`
	Slot  *int   `json:"slot"`
	Wants bool   `json:"wants"`
}

func (m *Member) public() memberJSON {
	return memberJSON{m.ID, m.Name, m.Role, m.Slot, m.Wants}
}

type Session struct {
	mu          sync.Mutex
	cfg         *Config
	pads        *PadBank
	members     map[string]*Member
	order       []string // join order
	host        *Member
	locked      bool
	sharing     bool
	thumb       []byte
	sessionName string
	gameName    string
	started     time.Time
	app         *App
}

func NewSession(cfg *Config, pads *PadBank) *Session {
	return &Session{
		cfg: cfg, pads: pads, members: map[string]*Member{},
		sessionName: cfg.SessionName, gameName: cfg.GameName, started: time.Now(),
	}
}

func newID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func cleanText(s string, limit int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > limit {
		s = strings.TrimSpace(string(r[:limit]))
	}
	return s
}

// ------------------------------------------------------------- read views --
// All methods ending in "L" expect s.mu to be held.

func (s *Session) guestsL() []*Member {
	var g []*Member
	for _, id := range s.order {
		if m := s.members[id]; m != nil && m.Role == "guest" {
			g = append(g, m)
		}
	}
	return g
}

type Counts struct {
	Watching       int `json:"watching"`
	Playing        int `json:"playing"`
	PlayerCapacity int `json:"player_capacity"`
	InLobby        int `json:"in_lobby"`
	MaxViewers     int `json:"max_viewers"`
}

func (s *Session) countsL() Counts {
	g := s.guestsL()
	playing := 0
	for _, m := range g {
		if m.Slot != nil {
			playing++
		}
	}
	hostN := 0
	if s.host != nil {
		hostN = 1
	}
	return Counts{
		Watching: len(g) - playing, Playing: playing + hostN,
		PlayerCapacity: s.pads.Len() + 1, InLobby: len(g) + hostN,
		MaxViewers: s.cfg.MaxViewers,
	}
}

type Info struct {
	Version     string     `json:"version"`
	SessionName string     `json:"session_name"`
	GameName    string     `json:"game_name"`
	HostOnline  bool       `json:"host_online"`
	Sharing     bool       `json:"sharing"`
	Locked      bool       `json:"locked"`
	Slots       int        `json:"slots"`
	Counts      Counts     `json:"counts"`
	Uptime      int        `json:"uptime"`
	Lobby       *LobbyView `json:"lobby,omitempty"`
}

func (s *Session) infoL() Info {
	inf := Info{
		Version: Version, SessionName: s.sessionName, GameName: s.gameName,
		HostOnline: s.host != nil, Sharing: s.sharing, Locked: s.locked,
		Slots: s.pads.Len(), Counts: s.countsL(), Uptime: int(time.Since(s.started).Seconds()),
	}
	if s.app != nil {
		v := s.app.lobbyView()
		inf.Lobby = &v
	}
	return inf
}

// Snapshot is a copy of the public session state (without lobby details).
type Snapshot struct {
	HostOnline, Sharing, Locked bool
	SessionName, GameName       string
	Counts                      Counts
}

func (s *Session) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Snapshot{HostOnline: s.host != nil, Sharing: s.sharing, Locked: s.locked,
		SessionName: s.sessionName, GameName: s.gameName, Counts: s.countsL()}
}

func (s *Session) PushRoster() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pushRosterL()
}

func (s *Session) SetName(n string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessionName = n
	s.pushRosterL()
}

func (s *Session) Info() Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.infoL()
}

func (s *Session) Thumb() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.thumb
}

// ------------------------------------------------------------- broadcasts --

func (s *Session) broadcastL(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	frame := encodeFrame(0x1, b)
	for _, m := range s.members {
		m.ws.enqueue(frame)
	}
}

func (s *Session) pushRosterL() {
	list := []memberJSON{}
	for _, id := range s.order {
		if m := s.members[id]; m != nil {
			list = append(list, m.public())
		}
	}
	s.broadcastL(map[string]any{"t": "roster", "members": list, "info": s.infoL()})
}

func (s *Session) systemChatL(text string) {
	s.broadcastL(map[string]any{"t": "chat", "from": nil, "text": text, "ts": float64(time.Now().UnixMilli()) / 1000})
}

// ------------------------------------------------------------------ slots --

func (s *Session) slotHolderL(slot int) *Member {
	for _, m := range s.guestsL() {
		if m.Slot != nil && *m.Slot == slot {
			return m
		}
	}
	return nil
}

func (s *Session) setSlotL(m *Member, slot *int) {
	old := m.Slot
	if slot != nil {
		if *slot < 0 || *slot >= s.pads.Len() {
			return
		}
		if h := s.slotHolderL(*slot); h != nil && h != m {
			h.Slot = nil
			h.ws.Send(map[string]any{"t": "slot", "slot": nil})
		}
	}
	if old != nil && (slot == nil || *old != *slot) {
		s.pads.Neutral(*old)
	}
	m.Slot = slot
	if slot != nil {
		m.Wants = false
	}
	m.lastSeq = -1
	m.ws.Send(map[string]any{"t": "slot", "slot": slot})
	if slot != nil {
		s.systemChatL(fmt.Sprintf("%s is on Pad %d.", m.Name, *slot+1))
	} else if old != nil {
		s.systemChatL(m.Name + " is now watching.")
	}
}

func (s *Session) handleInputL(m *Member, raw []float64) {
	if m == nil || m.Slot == nil {
		return
	}
	seq, st, ok := parseState(raw)
	if !ok || seq <= m.lastSeq {
		return // invalid, stale or duplicate (inputs may arrive out of order)
	}
	m.lastSeq = seq
	s.pads.Apply(*m.Slot, st)
}

// ------------------------------------------------------------- lifecycle --

type inbound struct {
	T           string          `json:"t"`
	Role        string          `json:"role"`
	Name        string          `json:"name"`
	To          string          `json:"to"`
	ID          string          `json:"id"`
	Data        json.RawMessage `json:"data"`
	S           []float64       `json:"s"`
	Text        string          `json:"text"`
	On          bool            `json:"on"`
	Slot        *int            `json:"slot"`
	SessionName string          `json:"session_name"`
	GameName    string          `json:"game_name"`
	Sharing     bool            `json:"sharing"`
	JPEG        string          `json:"jpeg"`
}

// Serve runs one WebSocket connection until it closes.
func (s *Session) Serve(ws *wsConn, isLocal bool) {
	defer ws.Close()
	raw, err := ws.Recv()
	if err != nil {
		return
	}
	var hello inbound
	if json.Unmarshal(raw, &hello) != nil || hello.T != "hello" {
		return
	}
	role := "guest"
	if hello.Role == "host" {
		role = "host"
	}
	name := cleanText(hello.Name, 24)
	if name == "" {
		if role == "host" {
			name = "Host"
		} else {
			name = "Guest"
		}
	}

	s.mu.Lock()
	reject := ""
	switch {
	case role == "host" && !isLocal:
		reject = "The host page only works on the host PC."
	case role == "host" && s.host != nil:
		reject = "A host page is already open. Close it first."
	case role == "guest" && s.locked:
		reject = "This session is closed to new people."
	case role == "guest" && len(s.guestsL()) >= s.cfg.MaxViewers:
		reject = "This session is full."
	}
	if reject != "" {
		s.mu.Unlock()
		ws.SendAndClose(map[string]any{"t": "error", "msg": reject})
		time.Sleep(200 * time.Millisecond)
		return
	}
	me := &Member{ID: newID(), Name: name, Role: role, ws: ws, lastSeq: -1}
	s.members[me.ID] = me
	s.order = append(s.order, me.ID)
	ws.Send(map[string]any{"t": "welcome", "id": me.ID, "role": role, "config": s.cfg.public()})
	if role == "host" {
		s.host = me
		for _, g := range s.guestsL() {
			ws.Send(map[string]any{"t": "joined", "id": g.ID, "name": g.Name})
		}
	} else {
		if s.host != nil {
			s.host.ws.Send(map[string]any{"t": "joined", "id": me.ID, "name": me.Name})
		}
		s.systemChatL(me.Name + " joined.")
	}
	s.pushRosterL()
	s.mu.Unlock()

	defer s.remove(me)
	for {
		raw, err := ws.Recv()
		if err != nil {
			return
		}
		var msg inbound
		if json.Unmarshal(raw, &msg) != nil {
			continue
		}
		s.mu.Lock()
		s.dispatchL(me, &msg)
		s.mu.Unlock()
	}
}

func (s *Session) remove(m *Member) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.members[m.ID] == nil {
		return
	}
	delete(s.members, m.ID)
	for i, id := range s.order {
		if id == m.ID {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	if m.Slot != nil {
		s.pads.Neutral(*m.Slot)
	}
	if m.Role == "host" {
		s.host, s.sharing = nil, false
		s.systemChatL("The host disconnected.")
	} else {
		if s.host != nil {
			s.host.ws.Send(map[string]any{"t": "left", "id": m.ID})
		}
		s.systemChatL(m.Name + " left.")
	}
	s.pushRosterL()
}

func (s *Session) dispatchL(me *Member, msg *inbound) {
	isHost := me.Role == "host"
	switch msg.T {
	case "signal":
		// WebRTC setup is only relayed between the host and a guest.
		t := s.members[msg.To]
		if t != nil && t != me && (isHost || t.Role == "host") {
			t.ws.Send(map[string]any{"t": "signal", "from": me.ID, "data": msg.Data})
		}
		return
	case "in":
		// From the host page: input relayed off a guest's data channel.
		// From a guest: fallback path when the data channel isn't open.
		who := me
		if isHost {
			who = s.members[msg.ID]
		}
		if who != nil && who.Role == "guest" {
			s.handleInputL(who, msg.S)
		}
		return
	case "chat":
		text := cleanText(msg.Text, 300)
		now := time.Now()
		recent := me.chatTimes[:0]
		for _, t := range me.chatTimes {
			if now.Sub(t) < 5*time.Second {
				recent = append(recent, t)
			}
		}
		me.chatTimes = recent
		if text != "" && len(recent) < 5 {
			me.chatTimes = append(me.chatTimes, now)
			s.broadcastL(map[string]any{"t": "chat", "from": me.Name, "host": isHost, "text": text,
				"ts": float64(now.UnixMilli()) / 1000})
		}
		return
	case "want":
		if !isHost {
			me.Wants = msg.On
			if me.Wants && me.Slot == nil {
				s.systemChatL(me.Name + " wants to play.")
			}
			s.pushRosterL()
		}
		return
	}

	if !isHost {
		return // everything below is host-only
	}
	switch msg.T {
	case "assign":
		if t := s.members[msg.ID]; t != nil && t.Role == "guest" {
			s.setSlotL(t, msg.Slot)
			s.pushRosterL()
		}
	case "kick":
		if t := s.members[msg.ID]; t != nil && t.Role == "guest" {
			t.ws.SendAndClose(map[string]any{"t": "error", "msg": "The host removed you from the session."})
		}
	case "lock":
		s.locked = msg.On
		if s.locked {
			s.systemChatL("Session closed to new people.")
		} else {
			s.systemChatL("Session open to new people.")
		}
		s.pushRosterL()
	case "meta":
		if n := cleanText(msg.SessionName, 48); n != "" {
			s.sessionName = n
		}
		if n := cleanText(msg.GameName, 48); n != "" {
			s.gameName = n
		}
		s.sharing = msg.Sharing
		s.pushRosterL()
	case "public":
		if s.app != nil {
			s.app.SetStreamPublic(msg.On)
			s.pushRosterL()
		}
	case "thumb":
		const prefix = "data:image/jpeg;base64,"
		if strings.HasPrefix(msg.JPEG, prefix) {
			if b, err := base64.StdEncoding.DecodeString(msg.JPEG[len(prefix):]); err == nil {
				s.thumb = b
			}
		}
	}
}
