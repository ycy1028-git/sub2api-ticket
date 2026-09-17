package repository

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

func TestTLSFingerprintHTTPSProxyFallsBackWithoutBypassingProxy(t *testing.T) {
	proxyURL, err := url.Parse("https://user:pass@proxy.example:8443")
	require.NoError(t, err)
	rt, err := buildUpstreamTransportWithTLSFingerprint(poolSettings{}, proxyURL, &tlsfingerprint.Profile{Name: "test"})
	require.NoError(t, err)
	transport, ok := rt.(*http.Transport)
	require.True(t, ok)
	require.NotNil(t, transport.Proxy)
	require.Nil(t, transport.DialTLSContext)
	req := &http.Request{URL: &url.URL{Scheme: "https", Host: "upstream.example"}}
	resolved, err := transport.Proxy(req)
	require.NoError(t, err)
	require.Equal(t, "https://user:pass@proxy.example:8443", resolved.String())
}

func TestTLSFingerprintHTTP2WhenProfileAdvertisesH2(t *testing.T) {
	rt, err := buildUpstreamTransportWithTLSFingerprint(poolSettings{}, nil, &tlsfingerprint.Profile{
		Name:          "codex rustls aws-lc-rs",
		ALPNProtocols: []string{"h2", "http/1.1"},
	})
	require.NoError(t, err)
	_, ok := rt.(*http2.Transport)
	require.True(t, ok, "h2 ALPN must use http2.Transport so chatgpt.com SETTINGS are not read as HTTP/1.1")
}

func TestTLSFingerprintHTTP1WhenProfileOmitsH2(t *testing.T) {
	rt, err := buildUpstreamTransportWithTLSFingerprint(poolSettings{}, nil, &tlsfingerprint.Profile{Name: "default"})
	require.NoError(t, err)
	transport, ok := rt.(*http.Transport)
	require.True(t, ok)
	require.False(t, transport.ForceAttemptHTTP2)
	require.NotNil(t, transport.DialTLSContext)
}
