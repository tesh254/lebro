package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestParseDecimalCanonicalizesWithoutRounding(t *testing.T) {
	t.Parallel()
	for input, want := range map[string]Decimal{
		"0.000000": "0",
		"001":      "",
		"1.2300":   "1.23",
		"12e-3":    "0.012",
		"1.2E3":    "1200",
		"1e33":     "",
	} {
		got, err := ParseDecimal(input)
		if want == "" {
			if err == nil {
				t.Errorf("ParseDecimal(%q) accepted a noncanonical input form", input)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("ParseDecimal(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
}

func TestModelCostOmitsZeroEffectiveAt(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(ModelCost{Currency: "USD", Amount: "1", Source: CostEstimated})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"currency":"USD","amount":"1","source":"estimated"}` {
		t.Fatalf("ModelCost JSON = %s", encoded)
	}
}

func TestOfficialPricingResolverEstimatesSupportedDomains(t *testing.T) {
	t.Parallel()
	resolver := OfficialPricingResolver{}
	tests := []struct {
		name       string
		attempt    ModelAttemptRecord
		wantAmount Decimal
	}{
		{name: "openai", attempt: pricingAttempt("openai", "gpt-6-astra", PricingDomainOpenAI, ModelUsage{InputTokens: 1_000_000, CacheReadTokens: 200_000, OutputTokens: 100_000}), wantAmount: "13.2"},
		{name: "anthropic cache classes", attempt: pricingAttempt("anthropic", "claude-sonnet-4.6", PricingDomainAnthropic, ModelUsage{InputTokens: 50_000, CacheReadTokens: 50_000, CacheWriteTokens: 50_000, CacheWrite1hTokens: 20_000, OutputTokens: 50_000}), wantAmount: "1.1475"},
		{name: "gemini developer api", attempt: pricingAttempt("gemini", "gemini-2.5-flash", PricingDomainGemini, ModelUsage{InputTokens: 1_000_000, CacheReadTokens: 200_000, OutputTokens: 100_000}), wantAmount: "0.496"},
		{name: "vertex ai", attempt: pricingAttempt("vertexai", "publishers/google/models/gemini-2.5-flash", PricingDomainVertexAI, ModelUsage{InputTokens: 1_000_000, CacheReadTokens: 200_000, OutputTokens: 100_000}), wantAmount: "0.496"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cost, err := resolver.ResolveCost(context.Background(), test.attempt)
			if err != nil {
				t.Fatal(err)
			}
			if cost.Source != CostEstimated || cost.Amount != test.wantAmount || cost.Currency != "USD" || cost.CatalogVersion != OfficialPricingCatalogVersion {
				t.Fatalf("ResolveCost() = %#v, want estimated USD %s from catalog %s", cost, test.wantAmount, OfficialPricingCatalogVersion)
			}
		})
	}
}

func TestOfficialPricingResolverSplitsCacheInclusiveUsage(t *testing.T) {
	t.Parallel()
	cost, err := (OfficialPricingResolver{}).ResolveCost(context.Background(), pricingAttempt("openai", "gpt-6-astra", PricingDomainOpenAI, ModelUsage{InputTokens: 1_000, CacheReadTokens: 900, OutputTokens: 10}))
	if err != nil {
		t.Fatal(err)
	}
	if cost.Amount != "0.0024" || len(cost.Components) != 3 || cost.Components[0].Amount != "0.001" || cost.Components[1].Amount != "0.0009" || cost.Components[2].Amount != "0.0005" {
		t.Fatalf("cache-inclusive split = %#v", cost)
	}
}

func TestOfficialPricingResolverExplainsUnavailablePricing(t *testing.T) {
	t.Parallel()
	resolver := OfficialPricingResolver{}
	for name, attempt := range map[string]ModelAttemptRecord{
		"unknown model": pricingAttempt("openai", "future-model", PricingDomainOpenAI, ModelUsage{InputTokens: 1}),
		"unsupported tier": func() ModelAttemptRecord {
			attempt := pricingAttempt("anthropic", "claude-sonnet-4.6", PricingDomainAnthropic, ModelUsage{InputTokens: 1})
			attempt.Accounting.ServiceTier = "priority"
			return attempt
		}(),
		"missing cache rate":               pricingAttempt("openai", "gpt-5.6-sol", PricingDomainOpenAI, ModelUsage{InputTokens: 2, CacheWriteTokens: 1}),
		"long sonnet context":              pricingAttempt("anthropic", "claude-sonnet-5", PricingDomainAnthropic, ModelUsage{InputTokens: 200_001}),
		"long gemini context":              pricingAttempt("gemini", "gemini-3.8-flash", PricingDomainGemini, ModelUsage{InputTokens: 200_001}),
		"openrouter without reported cost": pricingAttempt("openrouter", "openai/gpt-6-astra", PricingDomainOpenRouter, ModelUsage{InputTokens: 1}),
	} {
		t.Run(name, func(t *testing.T) {
			cost, err := resolver.ResolveCost(context.Background(), attempt)
			if err != nil {
				t.Fatal(err)
			}
			if cost.Source != CostUnavailable || cost.Domain == "" || cost.UnavailableReason == "" || cost.Amount != "" {
				t.Fatalf("ResolveCost() = %#v, want an explicit domain-scoped unavailable result", cost)
			}
		})
	}
}

func TestOfficialPricingResolverUsesRoutedModelWhenProviderModelIsEmpty(t *testing.T) {
	t.Parallel()
	attempt := pricingAttempt("anthropic", "", PricingDomainAnthropic, ModelUsage{InputTokens: 1_000, OutputTokens: 1_000})
	attempt.RoutedModel = "claude-opus-5"
	cost, err := (OfficialPricingResolver{}).ResolveCost(context.Background(), attempt)
	if err != nil || cost.Source != CostEstimated || cost.Amount != "0.03" {
		t.Fatalf("ResolveCost() = %#v, %v", cost, err)
	}
}

func TestAggregateModelCostsGroupsExactAmountsByCurrencyAndSource(t *testing.T) {
	t.Parallel()
	repo := pagedAttemptRepository{records: []ModelAttemptRecord{
		{Accounting: ModelAccounting{Costs: []ModelCost{{Currency: "usd", Amount: "0.1000001", Source: CostEstimated}}}},
		{Accounting: ModelAccounting{Costs: []ModelCost{{Currency: "USD", Amount: "0.2000002", Source: CostEstimated}, {Currency: "USD", Amount: "0", Source: CostProviderReported}}}},
		{Accounting: ModelAccounting{Costs: []ModelCost{UnavailableModelCost(PricingDomainOpenAI, CostUnavailableUnknownModel)}}},
	}}
	totals, err := AggregateModelCosts(context.Background(), repo, ModelAttemptFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(totals) != 2 || totals[0].Source != CostEstimated || totals[0].Amount != "0.3000003" || totals[1].Source != CostProviderReported || totals[1].Amount != "0" {
		t.Fatalf("AggregateModelCosts() = %#v", totals)
	}
}

func TestAggregateModelCostsSkipsInvalidCustomStoreRows(t *testing.T) {
	t.Parallel()
	repo := pagedAttemptRepository{records: []ModelAttemptRecord{{Accounting: ModelAccounting{Costs: []ModelCost{
		{Currency: "USD", Amount: "bad", Source: CostEstimated},
		{Currency: "USD", Amount: "1.25", Source: CostEstimated},
	}}}}}
	totals, err := AggregateModelCosts(context.Background(), repo, ModelAttemptFilter{})
	if err != nil || len(totals) != 1 || totals[0].Amount != "1.25" {
		t.Fatalf("AggregateModelCosts() = %#v, %v", totals, err)
	}
}

func TestAgentCostResolverIsFallbackAndFailureIsIsolated(t *testing.T) {
	t.Parallel()
	unavailable := ModelAccounting{ProviderRequestID: "req-1", Costs: []ModelCost{UnavailableModelCost(PricingDomainOpenAI, CostUnavailableProviderOmitted)}}
	called := 0
	agent := &Agent{costResolver: CostResolverFunc(func(_ context.Context, attempt ModelAttemptRecord) (ModelCost, error) {
		called++
		if attempt.Model != "gpt-6-astra" || attempt.Usage.TotalTokens != 3 || attempt.Accounting.ProviderRequestID != "req-1" {
			t.Fatalf("resolver attempt = %#v", attempt)
		}
		return ModelCost{}, errors.New("pricing service offline")
	})}
	got := agent.resolveAccounting(context.Background(), nil, ModelResponse{Usage: ModelUsage{InputTokens: 2, OutputTokens: 1, TotalTokens: 3}, Accounting: unavailable}, nil, "gpt-6-astra")
	if called != 1 || len(got.Costs) != 2 || got.Costs[1].Source != CostUnavailable || got.Costs[1].UnavailableReason != CostUnavailableResolverFailed {
		t.Fatalf("resolved accounting = %#v, calls=%d", got, called)
	}

	provider := ModelAccounting{Costs: []ModelCost{{Currency: "CREDITS", Amount: "0", Source: CostProviderReported, Domain: PricingDomainOpenRouter}}}
	got = agent.resolveAccounting(context.Background(), nil, ModelResponse{Accounting: provider}, nil, "vendor/model")
	if called != 1 || len(got.Costs) != 1 || got.Costs[0].Source != CostProviderReported {
		t.Fatalf("provider accounting should bypass resolver: %#v, calls=%d", got, called)
	}
}

func TestAgentCostResolverRecordsRejectedAndInvalidResultsSeparately(t *testing.T) {
	t.Parallel()
	base := ModelResponse{Accounting: ModelAccounting{Costs: []ModelCost{UnavailableModelCost(PricingDomainOpenAI, CostUnavailableProviderOmitted)}}}
	for name, resolved := range map[string]ModelCost{
		"provider reported": {Currency: "USD", Amount: "1", Source: CostProviderReported},
		"invalid":           {Currency: "USD", Amount: "bad", Source: CostEstimated},
	} {
		t.Run(name, func(t *testing.T) {
			agent := &Agent{costResolver: CostResolverFunc(func(context.Context, ModelAttemptRecord) (ModelCost, error) { return resolved, nil })}
			got := agent.resolveAccounting(context.Background(), nil, base, nil, "gpt-6-astra")
			last := got.Costs[len(got.Costs)-1]
			if last.Source != CostUnavailable {
				t.Fatalf("accounting = %#v", got)
			}
			if name == "provider reported" && last.UnavailableReason != CostUnavailableResolverRejected {
				t.Fatalf("rejected result = %#v", last)
			}
			if name == "invalid" && last.UnavailableReason != CostUnavailableResolverInvalid {
				t.Fatalf("invalid result = %#v", last)
			}
		})
	}
}

func pricingAttempt(provider ProviderID, model string, domain PricingDomain, usage ModelUsage) ModelAttemptRecord {
	return ModelAttemptRecord{Provider: provider, Model: model, Usage: usage, Accounting: ModelAccounting{Costs: []ModelCost{UnavailableModelCost(domain, CostUnavailableProviderOmitted)}}}
}

type pagedAttemptRepository struct{ records []ModelAttemptRecord }

func (p pagedAttemptRepository) SaveModelAttempts(context.Context, []ModelAttemptRecord) error {
	return nil
}
func (p pagedAttemptRepository) ListModelAttempts(_ context.Context, _ ModelAttemptFilter, page PageRequest) (Page[ModelAttemptRecord], error) {
	if page.Cursor != "" {
		return Page[ModelAttemptRecord]{}, nil
	}
	return Page[ModelAttemptRecord]{Records: p.records}, nil
}
