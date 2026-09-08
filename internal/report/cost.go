package report

import (
	"math"
	"strings"

	"github.com/AngelVRodC/tare/internal/transcript"
)

// AllocationMethod names how a dollar figure was arrived at. Every estimated
// row carries it; no dollar row is ever tagged measured.
const AllocationMethod = "session-cost-by-weighted-tokens"

// The published Anthropic price multipliers, relative to base input price.
// Ratios only — no absolute price ever enters this program, and the
// allocation is pinned to the measured costUSD, so a wrong ratio distorts the
// split between rows without moving the total.
const (
	weightInput          = 1.0
	weightOutput         = 5.0
	weightCacheRead      = 0.1
	weightCacheReadFable = 0.025
	weightCacheCreate5m  = 1.25
	weightCacheCreate1h  = 2.0
)

// fableMarkers pick the model families whose cache reads are priced at
// weightCacheReadFable, a quarter of the default. The match is a lowercase
// substring on the model id — version suffixes ride along, unknown ids fall
// back to weightCacheRead fail-closed. Documented multipliers, asserted by
// TestFableWeightsMatchBilling against billing computed at the same ratios.
var fableMarkers = []string{"fable-5-1", "mythos-5-1"}

// cacheReadWeight selects the cache-read multiplier for one response's model.
// "<synthetic>", the empty id and anything unrecognized weigh at the default.
func cacheReadWeight(model string) float64 {
	id := strings.ToLower(model)
	for _, marker := range fableMarkers {
		if strings.Contains(id, marker) {
			return weightCacheReadFable
		}
	}
	return weightCacheRead
}

// responseWeight scores one response in units of base input tokens.
//
// Thinking tokens are not added: they are already inside Output and billed at
// the output rate, so counting them again would over-weight a reasoning-heavy
// response against the rest of its session.
func responseWeight(u *transcript.Usage) float64 {
	return weightInput*float64(u.Input) +
		weightOutput*float64(u.Output) +
		cacheReadWeight(u.Model)*float64(u.CacheRead) +
		weightCacheCreate5m*float64(u.CacheCreate5m) +
		weightCacheCreate1h*float64(u.CacheCreate1h)
}

// The cost-state record is deliberately never weighted: it reports one
// cacheCreationInputTokens total with no ephemeral split, and scoring that at
// either tier's rate is wrong whenever a session used the other. The
// transcript carries the split per response, so every weight in this program
// comes from responseWeight.

// sessionModel identifies one model's slice of one session — the grain at
// which a bill exists and therefore the only grain allocation can start from.
type sessionModel struct {
	session string
	model   string
}

// allocate splits a measured session-model bill across a share of its weight,
// returned in integer microdollars — the unit every money row carries.
//
// The rounding boundary sits here, once (microdollar-cost-2): each row is the
// nearest micro to its exact share, so a set of rows summed back can drift
// from the bill by up to one micro per row. That drift is bounded and ordered,
// so integer sums stay associative and two runs over one corpus are
// byte-identical — the property float money could only hold by sorting first.
// The result is always estimated — no dollar figure is ever measured, not even
// for a single-tool turn, because the charge includes the re-sent prefix and
// the output tokens and neither belongs to any one tool.
func allocate(costUSD, share, total float64) int64 {
	if total <= 0 {
		return 0
	}
	return int64(math.Round(costUSD * share / total * 1e6))
}

// coverage is transcript tokens over cost-state tokens for one session-model.
//
// It is reported before any dollar figure because the dollars are meaningless
// without it. Measured 2026-09-05, most pairs under-count and some models in a
// bill have no transcript events at all — background haiku calls, title
// generation, compaction. Billed work exists that the transcript never
// records, and that is the honest finding, not a bug to paper over.
func coverage(transcriptTokens, billedTokens int64) float64 {
	if billedTokens == 0 {
		return 0
	}
	return float64(transcriptTokens) / float64(billedTokens)
}
