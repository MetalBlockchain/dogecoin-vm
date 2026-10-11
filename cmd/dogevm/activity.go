package main

import (
	"fmt"
	"sort"
)

// activity tells the monitor's alert channel when someone uses the bridge:
// a deposit arriving, its credit on DogecoinVM, a withdrawal, its payment, a
// refund, and a new deposit address. It remembers what it has announced in
// memory only: the first pass after a start announces nothing, so a restart
// doesn't repeat history (anything that happened while the monitor was down
// isn't announced).
type activity struct {
	seen       map[string]bool
	registered int
	primed     bool
}

func newActivity() *activity { return &activity{seen: map[string]bool{}} }

// events is what changed in s since the last call, one line each.
func (a *activity) events(s *pegState, registered int) []string {
	var lines []string
	note := func(key, line string) {
		if a.seen[key] {
			return
		}
		a.seen[key] = true
		if a.primed {
			lines = append(lines, line)
		}
	}
	short := func(s string) string {
		if len(s) > 12 {
			return s[:12] + "…"
		}
		return s
	}
	confs := func(n int64) string {
		if n <= 0 {
			return "unconfirmed"
		}
		if n == 1 {
			return "1 confirmation"
		}
		return fmt.Sprintf("%d confirmations", n)
	}

	var all []deposit
	all = append(all, s.deposits...)
	all = append(all, s.held...)
	all = append(all, s.settled...)
	sort.Slice(all, func(i, j int) bool { return all[i].time < all[j].time })
	for _, d := range all {
		op := d.outPoint.String()
		why := ""
		if !d.valid {
			why = ", held: no destination, or outside the deposit limits"
		}
		note("deposit:"+op, fmt.Sprintf("Deposit received: %s DOGE on Dogecoin (%s%s), tx %s",
			formatDoge(d.value), confs(d.confirmations), why, short(d.outPoint.Hash.String())))
		if txid, ok := s.released[d.outPoint]; ok {
			note("credit:"+op, fmt.Sprintf("Deposit credited: %s DOGE on DogecoinVM, tx %s", formatDoge(d.value), short(txid.String())))
		}
		if txid, ok := s.refunded[d.outPoint]; ok {
			note("refund:"+op, fmt.Sprintf("Deposit refunded: %s DOGE on Dogecoin, tx %s", formatDoge(d.value), short(txid.String())))
		}
	}
	pegOuts := append([]pegOut(nil), s.pegOuts...)
	sort.Slice(pegOuts, func(i, j int) bool { return pegOuts[i].time < pegOuts[j].time })
	for _, p := range pegOuts {
		id := p.txid.String()
		note("withdraw:"+id, fmt.Sprintf("Withdrawal requested: %s DOGE from DogecoinVM, tx %s", formatDoge(p.value), short(id)))
		if txid, ok := s.paid[p.txid]; ok {
			note("paid:"+id, fmt.Sprintf("Withdrawal paid: %s DOGE on Dogecoin, tx %s", formatDoge(p.value), short(txid.String())))
		}
	}
	// Each new deposit address is someone starting a deposit on the site.
	if a.primed && registered > a.registered {
		what := fmt.Sprintf("%d new deposit addresses", registered-a.registered)
		if registered-a.registered == 1 {
			what = "A new deposit address"
		}
		lines = append(lines, fmt.Sprintf("%s registered (%d in all)", what, registered))
	}
	a.registered = max(a.registered, registered)
	a.primed = true
	return lines
}
