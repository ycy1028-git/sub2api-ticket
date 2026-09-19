package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func fakeCodexTicketState(n int) string {
	if n < len(openAICodexTicketStatePrefix) {
		return strings.Repeat("A", n)
	}
	return openAICodexTicketStatePrefix + strings.Repeat("B", n-len(openAICodexTicketStatePrefix))
}

func ticketTestAccount(id int64) *Account {
	return &Account{
		ID:          id,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Schedulable: true,
		Credentials: map[string]any{"access_token": "tok", "chatgpt_account_id": "acc-1"},
	}
}

func ticketTestService(t *testing.T, cfg config.OpenAICodexTicketConfig, upstream HTTPUpstream) *OpenAIGatewayService {
	t.Helper()
	return &OpenAIGatewayService{
		cfg: &config.Config{
			Gateway: config.GatewayConfig{OpenAICodexTicket: cfg},
		},
		httpUpstream: upstream,
	}
}

func TestApplyOpenAICodexTicket_ReplacesHeader(t *testing.T) {
	state := fakeCodexTicketState(292)
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled:      true,
		TargetLength: 292,
		TTLSeconds:   3600,
		FailClosed:   true,
	}, nil)
	account := ticketTestAccount(41)
	svc.storeOpenAICodexTicket(context.Background(), account, &openAICodexTicket{
		AccountID:  41,
		Model:      "gpt-6-astra",
		State:      state,
		Length:     292,
		CapturedAt: time.Now(),
		ExpiresAt:  time.Now().Add(time.Hour),
	})

	h := http.Header{}
	h.Set(openAICodexTurnStateHeader, fakeCodexTicketState(312))
	err := svc.applyOpenAICodexTicket(context.Background(), account, "gpt-6-astra", h)
	require.NoError(t, err)
	require.Equal(t, state, h.Get(openAICodexTurnStateHeader))
	require.Equal(t, 292, len(h.Get(openAICodexTurnStateHeader)))
}

func TestApplyOpenAICodexTicket_DoesNotReuseOtherModelOrAccount(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled:         true,
		TargetLength:    292,
		TTLSeconds:      3600,
		FailClosed:      true,
		HarvestProxyURL: "socks5h://harvest",
	}, &httpUpstreamRecorder{err: io.EOF})
	a := ticketTestAccount(41)
	b := ticketTestAccount(42)
	astra := fakeCodexTicketState(292)
	svc.storeOpenAICodexTicket(context.Background(), a, &openAICodexTicket{
		AccountID:  41,
		Model:      "gpt-6-astra",
		State:      astra,
		Length:     292,
		CapturedAt: time.Now(),
		ExpiresAt:  time.Now().Add(time.Hour),
	})

	h := http.Header{}
	h.Set(openAICodexTurnStateHeader, "keep-ungated")
	err := svc.applyOpenAICodexTicket(context.Background(), a, "gpt-5.5", h)
	require.NoError(t, err)
	require.Equal(t, "keep-ungated", h.Get(openAICodexTurnStateHeader))
	require.False(t, svc.openAICodexTicketBlocksAccount(a, "gpt-5.5"))
	require.True(t, svc.openAICodexTicketBlocksAccount(b, "gpt-6-astra"))
	require.False(t, svc.openAICodexTicketBlocksAccount(a, "gpt-6-astra"))

	h = http.Header{}
	err = svc.applyOpenAICodexTicket(context.Background(), b, "gpt-6-astra", h)
	require.ErrorIs(t, err, ErrOpenAICodexTicketUnavailable)
	require.Empty(t, h.Get(openAICodexTurnStateHeader))
}

func TestLookupOpenAICodexTicket_PrefersNewerExtra(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, TargetLength: 292, TTLSeconds: 3600}, nil)
	account := ticketTestAccount(41)
	oldState := fakeCodexTicketState(292)
	newState := openAICodexTicketStatePrefix + strings.Repeat("C", 286)
	svc.storeOpenAICodexTicket(context.Background(), account, &openAICodexTicket{
		AccountID:  41,
		Model:      "gpt-6-astra",
		State:      oldState,
		Length:     292,
		CapturedAt: time.Now().Add(-30 * time.Minute),
		ExpiresAt:  time.Now().Add(-time.Minute),
	})
	account.Extra = map[string]any{openAICodexTicketExtraKey("gpt-6-astra"): &openAICodexTicket{
		Model:      "gpt-6-astra",
		State:      newState,
		Length:     292,
		CapturedAt: time.Now(),
		ExpiresAt:  time.Now().Add(time.Hour),
	},
	}
	got := svc.lookupOpenAICodexTicket(account, "gpt-6-astra")
	require.NotNil(t, got)
	require.Equal(t, newState, got.State)
	require.True(t, got.valid(time.Now(), 292))
}

func TestApplyOpenAICodexTicket_ExpiredNotInjected(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled:         true,
		TargetLength:    292,
		TTLSeconds:      3600,
		FailClosed:      true,
		HarvestProxyURL: "socks5h://harvest",
	}, &httpUpstreamRecorder{err: io.EOF})
	account := ticketTestAccount(41)
	svc.storeOpenAICodexTicket(context.Background(), account, &openAICodexTicket{
		AccountID:  41,
		Model:      "gpt-6-astra",
		State:      fakeCodexTicketState(292),
		Length:     292,
		CapturedAt: time.Now().Add(-2 * time.Hour),
		ExpiresAt:  time.Now().Add(-time.Minute),
	})
	h := http.Header{}
	err := svc.applyOpenAICodexTicket(context.Background(), account, "gpt-6-astra", h)
	require.ErrorIs(t, err, ErrOpenAICodexTicketUnavailable)
	require.Empty(t, h.Get(openAICodexTurnStateHeader))
}

func TestApplyOpenAICodexTicket_WrongLengthNotInjected(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled:      true,
		TargetLength: 292,
		TTLSeconds:   3600,
		FailClosed:   true,
	}, nil)
	account := ticketTestAccount(41)
	svc.storeOpenAICodexTicket(context.Background(), account, &openAICodexTicket{
		AccountID:  41,
		Model:      "gpt-6-astra",
		State:      fakeCodexTicketState(312),
		Length:     312,
		CapturedAt: time.Now(),
		ExpiresAt:  time.Now().Add(time.Hour),
	})
	h := http.Header{}
	err := svc.applyOpenAICodexTicket(context.Background(), account, "gpt-6-astra", h)
	require.ErrorIs(t, err, ErrOpenAICodexTicketUnavailable)
	require.Empty(t, h.Get(openAICodexTurnStateHeader))
}

func TestApplyOpenAICodexTicket_FailOpenSkipsInject(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled:    true,
		FailClosed: false,
	}, nil)
	h := http.Header{}
	h.Set(openAICodexTurnStateHeader, "client-state")
	err := svc.applyOpenAICodexTicket(context.Background(), ticketTestAccount(41), "gpt-6-astra", h)
	require.NoError(t, err)
	require.Equal(t, "client-state", h.Get(openAICodexTurnStateHeader))
	require.False(t, svc.openAICodexTicketBlocksAccount(ticketTestAccount(41), "gpt-6-astra"))
}

func TestApplyOpenAICodexTicket_DisabledNoop(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: false, FailClosed: true}, nil)
	h := http.Header{}
	h.Set(openAICodexTurnStateHeader, "client-state")
	err := svc.applyOpenAICodexTicket(context.Background(), ticketTestAccount(41), "gpt-6-astra", h)
	require.NoError(t, err)
	require.Equal(t, "client-state", h.Get(openAICodexTurnStateHeader))
}

func TestHarvestOpenAICodexTicket_StopsAt292AndUsesHarvestProxy(t *testing.T) {
	state312 := fakeCodexTicketState(312)
	state292 := fakeCodexTicketState(292)
	header312 := http.Header{}
	header312.Set(openAICodexTurnStateHeader, state312)
	header292 := http.Header{}
	header292.Set(openAICodexTurnStateHeader, state292)
	upstream := &httpUpstreamRecorder{
		responses: []*http.Response{
			{
				StatusCode: http.StatusOK,
				Header:     header312,
				Body:       io.NopCloser(strings.NewReader("data: {}\n\n")),
			},
			{
				StatusCode: http.StatusOK,
				Header:     header292,
				Body:       io.NopCloser(strings.NewReader("data: {}\n\n")),
			},
		},
	}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled:                      true,
		TargetLength:                 292,
		TTLSeconds:                   3600,
		HarvestProxyURL:              "socks5h://user:pass@harvest.example:31",
		HarvestAttemptTimeoutSeconds: 5,
		FailClosed:                   true,
	}, upstream)
	account := ticketTestAccount(41)

	svc.probeOnceOpenAICodexTicket(context.Background(), account, "gpt-6-astra")
	require.Nil(t, svc.lookupOpenAICodexTicket(account, "gpt-6-astra"))
	svc.probeOnceOpenAICodexTicket(context.Background(), account, "gpt-6-astra")
	ticket := svc.lookupOpenAICodexTicket(account, "gpt-6-astra")
	require.NotNil(t, ticket)
	require.Equal(t, state292, ticket.State)
	h := http.Header{}
	h.Set(openAICodexTurnStateHeader, "stale")
	require.NoError(t, svc.applyOpenAICodexTicket(context.Background(), account, "gpt-6-astra", h))
	require.Equal(t, state292, h.Get(openAICodexTurnStateHeader))
	require.Equal(t, "socks5h://user:pass@harvest.example:31", upstream.lastProxyURL)
	require.Len(t, upstream.requests, 2)
	require.Empty(t, upstream.requests[0].Header.Get(openAICodexTurnStateHeader))
	require.Equal(t, openAICodexAstraMinVersion, upstream.requests[0].Header.Get("version"))
	require.Equal(t, HTTPUpstreamProfileOpenAIHarvest, HTTPUpstreamProfileFromContext(upstream.requests[0].Context()))
	require.True(t, upstream.requests[0].Close)
}

func TestHarvestOpenAICodexTicket_HTTP503DoesNotAbortHunt(t *testing.T) {
	state292 := fakeCodexTicketState(292)
	header503 := http.Header{}
	header292 := http.Header{}
	header292.Set(openAICodexTurnStateHeader, state292)
	responses := make([]*http.Response, 0, 3)
	responses = append(responses, &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Header:     header503,
		Body:       io.NopCloser(strings.NewReader(`{"error":"overloaded"}`)),
	})
	responses = append(responses, &http.Response{
		StatusCode: http.StatusOK,
		Header:     header292,
		Body:       io.NopCloser(strings.NewReader("data: {}\n\n")),
	})
	upstream := &httpUpstreamRecorder{responses: responses}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled:                      true,
		TargetLength:                 292,
		TTLSeconds:                   3600,
		HarvestProxyURL:              "socks5h://harvest.example:31",
		HarvestAttemptTimeoutSeconds: 5,
		FailClosed:                   true,
	}, upstream)
	account := ticketTestAccount(41)
	svc.probeOnceOpenAICodexTicket(context.Background(), account, "gpt-6-astra")
	require.Nil(t, svc.lookupOpenAICodexTicket(account, "gpt-6-astra"))
	svc.probeOnceOpenAICodexTicket(context.Background(), account, "gpt-6-astra")
	ticket := svc.lookupOpenAICodexTicket(account, "gpt-6-astra")
	require.NotNil(t, ticket)
	require.Equal(t, state292, ticket.State)
	require.Len(t, upstream.requests, 2)
}

func TestCaptureOpenAICodexTicketFromUpstreamPersistsTargetState(t *testing.T) {
	account := ticketTestAccount(41)
	account.Extra = map[string]any{}
	repo := &codexTicketRefreshRepo{}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled:              true,
		TargetLength:         292,
		TTLSeconds:           3600,
		RefreshBeforeSeconds: 600,
	}, nil)
	svc.accountRepo = repo
	request, err := http.NewRequest(http.MethodPost, "https://upstream.example/responses", strings.NewReader(`{"model":"gpt-6-astra"}`))
	require.NoError(t, err)
	require.Equal(t, "gpt-6-astra", openAICodexTicketModelFromRequest(request))
	require.True(t, isOpenAICodexTicketAccount(account))
	require.True(t, svc.openAICodexTicketEnabled())
	require.True(t, svc.openAICodexTicketGatedModel("gpt-6-astra"))
	require.Equal(t, 292, resolveOpenAICodexTicketPolicy(account, svc.openAICodexTicketConfig()).TargetLength)
	responseHeader := http.Header{}
	responseHeader.Set(openAICodexTurnStateHeader, fakeCodexTicketState(292))
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     responseHeader,
		Body:       io.NopCloser(strings.NewReader("data: {}\n\n")),
	}
	state := extractOpenAICodexTurnState(response.Header)
	require.Len(t, state, 292)
	require.True(t, strings.HasPrefix(state, openAICodexTicketStatePrefix))

	svc.captureOpenAICodexTicketFromUpstream(request, account, response)

	ticket := svc.lookupOpenAICodexTicket(account, "gpt-6-astra")
	require.NotNil(t, ticket)
	require.Equal(t, 292, ticket.Length)
	require.Contains(t, repo.updates, openAICodexTicketExtraKey("gpt-6-astra"))
	require.Contains(t, repo.updates, openAICodexTicketObservationExtraKey("gpt-6-astra"))
}

func TestCaptureOpenAICodexTicketFromUpstreamKeepsFreshTargetState(t *testing.T) {
	state := fakeCodexTicketState(292)
	account := ticketTestAccount(41)
	account.Extra = map[string]any{}
	repo := &codexTicketRefreshRepo{}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled:              true,
		TargetLength:         292,
		TTLSeconds:           3600,
		RefreshBeforeSeconds: 600,
	}, nil)
	svc.accountRepo = repo
	svc.storeOpenAICodexTicket(context.Background(), account, &openAICodexTicket{
		AccountID:  account.ID,
		Model:      "gpt-6-astra",
		State:      state,
		Length:     292,
		CapturedAt: time.Now(),
		ExpiresAt:  time.Now().Add(time.Hour),
	})
	repo.updates = nil
	request, err := http.NewRequest(http.MethodPost, "https://upstream.example/responses", strings.NewReader(`{"model":"gpt-6-astra"}`))
	require.NoError(t, err)
	responseHeader := http.Header{}
	responseHeader.Set(openAICodexTurnStateHeader, fakeCodexTicketState(312))
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     responseHeader,
		Body:       io.NopCloser(strings.NewReader("data: {}\n\n")),
	}

	svc.captureOpenAICodexTicketFromUpstream(request, account, response)

	ticket := svc.lookupOpenAICodexTicket(account, "gpt-6-astra")
	require.NotNil(t, ticket)
	require.Equal(t, state, ticket.State)
	require.Nil(t, repo.updates)
}

func TestLookupOpenAICodexTicket_HydratesFromExtra(t *testing.T) {
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, TargetLength: 292, TTLSeconds: 3600}, nil)
	state := fakeCodexTicketState(292)
	account := ticketTestAccount(9)
	account.Extra = map[string]any{
		openAICodexTicketExtraKey("gpt-6-astra"): map[string]any{
			"state":       state,
			"length":      292,
			"model":       "gpt-6-astra",
			"captured_at": time.Now().Add(-time.Minute),
			"expires_at":  time.Now().Add(time.Hour),
		},
	}
	got := svc.lookupOpenAICodexTicket(account, "gpt-6-astra")
	require.NotNil(t, got)
	require.Equal(t, state, got.State)
	require.True(t, got.valid(time.Now(), 292))
}

func TestOpenAICodexTicketStatuses_ReportsRemainingTTL(t *testing.T) {
	account := ticketTestAccount(41)
	account.Extra = map[string]any{
		openAICodexTicketExtraKey("gpt-6-astra"): map[string]any{
			"state":       fakeCodexTicketState(292),
			"length":      292,
			"model":       "gpt-6-astra",
			"captured_at": time.Now().Add(-10 * time.Minute),
			"expires_at":  time.Now().Add(50 * time.Minute),
		},
	}
	now := time.Now()
	got := OpenAICodexTicketStatuses(account, config.OpenAICodexTicketConfig{Enabled: true, FailClosed: true}, now)
	require.Len(t, got, 2)
	require.Equal(t, "gpt-6-astra", got[0].Model)
	require.True(t, got[0].Ready)
	require.Greater(t, got[0].RemainingSeconds, int64(40*60))
	require.LessOrEqual(t, got[0].RemainingSeconds, int64(50*60))
	require.Equal(t, "gpt-5.6-sol", got[1].Model)
	require.False(t, got[1].Ready)
}

func TestExtractOpenAICodexTicketModel(t *testing.T) {
	require.Equal(t, "gpt-6-astra", extractOpenAICodexTicketModel([]byte(`{"model":"gpt-6-astra"}`)))
	require.Empty(t, extractOpenAICodexTicketModel([]byte(`{}`)))
}

// These stubs exercise the real continuous refresh path with both default models
// completing together. Run under -race to catch writes to the shared account maps.
type codexTicketRefreshRepo struct {
	AccountRepository
	accounts []Account
	mu       sync.Mutex
	updates  map[string]any
}

func (r *codexTicketRefreshRepo) ListByPlatform(context.Context, string) ([]Account, error) {
	return r.accounts, nil
}
func (r *codexTicketRefreshRepo) ListSchedulableByPlatform(context.Context, string) ([]Account, error) {
	return r.accounts, nil
}
func (r *codexTicketRefreshRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.updates == nil {
		r.updates = make(map[string]any)
	}
	for k, v := range updates {
		r.updates[k] = v
	}
	return nil
}

type codexTicketConcurrentUpstream struct {
	HTTPUpstream
	started atomic.Int64
	ready   chan struct{}
}

func (u *codexTicketConcurrentUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	if u.started.Add(1) == 2 {
		close(u.ready)
	}
	select {
	case <-u.ready:
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	h := http.Header{}
	h.Set(openAICodexTurnStateHeader, fakeCodexTicketState(292))
	return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader("data: {}\n\n"))}, nil
}
func TestRefreshOpenAICodexTickets_ConcurrentModelsPreserveAccountSnapshot(t *testing.T) {
	account := ticketTestAccount(41)
	account.Status = StatusActive
	account.Extra = map[string]any{"existing": true}
	repo := &codexTicketRefreshRepo{accounts: []Account{*account}}
	upstream := &codexTicketConcurrentUpstream{ready: make(chan struct{})}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, HarvestProxyURL: "socks5h://proxy.example.com:1080"}, upstream)
	svc.accountRepo = repo
	require.True(t, svc.refreshOpenAICodexTickets(context.Background()), "missing tickets should be probed once")
	require.Equal(t, int64(2), upstream.started.Load())
	require.Equal(t, map[string]any{"existing": true}, account.Extra)
	require.Len(t, repo.updates, 4)
	for _, model := range []string{openAICodexTicketDefaultModel, openAICodexTicketDefaultSolModel} {
		ticket := svc.lookupOpenAICodexTicket(account, model)
		require.NotNil(t, ticket)
		require.True(t, ticket.valid(time.Now(), 292))
	}
	// Valid tickets do not produce another probe on the next cycle.
	require.False(t, svc.refreshOpenAICodexTickets(context.Background()), "valid tickets should not be probed again")
	require.Equal(t, int64(2), upstream.started.Load())
}

func TestRefreshOpenAICodexTickets_SkipsUnschedulableAccounts(t *testing.T) {
	active := ticketTestAccount(41)
	active.Status = StatusActive
	paused := ticketTestAccount(42)
	paused.Status = StatusActive
	paused.Schedulable = false
	rateLimitedUntil := time.Now().Add(time.Hour)
	rateLimited := ticketTestAccount(43)
	rateLimited.Status = StatusActive
	rateLimited.RateLimitResetAt = &rateLimitedUntil
	repo := &codexTicketRefreshRepo{accounts: []Account{*active, *paused, *rateLimited}}
	upstream := &codexTicketConcurrentUpstream{ready: make(chan struct{})}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, HarvestProxyURL: "socks5h://proxy.example.com:1080"}, upstream)
	svc.accountRepo = repo

	require.True(t, svc.refreshOpenAICodexTickets(context.Background()))

	require.Equal(t, int64(2), upstream.started.Load(), "only the schedulable account's two models should be probed")
}

func TestRefreshOpenAICodexTickets_HonorsRateLimitNextProbeAt(t *testing.T) {
	now := time.Now()
	account := ticketTestAccount(41)
	account.Status = StatusActive
	account.Extra = map[string]any{
		openAICodexTicketObservationExtraKey("gpt-6-astra"): openAICodexTicketObservation{
			Model:        "gpt-6-astra",
			TargetLength: 292,
			Outcome:      "rate_limited",
			HTTPStatus:   http.StatusTooManyRequests,
			ObservedAt:   now,
			NextProbeAt:  now.Add(time.Hour),
		},
	}
	repo := &codexTicketRefreshRepo{accounts: []Account{*account}}
	upstream := &codexTicketConcurrentUpstream{ready: make(chan struct{})}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled: true, Models: []string{"gpt-6-astra"}, HarvestProxyURL: "socks5h://proxy.example.com:1080",
	}, upstream)
	svc.accountRepo = repo

	require.False(t, svc.refreshOpenAICodexTickets(context.Background()), "a future NextProbeAt must suppress the probe")
	require.Zero(t, upstream.started.Load())

	repo.accounts[0].Extra[openAICodexTicketObservationExtraKey("gpt-6-astra")] = openAICodexTicketObservation{
		Model:        "gpt-6-astra",
		TargetLength: 292,
		Outcome:      "rate_limited",
		HTTPStatus:   http.StatusTooManyRequests,
		ObservedAt:   now.Add(-time.Hour),
		NextProbeAt:  now.Add(-time.Minute),
	}
	require.True(t, svc.refreshOpenAICodexTickets(context.Background()))
	require.Equal(t, int64(1), upstream.started.Load())
}

func TestOpenAICodexTicketRateLimitRetryAtWaitsForQuotaReset(t *testing.T) {
	now := time.Now()
	resetAt := now.Add(6 * time.Hour)
	account := ticketTestAccount(41)
	account.Extra = map[string]any{
		"codex_7d_used_percent": 100.0,
		"codex_7d_reset_at":     resetAt.Format(time.RFC3339Nano),
	}
	require.True(t, resetAt.Equal(openAICodexTicketRateLimitRetryAt(account, now, time.Hour)))

	account.Extra = map[string]any{"codex_7d_used_percent": 42.0}
	require.WithinDuration(t, now.Add(time.Hour), openAICodexTicketRateLimitRetryAt(account, now, time.Hour), time.Second)
}

func TestResolveOpenAICodexTicketPolicy_AutoAndManual(t *testing.T) {
	cfg := config.OpenAICodexTicketConfig{TargetLength: 292, FailClosed: true}
	business := ticketTestAccount(1)
	business.Credentials["plan_type"] = "self_serve_business_prolite"
	policy := resolveOpenAICodexTicketPolicy(business, cfg)
	require.Equal(t, 332, policy.TargetLength)
	require.Equal(t, "auto_business", policy.TargetSource)
	require.Equal(t, "pause", policy.MissingPolicy)

	personal := ticketTestAccount(2)
	personal.Credentials["plan_type"] = "pro"
	policy = resolveOpenAICodexTicketPolicy(personal, cfg)
	require.Equal(t, 292, policy.TargetLength)
	require.Equal(t, "auto_personal", policy.TargetSource)

	personal.Extra = map[string]any{
		OpenAICodexTicketTargetModeExtraKey:    "manual",
		OpenAICodexTicketTargetLengthExtraKey:  356,
		OpenAICodexTicketMissingPolicyExtraKey: "allow",
	}
	policy = resolveOpenAICodexTicketPolicy(personal, cfg)
	require.Equal(t, 356, policy.TargetLength)
	require.Equal(t, "manual", policy.TargetSource)
	require.Equal(t, "allow", policy.MissingPolicy)

	policy = resolveOpenAICodexTicketPolicy(ticketTestAccount(3), config.OpenAICodexTicketConfig{TargetLength: 292})
	require.Equal(t, "allow", policy.MissingPolicy, "missing tickets must keep scheduling by default")
}

func TestNormalizeOpenAICodexTicketPolicyExtra(t *testing.T) {
	extra, err := NormalizeOpenAICodexTicketPolicyExtra(PlatformOpenAI, AccountTypeOAuth, map[string]any{
		OpenAICodexTicketTargetModeExtraKey:    "manual",
		OpenAICodexTicketTargetLengthExtraKey:  332.0,
		OpenAICodexTicketMissingPolicyExtraKey: "allow",
	})
	require.NoError(t, err)
	require.Equal(t, 332, extra[OpenAICodexTicketTargetLengthExtraKey])

	extra, err = NormalizeOpenAICodexTicketPolicyExtra(PlatformOpenAI, AccountTypeOAuth, map[string]any{
		OpenAICodexTicketTargetModeExtraKey: "auto",
	})
	require.NoError(t, err)
	require.Equal(t, "allow", extra[OpenAICodexTicketMissingPolicyExtraKey])

	_, err = NormalizeOpenAICodexTicketPolicyExtra(PlatformOpenAI, AccountTypeOAuth, map[string]any{
		OpenAICodexTicketTargetModeExtraKey:   "manual",
		OpenAICodexTicketTargetLengthExtraKey: 0,
	})
	require.Error(t, err)
}

func TestOpenAICodexTicketStatuses_UsesAccountPolicyAndObservation(t *testing.T) {
	now := time.Now()
	account := ticketTestAccount(41)
	account.Credentials["plan_type"] = "team"
	account.Extra = map[string]any{
		OpenAICodexTicketMissingPolicyExtraKey: "allow",
		openAICodexTicketObservationExtraKey("gpt-6-astra"): map[string]any{
			"model": "gpt-6-astra", "target_length": 332, "length": 356,
			"http_status": 200, "outcome": "non_target", "observed_at": now,
		},
	}
	status := OpenAICodexTicketStatuses(account, config.OpenAICodexTicketConfig{
		Enabled: true, FailClosed: true, Models: []string{"gpt-6-astra"},
	}, now)
	require.Len(t, status, 1)
	require.Equal(t, 332, status[0].TargetLength)
	require.Equal(t, 356, status[0].ObservedLength)
	require.Equal(t, "non_target", status[0].TicketType)
	require.False(t, status[0].Blocked)
}

func TestOpenAICodexTicketStatuses_ExpiredTicketUsesLatestObservedLength(t *testing.T) {
	now := time.Now()
	account := ticketTestAccount(41)
	account.Credentials["plan_type"] = "team"
	account.Extra = map[string]any{
		openAICodexTicketExtraKey("gpt-6-astra"): map[string]any{
			"model": "gpt-6-astra", "state": fakeCodexTicketState(332), "length": 332,
			"captured_at": now.Add(-2 * time.Hour), "expires_at": now.Add(-time.Hour),
		},
		openAICodexTicketObservationExtraKey("gpt-6-astra"): map[string]any{
			"model": "gpt-6-astra", "target_length": 332, "length": 356,
			"http_status": 200, "outcome": "non_target", "observed_at": now,
		},
	}
	status := OpenAICodexTicketStatuses(account, config.OpenAICodexTicketConfig{
		Enabled: true, Models: []string{"gpt-6-astra"},
	}, now)
	require.Len(t, status, 1)
	require.False(t, status[0].Ready)
	require.Equal(t, "non_target", status[0].TicketType)
	require.Zero(t, status[0].Length, "expired persisted ticket must not be reported as the current valid length")
	require.Equal(t, 356, status[0].ObservedLength)
}
func TestOpenAICodexTicketStatuses_RespectRuntimeConfiguration(t *testing.T) {
	account := ticketTestAccount(41)
	require.Empty(t, OpenAICodexTicketStatuses(account, config.OpenAICodexTicketConfig{}, time.Now()))
	cfg := config.OpenAICodexTicketConfig{Enabled: true, Models: []string{"custom-model"}}
	status := OpenAICodexTicketStatuses(account, cfg, time.Now())
	require.Len(t, status, 1)
	require.Equal(t, "custom-model", status[0].Model)
	require.False(t, status[0].Blocked)
	cfg.FailClosed = true
	require.True(t, OpenAICodexTicketStatuses(account, cfg, time.Now())[0].Blocked)
}
func TestProbeOpenAICodexTicket_RejectsInvalidState(t *testing.T) {
	for _, state := range []string{fakeCodexTicketState(312), strings.Repeat("X", 292), ""} {
		h := http.Header{}
		h.Set(openAICodexTurnStateHeader, state)
		upstream := &httpUpstreamRecorder{responses: []*http.Response{{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(""))}}}
		svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, HarvestProxyURL: "http://proxy.example.com:8080"}, upstream)
		account := ticketTestAccount(41)
		svc.probeOnceOpenAICodexTicket(context.Background(), account, "gpt-6-astra")
		require.Nil(t, svc.lookupOpenAICodexTicket(account, "gpt-6-astra"))
	}
}
func TestOpenAICodexTicket_RequiresActualLengthAndExpiry(t *testing.T) {
	ticket := &openAICodexTicket{State: fakeCodexTicketState(312), Length: 292, ExpiresAt: time.Now().Add(time.Hour)}
	require.False(t, ticket.valid(time.Now(), 292))
	ticket.State = fakeCodexTicketState(292)
	ticket.ExpiresAt = time.Time{}
	require.False(t, ticket.valid(time.Now(), 292))
}

// /responses/compact 的出站模型被 Forward 改写为 gateway.openai_compact_model
// （默认非空），门票门控必须按该出站模型判定。否则对门控模型发 compact 请求时，
// 所有无票账号都会被 fail_closed 误判为不可调度，而这些请求实际不需要票。
func TestOpenAICodexTicketGate_CompactRequestUsesForwardOutboundModel(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{
		OpenAICompactModel: "gpt-5.5",
		OpenAICodexTicket: config.OpenAICodexTicketConfig{
			Enabled:      true,
			TargetLength: 292,
			TTLSeconds:   3600,
			FailClosed:   true,
			Models:       []string{"gpt-6-astra"},
		},
	}}}
	account := ticketTestAccount(41) // 无票

	// 出站模型预测必须与 Forward 的解析链一致。
	require.Equal(t, "gpt-6-astra", svc.openAICodexTicketOutboundModel(account, "gpt-6-astra", false))
	require.Equal(t, "gpt-5.5", svc.openAICodexTicketOutboundModel(account, "gpt-6-astra", true))

	// 普通请求：出站仍是门控模型且无票 → fail_closed 必须拦号。
	require.True(t, svc.isOpenAIAccountRequestRuntimeBlocked(account, "gpt-6-astra", false))

	// compact 请求：出站已被改写成非门控的 gpt-5.5 → 不得拦号。
	require.False(t, svc.isOpenAIAccountRequestRuntimeBlocked(account, "gpt-6-astra", true))

	// 回归锚点：按客户端原始模型判定（旧实现的口径）在 compact 下必然误拦。
	require.True(t, svc.openAICodexTicketBlocksAccount(account, canonicalOpenAIAccountSchedulingModel(account, "gpt-6-astra")))
}
