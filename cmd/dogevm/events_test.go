package main

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestEventStream checks a client gets the latest heights on connecting,
// then each new block as it is published, and that the stream outlives the
// server's write timeout.
func TestEventStream(t *testing.T) {
	require := require.New(t)
	hub := newEventHub()
	hub.publish(chainEvent{"dogecoinvm", 7})

	srv := &server{}
	ts := httptest.NewUnstartedServer(srv.events(hub))
	ts.Config.WriteTimeout = 200 * time.Millisecond
	ts.Start()
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	require.NoError(err)
	defer resp.Body.Close()
	require.Equal("text/event-stream", resp.Header.Get("Content-Type"))

	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data: ") {
				lines <- sc.Text()
			}
		}
		close(lines)
	}()
	next := func() string {
		select {
		case l := <-lines:
			return l
		case <-time.After(2 * time.Second):
			t.Fatal("no event")
			return ""
		}
	}

	require.Equal(`data: {"chain":"dogecoinvm","height":7}`, next())

	// After the write timeout has passed, a new block still arrives.
	time.Sleep(300 * time.Millisecond)
	hub.publish(chainEvent{"dogecoinvm", 8})
	require.Equal(`data: {"chain":"dogecoinvm","height":8}`, next())
	hub.publish(chainEvent{"dogecoin", 100})
	require.Equal(`data: {"chain":"dogecoin","height":100}`, next())
}

// TestEventHubLimit checks streams are refused beyond the limit, and a
// slow stream doesn't hold up the others.
func TestEventHubLimit(t *testing.T) {
	require := require.New(t)
	hub := newEventHub()
	for i := 0; i < maxEventClients; i++ {
		_, _, ok := hub.subscribe()
		require.True(ok)
	}
	_, _, ok := hub.subscribe()
	require.False(ok)

	// Every channel is full after 8 events; publishing more must not block.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 20; i++ {
			hub.publish(chainEvent{"dogecoinvm", int64(i)})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publish blocked on a slow stream")
	}
}
