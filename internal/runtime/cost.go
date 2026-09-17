package runtime

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Decimal is a canonical, non-negative base-10 amount. It deliberately does
// not expose floating-point construction: accounting values retain every
// decimal digit supplied by a provider or pricing catalog.
type Decimal string

var decimalPattern = regexp.MustCompile(`^(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)

// ParseDecimal validates and canonicalizes a non-negative decimal. Exponents
// are expanded so durable records and equality checks have one representation.
func ParseDecimal(value string) (Decimal, error) {
	value = strings.TrimSpace(value)
	if !decimalPattern.MatchString(value) {
		return "", fmt.Errorf("lebro: invalid decimal %q", value)
	}
	mantissa, exponent := value, 0
	if at := strings.IndexAny(mantissa, "eE"); at >= 0 {
		if _, err := fmt.Sscanf(mantissa[at+1:], "%d", &exponent); err != nil {
			return "", fmt.Errorf("lebro: invalid decimal exponent %q", value)
		}
		if exponent < -32 || exponent > 32 {
			return "", fmt.Errorf("lebro: decimal exponent %q exceeds supported range", value)
		}
		mantissa = mantissa[:at]
	}
	parts := strings.SplitN(mantissa, ".", 2)
	digits, scale := parts[0], 0
	if len(parts) == 2 {
		digits += parts[1]
		scale = len(parts[1])
	}
	scale -= exponent
	if scale < 0 {
		digits += strings.Repeat("0", -scale)
		scale = 0
	}
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return Decimal("0"), nil
	}
	if scale >= len(digits) {
		digits = strings.Repeat("0", scale-len(digits)+1) + digits
	}
	if scale > 0 {
		at := len(digits) - scale
		digits = digits[:at] + "." + digits[at:]
		digits = strings.TrimRight(digits, "0")
		digits = strings.TrimSuffix(digits, ".")
	}
	digits = strings.TrimLeft(digits, "0")
	if digits == "" || strings.HasPrefix(digits, ".") {
		digits = "0" + digits
	}
	return Decimal(digits), nil
}

func (d Decimal) validate() error {
	if d == "" {
		return nil
	}
	canonical, err := ParseDecimal(string(d))
	if err != nil || canonical != d {
		return fmt.Errorf("lebro: decimal %q is not canonical", d)
	}
	return nil
}

type CostSource string

const (
	CostUnavailable       CostSource = "unavailable"
	CostProviderReported  CostSource = "provider_reported"
	CostDeveloperSupplied CostSource = "developer_supplied"
	CostEstimated         CostSource = "estimated"
)

type PricingDomain string

const (
	PricingDomainOpenRouter       PricingDomain = "openrouter"
	PricingDomainOpenAI           PricingDomain = "openai_api"
	PricingDomainOpenAICompatible PricingDomain = "openai_compatible"
	PricingDomainAnthropic        PricingDomain = "anthropic_api"
	PricingDomainGemini           PricingDomain = "gemini_developer_api"
	PricingDomainVertexAI         PricingDomain = "vertex_ai"
)

type CostUnavailableReason string

const (
	CostUnavailableProviderOmitted       CostUnavailableReason = "provider_omitted"
	CostUnavailableUnknownModel          CostUnavailableReason = "unknown_model"
	CostUnavailableMissingUsage          CostUnavailableReason = "missing_usage"
	CostUnavailableMissingDimension      CostUnavailableReason = "missing_pricing_dimension"
	CostUnavailableUnsupportedTerms      CostUnavailableReason = "unsupported_account_terms"
	CostUnavailableResolverNotConfigured CostUnavailableReason = "resolver_not_configured"
	CostUnavailableResolverFailed        CostUnavailableReason = "resolver_failed"
	CostUnavailableResolverRejected      CostUnavailableReason = "resolver_rejected"
	CostUnavailableResolverInvalid       CostUnavailableReason = "resolver_invalid_result"
)

type CostComponentKind string

const (
	CostComponentInput      CostComponentKind = "input"
	CostComponentOutput     CostComponentKind = "output"
	CostComponentReasoning  CostComponentKind = "reasoning"
	CostComponentCacheRead  CostComponentKind = "cache_read"
	CostComponentCacheWrite CostComponentKind = "cache_write"
	CostComponentUpstream   CostComponentKind = "upstream"
	CostComponentModality   CostComponentKind = "modality"
)

type CostComponent struct {
	Kind     CostComponentKind `json:"kind"`
	Modality string            `json:"modality,omitempty"`
	Amount   Decimal           `json:"amount"`
}

// ModelCost is one independently sourced accounting result. Provider-reported
// and estimated entries coexist in ModelAccounting; estimates never replace a
// provider amount. Amount "0" is a known zero, while an empty Amount with the
// unavailable source is unknown.
type ModelCost struct {
	Currency          string                `json:"currency,omitempty"`
	Amount            Decimal               `json:"amount,omitempty"`
	Source            CostSource            `json:"source"`
	Domain            PricingDomain         `json:"domain,omitempty"`
	Components        []CostComponent       `json:"components,omitempty"`
	Provenance        string                `json:"provenance,omitempty"`
	CatalogVersion    string                `json:"catalog_version,omitempty"`
	EffectiveAt       time.Time             `json:"effective_at,omitzero"`
	UnavailableReason CostUnavailableReason `json:"unavailable_reason,omitempty"`
}

func UnavailableModelCost(domain PricingDomain, reason CostUnavailableReason) ModelCost {
	return ModelCost{Source: CostUnavailable, Domain: domain, UnavailableReason: reason}
}

func (c ModelCost) Validate() error {
	switch c.Source {
	case CostUnavailable:
		if c.Amount != "" || c.Currency != "" || len(c.Components) != 0 {
			return errors.New("lebro: unavailable cost must not contain an amount")
		}
		if c.UnavailableReason == "" {
			return errors.New("lebro: unavailable cost requires a reason")
		}
	case CostProviderReported, CostDeveloperSupplied, CostEstimated:
		if c.Amount == "" || strings.TrimSpace(c.Currency) == "" {
			return errors.New("lebro: available cost requires currency and amount")
		}
		if err := c.Amount.validate(); err != nil {
			return err
		}
		if c.UnavailableReason != "" {
			return errors.New("lebro: available cost must not have an unavailable reason")
		}
	default:
		return fmt.Errorf("lebro: invalid cost source %q", c.Source)
	}
	for _, component := range c.Components {
		if component.Kind == "" || component.Amount == "" {
			return errors.New("lebro: cost component requires kind and amount")
		}
		if err := component.Amount.validate(); err != nil {
			return err
		}
	}
	return nil
}

func validCostSource(source CostSource) bool {
	switch source {
	case "", CostUnavailable, CostProviderReported, CostDeveloperSupplied, CostEstimated:
		return true
	default:
		return false
	}
}

type ModelAccounting struct {
	ProviderRequestID string      `json:"provider_request_id,omitempty"`
	ServiceTier       string      `json:"service_tier,omitempty"`
	Region            string      `json:"region,omitempty"`
	Costs             []ModelCost `json:"costs,omitempty"`
}

func (a ModelAccounting) IsZero() bool {
	return a.ProviderRequestID == "" && a.ServiceTier == "" && a.Region == "" && len(a.Costs) == 0
}

func (a ModelAccounting) Clone() ModelAccounting {
	cloned := a
	if len(a.Costs) == 0 {
		cloned.Costs = nil
		return cloned
	}
	cloned.Costs = make([]ModelCost, len(a.Costs))
	for i, cost := range a.Costs {
		cloned.Costs[i] = cost
		cloned.Costs[i].Components = append([]CostComponent(nil), cost.Components...)
	}
	return cloned
}

func (a ModelAccounting) Validate() error {
	providerReported := 0
	for _, cost := range a.Costs {
		if err := cost.Validate(); err != nil {
			return err
		}
		if cost.Source == CostProviderReported {
			providerReported++
		}
	}
	if providerReported > 1 {
		return errors.New("lebro: accounting contains multiple provider-reported totals")
	}
	return nil
}

func (a ModelAccounting) HasProviderReportedCost() bool {
	for _, cost := range a.Costs {
		if cost.Source == CostProviderReported {
			return true
		}
	}
	return false
}

func (a ModelAccounting) HasAvailableCost() bool {
	for _, cost := range a.Costs {
		if cost.Source != CostUnavailable {
			return true
		}
	}
	return false
}

// CostResolver receives a content-free, immutable attempt snapshot after a
// provider call. It may return developer-supplied or estimated accounting.
type CostResolver interface {
	ResolveCost(context.Context, ModelAttemptRecord) (ModelCost, error)
}

type CostResolverFunc func(context.Context, ModelAttemptRecord) (ModelCost, error)

func (f CostResolverFunc) ResolveCost(ctx context.Context, attempt ModelAttemptRecord) (ModelCost, error) {
	return f(ctx, attempt)
}

const OfficialPricingCatalogVersion = "2026-09-15"

// OfficialPricingResolver estimates standard pay-as-you-go text-token cost
// from the bundled, dated provider catalog. It makes no network calls and
// reports why a safe estimate cannot be produced.
type OfficialPricingResolver struct{}

func NewOfficialPricingResolver() CostResolver { return OfficialPricingResolver{} }

type tokenRate struct {
	input, output, cacheRead, cacheWrite5m, cacheWrite1h Decimal
	provenance                                           string
	longContextThreshold                                 int64
}

var officialTokenRates = map[PricingDomain]map[string]tokenRate{
	PricingDomainOpenAI: {
		"gpt-6-astra":   {input: "10", cacheRead: "1", cacheWrite5m: "12.5", output: "50", provenance: "https://developers.openai.com/api/docs/models/compare"},
		"gpt-5.6-sol":   {input: "4", cacheRead: "0.4", output: "20", provenance: "https://developers.openai.com/api/docs/models/compare"},
		"gpt-5.6-terra": {input: "2", cacheRead: "0.2", output: "12", provenance: "https://developers.openai.com/api/docs/models/compare"},
	},
	PricingDomainAnthropic: {
		"claude-fable-5":    {input: "10", cacheWrite5m: "12.5", cacheWrite1h: "20", cacheRead: "1", output: "50", provenance: "https://platform.claude.com/docs/en/about-claude/pricing"},
		"claude-opus-5":     {input: "5", cacheWrite5m: "6.25", cacheWrite1h: "10", cacheRead: "0.5", output: "25", provenance: "https://platform.claude.com/docs/en/about-claude/pricing"},
		"claude-opus-4.8":   {input: "5", cacheWrite5m: "6.25", cacheWrite1h: "10", cacheRead: "0.5", output: "25", provenance: "https://platform.claude.com/docs/en/about-claude/pricing"},
		"claude-opus-4.7":   {input: "5", cacheWrite5m: "6.25", cacheWrite1h: "10", cacheRead: "0.5", output: "25", provenance: "https://platform.claude.com/docs/en/about-claude/pricing"},
		"claude-opus-4.6":   {input: "5", cacheWrite5m: "6.25", cacheWrite1h: "10", cacheRead: "0.5", output: "25", provenance: "https://platform.claude.com/docs/en/about-claude/pricing"},
		"claude-opus-4.5":   {input: "5", cacheWrite5m: "6.25", cacheWrite1h: "10", cacheRead: "0.5", output: "25", provenance: "https://platform.claude.com/docs/en/about-claude/pricing"},
		"claude-sonnet-5":   {input: "2", cacheWrite5m: "2.5", cacheWrite1h: "4", cacheRead: "0.2", output: "10", provenance: "https://platform.claude.com/docs/en/about-claude/pricing", longContextThreshold: 200_000},
		"claude-sonnet-4.6": {input: "3", cacheWrite5m: "3.75", cacheWrite1h: "6", cacheRead: "0.3", output: "15", provenance: "https://platform.claude.com/docs/en/about-claude/pricing", longContextThreshold: 200_000},
		"claude-sonnet-4.5": {input: "3", cacheWrite5m: "3.75", cacheWrite1h: "6", cacheRead: "0.3", output: "15", provenance: "https://platform.claude.com/docs/en/about-claude/pricing", longContextThreshold: 200_000},
		"claude-haiku-4.5":  {input: "1", cacheWrite5m: "1.25", cacheWrite1h: "2", cacheRead: "0.1", output: "5", provenance: "https://platform.claude.com/docs/en/about-claude/pricing"},
	},
	PricingDomainGemini: {
		"gemini-3.8-flash":      {input: "0.75", cacheRead: "0.075", output: "3.75", provenance: "https://ai.google.dev/gemini-api/docs/pricing", longContextThreshold: 200_000},
		"gemini-3.5-flash":      {input: "1.5", cacheRead: "0.15", output: "9", provenance: "https://ai.google.dev/gemini-api/docs/pricing", longContextThreshold: 200_000},
		"gemini-3.5-flash-lite": {input: "0.3", cacheRead: "0.03", output: "2.5", provenance: "https://ai.google.dev/gemini-api/docs/pricing", longContextThreshold: 200_000},
		"gemini-3.1-flash-lite": {input: "0.25", cacheRead: "0.025", output: "1.5", provenance: "https://ai.google.dev/gemini-api/docs/pricing", longContextThreshold: 200_000},
		"gemini-2.5-flash":      {input: "0.3", cacheRead: "0.03", output: "2.5", provenance: "https://ai.google.dev/gemini-api/docs/pricing"},
		"gemini-2.5-flash-lite": {input: "0.1", cacheRead: "0.01", output: "0.4", provenance: "https://ai.google.dev/gemini-api/docs/pricing"},
		"gemini-2.5-pro":        {input: "1.25", cacheRead: "0.125", output: "10", provenance: "https://ai.google.dev/gemini-api/docs/pricing", longContextThreshold: 200_000},
	},
	PricingDomainVertexAI: {
		"gemini-2.5-flash":      {input: "0.3", cacheRead: "0.03", output: "2.5", provenance: "https://cloud.google.com/vertex-ai/generative-ai/pricing"},
		"gemini-2.5-flash-lite": {input: "0.1", cacheRead: "0.01", output: "0.4", provenance: "https://cloud.google.com/vertex-ai/generative-ai/pricing"},
		"gemini-2.5-pro":        {input: "1.25", cacheRead: "0.125", output: "10", provenance: "https://cloud.google.com/vertex-ai/generative-ai/pricing", longContextThreshold: 200_000},
	},
}

func (OfficialPricingResolver) ResolveCost(_ context.Context, attempt ModelAttemptRecord) (ModelCost, error) {
	domain := attempt.Accounting.Domain()
	if domain == PricingDomainOpenRouter && attempt.Accounting.HasProviderReportedCost() {
		return ModelCost{}, nil
	}
	if domain == PricingDomainOpenRouter {
		return UnavailableModelCost(domain, CostUnavailableUnsupportedTerms), nil
	}
	if domain == "" {
		domain = domainForProvider(attempt.Provider)
	}
	rates := officialTokenRates[domain]
	model := attempt.Model
	if model == "" {
		model = attempt.RoutedModel
	}
	rate, ok := rates[canonicalModelName(model)]
	if !ok {
		return UnavailableModelCost(domain, CostUnavailableUnknownModel), nil
	}
	if attempt.Usage == (ModelUsage{}) {
		return UnavailableModelCost(domain, CostUnavailableMissingUsage), nil
	}
	if attempt.Accounting.ServiceTier != "" && attempt.Accounting.ServiceTier != "standard" {
		return UnavailableModelCost(domain, CostUnavailableUnsupportedTerms), nil
	}
	contextTokens := attempt.Usage.InputTokens
	if domain == PricingDomainAnthropic {
		contextTokens += attempt.Usage.CacheReadTokens + attempt.Usage.CacheWriteTokens
	}
	if rate.longContextThreshold > 0 && contextTokens > rate.longContextThreshold {
		return UnavailableModelCost(domain, CostUnavailableUnsupportedTerms), nil
	}
	if (attempt.Usage.InputTokens > 0 && rate.input == "") ||
		(attempt.Usage.OutputTokens > 0 && rate.output == "") ||
		(attempt.Usage.CacheReadTokens > 0 && rate.cacheRead == "") ||
		(attempt.Usage.CacheWriteTokens > attempt.Usage.CacheWrite1hTokens && rate.cacheWrite5m == "") ||
		(attempt.Usage.CacheWrite1hTokens > 0 && rate.cacheWrite1h == "") {
		return UnavailableModelCost(domain, CostUnavailableMissingDimension), nil
	}
	components := make([]CostComponent, 0, 5)
	add := func(kind CostComponentKind, tokens int64, perMillion Decimal) {
		if tokens == 0 || perMillion == "" {
			return
		}
		components = append(components, CostComponent{Kind: kind, Amount: decimalMulPerMillion(perMillion, tokens)})
	}
	uncachedInput := attempt.Usage.InputTokens - attempt.Usage.CacheReadTokens - attempt.Usage.CacheWriteTokens
	if domain == PricingDomainAnthropic {
		uncachedInput = attempt.Usage.InputTokens
	}
	if uncachedInput < 0 {
		return UnavailableModelCost(domain, CostUnavailableMissingDimension), nil
	}
	add(CostComponentInput, uncachedInput, rate.input)
	add(CostComponentCacheRead, attempt.Usage.CacheReadTokens, rate.cacheRead)
	cacheWrite5m := attempt.Usage.CacheWriteTokens - attempt.Usage.CacheWrite1hTokens
	if cacheWrite5m < 0 {
		return UnavailableModelCost(domain, CostUnavailableMissingDimension), nil
	}
	add(CostComponentCacheWrite, cacheWrite5m, rate.cacheWrite5m)
	add(CostComponentCacheWrite, attempt.Usage.CacheWrite1hTokens, rate.cacheWrite1h)
	add(CostComponentOutput, attempt.Usage.OutputTokens, rate.output)
	total := Decimal("0")
	for _, component := range components {
		total = decimalAdd(total, component.Amount)
	}
	return ModelCost{Currency: "USD", Amount: total, Source: CostEstimated, Domain: domain, Components: components, Provenance: rate.provenance, CatalogVersion: OfficialPricingCatalogVersion, EffectiveAt: time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)}, nil
}

func (a ModelAccounting) Domain() PricingDomain {
	for _, cost := range a.Costs {
		if cost.Domain != "" {
			return cost.Domain
		}
	}
	return ""
}

func domainForProvider(provider ProviderID) PricingDomain {
	switch provider {
	case "openai":
		return PricingDomainOpenAI
	case "anthropic":
		return PricingDomainAnthropic
	case "gemini":
		return PricingDomainGemini
	case "vertexai":
		return PricingDomainVertexAI
	default:
		return PricingDomainOpenAICompatible
	}
}

func canonicalModelName(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if at := strings.LastIndex(model, "/"); at >= 0 {
		model = model[at+1:]
	}
	return model
}

func decimalParts(d Decimal) (*big.Int, int) {
	text := string(d)
	parts := strings.SplitN(text, ".", 2)
	scale := 0
	if len(parts) == 2 {
		scale = len(parts[1])
		text = parts[0] + parts[1]
	}
	n := new(big.Int)
	n.SetString(text, 10)
	return n, scale
}

func decimalFromParts(n *big.Int, scale int) Decimal {
	digits := n.String()
	if scale > 0 {
		if len(digits) <= scale {
			digits = strings.Repeat("0", scale-len(digits)+1) + digits
		}
		at := len(digits) - scale
		digits = digits[:at] + "." + digits[at:]
	}
	value, _ := ParseDecimal(digits)
	return value
}

func decimalMulPerMillion(rate Decimal, tokens int64) Decimal {
	n, scale := decimalParts(rate)
	n.Mul(n, big.NewInt(tokens))
	return decimalFromParts(n, scale+6)
}

func decimalAdd(a, b Decimal) Decimal {
	an, as := decimalParts(a)
	bn, bs := decimalParts(b)
	if as < bs {
		an.Mul(an, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(bs-as)), nil))
		as = bs
	} else if bs < as {
		bn.Mul(bn, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(as-bs)), nil))
	}
	an.Add(an, bn)
	return decimalFromParts(an, as)
}

// AggregateModelCosts paginates a repository and returns exact totals grouped
// by currency and source. Repository filters enforce tenant/owner isolation.
func AggregateModelCosts(ctx context.Context, repo ModelAttemptRepository, filter ModelAttemptFilter) ([]ModelCost, error) {
	if repo == nil {
		return nil, errors.New("lebro: model attempt repository is required")
	}
	totals := map[string]ModelCost{}
	cursor := ""
	for {
		page, err := repo.ListModelAttempts(ctx, filter, PageRequest{Limit: 250, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, attempt := range page.Records {
			for _, cost := range attempt.Accounting.Costs {
				if cost.Source == CostUnavailable || (filter.CostSource != "" && cost.Source != filter.CostSource) {
					continue
				}
				if cost.Validate() != nil {
					continue
				}
				key := strings.ToUpper(cost.Currency) + "\x00" + string(cost.Source)
				total := totals[key]
				if total.Source == "" {
					total = ModelCost{Currency: strings.ToUpper(cost.Currency), Source: cost.Source, Amount: "0"}
				}
				total.Amount = decimalAdd(total.Amount, cost.Amount)
				totals[key] = total
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	result := make([]ModelCost, 0, len(totals))
	for _, total := range totals {
		result = append(result, total)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Currency == result[j].Currency {
			return result[i].Source < result[j].Source
		}
		return result[i].Currency < result[j].Currency
	})
	return result, nil
}
