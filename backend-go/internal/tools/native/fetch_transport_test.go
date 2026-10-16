package native

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func countingFetchServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var opened atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		_, _ = writer.Write([]byte("ok"))
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			opened.Add(1)
		}
	}
	server.Start()
	t.Cleanup(server.Close)
	return server, &opened
}

func pinnedTarget(t *testing.T, server *httptest.Server, hostname string) validatedFetchURL {
	t.Helper()
	port := strings.TrimPrefix(server.URL, "http://127.0.0.1:")
	target, err := url.Parse("http://" + hostname + ":" + port + "/")
	if err != nil {
		t.Fatal(err)
	}
	return validatedFetchURL{URL: target, Address: net.IPAddr{IP: net.ParseIP("127.0.0.1")}}
}

func fetchOnce(t *testing.T, requester pinnedFetchRequester, target validatedFetchURL) {
	t.Helper()
	response, err := requester.Do(context.Background(), target)
	if err != nil {
		t.Fatalf("pinned request: %v", err)
	}
	if _, err := readFetchResponse(response, 1_000); err != nil {
		t.Fatalf("read response: %v", err)
	}
}

func TestPinnedFetchRequesterReusesConnectionWhenKeepAliveIsOn(t *testing.T) {
	server, opened := countingFetchServer(t)
	pool := &fetchTransportPool{}
	t.Cleanup(pool.close)
	requester := pinnedFetchRequester{pool: pool, keepAlive: time.Minute, maxHosts: 4}
	target := pinnedTarget(t, server, "reuse.invalid")
	fetchOnce(t, requester, target)
	fetchOnce(t, requester, target)
	fetchOnce(t, requester, target)
	if got := opened.Load(); got != 1 {
		t.Fatalf("connections opened with keep-alive = %d, want 1", got)
	}
}

func TestPinnedFetchRequesterOpensFreshConnectionWhenKeepAliveIsOff(t *testing.T) {
	server, opened := countingFetchServer(t)
	pool := &fetchTransportPool{}
	t.Cleanup(pool.close)
	for _, requester := range []pinnedFetchRequester{
		{},
		{pool: pool, keepAlive: 0, maxHosts: 4},
	} {
		opened.Store(0)
		target := pinnedTarget(t, server, "fresh.invalid")
		fetchOnce(t, requester, target)
		fetchOnce(t, requester, target)
		if got := opened.Load(); got != 2 {
			t.Fatalf("connections opened without keep-alive = %d, want 2", got)
		}
	}
}

func TestPinnedFetchRequesterDoesNotShareConnectionsAcrossHostnames(t *testing.T) {
	server, opened := countingFetchServer(t)
	pool := &fetchTransportPool{}
	t.Cleanup(pool.close)
	requester := pinnedFetchRequester{pool: pool, keepAlive: time.Minute, maxHosts: 4}
	fetchOnce(t, requester, pinnedTarget(t, server, "one.invalid"))
	fetchOnce(t, requester, pinnedTarget(t, server, "two.invalid"))
	if got := opened.Load(); got != 2 {
		t.Fatalf("connections opened for two hostnames on one address = %d, want 2", got)
	}
}

func TestPinnedFetchRequesterDoesNotShareConnectionsAcrossAddresses(t *testing.T) {
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Skipf("cannot listen on all interfaces: %v", err)
	}
	var opened atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		_, _ = writer.Write([]byte("ok"))
	}))
	server.Listener.Close()
	server.Listener = listener
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			opened.Add(1)
		}
	}
	server.Start()
	t.Cleanup(server.Close)
	port := listener.Addr().(*net.TCPAddr).Port
	pool := &fetchTransportPool{}
	t.Cleanup(pool.close)
	requester := pinnedFetchRequester{pool: pool, keepAlive: time.Minute, maxHosts: 4}
	target, err := url.Parse("http://same-name.invalid:" + strconv.Itoa(port) + "/")
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"127.0.0.1", "127.0.0.2", "127.0.0.1"} {
		fetchOnce(t, requester, validatedFetchURL{URL: target, Address: net.IPAddr{IP: net.ParseIP(address)}})
	}
	if got := opened.Load(); got != 2 {
		t.Fatalf("connections opened for one name on two addresses = %d, want 2", got)
	}
}

func TestPinnedFetchRequesterAfterPoolCloseStillFetches(t *testing.T) {
	server, opened := countingFetchServer(t)
	pool := &fetchTransportPool{}
	requester := pinnedFetchRequester{pool: pool, keepAlive: time.Minute, maxHosts: 4}
	target := pinnedTarget(t, server, "closed.invalid")
	fetchOnce(t, requester, target)
	pool.close()
	fetchOnce(t, requester, target)
	fetchOnce(t, requester, target)
	if got := opened.Load(); got != 3 {
		t.Fatalf("connections opened around a closed pool = %d, want 3", got)
	}
	if len(pool.entries) != 0 {
		t.Fatalf("closed pool kept %d entries", len(pool.entries))
	}
}

func TestFetchTransportKeyIncludesValidatedAddress(t *testing.T) {
	pool := &fetchTransportPool{}
	t.Cleanup(pool.close)
	create := func() *http.Transport { return &http.Transport{} }
	a := fetchTransportKey{scheme: "https", host: "example.com", port: "443", ip: "93.184.216.34"}
	b := a
	b.ip = "93.184.216.35"
	first := pool.get(a, time.Minute, 4, create)
	if pool.get(a, time.Minute, 4, create) != first {
		t.Fatal("same destination did not return the pooled transport")
	}
	if pool.get(b, time.Minute, 4, create) == first {
		t.Fatal("a different validated address reused the transport of another address")
	}
}

func TestFetchTransportPoolEvictsLeastRecentlyUsedAtLimit(t *testing.T) {
	pool := &fetchTransportPool{}
	t.Cleanup(pool.close)
	create := func() *http.Transport { return &http.Transport{} }
	key := func(host string) fetchTransportKey {
		return fetchTransportKey{scheme: "https", host: host, port: "443", ip: "93.184.216.34"}
	}
	first := pool.get(key("a"), time.Hour, 2, create)
	pool.get(key("b"), time.Hour, 2, create)
	pool.get(key("a"), time.Hour, 2, create)
	pool.get(key("c"), time.Hour, 2, create)
	if len(pool.entries) != 2 {
		t.Fatalf("pool holds %d entries, want 2", len(pool.entries))
	}
	if _, ok := pool.entries[key("b")]; ok {
		t.Fatal("least recently used host was kept")
	}
	if pool.entries[key("a")].transport != first {
		t.Fatal("recently used host lost its transport")
	}
}

func TestFetchTransportPoolDropsIdleEntries(t *testing.T) {
	pool := &fetchTransportPool{}
	t.Cleanup(pool.close)
	create := func() *http.Transport { return &http.Transport{} }
	old := fetchTransportKey{scheme: "https", host: "old", port: "443", ip: "93.184.216.34"}
	pool.get(old, time.Millisecond, 4, create)
	time.Sleep(20 * time.Millisecond)
	pool.get(fetchTransportKey{scheme: "https", host: "new", port: "443", ip: "93.184.216.34"}, time.Millisecond, 4, create)
	if _, ok := pool.entries[old]; ok {
		t.Fatal("idle entry was kept")
	}
}

func TestFetchTransportPoolTreatsNonPositiveMaxHostsAsOne(t *testing.T) {
	pool := &fetchTransportPool{}
	t.Cleanup(pool.close)
	create := func() *http.Transport { return &http.Transport{} }
	for _, host := range []string{"a", "b"} {
		if pool.get(fetchTransportKey{host: host}, time.Hour, 0, create) == nil {
			t.Fatal("get returned nil")
		}
	}
	if len(pool.entries) != 1 {
		t.Fatalf("pool holds %d entries, want 1", len(pool.entries))
	}
}

func TestPinnedFetchRequesterConcurrentCallsAcrossEvictionAreRaceFree(t *testing.T) {
	server, _ := countingFetchServer(t)
	pool := &fetchTransportPool{}
	requester := pinnedFetchRequester{pool: pool, keepAlive: time.Minute, maxHosts: 1}
	var group sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			for round := 0; round < 10; round++ {
				name := "w" + strconv.Itoa((worker+round)%3) + ".invalid"
				response, err := requester.Do(context.Background(), pinnedTarget(t, server, name))
				if err != nil {
					t.Errorf("pinned request: %v", err)
					return
				}
				if _, err := readFetchResponse(response, 1_000); err != nil {
					t.Errorf("read response: %v", err)
					return
				}
			}
		}(worker)
	}
	group.Wait()
	pool.close()
}

func TestPinnedFetchRequesterIsBuiltFromFetchSettings(t *testing.T) {
	for _, test := range []struct {
		keepAliveMS int64
		opened      int64
	}{{60_000, 1}, {0, 2}} {
		server, opened := countingFetchServer(t)
		store := NewFetchStore(t.TempDir())
		settings := fetchTestSettings(store.directory)
		settings.KeepAliveMS, settings.MaxKeepAliveHosts = test.keepAliveMS, 4
		requester := newPinnedFetchRequester(settings, store)
		target := pinnedTarget(t, server, "wired.invalid")
		fetchOnce(t, requester, target)
		fetchOnce(t, requester, target)
		store.Close()
		if got := opened.Load(); got != test.opened {
			t.Fatalf("keep_alive_ms=%d opened %d connections, want %d", test.keepAliveMS, got, test.opened)
		}
	}
}
