package main

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// confirmationTier lets smaller deposits be credited sooner: a deposit of at
// most UpTo koinu needs Confirmations Dogecoin confirmations. Deposits above
// every tier need the bridge's full -confirmations. Waiting guards against
// a Dogecoin reorganisation undoing a deposit already credited, and the
// risk of one scales with what's at stake.
type confirmationTier struct {
	UpTo          int64 `json:"upTo"`
	Confirmations int64 `json:"confirmations"`
}

// confirmationsFor is how many Dogecoin confirmations a deposit of value
// koinu needs before it's credited.
func (b *bridge) confirmationsFor(value int64) int64 {
	for _, t := range b.confirmationTiers {
		if value <= t.UpTo {
			return t.Confirmations
		}
	}
	return b.depositConfirmations
}

// parseConfirmationTiers reads "10:6,50:12" (up to 10 DOGE: 6
// confirmations; up to 50 DOGE: 12). Tiers must rise in both amount and
// confirmations, and none may ask for more than full, the confirmations
// every larger deposit needs.
func parseConfirmationTiers(s string, full int64) ([]confirmationTier, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var tiers []confirmationTier
	for _, part := range strings.Split(s, ",") {
		amount, confs, ok := strings.Cut(strings.TrimSpace(part), ":")
		if !ok {
			return nil, fmt.Errorf("confirmation tier %q is not DOGE:CONFIRMATIONS", part)
		}
		upTo, err := parseDoge(amount)
		if err != nil {
			return nil, fmt.Errorf("confirmation tier %q: %w", part, err)
		}
		n, err := strconv.ParseInt(confs, 10, 64)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("confirmation tier %q: confirmations must be a positive whole number", part)
		}
		if n > full {
			return nil, fmt.Errorf("confirmation tier %q asks for more than -confirmations (%d)", part, full)
		}
		if k := len(tiers); k > 0 && (upTo <= tiers[k-1].UpTo || n < tiers[k-1].Confirmations) {
			return nil, fmt.Errorf("confirmation tiers must rise in amount and confirmations: %q", s)
		}
		tiers = append(tiers, confirmationTier{UpTo: upTo, Confirmations: n})
	}
	return tiers, nil
}

// formatConfirmationTiers is the flag form of tiers.
func formatConfirmationTiers(tiers []confirmationTier) string {
	parts := make([]string, len(tiers))
	for i, t := range tiers {
		parts[i] = strings.TrimRight(strings.TrimRight(formatDoge(t.UpTo), "0"), ".") + ":" + strconv.FormatInt(t.Confirmations, 10)
	}
	return strings.Join(parts, ",")
}

// blockCounter is a chain that can report its height cheaply.
type blockCounter interface {
	blockCount() (int64, error)
}

func (c *vmChain) blockCount() (int64, error) {
	var h int64
	err := c.rpc.call(&h, "getblockcount")
	return h, err
}

// waitForBlock returns as soon as either chain has a new block, or after
// max. The bridge then acts on a final withdrawal or a new confirmation the
// moment it lands, rather than on its next poll. DogecoinVM is checked four
// times a second, Dogecoin once a second.
func (b *bridge) waitForBlock(max time.Duration) {
	vm, vmOK := b.vm.(blockCounter)
	doge, dogeOK := b.doge.(blockCounter)
	if !vmOK && !dogeOK {
		time.Sleep(max)
		return
	}
	height := func(c blockCounter, ok bool) int64 {
		if !ok {
			return -1
		}
		h, err := c.blockCount()
		if err != nil {
			return -1
		}
		return h
	}
	startVM, startDoge := height(vm, vmOK), height(doge, dogeOK)
	deadline := time.Now().Add(max)
	for n := 1; time.Now().Before(deadline); n++ {
		time.Sleep(min(250*time.Millisecond, time.Until(deadline)))
		if h := height(vm, vmOK); h >= 0 && startVM >= 0 && h != startVM {
			return
		}
		if n%4 == 0 {
			if h := height(doge, dogeOK); h >= 0 && startDoge >= 0 && h != startDoge {
				return
			}
		}
	}
}

// sameTiers reports whether two tier lists are equal.
func sameTiers(a, b []confirmationTier) bool { return slices.Equal(a, b) }

// tiersForAPI is the tiers with amounts as DOGE strings, like every other
// amount the API returns.
func tiersForAPI(tiers []confirmationTier) []map[string]any {
	out := make([]map[string]any, len(tiers))
	for i, t := range tiers {
		out[i] = map[string]any{"upTo": formatDoge(t.UpTo), "confirmations": t.Confirmations}
	}
	return out
}
