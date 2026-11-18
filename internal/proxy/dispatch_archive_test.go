package proxy

import (
	"encoding/json"
	"testing"
)

func TestArchiveRecoveryRequiresTheAuthorizedNativeFailure(t *testing.T) {
	t.Parallel()
	const id = "thread-a"
	const command = "command-a"
	for _, code := range []string{"runtime_mode_escalation_denied", "interaction_mode_escalation_denied",
		"capability_denied", "orchestration_error"} {
		payload, err := json.Marshal(map[string]string{
			"_tag": "OrchestratorMcpFailure", "code": code, "message": alreadyArchivedMessage(id),
		})
		if err != nil {
			t.Fatal(err)
		}
		peer := map[string]any{"result": map[string]any{"isError": true, "content": []any{
			map[string]any{"type": "text", "text": string(payload)},
		}}}
		if got := peerAlreadyArchived(peer, id); got != (code == "orchestration_error") {
			t.Errorf("peer cleanup authorization for %q = %t", code, got)
		}
		if peerAlreadyArchived(peer, "another-thread") {
			t.Errorf("peer failure for %q authorized a different worker", code)
		}
	}

	failure := map[string]any{
		"_tag": "OrchestrationV2DispatchCommandError", "commandType": "thread.archive",
		"commandId": command, "detail": alreadyArchivedMessage(id),
	}
	exit := map[string]any{"_tag": "Failure", "cause": []any{
		map[string]any{"_tag": "Fail", "error": failure},
	}}
	if !desktopAlreadyArchived(exit, id, command) || desktopAlreadyArchived(exit, id, "another-command") {
		t.Fatal("desktop recovery did not match the exact command")
	}
	failure["_tag"] = "EnvironmentAuthorizationError"
	if desktopAlreadyArchived(exit, id, command) {
		t.Fatal("desktop authorization failure allowed cleanup")
	}
}
