package fleet

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/flemzord/t3fleet/pkg/provider"
)

func closeEndpoint(endpoint provider.Endpoint) {
	if endpoint.Close != nil {
		_ = endpoint.Close()
	}
}
func endpointClient(endpoint provider.Endpoint) (*http.Client, string, error) {
	u, err := url.Parse(endpoint.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return nil, "", errors.New("fleet: invalid private endpoint")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if endpoint.DialContext != nil {
		transport.Proxy = nil
		transport.DialContext = endpoint.DialContext
	}
	transport.ResponseHeaderTimeout = 15 * time.Second
	return &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, strings.TrimRight(endpoint.URL, "/"), nil
}
func applyEndpoint(req *http.Request, endpoint provider.Endpoint, path string) {
	u, _ := url.Parse(endpoint.URL)
	u.Path = strings.TrimRight(u.Path, "/") + path
	req.URL = u
	req.Host = u.Host
	for key, values := range endpoint.Headers {
		req.Header[key] = values
	}
}
