package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"testing"
	"time"
)

func testMapGLB(document string) (string, []byte) {
	for len(document)%4 != 0 {
		document += " "
	}
	content := make([]byte, 20+len(document))
	copy(content, "glTF")
	binary.LittleEndian.PutUint32(content[4:], 2)
	binary.LittleEndian.PutUint32(content[8:], uint32(len(content)))
	binary.LittleEndian.PutUint32(content[12:], uint32(len(document)))
	copy(content[16:], "JSON")
	copy(content[20:], document)
	return fmt.Sprintf("%x", sha256.Sum256(content)), content
}

func TestPilotMapAssetIntegrityAndLocalContent(t *testing.T) {
	hash, content := testMapGLB(`{"asset":{"version":"2.0"}}`)
	if err := ValidatePilotMapAsset(hash, content); err != nil {
		t.Fatal(err)
	}
	broken := bytes.Clone(content)
	broken[4] = 1
	if ValidatePilotMapAsset(hash, broken) == nil {
		t.Fatal("modified GLB accepted")
	}
	hash, content = testMapGLB(`{"images":[{"uri":"https://outside/image.png"}]}`)
	if ValidatePilotMapAsset(hash, content) == nil {
		t.Fatal("external dependency accepted")
	}
	hash, content = testMapGLB(`{"buffers":[]}`)
	content = content[:len(content)-1]
	if ValidatePilotMapAsset(hash, content) == nil {
		t.Fatal("truncated GLB accepted")
	}
}

func TestPilotMapAssetCacheProjectionAndHistory(t *testing.T) {
	st := openMigratedStore(t)
	p, _ := st.CreateProject("owner", "map")
	other, _ := st.CreateProject("owner", "empty")
	hash, content := testMapGLB(`{"asset":{"version":"2.0"}}`)
	descriptor := map[string]any{"visual_id": "table", "version": "1.0.0", "map_visual": map[string]any{"format": "glb", "sha256": hash}}
	snapshot := PilotMapSnapshot{SchemaVersion: "1.1.0", FrameID: "map", Units: "m", MapHash: "map", MappingRevision: "rev",
		VisualAssets: []PilotMapAsset{{SHA256: hash, Bytes: len(content), Format: "glb"}},
		Entities:     []MapSourceEntity{{SourceID: "table", Entity: MapEntity{Type: "table", Name: "table", FrameID: "map", Pose: Pose{Orientation: Quaternion{W: 1}}, Bounds: Bounds{Kind: "point"}, Properties: map[string]any{"visual_descriptor": descriptor}}}}}
	if _, err := st.SavePilotMapSource("pilot", "robot", snapshot, time.Now()); err == nil {
		t.Fatal("missing asset accepted")
	}
	for i := 0; i < 2; i++ {
		if err := st.PutPilotMapAsset("pilot", hash, content); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.PilotMapAssetSize("other", hash); err != ErrNotFound {
		t.Fatal("other pilot can claim cached asset")
	}
	source, err := st.SavePilotMapSource("pilot", "robot", snapshot, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	first, err := st.ApplyPilotMapSource(p.ID, source, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if descriptor["map_visual"].(map[string]any)["content_url"] != nil {
		t.Fatal("projection mutated source")
	}
	second, err := st.ApplyPilotMapSource(p.ID, source, time.Now())
	if err != nil || second.Map.Revision != first.Map.Revision {
		t.Fatal("not idempotent", err)
	}
	got, err := st.ProjectMapAsset(p.ID, hash)
	if err != nil || !bytes.Equal(content, got) {
		t.Fatal("asset missing", err)
	}
	if _, err := st.ProjectMapAsset(other.ID, hash); err != ErrNotFound {
		t.Fatal("unrelated project can read asset")
	}
	source.Snapshot.MapHash = "replacement"
	source.Snapshot.VisualAssets = nil
	source.Snapshot.Entities = nil
	if _, err := st.ApplyPilotMapSource(p.ID, source, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ProjectMapAsset(p.ID, hash); err != nil {
		t.Fatal("historical asset lost", err)
	}
}
