package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ikemenparty/lobbyapi"
)

func TestLobbyLifecycle(t *testing.T) {
	s := newServer(false)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	c := lobbyapi.NewClient(ts.URL)
	ctx := context.Background()

	stream := lobbyapi.SessionInfo{Name: "Ryu's room", Game: "Ikemen GO", HostName: "Ryu", Mode: "stream",
		Fingerprint: "abc", Status: "live", JoinURL: "https://x.trycloudflare.com/",
		Counts: lobbyapi.Counts{Watching: 2, Playing: 1, PlayerCapacity: 5, InLobby: 3}}
	r1, err := c.Register(ctx, stream)
	if err != nil {
		t.Fatal(err)
	}
	if r1.PublicIP != "127.0.0.1" {
		t.Fatalf("public ip %q", r1.PublicIP)
	}
	net := lobbyapi.SessionInfo{Name: "Ken 1v1", HostName: "Ken", Mode: "netplay", NetplayPort: 7500,
		RollbackPort: 7550, Rollback: true, Status: "open"}
	r2, err := c.Register(ctx, net)
	if err != nil {
		t.Fatal(err)
	}

	list, err := c.List(ctx)
	if err != nil || len(list.Sessions) != 2 {
		t.Fatalf("list: %v %+v", err, list)
	}
	for _, s := range list.Sessions {
		if s.JoinURL != "" || (s.Mode == "netplay" && s.NetplayPort != 7500) {
			t.Fatalf("listing leaks/loses data: %+v", s)
		}
	}

	j, err := c.Join(ctx, r2.ID)
	if err != nil || j.Address != "127.0.0.1" || j.NetplayPort != 7500 || j.RollbackPort != 7550 {
		t.Fatalf("netplay join: %v %+v", err, j)
	}
	j, err = c.Join(ctx, r1.ID)
	if err != nil || j.JoinURL != stream.JoinURL || j.Address != "" {
		t.Fatalf("stream join: %v %+v", err, j)
	}

	// Wrong secret is rejected; right one updates.
	stream.Counts.Watching = 9
	if err := c.Update(ctx, r1.ID, "nope", stream); err == nil {
		t.Fatal("bad secret accepted")
	}
	if err := c.Update(ctx, r1.ID, r1.Secret, stream); err != nil {
		t.Fatal(err)
	}
	if err := c.PutThumb(ctx, r1.ID, r1.Secret, []byte{0xFF, 0xD8, 0xFF, 0xE0, 1, 2}); err != nil {
		t.Fatal(err)
	}
	if err := c.PutThumb(ctx, r1.ID, r1.Secret, []byte("<svg>")); err == nil {
		t.Fatal("non-jpeg accepted")
	}

	// Validation
	if _, err := c.Register(ctx, lobbyapi.SessionInfo{Name: "x", Mode: "stream"}); err == nil {
		t.Fatal("stream without url accepted")
	}
	// Per-IP cap (2 registered already, 3rd ok, 4th refused)
	if _, err := c.Register(ctx, net); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Register(ctx, net); err == nil {
		t.Fatal("per-ip cap not enforced")
	}

	// Expiry
	s.mu.Lock()
	s.sessions[r2.ID].lastSeen = time.Now().Add(-time.Minute)
	s.mu.Unlock()
	list, _ = c.List(ctx)
	for _, x := range list.Sessions {
		if x.ID == r2.ID {
			t.Fatal("stale session not expired")
		}
	}

	if err := c.Unregister(ctx, r1.ID, r1.Secret); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Join(ctx, r1.ID); err == nil {
		t.Fatal("joined a removed session")
	}

	// HTML page escapes names
	if _, err := c.Register(ctx, lobbyapi.SessionInfo{Name: "<script>x</script>", Mode: "stream", JoinURL: "https://a"}); err != nil {
		// per-ip cap may refuse; free a slot first
		t.Log("skipped html check:", err)
	} else {
		resp, _ := ts.Client().Get(ts.URL + "/")
		buf := new(strings.Builder)
		_, _ = ioCopy(buf, resp)
		if strings.Contains(buf.String(), "<script>x</script>") {
			t.Fatal("html not escaped")
		}
	}
}
