package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

const (
	openAICodexTicketExtraKeyPrefix            = "codex_turn_ticket:"
	openAICodexTicketObservationKeyPrefix      = "codex_turn_ticket_observation:"
	OpenAICodexTicketTargetModeExtraKey        = "codex_ticket_target_mode"
	OpenAICodexTicketTargetLengthExtraKey      = "codex_ticket_target_length"
	OpenAICodexTicketMissingPolicyExtraKey     = "codex_ticket_missing_policy"
	OpenAICodexTicketModelPolicyExtraKeyPrefix = "codex_ticket_policy:"
	openAICodexTicketTargetModeAuto            = "auto"
	openAICodexTicketTargetModeManual          = "manual"
	openAICodexTicketMissingPolicyPause        = "pause"
	openAICodexTicketMissingPolicyAllow        = "allow"
	openAICodexTicketPersonalTargetLength      = 292
	openAICodexTicketBusinessTargetLength      = 332
	openAICodexTicketMinTargetLength           = 128
	openAICodexTicketMaxTargetLength           = 2048
	openAICodexTicketMissingRetryInterval      = 30 * time.Minute
	openAICodexTicketFailureBackoff            = openAICodexTicketMissingRetryInterval
	openAICodexTicketRateLimitBackoff          = time.Hour
	openAICodexTicketHarvestScanInterval       = 10 * time.Second
	openAICodexAstraMinVersion                 = "0.153.4"
	openAICodexTicketStatePrefix               = "gAAAAA"
	openAICodexTicketDefaultModel              = "gpt-6-astra"
	openAICodexTicketDefaultSolModel           = "gpt-5.6-sol"

	openAICodexTicketDefaultMissRetrySeconds      = 1800
	openAICodexTicketDefaultRateLimitRetrySeconds = 3600
	openAICodexTicketMinMissRetrySeconds          = 1
	openAICodexTicketMaxMissRetrySeconds          = 86400
	openAICodexTicketMinRateLimitRetrySeconds     = 1
	openAICodexTicketMaxRateLimitRetrySeconds     = 604800
)

func parseOpenAICodexTicketRetrySeconds(raw string, fallback, min, max int) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value < min || value > max {
		return fallback
	}
	return value
}

func ValidateOpenAICodexTicketMissRetrySeconds(value int) error {
	if value < openAICodexTicketMinMissRetrySeconds || value > openAICodexTicketMaxMissRetrySeconds {
		return fmt.Errorf("must be between %d and %d seconds", openAICodexTicketMinMissRetrySeconds, openAICodexTicketMaxMissRetrySeconds)
	}
	return nil
}

func ValidateOpenAICodexTicketRateLimitRetrySeconds(value int) error {
	if value < openAICodexTicketMinRateLimitRetrySeconds || value > openAICodexTicketMaxRateLimitRetrySeconds {
		return fmt.Errorf("must be between %d and %d seconds", openAICodexTicketMinRateLimitRetrySeconds, openAICodexTicketMaxRateLimitRetrySeconds)
	}
	return nil
}

// ErrOpenAICodexTicketUnavailable 表示该号该模型没有可用的 292 门票，
// 且 fail_closed 禁止裸打业务请求。
var ErrOpenAICodexTicketUnavailable = errors.New("codex turn-state ticket unavailable")

type openAICodexTicket struct {
	AccountID  int64     `json:"account_id"`
	Model      string    `json:"model"`
	State      string    `json:"state"`
	Length     int       `json:"length"`
	CapturedAt time.Time `json:"captured_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Attempts   int       `json:"attempts"`
}

// openAICodexTicketObservation records only redacted probe facts. It never
// stores the state value and is safe to use for scheduling diagnostics.
type openAICodexTicketObservation struct {
	Model        string    `json:"model"`
	TargetLength int       `json:"target_length,omitempty"`
	Length       int       `json:"length,omitempty"`
	HTTPStatus   int       `json:"http_status,omitempty"`
	Outcome      string    `json:"outcome"`
	ObservedAt   time.Time `json:"observed_at"`
	NextProbeAt  time.Time `json:"next_probe_at,omitempty"`
	Error        string    `json:"error,omitempty"`
}

type openAICodexTicketPolicy struct {
	Enabled       bool
	TargetMode    string
	TargetLength  int
	TargetSource  string
	MissingPolicy string
	PlanType      string
}

func openAICodexTicketKey(accountID int64, model string) string {
	return fmt.Sprintf("%d\x00%s", accountID, strings.TrimSpace(model))
}

func openAICodexTicketExtraKey(model string) string {
	return openAICodexTicketExtraKeyPrefix + strings.TrimSpace(model)
}

func openAICodexTicketObservationExtraKey(model string) string {
	return openAICodexTicketObservationKeyPrefix + strings.TrimSpace(model)
}

func openAICodexTicketModelPolicyExtraKey(model string) string {
	return OpenAICodexTicketModelPolicyExtraKeyPrefix + strings.TrimSpace(model)
}

func openAICodexTicketExtraString(extra map[string]any, key string) string {
	if extra == nil {
		return ""
	}
	value, _ := extra[key].(string)
	return strings.TrimSpace(value)
}

func openAICodexTicketExtraInt(extra map[string]any, key string) int {
	if len(extra) == 0 {
		return 0
	}
	switch value := extra[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	case json.Number:
		parsed, _ := strconv.Atoi(value.String())
		return parsed
	case string:
		parsed, _ := strconv.Atoi(strings.TrimSpace(value))
		return parsed
	default:
		return 0
	}
}

func isOpenAICodexBusinessPlan(plan string) bool {
	plan = strings.ToLower(strings.TrimSpace(plan))
	return strings.Contains(plan, "business") || strings.Contains(plan, "team") ||
		strings.Contains(plan, "enterprise") || strings.Contains(plan, "workspace")
}

func isOpenAICodexPersonalPlan(plan string) bool {
	switch strings.ToLower(strings.TrimSpace(plan)) {
	case "free", "plus", "pro", "pro_5x", "pro_20x", "personal":
		return true
	default:
		return false
	}
}

func DefaultOpenAICodexTicketModelPolicies() map[string]config.OpenAICodexTicketModelPolicy {
	return map[string]config.OpenAICodexTicketModelPolicy{
		openAICodexTicketDefaultModel: {
			Enabled: true, TargetMode: openAICodexTicketTargetModeAuto,
			TargetLength: openAICodexTicketBusinessTargetLength, MissingPolicy: openAICodexTicketMissingPolicyAllow,
		},
		openAICodexTicketDefaultSolModel: {
			Enabled: true, TargetMode: openAICodexTicketTargetModeAuto,
			TargetLength: openAICodexTicketBusinessTargetLength, MissingPolicy: openAICodexTicketMissingPolicyAllow,
		},
	}
}

func DefaultOpenAICodexTicketModelPoliciesJSON() string {
	raw, _ := json.Marshal(DefaultOpenAICodexTicketModelPolicies())
	return string(raw)
}

func NormalizeOpenAICodexTicketModelPolicies(input map[string]config.OpenAICodexTicketModelPolicy) (map[string]config.OpenAICodexTicketModelPolicy, error) {
	result := DefaultOpenAICodexTicketModelPolicies()
	for model, value := range input {
		model = normalizeOpenAICodexTicketModel(model)
		if model != openAICodexTicketDefaultModel && model != openAICodexTicketDefaultSolModel {
			return nil, fmt.Errorf("unsupported codex ticket model %q", model)
		}
		if value.TargetMode == "" {
			value.TargetMode = openAICodexTicketTargetModeAuto
		}
		if value.TargetMode != openAICodexTicketTargetModeAuto && value.TargetMode != openAICodexTicketTargetModeManual {
			return nil, fmt.Errorf("%s target_mode must be auto or manual", model)
		}
		if value.TargetLength == 0 {
			value.TargetLength = openAICodexTicketBusinessTargetLength
		}
		if value.TargetLength < openAICodexTicketMinTargetLength || value.TargetLength > openAICodexTicketMaxTargetLength {
			return nil, fmt.Errorf("%s target_length must be between %d and %d", model, openAICodexTicketMinTargetLength, openAICodexTicketMaxTargetLength)
		}
		if value.MissingPolicy == "" {
			value.MissingPolicy = openAICodexTicketMissingPolicyAllow
		}
		if value.MissingPolicy != openAICodexTicketMissingPolicyPause && value.MissingPolicy != openAICodexTicketMissingPolicyAllow {
			return nil, fmt.Errorf("%s missing_policy must be pause or allow", model)
		}
		result[model] = value
	}
	return result, nil
}

func resolveOpenAICodexTicketPolicy(account *Account, cfg config.OpenAICodexTicketConfig) openAICodexTicketPolicy {
	return resolveOpenAICodexTicketPolicyForModel(account, cfg, "")
}

func resolveOpenAICodexTicketPolicyForModel(account *Account, cfg config.OpenAICodexTicketConfig, model string) openAICodexTicketPolicy {
	fallback := cfg.TargetLength
	if fallback <= 0 {
		fallback = openAICodexTicketPersonalTargetLength
	}
	policy := openAICodexTicketPolicy{
		Enabled:       true,
		TargetMode:    openAICodexTicketTargetModeAuto,
		TargetLength:  fallback,
		TargetSource:  "global_default",
		MissingPolicy: openAICodexTicketMissingPolicyAllow,
	}
	if cfg.FailClosed {
		policy.MissingPolicy = openAICodexTicketMissingPolicyPause
	}
	if account != nil {
		policy.PlanType = strings.TrimSpace(account.GetCredential("plan_type"))
	}
	applyAutoTarget := func(fallback int, source string) {
		policy.TargetMode = openAICodexTicketTargetModeAuto
		policy.TargetLength = fallback
		policy.TargetSource = source
		if isOpenAICodexBusinessPlan(policy.PlanType) {
			policy.TargetLength = openAICodexTicketBusinessTargetLength
			policy.TargetSource = "auto_business"
		} else if isOpenAICodexPersonalPlan(policy.PlanType) {
			policy.TargetLength = openAICodexTicketPersonalTargetLength
			policy.TargetSource = "auto_personal"
		}
	}
	applyAutoTarget(fallback, "global_default")
	if configured, ok := cfg.ModelPolicies[normalizeOpenAICodexTicketModel(model)]; ok {
		policy.Enabled = configured.Enabled
		if configured.TargetMode == openAICodexTicketTargetModeManual {
			policy.TargetMode = configured.TargetMode
			policy.TargetLength = configured.TargetLength
			policy.TargetSource = "model_manual"
		} else {
			applyAutoTarget(configured.TargetLength, "model_auto")
		}
		if configured.MissingPolicy == openAICodexTicketMissingPolicyPause || configured.MissingPolicy == openAICodexTicketMissingPolicyAllow {
			policy.MissingPolicy = configured.MissingPolicy
		}
	}
	if account == nil {
		return policy
	}
	mode := strings.ToLower(openAICodexTicketExtraString(account.Extra, OpenAICodexTicketTargetModeExtraKey))
	if mode == openAICodexTicketTargetModeManual {
		if target := openAICodexTicketExtraInt(account.Extra, OpenAICodexTicketTargetLengthExtraKey); target >= openAICodexTicketMinTargetLength && target <= openAICodexTicketMaxTargetLength {
			policy.TargetMode = mode
			policy.TargetLength = target
			policy.TargetSource = "manual"
		}
	}
	switch strings.ToLower(openAICodexTicketExtraString(account.Extra, OpenAICodexTicketMissingPolicyExtraKey)) {
	case openAICodexTicketMissingPolicyPause:
		policy.MissingPolicy = openAICodexTicketMissingPolicyPause
	case openAICodexTicketMissingPolicyAllow:
		policy.MissingPolicy = openAICodexTicketMissingPolicyAllow
	}
	if raw, ok := account.Extra[openAICodexTicketModelPolicyExtraKey(model)]; ok {
		if override, ok := raw.(map[string]any); ok {
			if enabled, ok := override["enabled"].(bool); ok {
				policy.Enabled = enabled
			}
			mode, _ := override[OpenAICodexTicketTargetModeExtraKey].(string)
			switch mode {
			case openAICodexTicketTargetModeManual:
				if target := openAICodexTicketExtraInt(override, OpenAICodexTicketTargetLengthExtraKey); target >= openAICodexTicketMinTargetLength && target <= openAICodexTicketMaxTargetLength {
					policy.TargetMode = mode
					policy.TargetLength = target
					policy.TargetSource = "account_model_manual"
				}
			case openAICodexTicketTargetModeAuto:
				applyAutoTarget(policy.TargetLength, "account_model_auto")
			}
			if missing, _ := override[OpenAICodexTicketMissingPolicyExtraKey].(string); missing == openAICodexTicketMissingPolicyPause || missing == openAICodexTicketMissingPolicyAllow {
				policy.MissingPolicy = missing
			}
		}
	}
	if !policy.Enabled {
		policy.MissingPolicy = openAICodexTicketMissingPolicyAllow
	}
	return policy
}

// NormalizeOpenAICodexTicketPolicyExtra validates account-editable ticket
// policy while keeping server-managed tickets and observations out of it.
func NormalizeOpenAICodexTicketPolicyExtra(platform, accountType string, extra map[string]any) (map[string]any, error) {
	if extra == nil {
		return nil, nil
	}
	if platform != PlatformOpenAI || (accountType != AccountTypeOAuth && accountType != AccountTypeSetupToken) {
		delete(extra, OpenAICodexTicketTargetModeExtraKey)
		delete(extra, OpenAICodexTicketTargetLengthExtraKey)
		delete(extra, OpenAICodexTicketMissingPolicyExtraKey)
		return extra, nil
	}
	mode := strings.ToLower(openAICodexTicketExtraString(extra, OpenAICodexTicketTargetModeExtraKey))
	if mode == "" {
		mode = openAICodexTicketTargetModeAuto
	}
	if mode != openAICodexTicketTargetModeAuto && mode != openAICodexTicketTargetModeManual {
		return nil, errors.New("codex_ticket_target_mode must be auto or manual")
	}
	extra[OpenAICodexTicketTargetModeExtraKey] = mode
	if mode == openAICodexTicketTargetModeManual {
		target := openAICodexTicketExtraInt(extra, OpenAICodexTicketTargetLengthExtraKey)
		if target < openAICodexTicketMinTargetLength || target > openAICodexTicketMaxTargetLength {
			return nil, fmt.Errorf("codex_ticket_target_length must be between %d and %d", openAICodexTicketMinTargetLength, openAICodexTicketMaxTargetLength)
		}
		extra[OpenAICodexTicketTargetLengthExtraKey] = target
	} else {
		delete(extra, OpenAICodexTicketTargetLengthExtraKey)
	}
	missingPolicy := strings.ToLower(openAICodexTicketExtraString(extra, OpenAICodexTicketMissingPolicyExtraKey))
	if missingPolicy == "" {
		missingPolicy = openAICodexTicketMissingPolicyAllow
	}
	if missingPolicy != openAICodexTicketMissingPolicyPause && missingPolicy != openAICodexTicketMissingPolicyAllow {
		return nil, errors.New("codex_ticket_missing_policy must be pause or allow")
	}
	extra[OpenAICodexTicketMissingPolicyExtraKey] = missingPolicy
	return extra, nil
}

func normalizeOpenAICodexTicketModel(model string) string {
	return strings.TrimSpace(model)
}

func extractOpenAICodexTicketModel(body []byte) string {
	return normalizeOpenAICodexTicketModel(gjson.GetBytes(body, "model").String())
}

func (s *OpenAIGatewayService) openAICodexTicketConfig() config.OpenAICodexTicketConfig {
	cfg := config.OpenAICodexTicketConfig{}
	if s != nil && s.cfg != nil {
		cfg = s.cfg.Gateway.OpenAICodexTicket
	}
	if cfg.TargetLength <= 0 {
		cfg.TargetLength = 292
	}
	if cfg.TTLSeconds <= 0 {
		cfg.TTLSeconds = 3600
	}
	if cfg.RefreshBeforeSeconds <= 0 {
		cfg.RefreshBeforeSeconds = 600
	}
	if cfg.HarvestProbeIntervalSeconds <= 0 {
		cfg.HarvestProbeIntervalSeconds = 1800
	}
	if cfg.HarvestAttemptTimeoutSeconds <= 0 {
		cfg.HarvestAttemptTimeoutSeconds = 25
	}
	if len(cfg.Models) == 0 {
		cfg.Models = []string{openAICodexTicketDefaultModel, openAICodexTicketDefaultSolModel}
	}
	if s != nil && s.settingService != nil {
		cfg.ModelPolicies = s.settingService.GetOpenAICodexTicketModelPolicies(context.Background(), cfg.ModelPolicies)
	}
	return cfg
}

// captureOpenAICodexTicketFromUpstream observes turn-state headers on real
// production responses. This is the opportunistic mint path: a valid target
// state is persisted immediately and reused by later requests, without waiting
// for the next synthetic harvest cycle.
func (s *OpenAIGatewayService) captureOpenAICodexTicketFromUpstream(request *http.Request, account *Account, response *http.Response) {
	if s == nil || request == nil || account == nil || response == nil || response.StatusCode != http.StatusOK {
		return
	}
	if !isOpenAICodexTicketAccount(account) || !s.openAICodexTicketEnabledContext(request.Context()) {
		return
	}
	state := extractOpenAICodexTurnState(response.Header)
	if state == "" {
		return
	}
	model := normalizeOpenAICodexTicketModel(openAICodexTicketModelFromRequest(request))
	if model == "" || !s.openAICodexTicketGatedModel(model) {
		return
	}
	cfg := s.openAICodexTicketConfig()
	policy := resolveOpenAICodexTicketPolicyForModel(account, cfg, model)
	if !policy.Enabled {
		return
	}
	if len(state) != policy.TargetLength || !strings.HasPrefix(state, openAICodexTicketStatePrefix) {
		return
	}

	now := time.Now()
	refreshBefore := time.Duration(cfg.RefreshBeforeSeconds) * time.Second
	existing := s.lookupOpenAICodexTicket(account, model)
	if existing.valid(now, policy.TargetLength) && !existing.needsRefresh(now, refreshBefore) {
		return
	}
	ticket := &openAICodexTicket{
		AccountID:  account.ID,
		Model:      model,
		State:      state,
		Length:     len(state),
		CapturedAt: now,
		ExpiresAt:  now.Add(time.Duration(cfg.TTLSeconds) * time.Second),
		Attempts:   1,
	}
	observation := &openAICodexTicketObservation{
		Model:        model,
		TargetLength: policy.TargetLength,
		Length:       ticket.Length,
		HTTPStatus:   response.StatusCode,
		Outcome:      "target",
		ObservedAt:   now,
		NextProbeAt:  ticket.ExpiresAt.Add(-refreshBefore),
	}
	s.storeOpenAICodexTicketWithObservation(context.WithoutCancel(request.Context()), account, ticket, observation)
	logger.L().Info("openai_codex_ticket captured from production response",
		zap.Int64("account_id", account.ID),
		zap.String("model", model),
		zap.Int("length", ticket.Length),
	)
}

func openAICodexTicketModelFromRequest(request *http.Request) string {
	if request == nil {
		return ""
	}
	if request.GetBody == nil {
		if request.Body == nil {
			return ""
		}
		raw, err := io.ReadAll(io.LimitReader(request.Body, 64*1024))
		if err != nil {
			return ""
		}
		request.Body = io.NopCloser(bytes.NewReader(raw))
		return extractOpenAICodexTicketModel(raw)
	}
	body, err := request.GetBody()
	if err != nil {
		return ""
	}
	defer func() {
		_ = body.Close()
	}()
	raw, err := io.ReadAll(io.LimitReader(body, 64*1024))
	if err != nil {
		return ""
	}
	return extractOpenAICodexTicketModel(raw)
}

func (s *OpenAIGatewayService) openAICodexTicketRetryIntervals(ctx context.Context) (miss, rateLimit time.Duration) {
	miss = openAICodexTicketMissingRetryInterval
	rateLimit = openAICodexTicketRateLimitBackoff
	if s == nil || s.settingService == nil {
		return miss, rateLimit
	}
	policy := s.settingService.GetOpenAICodexTicketRetryPolicy(
		ctx,
		int(miss/time.Second),
		int(rateLimit/time.Second),
	)
	if policy.MissRetrySeconds > 0 {
		miss = time.Duration(policy.MissRetrySeconds) * time.Second
	}
	if policy.RateLimitRetrySeconds > 0 {
		rateLimit = time.Duration(policy.RateLimitRetrySeconds) * time.Second
	}
	return miss, rateLimit
}

func (s *OpenAIGatewayService) openAICodexTicketGatedModel(model string) bool {
	model = normalizeOpenAICodexTicketModel(model)
	if model == "" || !s.openAICodexTicketEnabled() {
		return false
	}
	for _, item := range s.openAICodexTicketConfig().Models {
		if normalizeOpenAICodexTicketModel(item) == model {
			return true
		}
	}
	return false
}

// OpenAICodexTicketStatus 是给管理端看的门票摘要，不含 state blob。
type OpenAICodexTicketStatus struct {
	Model              string     `json:"model"`
	Length             int        `json:"length,omitempty"`
	ObservedLength     int        `json:"observed_length,omitempty"`
	ObservedHTTPStatus int        `json:"observed_http_status,omitempty"`
	ObservedAt         *time.Time `json:"observed_at,omitempty"`
	NextProbeAt        *time.Time `json:"next_probe_at,omitempty"`
	ObservationOutcome string     `json:"observation_outcome,omitempty"`
	TicketType         string     `json:"ticket_type"`
	TargetLength       int        `json:"target_length"`
	TargetMode         string     `json:"target_mode"`
	TargetSource       string     `json:"target_source"`
	MissingPolicy      string     `json:"missing_policy"`
	PlanType           string     `json:"plan_type,omitempty"`
	Ready              bool       `json:"ready"`
	RemainingSeconds   int64      `json:"remaining_seconds"`
	Blocked            bool       `json:"blocked"`
	ExpiresAt          *time.Time `json:"expires_at,omitempty"`
}

func OpenAICodexTicketStatuses(account *Account, cfg config.OpenAICodexTicketConfig, now time.Time) []OpenAICodexTicketStatus {
	if !cfg.Enabled || !isOpenAICodexTicketAccount(account) {
		return nil
	}
	models := cfg.Models
	if len(models) == 0 {
		models = []string{openAICodexTicketDefaultModel, openAICodexTicketDefaultSolModel}
	}
	out := make([]OpenAICodexTicketStatus, 0, len(models))
	for _, model := range models {
		model = normalizeOpenAICodexTicketModel(model)
		if model == "" {
			continue
		}
		policy := resolveOpenAICodexTicketPolicyForModel(account, cfg, model)
		status := OpenAICodexTicketStatus{
			Model:         model,
			TicketType:    "missing",
			TargetLength:  policy.TargetLength,
			TargetMode:    policy.TargetMode,
			TargetSource:  policy.TargetSource,
			MissingPolicy: policy.MissingPolicy,
			PlanType:      policy.PlanType,
		}
		if !policy.Enabled {
			status.TicketType = "disabled"
			out = append(out, status)
			continue
		}
		ticket := parseOpenAICodexTicketFromAny(0, model, nil)
		observation := parseOpenAICodexTicketObservationFromAny(model, nil)
		if account != nil && account.Extra != nil {
			ticket = parseOpenAICodexTicketFromAny(account.ID, model, account.Extra[openAICodexTicketExtraKey(model)])
			observation = parseOpenAICodexTicketObservationFromAny(model, account.Extra[openAICodexTicketObservationExtraKey(model)])
		}
		if ticket != nil {
			if !ticket.ExpiresAt.IsZero() && !now.Before(ticket.ExpiresAt) {
				status.TicketType = "expired"
			} else if ticket.Length != policy.TargetLength {
				status.TicketType = "non_target"
			}
		}
		if observation != nil {
			status.ObservedLength = observation.Length
			status.ObservedHTTPStatus = observation.HTTPStatus
			status.ObservationOutcome = observation.Outcome
			observedAt := observation.ObservedAt
			status.ObservedAt = &observedAt
			if !observation.NextProbeAt.IsZero() {
				next := observation.NextProbeAt
				status.NextProbeAt = &next
			}
			if observation.Outcome == "rate_limited" || observation.Outcome == "quota_exhausted" || observation.Outcome == "error" {
				status.TicketType = observation.Outcome
			} else if observation.Length > 0 && observation.Length != policy.TargetLength && !status.Ready {
				status.TicketType = "non_target"
			}
		}
		if ticket.valid(now, policy.TargetLength) {
			status.Ready = true
			status.Length = ticket.Length
			status.TicketType = "target"
			remaining := int64(ticket.ExpiresAt.Sub(now) / time.Second)
			if remaining < 0 {
				remaining = 0
			}
			status.RemainingSeconds = remaining
			exp := ticket.ExpiresAt
			status.ExpiresAt = &exp
		}
		status.Blocked = policy.MissingPolicy == openAICodexTicketMissingPolicyPause && !status.Ready
		out = append(out, status)
	}
	return out
}

func parseOpenAICodexTicketObservationFromAny(model string, raw any) *openAICodexTicketObservation {
	if raw == nil {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var observation openAICodexTicketObservation
	if err := json.Unmarshal(b, &observation); err != nil {
		return nil
	}
	observation.Model = normalizeOpenAICodexTicketModel(model)
	if observation.ObservedAt.IsZero() {
		return nil
	}
	return &observation
}

func (s *OpenAIGatewayService) openAICodexTicketEnabled() bool {
	return s.openAICodexTicketEnabledContext(context.Background())
}

func (s *OpenAIGatewayService) openAICodexTicketEnabledContext(ctx context.Context) bool {
	if s == nil {
		return false
	}
	fallback := s.cfg != nil && s.cfg.Gateway.OpenAICodexTicket.Enabled
	if s.settingService != nil {
		return s.settingService.GetOpenAICodexTicketEnabled(ctx, fallback)
	}
	return fallback
}

func (s *OpenAIGatewayService) openAICodexTicketHarvestProxyURL() string {
	return s.openAICodexTicketHarvestProxyURLContext(context.Background())
}

func (s *OpenAIGatewayService) openAICodexTicketHarvestProxyURLContext(ctx context.Context) string {
	if s.settingService != nil {
		if proxy := s.settingService.GetOpenAICodexTicketHarvestProxyURL(ctx); proxy != "" {
			return proxy
		}
	}
	return strings.TrimSpace(s.openAICodexTicketConfig().HarvestProxyURL)
}

func (t *openAICodexTicket) valid(now time.Time, targetLen int) bool {
	if t == nil {
		return false
	}
	state := strings.TrimSpace(t.State)
	if len(state) != targetLen || t.Length != targetLen || !strings.HasPrefix(state, openAICodexTicketStatePrefix) {
		return false
	}
	if t.ExpiresAt.IsZero() || !now.Before(t.ExpiresAt) {
		return false
	}
	return true
}

func (t *openAICodexTicket) needsRefresh(now time.Time, refreshBefore time.Duration) bool {
	if t == nil || t.ExpiresAt.IsZero() {
		return true
	}
	return !t.ExpiresAt.After(now.Add(refreshBefore))
}

func (s *OpenAIGatewayService) lookupOpenAICodexTicket(account *Account, model string) *openAICodexTicket {
	if s == nil || account == nil || account.ID <= 0 {
		return nil
	}
	model = normalizeOpenAICodexTicketModel(model)
	if model == "" {
		return nil
	}
	key := openAICodexTicketKey(account.ID, model)
	targetLen := openAICodexTicketPersonalTargetLength
	if s != nil {
		targetLen = resolveOpenAICodexTicketPolicyForModel(account, s.openAICodexTicketConfig(), model).TargetLength
	}
	now := time.Now()
	var mem *openAICodexTicket
	if raw, ok := s.openaiCodexTickets.Load(key); ok {
		mem, _ = raw.(*openAICodexTicket)
	}
	var extra *openAICodexTicket
	if account.Extra != nil {
		extra = parseOpenAICodexTicketFromAny(account.ID, model, account.Extra[openAICodexTicketExtraKey(model)])
	}
	if extra.valid(now, targetLen) && (mem == nil || extra.CapturedAt.After(mem.CapturedAt)) {
		s.openaiCodexTickets.Store(key, extra)
		return extra
	}
	if mem.valid(now, targetLen) {
		return mem
	}
	if extra != nil {
		s.openaiCodexTickets.Store(key, extra)
		return extra
	}
	if mem != nil {
		s.openaiCodexTickets.Delete(key)
	}
	return nil
}

func parseOpenAICodexTicketFromAny(accountID int64, model string, raw any) *openAICodexTicket {
	if raw == nil {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var ticket openAICodexTicket
	if err := json.Unmarshal(b, &ticket); err != nil {
		return nil
	}
	ticket.AccountID = accountID
	if strings.TrimSpace(model) != "" {
		ticket.Model = model
	}
	ticket.State = strings.TrimSpace(ticket.State)
	if ticket.Length == 0 {
		ticket.Length = len(ticket.State)
	}
	if ticket.State == "" {
		return nil
	}
	return &ticket
}

func (s *OpenAIGatewayService) storeOpenAICodexTicket(ctx context.Context, account *Account, ticket *openAICodexTicket) {
	if s == nil || account == nil || ticket == nil || account.ID <= 0 {
		return
	}
	model := normalizeOpenAICodexTicketModel(ticket.Model)
	ticket.Model = model
	ticket.AccountID = account.ID
	s.openaiCodexTickets.Store(openAICodexTicketKey(account.ID, model), ticket)
	if s.accountRepo == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
		openAICodexTicketExtraKey(model): ticket,
	}); err != nil {
		logger.L().Warn("openai_codex_ticket persist failed",
			zap.Int64("account_id", account.ID),
			zap.String("model", model),
			zap.Error(err),
		)
	}
}

func (s *OpenAIGatewayService) storeOpenAICodexTicketObservation(ctx context.Context, account *Account, observation *openAICodexTicketObservation) {
	if s == nil || account == nil || observation == nil || account.ID <= 0 || s.accountRepo == nil || ctx.Err() != nil {
		return
	}
	observation.Model = normalizeOpenAICodexTicketModel(observation.Model)
	if observation.Model == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
		openAICodexTicketObservationExtraKey(observation.Model): observation,
	}); err != nil {
		logger.L().Warn("openai_codex_ticket observation persist failed",
			zap.Int64("account_id", account.ID),
			zap.String("model", observation.Model),
			zap.Error(err),
		)
	}
}

func (s *OpenAIGatewayService) storeOpenAICodexTicketWithObservation(ctx context.Context, account *Account, ticket *openAICodexTicket, observation *openAICodexTicketObservation) {
	if s == nil || account == nil || ticket == nil || observation == nil || account.ID <= 0 {
		return
	}
	model := normalizeOpenAICodexTicketModel(ticket.Model)
	ticket.Model = model
	ticket.AccountID = account.ID
	observation.Model = model
	s.openaiCodexTickets.Store(openAICodexTicketKey(account.ID, model), ticket)
	if s.accountRepo == nil || ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
		openAICodexTicketExtraKey(model):            ticket,
		openAICodexTicketObservationExtraKey(model): observation,
	}); err != nil {
		logger.L().Warn("openai_codex_ticket persist failed",
			zap.Int64("account_id", account.ID), zap.String("model", model), zap.Error(err))
	}
}

func openAICodexTicketNextProbeAt(account *Account, model string, targetLength int) time.Time {
	if account == nil || account.Extra == nil {
		return time.Time{}
	}
	observation := parseOpenAICodexTicketObservationFromAny(model, account.Extra[openAICodexTicketObservationExtraKey(model)])
	if observation == nil {
		return time.Time{}
	}
	if observation.TargetLength > 0 && observation.TargetLength != targetLength {
		return time.Time{}
	}
	return observation.NextProbeAt
}

// applyOpenAICodexTicket 在出站请求上覆盖 x-codex-turn-state。
// 请求路径只注入已捕获的有效门票，不现场打票；无票则返回
// ErrOpenAICodexTicketUnavailable。打票由后台 harvester 完成。
func (s *OpenAIGatewayService) applyOpenAICodexTicket(ctx context.Context, account *Account, model string, h http.Header) error {
	if s == nil || h == nil || !isOpenAICodexTicketAccount(account) || !s.openAICodexTicketEnabledContext(ctx) {
		return nil
	}
	model = normalizeOpenAICodexTicketModel(model)
	if model == "" || !s.openAICodexTicketGatedModel(model) {
		return nil
	}
	cfg := s.openAICodexTicketConfig()
	policy := resolveOpenAICodexTicketPolicyForModel(account, cfg, model)
	if !policy.Enabled {
		return nil
	}
	ticket := s.lookupOpenAICodexTicket(account, model)
	if ticket.valid(time.Now(), policy.TargetLength) {
		h.Set(openAICodexTurnStateHeader, ticket.State)
		return nil
	}
	if policy.MissingPolicy == openAICodexTicketMissingPolicyAllow {
		return nil
	}
	return ErrOpenAICodexTicketUnavailable
}

// openAICodexTicketOutboundModel 预测本请求真正出站的模型名，也就是
// applyOpenAICodexTicket 注入时读到的 body.model。
//
// 调度门控与注入必须按同一个模型名判定门票。普通请求下二者同源：Forward 的
// upstreamModel 与本函数都走 resolveOpenAIAccountUpstreamModelForRequest，且
// Forward 会把 body.model 改写成该值后才注入。但 /responses/compact 例外——
// Forward 会把出站模型进一步改写为 compact 映射或 gateway.openai_compact_model
// （默认非空），此时若门控仍按客户端原始模型判定，就会把「实际出站是非门控
// 模型、根本不需要票」的 compact 请求整片误拦成不可调度。
func (s *OpenAIGatewayService) openAICodexTicketOutboundModel(account *Account, requestedModel string, requireCompact bool) string {
	model := strings.TrimSpace(requestedModel)
	if account == nil || model == "" {
		return model
	}
	if !account.IsOpenAI() {
		return canonicalOpenAIAccountSchedulingModel(account, model)
	}
	_, upstreamModel := resolveOpenAIForwardMappedModels(account, model, requireCompact)
	if requireCompact {
		// 与 Forward 同序：compact 兜底模型优先于普通/compact 映射结果。
		if compactModel := strings.TrimSpace(s.resolveOpenAICompactFallbackModel(account, model)); compactModel != "" {
			upstreamModel = compactModel
		}
	}
	if upstreamModel = strings.TrimSpace(upstreamModel); upstreamModel != "" {
		return upstreamModel
	}
	return model
}

// outboundModel 必须是真正会发给上游的模型名（openAICodexTicketOutboundModel），
// 不是客户端原始模型：注入侧读的是出站 body.model，两侧口径必须一致。
func (s *OpenAIGatewayService) openAICodexTicketBlocksAccount(account *Account, outboundModel string) bool {
	if s == nil || !isOpenAICodexTicketAccount(account) || !s.openAICodexTicketEnabled() {
		return false
	}
	cfg := s.openAICodexTicketConfig()
	policy := resolveOpenAICodexTicketPolicyForModel(account, cfg, outboundModel)
	if !policy.Enabled {
		return false
	}
	if policy.MissingPolicy == openAICodexTicketMissingPolicyAllow {
		return false
	}
	model := normalizeOpenAICodexTicketModel(outboundModel)
	if !s.openAICodexTicketGatedModel(model) {
		return false
	}
	ticket := s.lookupOpenAICodexTicket(account, model)
	return !ticket.valid(time.Now(), policy.TargetLength)
}

func (s *OpenAIGatewayService) fireOpenAICodexTicketProbe(ctx context.Context, account *Account, token, model, proxyURL string, attemptTimeout time.Duration) (state string, status int, err error) {
	attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
	defer cancel()

	body := []byte(`{"model":` + jsonString(model) + `,"store":false,"stream":true,"instructions":"Reply with exactly: pong","input":[{"role":"user","content":[{"type":"input_text","text":"ping"}]}]}`)
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, chatgptCodexURL, bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAIHarvest))
	req.Close = true
	req.Host = "chatgpt.com"
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("session_id", uuid.NewString())
	if err := resolveAndSetOpenAIChatGPTAccountHeaders(attemptCtx, s.accountRepo, req.Header, account); err != nil {
		return "", 0, err
	}
	applyOpenAICodexTicketHarvestIdentity(req.Header, model)

	// Synthetic probes must use the dedicated no-reuse transport even when the
	// production account is bound to a plugin. This also avoids reading pluginManager
	// while handlers are still wiring it during gateway construction.
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		return "", 0, err
	}
	if resp == nil {
		return "", 0, errors.New("nil upstream response")
	}
	// Only the response header is needed; no connection will be reused.
	defer func() {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()
	return extractOpenAICodexTurnState(resp.Header), resp.StatusCode, nil
}

func jsonString(v string) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `""`
	}
	return string(b)
}

func applyOpenAICodexTicketHarvestIdentity(h http.Header, model string) {
	ensureCodexIdentityHeaders(h)
	enforceCodexIdentityHeaders(h)
	version := strings.TrimSpace(h.Get("version"))
	if needsOpenAICodexAstraVersion(model) && (version == "" || CompareVersions(version, openAICodexAstraMinVersion) < 0) {
		h.Set("version", openAICodexAstraMinVersion)
		h.Set("user-agent", buildCodexCLIUserAgent(openAICodexAstraMinVersion))
		h.Set("originator", openai.CodexDefaultOriginator)
	}
}

func needsOpenAICodexAstraVersion(model string) bool {
	m := strings.ToLower(normalizeOpenAICodexTicketModel(model))
	return strings.Contains(m, "gpt-6") || strings.Contains(m, "astra")
}

func (s *OpenAIGatewayService) StartOpenAICodexTicketHarvester() {
	if s == nil {
		return
	}
	s.openaiCodexTicketLifecycleMu.Lock()
	defer s.openaiCodexTicketLifecycleMu.Unlock()
	if s.openaiCodexTicketStopped || s.openaiCodexTicketDone != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.openaiCodexTicketCancel = cancel
	s.openaiCodexTicketDone = done
	go func() {
		defer close(done)
		s.openAICodexTicketHarvestLoop(ctx)
	}()
	logger.L().Info("openai_codex_ticket harvester started",
		zap.Int("ttl_seconds", s.openAICodexTicketConfig().TTLSeconds),
		zap.Int("target_length", s.openAICodexTicketConfig().TargetLength),
		zap.Strings("models", s.openAICodexTicketConfig().Models),
	)
}

func (s *OpenAIGatewayService) StopOpenAICodexTicketHarvester() {
	if s == nil {
		return
	}
	s.openaiCodexTicketLifecycleMu.Lock()
	s.openaiCodexTicketStopped = true
	cancel, done := s.openaiCodexTicketCancel, s.openaiCodexTicketDone
	s.openaiCodexTicketLifecycleMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// ProbeOpenAICodexTicket performs one immediate probe for an account/model and
// returns the redacted status view. It is used by the admin "manual harvest"
// action and shares the same proxy, identity, validation, and persistence path
// as the background harvester.
func (s *OpenAIGatewayService) ProbeOpenAICodexTicket(ctx context.Context, accountID int64, model string) ([]OpenAICodexTicketStatus, error) {
	if s == nil || s.accountRepo == nil {
		return nil, errors.New("codex ticket service unavailable")
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	model = normalizeOpenAICodexTicketModel(model)
	if model == "" || !isOpenAICodexTicketAccount(account) {
		return nil, errors.New("account or model is not eligible for Codex tickets")
	}
	cfg := s.openAICodexTicketConfig()
	policy := resolveOpenAICodexTicketPolicyForModel(account, cfg, model)
	if !policy.Enabled {
		return nil, fmt.Errorf("codex ticket model %s is disabled", model)
	}
	if !s.openAICodexTicketEnabledContext(ctx) {
		return nil, errors.New("codex ticket harvesting is disabled")
	}
	s.probeOnceOpenAICodexTicket(ctx, account, model)
	if refreshed, refreshErr := s.accountRepo.GetByID(ctx, accountID); refreshErr == nil && refreshed != nil {
		account = refreshed
	}
	return OpenAICodexTicketStatuses(account, s.openAICodexTicketConfig(), time.Now()), nil
}

func (s *OpenAIGatewayService) openAICodexTicketHarvestLoop(ctx context.Context) {
	interval := time.Duration(s.openAICodexTicketConfig().HarvestProbeIntervalSeconds) * time.Second
	if interval <= 0 || interval > openAICodexTicketHarvestScanInterval {
		interval = openAICodexTicketHarvestScanInterval
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			// This loop only scans local account state. Upstream probes are still
			// gated by each account/model's NextProbeAt, so a short scan interval
			// makes panel-configured retry times effective without recreating the
			// old high-frequency upstream probe storm.
			s.refreshOpenAICodexTickets(ctx)
			timer.Reset(interval)
		}
	}
}

// refreshOpenAICodexTickets probes each account/model with a missing or soon-to-expire
// ticket once. It returns true when at least one eligible account/model was probed.
// Per-observation NextProbeAt is authoritative: normal failures back off for 30
// minutes, 429s for one hour (or the quota reset), and confirmed tickets wait for
// their normal refresh window.
func (s *OpenAIGatewayService) refreshOpenAICodexTickets(ctx context.Context) bool {
	if s == nil || s.accountRepo == nil || ctx.Err() != nil || !s.openAICodexTicketEnabledContext(ctx) {
		return false
	}
	accounts, err := s.accountRepo.ListSchedulableByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		logger.L().Warn("openai_codex_ticket list accounts failed", zap.Error(err))
		return false
	}
	cfg := s.openAICodexTicketConfig()
	now := time.Now()
	refreshBefore := time.Duration(cfg.RefreshBeforeSeconds) * time.Second
	var wg sync.WaitGroup
	probed := 0
	for i := range accounts {
		account := accounts[i]
		if !account.IsSchedulable() || !isOpenAICodexTicketAccount(&account) {
			continue
		}
		for _, model := range cfg.Models {
			model := normalizeOpenAICodexTicketModel(model)
			if model == "" {
				continue
			}
			policy := resolveOpenAICodexTicketPolicyForModel(&account, cfg, model)
			if !policy.Enabled {
				continue
			}
			// 已有一张有效且未临近过期的目标票 → 本周期不打，省得白刷。
			if t := s.lookupOpenAICodexTicket(&account, model); t.valid(now, policy.TargetLength) && !t.needsRefresh(now, refreshBefore) {
				continue
			}
			if nextProbeAt := openAICodexTicketNextProbeAt(&account, model, policy.TargetLength); !nextProbeAt.IsZero() && now.Before(nextProbeAt) {
				continue
			}
			acc := account
			// Token/header helpers may update account metadata; each model owns its maps.
			acc.Extra = maps.Clone(account.Extra)
			acc.Credentials = maps.Clone(account.Credentials)
			probed++
			wg.Add(1)
			go func(acc Account, model string) {
				defer wg.Done()
				s.probeOnceOpenAICodexTicket(ctx, &acc, model)
			}(acc, model)
		}
	}
	wg.Wait()
	if probed > 0 {
		logger.L().Info("openai_codex_ticket probe cycle", zap.Int("probed", probed))
	}
	return probed > 0
}

// probeOnceOpenAICodexTicket 走打票代理打一发。命中合格 292（HTTP 200、长度==target、
// gAAAAA 前缀）就落库；否则记 Info miss，交给下个周期重试。同一 key 并发去重，避免上一发还没
// 回来又叠一发。
func (s *OpenAIGatewayService) probeOnceOpenAICodexTicket(ctx context.Context, account *Account, model string) {
	if s == nil || !isOpenAICodexTicketAccount(account) || ctx.Err() != nil || !s.openAICodexTicketEnabledContext(ctx) {
		return
	}
	cfg := s.openAICodexTicketConfig()
	policy := resolveOpenAICodexTicketPolicyForModel(account, cfg, model)
	if !policy.Enabled {
		return
	}
	missRetryInterval, rateLimitRetryInterval := s.openAICodexTicketRetryIntervals(ctx)
	proxyURL := s.openAICodexTicketHarvestProxyURLContext(ctx)
	if proxyURL == "" || s.httpUpstream == nil || ctx.Err() != nil {
		return
	}
	key := openAICodexTicketKey(account.ID, model)
	_, _, _ = s.openaiCodexTicketFlight.Do(key, func() (any, error) {
		now := time.Now()
		token, _, err := s.GetAccessToken(ctx, account)
		if err != nil || strings.TrimSpace(token) == "" {
			s.storeOpenAICodexTicketObservation(ctx, account, &openAICodexTicketObservation{
				Model: model, TargetLength: policy.TargetLength, Outcome: "token_error", ObservedAt: now,
				NextProbeAt: now.Add(missRetryInterval), Error: "token unavailable",
			})
			logger.L().Info("openai_codex_ticket probe miss",
				zap.Int64("account_id", account.ID), zap.String("model", model),
				zap.String("reason", "token"), zap.Error(err))
			return nil, nil
		}
		state, status, perr := s.fireOpenAICodexTicketProbe(ctx, account, token, model, proxyURL, time.Duration(cfg.HarvestAttemptTimeoutSeconds)*time.Second)
		if perr != nil {
			s.storeOpenAICodexTicketObservation(ctx, account, &openAICodexTicketObservation{
				Model: model, TargetLength: policy.TargetLength, Outcome: "error", ObservedAt: now,
				NextProbeAt: now.Add(missRetryInterval), Error: truncateCodexTicketObservationError(perr.Error()),
			})
			logger.L().Info("openai_codex_ticket probe miss",
				zap.Int64("account_id", account.ID), zap.String("model", model),
				zap.String("reason", "error"), zap.Error(perr))
			return nil, nil
		}
		if status != http.StatusOK || state == "" || len(state) != policy.TargetLength || !strings.HasPrefix(state, openAICodexTicketStatePrefix) {
			outcome := "non_target"
			nextProbeAt := now.Add(missRetryInterval)
			if status == http.StatusTooManyRequests {
				outcome = "rate_limited"
				nextProbeAt = openAICodexTicketRateLimitRetryAt(account, now, rateLimitRetryInterval)
			} else if status < http.StatusOK || status >= http.StatusMultipleChoices {
				outcome = "http_error"
			}
			s.storeOpenAICodexTicketObservation(ctx, account, &openAICodexTicketObservation{
				Model: model, TargetLength: policy.TargetLength, Length: len(state), HTTPStatus: status, Outcome: outcome,
				ObservedAt: now, NextProbeAt: nextProbeAt,
			})
			logger.L().Info("openai_codex_ticket probe miss",
				zap.Int64("account_id", account.ID), zap.String("model", model),
				zap.Int("http", status), zap.Int("len", len(state)),
				zap.Int("target_len", policy.TargetLength), zap.Time("next_probe_at", nextProbeAt))
			return nil, nil
		}
		ticket := &openAICodexTicket{
			AccountID:  account.ID,
			Model:      model,
			State:      state,
			Length:     len(state),
			CapturedAt: now,
			ExpiresAt:  now.Add(time.Duration(cfg.TTLSeconds) * time.Second),
			Attempts:   1,
		}
		nextProbeAt := ticket.ExpiresAt.Add(-time.Duration(cfg.RefreshBeforeSeconds) * time.Second)
		observation := &openAICodexTicketObservation{
			Model: model, TargetLength: policy.TargetLength, Length: ticket.Length, HTTPStatus: status, Outcome: "target",
			ObservedAt: now, NextProbeAt: nextProbeAt,
		}
		s.storeOpenAICodexTicketWithObservation(ctx, account, ticket, observation)
		logger.L().Info("openai_codex_ticket harvested",
			zap.Int64("account_id", account.ID), zap.String("model", model),
			zap.Int("length", ticket.Length), zap.String("mode", "continuous"))
		return nil, nil
	})
}

// openAICodexTicketRateLimitRetryAt keeps a transient 429 away from the
// account for at least one hour. If the account's latest quota snapshot says a
// 5-hour or 7-day window is actually exhausted, wait for that window's real
// reset instead of hammering the account every hour.
func openAICodexTicketRateLimitRetryAt(account *Account, now time.Time, rateLimitBackoff time.Duration) time.Time {
	next := now.Add(rateLimitBackoff)
	if account == nil {
		return next
	}
	if readOpenAIQuotaUsedPercent(account.Extra, "7d") >= 100 {
		if resetAt, ok := openAICodexWindowResetAt(account.Extra, "7d"); ok && resetAt.After(now) && resetAt.After(next) {
			return resetAt
		}
	}
	if readOpenAIQuotaUsedPercent(account.Extra, "5h") >= 100 {
		if resetAt, ok := openAICodexWindowResetAt(account.Extra, "5h"); ok && resetAt.After(now) && resetAt.After(next) {
			return resetAt
		}
	}
	return next
}

func truncateCodexTicketObservationError(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 240 {
		return value[:240]
	}
	return value
}

// IsOpenAICodexTicketExtraKey identifies server-managed ticket material.
func IsOpenAICodexTicketExtraKey(key string) bool {
	return strings.HasPrefix(key, openAICodexTicketExtraKeyPrefix) ||
		strings.HasPrefix(key, openAICodexTicketObservationKeyPrefix)
}

// MergeOpenAICodexTicketExtra preserves only persisted tickets, never summaries or
// blobs supplied by an account edit. The repository repeats this under the row
// lock so a concurrent harvest cannot be overwritten by a stale admin snapshot.
func MergeOpenAICodexTicketExtra(extra, current map[string]any) map[string]any {
	result := maps.Clone(extra)
	for key := range result {
		if IsOpenAICodexTicketExtraKey(key) {
			delete(result, key)
		}
	}
	for key, value := range current {
		if IsOpenAICodexTicketExtraKey(key) {
			if result == nil {
				result = make(map[string]any)
			}
			result[key] = value
		}
	}
	return result
}

// ValidateOpenAICodexTicketHarvestProxyURL validates only syntax, without making
// a network request or including credentials in validation errors.
func ValidateOpenAICodexTicketHarvestProxyURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return errors.New("harvest proxy must be an HTTP(S) or SOCKS5(h) URL with a host and no path, query or fragment")
	}
	switch parsed.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return errors.New("harvest proxy scheme must be http, https, socks5 or socks5h")
	}
	if port := parsed.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("harvest proxy port must be between 1 and 65535")
		}
	}
	return nil
}

// MaskProxyURL never returns a stored proxy password, even for invalid legacy data.
func MaskProxyURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || ValidateOpenAICodexTicketHarvestProxyURL(raw) != nil {
		return ""
	}
	parsed, _ := url.Parse(raw)
	if parsed.User != nil {
		if _, ok := parsed.User.Password(); ok {
			parsed.User = url.UserPassword(parsed.User.Username(), "***")
		}
	}
	return parsed.String()
}

// IsMaskedProxyURL recognizes the exact password placeholder emitted by the API.
func IsMaskedProxyURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return true
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil {
		return false
	}
	password, ok := parsed.User.Password()
	return ok && password == "***"
}

// Credential shadows do not own tickets. Keep their existing forwarding policy
// instead of imposing a gate for a key the harvester never populates.
func isOpenAICodexTicketAccount(account *Account) bool {
	return account != nil && account.IsOpenAIOAuthLike() && !account.IsShadow()
}

// IsOpenAICodexTicketPrivateExtraKey also covers the retired account-level proxy
// override, whose credentials may remain in older account records.
func IsOpenAICodexTicketPrivateExtraKey(key string) bool {
	return IsOpenAICodexTicketExtraKey(key) || key == "codex_harvest_proxy_url"
}

// RedactOpenAICodexTicketExtra strips ephemeral ticket material from exports
// without changing the source account or unrelated backup fields.
func RedactOpenAICodexTicketExtra(extra map[string]any) map[string]any {
	redacted := maps.Clone(extra)
	for key := range redacted {
		if IsOpenAICodexTicketPrivateExtraKey(key) {
			delete(redacted, key)
		}
	}
	return redacted
}
