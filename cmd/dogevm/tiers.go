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
//
// On mainnet nothing is credited at fewer than minMainnetConfirmations,
// whatever a tier says: a Dogecoin block can be reorganised away, and one
// confirmation (as the beta policy's smallest tier allowed) is undone by an
// ordinary orphan.
func (b *bridge) confirmationsFor(value int64) int64 {
	n := b.depositConfirmations
	for _, t := range b.confirmationTiers {
		if value <= t.UpTo {
			n = t.Confirmations
			break
		}
	}
	if n < b.confFloor {
		n = b.confFloor
	}
	return n
}

// minMainnetConfirmations is the fewest Dogecoin confirmations a mainnet
// deposit is credited at, however small.
const minMainnetConfirmations = 6

// shallowCap is the most the peg credits, in total, from deposits that
// don't yet have the full -confirmations: -max-shallow, or by default the
// largest tier's amount. Tiers bound each deposit, not their sum: many
// small deposits in one block would each pass the lowest tier, and one
// reorganisation would undo them all while their credits on DogecoinVM
// stand.
func (b *bridge) shallowCap() int64 {
	if b.maxShallow > 0 {
		return b.maxShallow
	}
	if n := len(b.confirmationTiers); n > 0 {
		return b.confirmationTiers[n-1].UpTo
	}
	return 0 // no tiers: nothing is credited before full confirmations
}

// shallowCredited is what the peg has credited from deposits still short of
// the full confirmations.
func (b *bridge) shallowCredited(s *pegState) int64 {
	var total int64
	for _, d := range s.deposits {
		if _, done := s.released[d.outPoint]; done && d.confirmations < b.depositConfirmations {
			total += d.value
		}
	}
	return total
}

// shallowRoom reports whether crediting d now keeps shallow credit within
// shallowCap, given other shallow credit already promised. A deposit with
// the full confirmations always fits.
func (b *bridge) shallowRoom(s *pegState, d deposit, promised int64) error {
	if d.confirmations >= b.depositConfirmations {
		return nil
	}
	limit := b.shallowCap()
	if total := b.shallowCredited(s) + promised + d.value; total > limit {
		return fmt.Errorf("crediting deposit %v at %d confirmations would put %s DOGE on deposits short of %d confirmations (limit %s DOGE); it waits for more",
			d.outPoint, d.confirmations, formatDoge(total), b.depositConfirmations, formatDoge(limit))
	}
	return nil
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
