package main

import (
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// Wallets and the explorer listen on /api/events (Server-Sent Events) and
// refresh the moment a block lands, instead of on a timer. On DogecoinVM a
// block is final once accepted, so a payment shows as soon as it is final.
//
// Events carry no account data, only which chain moved and its new height;
// each client then asks for what it needs.

// maxEventClients bounds open event streams, each of which holds a
// connection.
const maxEventClients = 5000

// chainEvent says a chain has a new block, or the Dogecoin index has caught
// up with one.
type chainEvent struct {
	Chain  string // "dogecoinvm" or "dogecoin"
	Height int64
}

// eventHub fans chain events out to every open stream.
type eventHub struct {
	mu   sync.Mutex
	subs map[chan chainEvent]struct{}
	last map[string]int64
}

func newEventHub() *eventHub {
	return &eventHub{subs: map[chan chainEvent]struct{}{}, last: map[string]int64{}}
}

// subscribe opens a stream's channel, with the latest heights to send
// first; ok is false when too many streams are open.
func (h *eventHub) subscribe() (ch chan chainEvent, latest []chainEvent, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) >= maxEventClients {
		return nil, nil, false
	}
	ch = make(chan chainEvent, 8)
	h.subs[ch] = struct{}{}
	for chain, height := range h.last {
		latest = append(latest, chainEvent{chain, height})
	}
	return ch, latest, true
}

func (h *eventHub) unsubscribe(ch chan chainEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, ch)
}

// publish sends e to every stream. A stream too far behind to take it
// misses it: events only say "refresh", and a later one says the same.
func (h *eventHub) publish(e chainEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.last[e.Chain] = e.Height
	for ch := range h.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// watchBlocks publishes an event for each new DogecoinVM block, checking
// four times a second, and for each Dogecoin block once the Dogecoin index
// (or, without one, the node) has it. It refreshes the status snapshot
// first, so clients that ask straight away see the new block.
func (srv *server) watchBlocks(hub *eventHub) {
	var vmHeight, dogeHeight int64 = -1, -1
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for n := 0; ; n++ {
		<-tick.C
		var h int64
		if err := srv.vm.rpc.call(&h, "getblockcount"); err == nil && h != vmHeight {
			at := time.Now()
			first := vmHeight < 0
			if !first && h > vmHeight {
				srv.finalityFor(vmHeight+1, h, at)
			}
			vmHeight = h
			if !first {
				srv.refresh()
			}
			hub.publish(chainEvent{"dogecoinvm", h})
		}
		if n%4 != 0 { // Dogecoin: once a second is plenty for one-minute blocks
			continue
		}
		var d int64 = -1
		if srv.dogeIdx != nil {
			d, _ = srv.dogeIdx.tip()
		} else if err := srv.doge.rpc.call(&d, "getblockcount"); err != nil {
			d = -1
		}
		if d >= 0 && d != dogeHeight {
			first := dogeHeight < 0
			dogeHeight = d
			if !first {
				srv.refresh()
			}
			hub.publish(chainEvent{"dogecoin", d})
		}
	}
}

// events streams chain events to one client until it disconnects.
func (srv *server) events(hub *eventHub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ch, latest, ok := hub.subscribe()
		if !ok {
			http.Error(w, "too many connections; try again shortly", http.StatusServiceUnavailable)
			return
		}
		defer hub.unsubscribe(ch)

		// The server's write timeout suits ordinary requests; a stream
		// stays open for as long as the page does.
		rc := http.NewResponseController(w)
		if err := rc.SetWriteDeadline(time.Time{}); err != nil {
			log.Printf("events: %v", err)
		}
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-store")
		h.Set("X-Accel-Buffering", "no")

		send := func(e chainEvent) error {
			_, err := fmt.Fprintf(w, "event: block\ndata: {\"chain\":%q,\"height\":%d}\n\n", e.Chain, e.Height)
			if err == nil {
				err = rc.Flush()
			}
			return err
		}
		// Reconnect after 3 seconds if the stream drops.
		if _, err := fmt.Fprint(w, "retry: 3000\n\n"); err != nil {
			return
		}
		for _, e := range latest {
			if send(e) != nil {
				return
			}
		}
		if rc.Flush() != nil {
			return
		}

		ping := time.NewTicker(25 * time.Second) // keeps proxies from closing an idle stream
		defer ping.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case e := <-ch:
				if send(e) != nil {
					return
				}
			case <-ping.C:
				if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
					return
				}
			}
		}
	}
}

// finalityFor stops the finality meter's clock for payments in blocks from
// through to, which were seen accepted at at.
func (srv *server) finalityFor(from, to int64, at time.Time) {
	if srv.finality == nil {
		return
	}
	for height := from; height <= to && height > to-10; height++ {
		var hash string
		if err := srv.vm.rpc.call(&hash, "getblockhash", height); err != nil {
			return
		}
		b, err := srv.block(hash, true)
		if err != nil {
			return
		}
		srv.finality.final(b.TxIDs, at)
	}
}
