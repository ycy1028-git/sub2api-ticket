package repository

import (
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestLookupOpenAITLSProfileUsesAttachedTemplate(t *testing.T) {
	upstream := NewHTTPUpstream(nil).(*httpUpstreamService)
	AttachOpenAITLSFingerprint(upstream, 1, func(id int64) *tlsfingerprint.Profile {
		require.Equal(t, int64(1), id)
		return &tlsfingerprint.Profile{Name: "codex-tui 0.154.0 macOS arm64 rustls"}
	})

	httpsReq, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	require.NoError(t, err)
	httpsReq = httpsReq.WithContext(service.WithHTTPUpstreamProfile(httpsReq.Context(), service.HTTPUpstreamProfileOpenAI))
	got := upstream.lookupOpenAITLSProfile(httpsReq)
	require.NotNil(t, got)
	require.Equal(t, "codex-tui 0.154.0 macOS arm64 rustls", got.Name)

	httpReq, err := http.NewRequest(http.MethodPost, "http://chatgpt.com/backend-api/codex/responses", nil)
	require.NoError(t, err)
	httpReq = httpReq.WithContext(service.WithHTTPUpstreamProfile(httpReq.Context(), service.HTTPUpstreamProfileOpenAI))
	require.Nil(t, upstream.lookupOpenAITLSProfile(httpReq))

	grokReq, err := http.NewRequest(http.MethodPost, "https://api.x.ai/v1/responses", nil)
	require.NoError(t, err)
	grokReq = grokReq.WithContext(service.WithHTTPUpstreamProfile(grokReq.Context(), service.HTTPUpstreamProfileGrok))
	require.Nil(t, upstream.lookupOpenAITLSProfile(grokReq))

	plainReq, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	require.NoError(t, err)
	require.Nil(t, upstream.lookupOpenAITLSProfile(plainReq))

	xaiReq, err := http.NewRequest(http.MethodPost, "https://api.x.ai/v1/chat/completions", nil)
	require.NoError(t, err)
	xaiReq = xaiReq.WithContext(service.WithHTTPUpstreamProfile(xaiReq.Context(), service.HTTPUpstreamProfileOpenAI))
	require.Nil(t, upstream.lookupOpenAITLSProfile(xaiReq))
}
