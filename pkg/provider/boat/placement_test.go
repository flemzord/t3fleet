package boat

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/flemzord/t3fleet/pkg/provider"
)

func TestCreateUsesReservedEnvironmentEvenWhenEmpty(t *testing.T) {
	for _, oldEnv := range []string{"", "previous-environment"} {
		t.Run("reserved-"+oldEnv, func(t *testing.T) {
			c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					respond(t, w, map[string]any{"ok": true, "sandboxes": []any{}})
					return
				}
				var body struct {
					Environment string `json:"environment"`
					NoEnv       bool   `json:"noEnv"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.Environment != oldEnv || body.NoEnv != (oldEnv == "") {
					t.Errorf("create used current environment instead of reserved environment: %+v", body)
				}
				respond(t, w, map[string]any{"ok": true, "sandbox": sandbox{ID: "bx_test", State: "provisioning"}})
			})
			c.environment = oldEnv
			saved := c.DefaultPlacement()
			c.environment = "different-with-private-credentials"
			_, err := c.Create(t.Context(), provider.CreateRequest{OperationID: "op", Template: provider.Template{Snapshot: "golden"}, Placement: &saved})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
