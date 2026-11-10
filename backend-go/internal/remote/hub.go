package remote

import (
	"context"
	"reflect"
	"sync"
)

// Hub owns the clients of one process and the goroutines that follow remote runs, so closing
// it ends them all. One Hub serves every session; each session names its own remotes folder.
type Hub struct {
	environment func(string) string

	mu      sync.Mutex
	clients map[string]*hubClient
	closed  bool
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

type hubClient struct {
	entry  Entry
	client *Client
}

// NewHub returns a hub whose clients read token_env through environment (nil is os.Getenv).
func NewHub(environment func(string) string) *Hub {
	ctx, cancel := context.WithCancel(context.Background())
	return &Hub{environment: environment, clients: map[string]*hubClient{}, ctx: ctx, cancel: cancel}
}

// Directory is the view of a hub onto one remotes folder.
type Directory struct {
	hub *Hub
	dir string
}

// Directory returns the hub's view of a remotes folder.
func (h *Hub) Directory(dir string) *Directory { return &Directory{hub: h, dir: dir} }

// Dir is the folder.
func (d *Directory) Dir() string { return d.dir }

// Entry reads the remote's file.
func (d *Directory) Entry(name string) (Entry, error) { return Load(d.dir, name) }

// Client returns the client of the named remote. The file is read on every call, so an edit
// applies to the next call; the client (and its connections) is reused while the entry is the same.
func (d *Directory) Client(name string) (*Client, error) {
	entry, err := Load(d.dir, name)
	if err != nil {
		return nil, err
	}
	return d.hub.client(d.dir+"\x00"+name, entry)
}

// ClientFor is Client for an entry the caller has already read and checked: the client is made for that
// entry, not for what the file holds now, so a caller that decided something from the entry (where it
// reaches) connects to what it decided on.
func (d *Directory) ClientFor(entry Entry) (*Client, error) {
	return d.hub.client(d.dir+"\x00"+entry.Name, entry)
}

func (h *Hub) client(key string, entry Entry) (*Client, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, context.Canceled
	}
	if existing := h.clients[key]; existing != nil && reflect.DeepEqual(existing.entry, entry) {
		return existing.client, nil
	}
	client := NewClient(entry, Options{Environment: h.environment})
	if existing := h.clients[key]; existing != nil {
		existing.client.Close()
	}
	h.clients[key] = &hubClient{entry: entry, client: client}
	return client, nil
}

// Go runs fn on a goroutine the hub waits for at Close; fn's context ends at Close. It returns
// false, without running fn, once the hub is closed.
func (d *Directory) Go(fn func(ctx context.Context)) bool { return d.hub.Go(fn) }

// Go is Directory.Go.
func (h *Hub) Go(fn func(ctx context.Context)) bool {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return false
	}
	h.wg.Add(1)
	h.mu.Unlock()
	go func() {
		defer h.wg.Done()
		fn(h.ctx)
	}()
	return true
}

// Context ends when the hub closes.
func (h *Hub) Context() context.Context { return h.ctx }

// Close cancels every followed run, waits for their goroutines and drops all connections.
func (h *Hub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	clients := h.clients
	h.clients = map[string]*hubClient{}
	h.mu.Unlock()
	h.cancel()
	h.wg.Wait()
	for _, entry := range clients {
		entry.client.Close()
	}
}
