package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestFillOpenAIOAuthCacheCreationResponse(t *testing.T) {
	oauthAccount := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	t.Run("responses usage", func(t *testing.T) {
		body := []byte(`{"type":"response.completed","response":{"usage":{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cached_tokens":40}}}}`)
		patched := fillOpenAIOAuthCacheCreationResponse(oauthAccount, "gpt-5.6-sol", body)
		require.Equal(t, int64(60), gjson.GetBytes(patched, "response.usage.input_tokens_details.cache_write_tokens").Int())
	})

	t.Run("chat completions zero value", func(t *testing.T) {
		body := []byte(`{"usage":{"prompt_tokens":80,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":30,"cache_write_tokens":0}}}`)
		patched := fillOpenAIOAuthCacheCreationResponse(oauthAccount, "gpt-5.6-sol", body)
		require.Equal(t, int64(50), gjson.GetBytes(patched, "usage.prompt_tokens_details.cache_write_tokens").Int())
	})

	t.Run("preserves explicit positive value", func(t *testing.T) {
		body := []byte(`{"usage":{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cached_tokens":40,"cache_write_tokens":12}}}`)
		patched := fillOpenAIOAuthCacheCreationResponse(oauthAccount, "gpt-5.6-sol", body)
		require.Equal(t, body, patched)
	})

	t.Run("ignores non OAuth account", func(t *testing.T) {
		body := []byte(`{"usage":{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cached_tokens":40}}}`)
		patched := fillOpenAIOAuthCacheCreationResponse(&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, "gpt-5.6-sol", body)
		require.Equal(t, body, patched)
	})
}
