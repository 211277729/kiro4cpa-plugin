package kiro

import (
	"math"
	"testing"

	"github.com/AkkunYo/kiro4cpa-plugin/internal/config"
)

func testAccounting(t *testing.T, rates map[string]config.RateSpec, ratio string) usageAccounting {
	t.Helper()
	settings, err := (config.PluginConfig{
		UsageMode:      "credit",
		CreditRates:    rates,
		CacheReadRatio: ratio,
	}).Usage()
	if err != nil {
		t.Fatal(err)
	}
	return usageAccounting{settings: settings}
}

func scalar(input, output string) config.RateSpec {
	return config.RateSpec{Input: input, Output: output}
}

// Rates measured against the live service on 2026-11-19.
func defaultTestAccounting(t *testing.T) usageAccounting {
	t.Helper()
	return testAccounting(t, map[string]config.RateSpec{
		"claude-sonnet*": scalar("3.53e-5", "2.87e-4"),
		"*":              scalar("4.24e-6", "4.24e-6"),
	}, "0.10")
}

// spendFor reproduces what the service charges: fresh input and cached input
// (at cacheReadRatio) plus output at its own rate.
func spendFor(in, out, ratio float64, fresh, cached, output int) float64 {
	return in*(float64(fresh)+float64(cached)*ratio) + out*float64(output)
}

func TestAccountingUpstreamReportWins(t *testing.T) {
	a := defaultTestAccounting(t)
	got := a.apply(UsageInput{
		Model:           "claude-sonnet-5.5",
		EstimatedInput:  4000,
		EstimatedOutput: 200,
		Credits:         1,
		SawCredits:      true,
		Upstream: &TokenUsage{
			UncachedInputTokens:   12,
			OutputTokens:          34,
			CacheReadInputTokens:  56,
			CacheWriteInputTokens: 78,
		},
	})
	want := UsageResult{InputTokens: 12, OutputTokens: 34, CacheReadInputTokens: 56, CacheCreationInputTokens: 78, Source: "upstream"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestAccountingIgnoresAllZeroUpstream(t *testing.T) {
	a := defaultTestAccounting(t)
	got := a.apply(UsageInput{
		Model:           "claude-sonnet-5.5",
		EstimatedInput:  1000,
		EstimatedOutput: 100,
		Upstream:        &TokenUsage{},
	})
	if got.Source != "estimate" || got.InputTokens != 1000 || got.OutputTokens != 100 {
		t.Fatalf("got %+v", got)
	}
}

func TestAccountingEstimateModes(t *testing.T) {
	cases := []struct {
		name    string
		mode    string
		credits float64
	}{
		{"estimate mode", "estimate", 0.5},
		{"off mode", "off", 0.5},
		{"no credits seen", "credit", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settings, err := (config.PluginConfig{
				UsageMode:      tc.mode,
				CreditRates:    map[string]config.RateSpec{"*": scalar("4.24e-6", "4.24e-6")},
				CacheReadRatio: "0.10",
			}).Usage()
			if err != nil {
				t.Fatal(err)
			}
			got := usageAccounting{settings: settings}.apply(UsageInput{
				Model:           "claude-sonnet-5.5",
				EstimatedInput:  2000,
				EstimatedOutput: 100,
				Credits:         tc.credits,
				SawCredits:      tc.credits > 0,
			})
			if got.Source != "estimate" || got.CacheReadInputTokens != 0 {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestAccountingRecoversCachedSplit(t *testing.T) {
	const in, out, ratio = 3.53e-5, 2.87e-4, 0.10
	a := defaultTestAccounting(t)
	const fresh, cached, output = 200, 800, 100
	credits := spendFor(in, out, ratio, fresh, cached, output)
	got := a.apply(UsageInput{
		Model:           "claude-sonnet-5.5",
		EstimatedInput:  fresh + cached,
		EstimatedOutput: output,
		Credits:         credits,
		SawCredits:      true,
	})
	if got.Source != "credit" {
		t.Fatalf("expected a credit-derived split, got %+v", got)
	}
	if got.InputTokens != fresh || got.CacheReadInputTokens != cached || got.OutputTokens != output {
		t.Fatalf("got %+v, want fresh=%d cached=%d output=%d", got, fresh, cached, output)
	}
}

// The live service bills a first-time prompt more than the caller's own tokens
// because it adds its own agent prompt. That must not read as a cache hit.
func TestAccountingFirstTimePromptHasNoCache(t *testing.T) {
	const in, out, ratio = 3.53e-5, 2.87e-4, 0.10
	a := defaultTestAccounting(t)
	const caller, output = 1200, 50
	// 1515 tokens billed for 1215 sent: measured on the live service.
	credits := spendFor(in, out, ratio, 1515, 0, output)
	got := a.apply(UsageInput{
		Model:           "claude-sonnet-5.5",
		EstimatedInput:  caller,
		EstimatedOutput: output,
		Credits:         credits,
		SawCredits:      true,
	})
	if got.CacheReadInputTokens != 0 {
		t.Fatalf("phantom cache read on a cache miss: %+v", got)
	}
	if got.Source != "estimate" || got.InputTokens != caller {
		t.Fatalf("got %+v", got)
	}
}

// The measured cache behaviour: an identical prompt re-billed at roughly a
// quarter of the first price for sonnet.
func TestAccountingMeasuredSonnetCacheHit(t *testing.T) {
	const in, out, ratio = 3.53e-5, 2.87e-4, 0.10
	a := defaultTestAccounting(t)
	const meter = 0.014581297578772804
	got := a.apply(UsageInput{
		Model:           "claude-sonnet-5.5",
		EstimatedInput:  1215,
		EstimatedOutput: 1,
		Credits:         meter,
		SawCredits:      true,
	})
	if got.Source != "credit" || got.CacheReadInputTokens <= 0 {
		t.Fatalf("expected a cache split, got %+v", got)
	}
	if got.InputTokens+got.CacheReadInputTokens != 1215 {
		t.Fatalf("split does not add up: %+v", got)
	}
	// Re-price the reported split and compare with the meter. Rounding the split
	// to whole tokens moves the re-priced value by a few 1e-6 credits.
	reprice := spendFor(in, out, ratio, int(got.InputTokens), int(got.CacheReadInputTokens), 1)
	if math.Abs(reprice-meter)/meter > 0.001 {
		t.Fatalf("re-priced %.8f, meter said %.8f", reprice, meter)
	}
}

func TestAccountingTinyPromptHasNoPhantomCache(t *testing.T) {
	a := defaultTestAccounting(t)
	got := a.apply(UsageInput{
		Model:           "claude-sonnet-5.5",
		EstimatedInput:  40,
		EstimatedOutput: 5,
		Credits:         0.0002,
		SawCredits:      true,
	})
	if got.CacheReadInputTokens != 0 {
		t.Fatalf("phantom cache read on a tiny prompt: %+v", got)
	}
}

func TestAccountingUnknownModelFallsBackToEstimate(t *testing.T) {
	a := testAccounting(t, map[string]config.RateSpec{
		"claude-sonnet*": scalar("3.53e-5", "2.87e-4"),
	}, "0.10")
	got := a.apply(UsageInput{
		Model:           "some-other-model",
		EstimatedInput:  4000,
		EstimatedOutput: 100,
		Credits:         0.01,
		SawCredits:      true,
	})
	if got.Source != "estimate" || got.InputTokens != 4000 {
		t.Fatalf("got %+v", got)
	}
}

// Output is billed about eight times input on this service, so pricing it as
// input would leave the output share inside "input" and inflate the report.
func TestAccountingPricesOutputSeparately(t *testing.T) {
	const in, out, ratio = 3.53e-5, 2.87e-4, 0.10
	a := defaultTestAccounting(t)
	const caller, output = 1200, 800
	credits := spendFor(in, out, ratio, 1200, 0, output)
	got := a.apply(UsageInput{
		Model:           "claude-sonnet-5.5",
		EstimatedInput:  caller,
		EstimatedOutput: output,
		Credits:         credits,
		SawCredits:      true,
	})
	if got.OutputTokens != output {
		t.Fatalf("output not preserved: %+v", got)
	}
	if got.CacheReadInputTokens != 0 {
		t.Fatalf("long output must not read as a cache hit: %+v", got)
	}
}

func TestAccountingClampsNeverExceedInput(t *testing.T) {
	a := defaultTestAccounting(t)
	got := a.apply(UsageInput{
		Model:           "claude-sonnet-5.5",
		EstimatedInput:  1000,
		EstimatedOutput: 10,
		Credits:         1e-9,
		SawCredits:      true,
	})
	if got.InputTokens < 0 || got.CacheReadInputTokens > 1000 {
		t.Fatalf("clamped badly: %+v", got)
	}
	if int64(got.InputTokens)+got.CacheReadInputTokens != 1000 {
		t.Fatalf("split does not add up: %+v", got)
	}
	if math.IsNaN(float64(got.InputTokens)) {
		t.Fatal("NaN leaked into usage")
	}
}

func TestCreditRateResolution(t *testing.T) {
	settings, err := (config.PluginConfig{
		UsageMode: "credit",
		CreditRates: map[string]config.RateSpec{
			"*":               scalar("1", "1"),
			"claude-opus*":    scalar("2", "9"),
			"claude-opus-5.5": scalar("3", "12"),
		},
		CacheReadRatio: "0.1",
	}).Usage()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][2]float64{
		"claude-opus-5.5":   {3, 12},
		"claude-opus-5":     {2, 9},
		"claude-sonnet-5.5": {1, 1},
	}
	for model, want := range cases {
		got, ok := settings.CreditRateFor(model)
		if !ok || got.Input != want[0] || got.Output != want[1] {
			t.Fatalf("%s: got %+v (ok=%v), want %v", model, got, ok, want)
		}
	}
}
