package robot

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"insightos.cn/semantic-framework/internal/store"
)

func TestPilotConnectionProbeDetectsSilentPeer(t *testing.T) {
	for _, responsive := range []bool{false, true} {
		t.Run(map[bool]string{false: "no_pong", true: "healthy"}[responsive], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				go watchPilotConnection(ctx, conn, 5*time.Millisecond, 100*time.Millisecond, cancel)
				_, _, _ = conn.Read(ctx)
			}))
			defer server.Close()
			conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			if responsive {
				conn.CloseRead(ctx)
				// Keep processing control frames beyond the probe timeout.
				select {
				case <-ctx.Done():
					t.Fatal("responsive socket was disconnected")
				case <-time.After(150 * time.Millisecond):
				}
			} else {
				// No reader means no pong, like a silent half-open TCP connection.
				select {
				case <-ctx.Done():
				case <-time.After(3 * time.Second):
					t.Fatal("silent peer stayed online")
				}
			}
		})
	}
}

func TestDisconnectPreservesLastHeartbeat(t *testing.T) {
	st := openRobotTestStore(t)
	svc := NewService(st, nil)
	now := time.Date(2026, 10, 13, 15, 15, 36, 0, time.UTC)
	svc.now = func() time.Time { return now }
	pilot := store.RobotPilot{PilotInstanceID: "pilot-heartbeat-time", RobotID: "robot-heartbeat-time", RobotStatus: "idle"}
	_, disconnect, err := svc.Connect(pilot)
	if err != nil {
		t.Fatal(err)
	}
	last := now
	now = now.Add(9 * time.Minute)
	disconnect()
	saved, err := st.GetRobotPilot(pilot.PilotInstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != "offline" || !saved.LastSeenAt.Equal(last) {
		t.Fatalf("disconnect fabricated heartbeat: %+v", saved)
	}
}
