package namespace

import (
	"net/http"
	"strings"
	"testing"

	"github.com/flemzord/t3fleet/pkg/provider"
)

func TestCreateUsesReservedSite(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, servicePath) {
		case "Fetch":
			w.WriteHeader(http.StatusNotFound)
		case "Create":
			input := body(t, r)
			if input["site"] != "sfo" {
				t.Errorf("wrong site: %v", input["site"])
			}
			reply(t, w, map[string]any{"devbox": map[string]any{"id": "devbox-id", "name": input["name"]}, "instanceId": "instance-1"})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}, nil)
	c.config.Site = "sfo"
	saved := c.DefaultPlacement()
	c.config.Site = "iad"
	if _, err := c.Create(t.Context(), provider.CreateRequest{Name: "fleet-test", OperationID: "op", Template: provider.Template{Image: "registered:org/image"}, Placement: &saved}); err != nil {
		t.Fatal(err)
	}
}
