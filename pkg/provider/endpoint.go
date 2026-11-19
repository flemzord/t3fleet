package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// NormalizeAPIEndpoint validates an origin before any credentials are loaded.
// Some APIs mount their RPCs under a path prefix. Namespace Connect does not.
func NormalizeAPIEndpoint(raw string, allowPath bool) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || strings.ContainsAny(raw, "?#") ||
		(!allowPath && u.EscapedPath() != "" && u.EscapedPath() != "/") || strings.HasSuffix(u.Host, ":") {
		return "", errors.New("invalid API endpoint; omit credentials, queries and fragments")
	}
	// url.Parse accepts unbracketed IPv6 authorities and bracketed names.
	// Neither is a valid address for an HTTP request.
	if strings.HasPrefix(u.Host, "[") {
		ip, err := netip.ParseAddr(u.Hostname())
		if err != nil || !ip.Is6() || ip.Zone() != "" {
			return "", errors.New("invalid IPv6 API endpoint")
		}
	} else if strings.Contains(u.Hostname(), ":") {
		return "", errors.New("IPv6 endpoint addresses require brackets")
	}
	u.Host = strings.ToLower(u.Host)
	switch u.Scheme {
	case "https":
	case "http":
		switch u.Hostname() {
		case "localhost", "127.0.0.1", "::1":
		default:
			return "", errors.New("API endpoint requires HTTPS outside loopback")
		}
	default:
		return "", errors.New("API endpoint requires HTTPS outside loopback")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n == 0 {
			return "", errors.New("invalid API endpoint port")
		}
		if (u.Scheme == "https" && n == 443) || (u.Scheme == "http" && n == 80) {
			u.Host = strings.TrimSuffix(u.Host, ":"+port)
		}
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// ValidAccountID accepts an opaque provider account ID suitable for an HTTP
// header or exact token-claim comparison.
func ValidAccountID(id string) bool {
	if id == "" {
		return false
	}
	for _, ch := range id {
		if ch < '!' || ch > '~' {
			return false
		}
	}
	return true
}

// ScopeIdentifier names the non-secret account scope in which an adapter
// interprets machine IDs. Credential values must never influence this ID.
type ScopeIdentifier interface{ ScopeID() string }

// ScopeFingerprint makes a durable identifier without writing account names,
// credential selectors or private endpoint paths into the allocation record.
func ScopeFingerprint(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}
