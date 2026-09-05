package report

import "github.com/AngelVRodC/tare/internal/transcript"

// rebill is the re-billing triple plus what the cost allocation needs, rolled
// up over some set of deduplicated responses.
//
// This is the product thesis as arithmetic. `cache_read_input_tokens` on a
// response is exactly the prior context charged again on that turn — nothing
// is derived, the harness records it. A skill that injects 78 KB of context
// once is billed for it again on every turn until compaction, and the
// multiplier is how many times over.
type rebill struct {
	Responses int64
	// Fresh is context that entered the window on this turn for the first
	// time: Input plus both cache-creation tiers.
	Fresh int64
	// Rebilled is prior context re-charged: cache reads, nothing else.
	Rebilled int64
	Output   int64
	Thinking int64
	// Weight is the sum of responseWeight, carried here so the cost allocation
	// in cost.go needs no second pass over the corpus.
	Weight float64
}

func (r *rebill) add(u *transcript.Usage) {
	r.Responses++
	r.Fresh += u.Input + u.CacheCreate5m + u.CacheCreate1h
	r.Rebilled += u.CacheRead
	r.Output += u.Output
	r.Thinking += u.Thinking
	r.Weight += responseWeight(u)
}

// Multiplier is RebilledTokens / FreshTokens: how many times over the context
// this tooling put into the window was charged again.
//
// Zero fresh tokens means nothing was ever put in, so nothing was re-billed
// against it — a multiplier of zero, not a division by zero.
func (r rebill) Multiplier() float64 {
	if r.Fresh == 0 {
		return 0
	}
	return float64(r.Rebilled) / float64(r.Fresh)
}

// Tokens is every billed token in the rollup. Thinking is excluded because it
// is already counted inside Output.
func (r rebill) Tokens() int64 {
	return r.Fresh + r.Rebilled + r.Output
}
