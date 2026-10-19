package pipeline

import (
	"context"
	"errors"
)

// ErrExpansionDeadline is a fixed, retryable time refusal (CFG-4).
var ErrExpansionDeadline = errors.New("platform expansion budget exhausted; retry with a new Idempotency-Key")

// ErrExpansionBudget is a fixed, permanent resource refusal (CFG-4).
var ErrExpansionBudget = errors.New("platform expansion exceeds its work budget")

type expansionBudgetKey struct{}
type expansionBudget struct {
	remaining int64
	exhausted bool
}

// WithExpansionBudget bounds total work across sequential expansions on ctx.
// Input and constructed string bytes plus node visits consume work; it is not shared across goroutines.
func WithExpansionBudget(ctx context.Context, units int64) context.Context {
	return context.WithValue(ctx, expansionBudgetKey{}, &expansionBudget{remaining: units})
}

// ExpansionStep checks cancellation and charges work against a shared expansion budget, if present.
func ExpansionStep(ctx context.Context, units int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if b, ok := ctx.Value(expansionBudgetKey{}).(*expansionBudget); ok {
		if b.exhausted || units > b.remaining {
			b.exhausted = true
			return ErrExpansionBudget
		}
		b.remaining -= units
	}
	return nil
}

type expansionWork struct {
	ctx    context.Context
	err    error
	output int64
}

func (w *expansionWork) step(units int64) bool {
	if w.err == nil {
		w.err = ExpansionStep(w.ctx, units)
	}
	return w.err == nil
}

// reserveOutput bounds projected JSON before materializing paths or serialization.
func (w *expansionWork) reserveOutput(bytes int64) bool {
	if w.err == nil && bytes > maxExpansionBytes-w.output {
		w.err = ErrExpansionBudget
	}
	if w.err != nil {
		return false
	}
	w.output += bytes
	return true
}

// jsonStringSize bounds encoding/json escaping without allocating a quoted copy.
func jsonStringSize(s string) int64 {
	n := int64(2)
	for _, r := range s {
		switch {
		case r < 0x20 || r == '\ufffd':
			n += 6
		case r == '"' || r == '\\':
			n += 2
		case r == '<' || r == '>' || r == '&' || r == '\u2028' || r == '\u2029':
			n += 6
		case r < 0x80:
			n++
		case r < 0x800:
			n += 2
		case r < 0x10000:
			n += 3
		default:
			n += 4
		}
	}
	return n
}
