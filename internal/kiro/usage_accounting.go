package kiro

import (
	"math"

	"github.com/AkkunYo/kiro4cpa-plugin/internal/config"
)

// usageAccounting turns Amazon Q's metered credit spend into the token fields
// Anthropic clients expect.
//
// The service never itemizes tokens. It reports one number per turn:
//
//	meteringEvent {"unit":"credit","usage":0.0012}
//
// and that number already reflects any prompt caching the backend applied, so
// local character heuristics cannot see the discount -- which made cache reads
// report as zero and overcharged every cached turn.
//
// Measured rates (2026-11-19, 1000 credits = $20) show the charge is:
//
//	credit = inputRate*(fresh + cached*cacheReadRatio) + outputRate*output
//
// with output priced roughly eight times input. Deriving the cached share from
// the credit spend therefore needs the model's input and output rates, not a
// single blended one.
type usageAccounting struct {
	settings config.UsageSettings
}

func newUsageAccounting() usageAccounting {
	settings, err := config.Get().Usage()
	if err != nil {
		// UpdateFromYAML rejects blocks that fail this parse, so a failure here
		// means the defaults are in effect.
		settings = config.DefaultUsageSettings()
	}
	return usageAccounting{settings: settings}
}

// UsageInput is everything the accounting decision depends on.
type UsageInput struct {
	Model string
	// EstimatedInput/Output are the local heuristics, always available.
	EstimatedInput  int
	EstimatedOutput int
	// Credits and SawCredits come from the metering frame, when present.
	Credits    float64
	SawCredits bool
	// Upstream is the real report, when the backend sends one at all.
	Upstream *TokenUsage
}

// UsageResult is the token breakdown written to the client response.
type UsageResult struct {
	InputTokens              int64
	OutputTokens             int64
	CacheReadInputTokens     int64
	CacheCreationInputTokens int64
	// Source says where the numbers came from, for logs and diagnostics.
	Source string
}

// Guards against claiming a cache read that is really rounding noise.
const (
	minCachedTokens = 64
	minCachedShare  = 0.02
)

// apply decides the reported usage.
func (a usageAccounting) apply(in UsageInput) UsageResult {
	// A real upstream report always wins: it is the only itemized source.
	if in.Upstream != nil && in.Upstream.hasCounts() {
		return UsageResult{
			InputTokens:              max(in.Upstream.UncachedInputTokens, 0),
			OutputTokens:             max(in.Upstream.OutputTokens, 0),
			CacheReadInputTokens:     max(in.Upstream.CacheReadInputTokens, 0),
			CacheCreationInputTokens: max(in.Upstream.CacheWriteInputTokens, 0),
			Source:                   "upstream",
		}
	}

	estimate := UsageResult{
		InputTokens:  int64(max(in.EstimatedInput, 0)),
		OutputTokens: int64(max(in.EstimatedOutput, 0)),
		Source:       "estimate",
	}
	if a.settings.Mode != "credit" || !in.SawCredits || in.Credits <= 0 {
		return estimate
	}

	rate, ok := a.settings.CreditRateFor(in.Model)
	if !ok || rate.Input <= 0 || rate.Output <= 0 {
		// No rate for this model: report the estimate rather than inventing a
		// conversion that would silently misprice the request.
		return estimate
	}

	// Price the output the service charged for, using the model's own output
	// rate. What remains of the credit spend is input, billed at the input rate.
	outputTokens := float64(max(in.EstimatedOutput, 0))
	billedInput := in.Credits - outputTokens*rate.Output
	if billedInput < 0 {
		// The credit spend does not even cover the output at the configured
		// rate: the model's rates are wrong for this turn and any "cache" we
		// derived would be fiction.
		return estimate
	}
	// Input tokens the service charged for, at full price.
	billedInputTokens := billedInput / rate.Input

	// The caller's own input excludes what the service adds on top (its agent
	// prompt), and the estimate is the only view of the caller's tokens.
	total := float64(max(in.EstimatedInput, 0))
	if total <= 0 {
		return estimate
	}
	ratio := a.settings.CacheReadRatio
	if ratio >= 1 {
		return estimate
	}

	// Billed input is less than the caller's total when part of it was served
	// from cache. Solve for that share:
	//
	//	fresh + cached*ratio = billedInputTokens
	//	fresh + cached       = total
	//
	// => cached = (total - billedInputTokens) / (1 - ratio)
	//
	// On a first-time prompt the service also bills input the caller never sent
	// (its own agent prompt), so billedInputTokens lands above total and no
	// cache is reported -- which is the correct answer for a cache miss.
	delta := total - billedInputTokens
	if delta <= 0 {
		return estimate
	}

	cached := delta / (1 - ratio)
	if math.IsNaN(cached) || cached < 0 {
		return estimate
	}
	// The service bills one blended number, so a cheap turn can imply "the whole
	// prompt was cached". Claiming that leaves the caller paying the cache rate
	// for everything while the operator still pays for the full prompt, so the
	// configured share is a hard ceiling on what the derivation may attribute to
	// the cache. 1 disables the ceiling, 0 disables the cache claim.
	if maxShare := a.settings.MaxCacheReadShare; maxShare < 1 {
		if ceiling := total * maxShare; cached > ceiling {
			cached = ceiling
		}
	}
	if cached > total {
		cached = total
	}
	if cached < minCachedTokens || cached/total < minCachedShare {
		return estimate
	}

	fresh := total - cached
	return UsageResult{
		InputTokens:          int64(math.Round(fresh)),
		OutputTokens:         estimate.OutputTokens,
		CacheReadInputTokens: int64(math.Round(cached)),
		Source:               "credit",
	}
}
