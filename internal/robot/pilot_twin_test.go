package robot

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"insightos.cn/semantic-framework/internal/store"
	"strings"
	"testing"
	"time"
)

func twinMap(t *testing.T, s *Service, pilot, project string) string {
	t.Helper()
	doc := []byte(`{"asset":{"version":"2.0"}}`)
	for len(doc)%4 != 0 {
		doc = append(doc, ' ')
	}
	b := make([]byte, 20+len(doc))
	copy(b, "glTF")
	binary.LittleEndian.PutUint32(b[4:], 2)
	binary.LittleEndian.PutUint32(b[8:], uint32(len(b)))
	binary.LittleEndian.PutUint32(b[12:], uint32(len(doc)))
	copy(b[16:], "JSON")
	copy(b[20:], doc)
	hash := sha256.Sum256(b)
	key := hex.EncodeToString(hash[:])
	if err := s.st.PutPilotMapAsset(pilot, key, b); err != nil {
		t.Fatal(err)
	}
	snapshot := mapFixture()
	snapshot.SchemaVersion = "1.1.0"
	snapshot.VisualAssets = []store.PilotMapAsset{{SHA256: key, Bytes: len(b), Format: "glb"}}
	snapshot.Mapping = map[string]any{"robot_visual": map[string]any{"urdf_sha256": strings.Repeat("a", 64), "map_visual": map[string]any{"sha256": key, "format": "glb"}}}
	if _, err := s.SyncPilotMap(pilot, project, snapshot); err != nil {
		t.Fatal(err)
	}
	return key
}

func TestTwinIsEphemeralBoundToPilotMapAndSession(t *testing.T) {
	st := openRobotTestStore(t)
	s := NewService(st, nil)
	p, _ := st.CreateProject("owner", "lab")
	close := enrollMapPilot(t, s, "owner", "pilot", "r1pro", p.ID)
	defer close()
	model := twinMap(t, s, "pilot", p.ID)
	now := time.Now()
	s.now = func() time.Time { return now }
	age := .02
	v := PilotTwinState{SchemaVersion: "1.0.0", Frame: "map", Units: "m_rad", StreamID: "boot-a", Sequence: 1, MapHash: "room", MappingRevision: "align", ModelSHA256: model, URDFSHA256: strings.Repeat("a", 64), Pose: []float64{1, 2, .168, 0}, PoseAge: &age, Joints: map[string]float64{"torso_joint1": .4}}
	before, _ := st.GetMapSnapshot(p.ID, store.MapSlotReal, 0)
	if err := s.SyncPilotTwin("pilot", v); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncPilotTwin("pilot", v); err == nil {
		t.Fatal("reordered/duplicate frame accepted")
	}
	now = now.Add(2 * time.Second)
	out, err := s.ProjectTwin("owner", p.ID)
	if err != nil || out["online"] != true || out["age_s"] != float64(2) {
		t.Fatalf("%+v %v", out, err)
	}
	after, _ := st.GetMapSnapshot(p.ID, store.MapSlotReal, 0)
	if before.Map.Revision != after.Map.Revision {
		t.Fatal("telemetry mutated static map")
	}
	if _, err = st.ProjectMapAsset(p.ID, model); err != nil {
		t.Fatal("robot model asset not readable", err)
	}
	if _, err = s.ProjectTwin("foreign", p.ID); err == nil {
		t.Fatal("cross-owner telemetry")
	}
	v.Sequence++
	v.MapHash = "other-room"
	if err = s.SyncPilotTwin("pilot", v); err == nil {
		t.Fatal("wrong coordinate map accepted")
	}
	close()
	out, err = s.ProjectTwin("owner", p.ID)
	if err != nil || out["online"] != false {
		t.Fatal("disconnected pilot is live", out, err)
	}
	fresh := NewService(st, nil)
	out, err = fresh.ProjectTwin("owner", p.ID)
	if err != nil || out["available"] != false {
		t.Fatal("reboot revived old telemetry")
	}
	if _, err = s.SelectMapSource("owner", p.ID, ""); err != nil {
		t.Fatal(err)
	}
	out, err = s.ProjectTwin("owner", p.ID)
	if err != nil || out["available"] != false {
		t.Fatal("unsubscribed pilot leaked")
	}
}

func TestTwinValidation(t *testing.T) {
	age := .1
	v := PilotTwinState{SchemaVersion: "1.0.0", Frame: "map", Units: "m_rad", StreamID: "a", Sequence: 1, ModelSHA256: strings.Repeat("a", 64), URDFSHA256: strings.Repeat("b", 64), Pose: []float64{0, 0, 0, 0}, PoseAge: &age}
	if !validTwin(v) {
		t.Fatal("valid frame rejected")
	}
	v.PoseAge = nil
	if validTwin(v) {
		t.Fatal("undated pose accepted")
	}
	v.Pose = nil
	if !validTwin(v) {
		t.Fatal("missing feedback must be publishable")
	}
}
