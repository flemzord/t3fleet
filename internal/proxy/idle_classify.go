package proxy

import (
	"encoding/json"
	"strings"
)

// Explicit protocol-2 background shapes, studied from t3boat's classify.rs.
// Unknown requests are useful work: a newer protocol cannot silently permit sleep.
func passiveRPC(v map[string]any) bool {
	tag, _ := v["tag"].(string)
	payload, _ := v["payload"].(map[string]any)
	switch tag {
	case "server.probe", "server.getConfig", "server.reportClientActivity", "preview.reportProfiles", "preview.reportStatus",
		"subscribeServerConfig", "subscribeServerLifecycle", "subscribeAuthAccess", "subscribeResourceTelemetry",
		"subscribeProjectClones", "subscribeDeviceState", "subscribeVcsStatus", "subscribeVcsBranch",
		"subscribeWorktreeSetup", "subscribeTerminalMetadata", "subscribeDiscoveredLocalServers", "subscribePreviewEvents",
		"orchestration.subscribeShell", "orchestration.subscribeThread", "orchestration.subscribePromptSuggestion", "provider.install.subscribe",
		"server.discoverSourceControl", "agentSessions.scan", "vcs.refreshStatus", "preview.list",
		"pullRequests.subscribeRefreshes", "pullRequests.subscribeLinkedThreadRefreshes", "pullRequests.summary", "pullRequests.checks",
		"orchestration.getThreadLinkMetadata", "orchestration.getThreadTitles", "orchestration.getArchivedShellSnapshot",
		"server.getHostResources", "server.getUsageSummary", "server.refreshUsageRates", "server.retryResourceTelemetry",
		"peerLinks.list", "cloud.getRelayClientStatus", "scheduledTasks.subscribe", "sourceControl.lookupRepository",
		"assets.createUrl", "filesystem.getMetadata":
		return true
	case "device.list":
		return payload["retryHostId"] == nil && payload["updateTool"] == nil
	case "orchestration.dispatchCommand":
		return payload["type"] == "thread.visit" || payload["type"] == "thread.mark-unread"
	case "server.refreshProviders":
		return payload["reloadSecrets"] != true
	case "server.searchAcpRegistry":
		return payload["query"] == ""
	case "orchestration.getOlderSettledThreads":
		return rpcNumber(payload["limit"], 0)
	case "vcs.listRefs":
		return payload["cursor"] == nil && (payload["query"] == nil || rpcNumber(payload["limit"], 10))
	case "review.getDiffPreview":
		return payload["metadataOnly"] == true && payload["sourceKind"] == "branch-range"
	default:
		return false
	}
}
func passiveHTTP(path string) bool {
	return mediaPath(path) || path == "/api/orchestration/shell" || strings.HasPrefix(path, "/api/orchestration/threads/") && strings.HasSuffix(path, "/bounded") || path == "/api/telemetry/traces"
}

func rpcNumber(value any, want float64) bool {
	switch v := value.(type) {
	case float64:
		return v == want
	case json.Number:
		n, err := v.Float64()
		return err == nil && n == want
	default:
		return false
	}
}
