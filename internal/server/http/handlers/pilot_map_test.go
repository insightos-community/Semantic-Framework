package handlers

import (
	"github.com/go-chi/chi/v5"
	"net/http"
	"testing"
)

func TestProjectMapSourceAuthorization(t *testing.T) {
	handler, st, service, _ := newRobotHandlerTest(t)
	r := handler.(*chi.Mux)
	h := NewRobotsHandler(service, st)
	r.Get("/api/v1/projects/{id}/maps/real_map/sources", h.HandleMapSources)
	r.Put("/api/v1/projects/{id}/maps/real_map/source", h.HandleSelectMapSource)
	project, _ := st.CreateProject("owner", "kitchen")
	base := "/api/v1/projects/" + project.ID + "/maps/real_map"
	code, _ := requestRobotJSON(t, r, http.MethodGet, base+"/sources", "foreign", nil)
	if code != 404 {
		t.Fatalf("foreign project visible: %d", code)
	}
	code, body := requestRobotJSON(t, r, http.MethodGet, base+"/sources", "owner", nil)
	if code != 200 || len(body["sources"].([]any)) != 0 {
		t.Fatalf("source list: %d %+v", code, body)
	}
	for _, payload := range []string{`{}`, `{"pilot_id":null}`, `{"pilot_id":"","url":"http://host"}`} {
		code, _ = requestRobotJSON(t, r, http.MethodPut, base+"/source", "owner", []byte(payload))
		if code != 400 {
			t.Fatalf("invalid body accepted: %d %s", code, payload)
		}
	}
	code, _ = requestRobotJSON(t, r, http.MethodPut, base+"/source", "owner", []byte(`{"pilot_id":""}`))
	if code != 200 {
		t.Fatalf("disconnect failed: %d", code)
	}
}
