// Package lobbyapi holds the Online Lobby wire format and a small client,
// shared by Ikemen Party and the lobby server.
package lobbyapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	ModeStream  = "stream"
	ModeNetplay = "netplay"

	// A session disappears if it misses heartbeats for this long.
	SessionTTL        = 45 * time.Second
	HeartbeatInterval = 15 * time.Second
	MaxThumbBytes     = 100 << 10
)

// Counts mirrors the session counts shown on every lobby card.
type Counts struct {
	Watching       int `json:"watching"`
	Playing        int `json:"playing"`
	PlayerCapacity int `json:"player_capacity"`
	InLobby        int `json:"in_lobby"`
}

// SessionInfo is what a host publishes.
type SessionInfo struct {
	Name         string `json:"name"`
	Game         string `json:"game"`
	HostName     string `json:"host_name"`
	Mode         string `json:"mode"`        // stream | netplay
	Fingerprint  string `json:"fingerprint"` // install version tag
	Status       string `json:"status"`      // open | live | in_match | full
	Locked       bool   `json:"locked"`
	Counts       Counts `json:"counts"`
	JoinURL      string `json:"join_url,omitempty"`      // stream: public https link
	NetplayPort  int    `json:"netplay_port,omitempty"`  // netplay: TCP port
	RollbackPort int    `json:"rollback_port,omitempty"` // netplay: UDP port (rollback)
	Rollback     bool   `json:"rollback,omitempty"`      // netplay: rollback netcode on
	AppVersion   string `json:"app_version"`
}

// Session is a listing entry as returned by the server.
type Session struct {
	ID string `json:"id"`
	SessionInfo
	HasThumb   bool  `json:"has_thumb"`
	AgeSeconds int64 `json:"age_seconds"`
}

type RegisterResponse struct {
	ID       string `json:"id"`
	Secret   string `json:"secret"`
	PublicIP string `json:"public_ip"`
}

type ListResponse struct {
	Sessions []Session `json:"sessions"`
	Server   string    `json:"server"`
}

// JoinResponse gives the address needed to join (netplay) or watch (stream).
type JoinResponse struct {
	Mode         string `json:"mode"`
	JoinURL      string `json:"join_url,omitempty"`
	Address      string `json:"address,omitempty"`
	NetplayPort  int    `json:"netplay_port,omitempty"`
	RollbackPort int    `json:"rollback_port,omitempty"`
}

// Validate cleans a SessionInfo in place and reports problems.
func (s *SessionInfo) Validate() error {
	clip := func(v string, n int) string {
		v = strings.Map(func(r rune) rune {
			if r < 32 || r == 127 {
				return -1
			}
			return r
		}, v)
		v = strings.TrimSpace(v)
		if r := []rune(v); len(r) > n {
			v = string(r[:n])
		}
		return v
	}
	s.Name = clip(s.Name, 48)
	s.Game = clip(s.Game, 48)
	s.HostName = clip(s.HostName, 24)
	s.Fingerprint = clip(s.Fingerprint, 64)
	s.Status = clip(s.Status, 16)
	s.AppVersion = clip(s.AppVersion, 16)
	s.JoinURL = clip(s.JoinURL, 200)
	if s.Name == "" {
		return errors.New("name is required")
	}
	switch s.Mode {
	case ModeStream:
		if !strings.HasPrefix(s.JoinURL, "https://") && !strings.HasPrefix(s.JoinURL, "http://") {
			return errors.New("stream sessions need a join_url")
		}
	case ModeNetplay:
		if s.NetplayPort <= 0 || s.NetplayPort > 65535 {
			return errors.New("netplay sessions need a netplay_port")
		}
		if s.RollbackPort < 0 || s.RollbackPort > 65535 {
			return errors.New("bad rollback_port")
		}
		s.JoinURL = ""
	default:
		return errors.New("mode must be stream or netplay")
	}
	return nil
}

// ------------------------------------------------------------------ client --

type Client struct {
	Base string
	HTTP *http.Client
}

func NewClient(base string) *Client {
	return &Client{Base: strings.TrimRight(base, "/"), HTTP: &http.Client{Timeout: 10 * time.Second}}
}

func (c *Client) do(ctx context.Context, method, path, secret string, body any, out any) error {
	var rd io.Reader
	ctype := "application/json"
	switch b := body.(type) {
	case nil:
	case []byte:
		rd, ctype = bytes.NewReader(b), "image/jpeg"
	default:
		buf, err := json.Marshal(b)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rd)
	if err != nil {
		return err
	}
	if rd != nil {
		req.Header.Set("Content-Type", ctype)
	}
	if secret != "" {
		req.Header.Set("X-Session-Secret", secret)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return &HTTPError{resp.StatusCode, e.Error}
		}
		return &HTTPError{resp.StatusCode, http.StatusText(resp.StatusCode)}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

type HTTPError struct {
	Code int
	Msg  string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("lobby: %s (%d)", e.Msg, e.Code) }

func (c *Client) List(ctx context.Context) (*ListResponse, error) {
	var r ListResponse
	return &r, c.do(ctx, "GET", "/api/v1/sessions", "", nil, &r)
}

func (c *Client) Register(ctx context.Context, s SessionInfo) (*RegisterResponse, error) {
	var r RegisterResponse
	return &r, c.do(ctx, "POST", "/api/v1/sessions", "", s, &r)
}

func (c *Client) Update(ctx context.Context, id, secret string, s SessionInfo) error {
	return c.do(ctx, "PUT", "/api/v1/sessions/"+id, secret, s, nil)
}

func (c *Client) Unregister(ctx context.Context, id, secret string) error {
	return c.do(ctx, "DELETE", "/api/v1/sessions/"+id, secret, nil, nil)
}

func (c *Client) PutThumb(ctx context.Context, id, secret string, jpeg []byte) error {
	return c.do(ctx, "PUT", "/api/v1/sessions/"+id+"/thumb", secret, jpeg, nil)
}

func (c *Client) Join(ctx context.Context, id string) (*JoinResponse, error) {
	var r JoinResponse
	return &r, c.do(ctx, "POST", "/api/v1/sessions/"+id+"/join", "", struct{}{}, &r)
}
