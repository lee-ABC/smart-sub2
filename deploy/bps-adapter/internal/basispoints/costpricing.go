package basispoints

import "strings"

// Cost estimate: pricing.
//
// Basis Points reports nothing about money. Responses carry token usage, and
// the ChatGPT usage endpoint reports only what percentage of each quota window
// is used. To show what a credential's usage is worth, every request is priced
// here at the list prices the main site bills with -- the LiteLLM table it
// syncs into model_pricing.json -- so the plugin's figures and the main site's
// can be reconciled against each other.
//
// The formula reproduces the main site's usage_logs.total_cost exactly: it
// matched 126,966 of 126,966 standard-tier rows for this gateway's account on
// 2026-10-01. Uncached input, cached input and output are priced separately,
// and a request whose whole prompt -- cached part included -- exceeds 272,000
// tokens is priced entirely at the long-context rates. The main site also bills
// requests that asked for the priority tier at twice the rate; this plugin
// never forwards service_tier, so upstream those run at the standard tier and
// are priced at it here.

const longContextThresholdTokens = 272000

// modelPrice is in US dollars per million tokens.
type modelPrice struct {
	Input, Cached, Output             float64
	LongInput, LongCached, LongOutput float64
}

// modelPriceConfig is an operator override, in dollars per million tokens.
type modelPriceConfig struct {
	Input      float64 `yaml:"input" json:"input"`
	Cached     float64 `yaml:"cached" json:"cached"`
	Output     float64 `yaml:"output" json:"output"`
	LongInput  float64 `yaml:"long_input" json:"long_input"`
	LongCached float64 `yaml:"long_cached" json:"long_cached"`
	LongOutput float64 `yaml:"long_output" json:"long_output"`
}

// defaultModelPrices mirrors the main site's model_pricing.json for the models
// this plugin serves, as read on 2026-10-01.
var defaultModelPrices = map[string]modelPrice{
	"gpt-6-astra":   {Input: 10, Cached: 1, Output: 50, LongInput: 20, LongCached: 2, LongOutput: 75},
	"gpt-5.6-sol":   {Input: 5, Cached: 0.5, Output: 30, LongInput: 10, LongCached: 1, LongOutput: 45},
	"gpt-6-sol":     {Input: 2, Cached: 0.2, Output: 10, LongInput: 4, LongCached: 0.4, LongOutput: 15},
	"gpt-5.6-terra": {Input: 2, Cached: 0.2, Output: 12, LongInput: 4, LongCached: 0.4, LongOutput: 18},
	"gpt-5.6-luna":  {Input: 0.2, Cached: 0.02, Output: 1.2, LongInput: 0.4, LongCached: 0.04, LongOutput: 1.8},
}

// tokenUsage follows the Responses API: Input is the whole prompt with the
// cached part included, and Output includes reasoning.
type tokenUsage struct {
	Input  int64
	Cached int64
	Output int64
	// CacheWrite is what the upstream charges at a premium for putting a new
	// prefix into its cache. It is read here because this is the last place it
	// exists: dropNonStandardUsageDetails removes the field before the response
	// reaches the caller, and without it a change that rewrites cached prefixes
	// looks free in the ledger while being billed upstream.
	CacheWrite int64
}

// priceFor finds a model's price, an operator override first.
func (c Config) priceFor(model string) (modelPrice, bool) {
	name := normalizePriceModel(model)
	if name == "" {
		return modelPrice{}, false
	}
	for key, override := range c.ModelPrices {
		if normalizePriceModel(key) == name {
			return override.price(), true
		}
	}
	price, ok := defaultModelPrices[name]
	return price, ok
}

// normalizePriceModel lowercases a model name and drops a thinking level CPA
// may carry in parentheses, such as gpt-6-sol(high).
func normalizePriceModel(model string) string {
	name := strings.ToLower(strings.TrimSpace(model))
	if cut := strings.IndexByte(name, '('); cut > 0 {
		name = strings.TrimSpace(name[:cut])
	}
	return name
}

func (p modelPriceConfig) price() modelPrice {
	price := modelPrice{
		Input: p.Input, Cached: p.Cached, Output: p.Output,
		LongInput: p.LongInput, LongCached: p.LongCached, LongOutput: p.LongOutput,
	}
	// An override that names no long-context rates means the model has none.
	if price.LongInput == 0 && price.LongCached == 0 && price.LongOutput == 0 {
		price.LongInput, price.LongCached, price.LongOutput = price.Input, price.Cached, price.Output
	}
	return price
}

// cost prices one request in US dollars.
func (p modelPrice) cost(u tokenUsage) float64 {
	uncached := u.Input - u.Cached
	if uncached < 0 {
		uncached = 0
	}
	input, cached, output := p.Input, p.Cached, p.Output
	if u.Input > longContextThresholdTokens {
		input, cached, output = p.LongInput, p.LongCached, p.LongOutput
	}
	return (float64(uncached)*input + float64(u.Cached)*cached + float64(u.Output)*output) / 1e6
}

// servedModel is the model the upstream actually answered with.
//
// It is not always the one that was asked for: the upstream substitutes a
// cheaper model at times -- on 2026-10-01 every gpt-6 request came back as
// gpt-5.6-luna, which lists at a fortieth of gpt-6-astra's output price.
// Pricing such a request at what was requested overstates it by that much, so
// what was served is what gets priced, and the substitution is counted.
func servedModel(response map[string]any, requested string) string {
	if served := strings.TrimSpace(stringValue(response["model"])); served != "" {
		return served
	}
	return requested
}

// usageFromResponse reads token usage from a Responses object.
func usageFromResponse(response map[string]any) (tokenUsage, bool) {
	usage := objectValue(response["usage"])
	if usage == nil {
		return tokenUsage{}, false
	}
	u := tokenUsage{
		Input:  numberValue(usage["input_tokens"]),
		Output: numberValue(usage["output_tokens"]),
	}
	if details := objectValue(usage["input_tokens_details"]); details != nil {
		u.Cached = numberValue(details["cached_tokens"])
		u.CacheWrite = numberValue(details["cache_write_tokens"])
	}
	return u, u.Input > 0 || u.Output > 0
}
