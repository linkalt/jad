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

// Directory is the view of a hub onto one remotes folder, as one session sees it.
type Directory struct {
	hub   *Hub
	dir   string
	scope Scope
}

// Scope is what makes a Directory a session's: whether the folder holds the bindings of the
// workspace layout (v2), and the keys the session may use, by binding. It is the one place a
// credential is chosen: every remote tool reads its entries and clients through the Directory,
// and nothing else can name a key file. The zero Scope is the v1 layout, whose entries carry
// their own token source.
type Scope struct {
	// Workspace says the folder holds bindings (workspace layout).
	Workspace bool
	// Keys are the session's effective keys by binding name (Key.File is a key file).
	Keys map[string]Key
}

// Key is one key a session may use: its name and the key file.
type Key struct{ Name, File string }

// Directory returns the hub's view of a remotes folder, for a v1 session.
func (h *Hub) Directory(dir string) *Directory { return &Directory{hub: h, dir: dir} }

// Scoped is the view with the scope of a session. The hub and its clients are shared.
func (d *Directory) Scoped(scope Scope) *Directory {
	return &Directory{hub: d.hub, dir: d.dir, scope: scope}
}

// Dir is the folder.
func (d *Directory) Dir() string { return d.dir }

// Workspace says the folder holds the bindings of the workspace layout, so the references are
// remote:<binding>/<session>/... and a session is sent qualified with the binding's workspace.
func (d *Directory) Workspace() bool { return d.scope.Workspace }

// ParseProfile reads a profile reference by the grammar of the directory's layout.
func (d *Directory) ParseProfile(s string) (Target, error) {
	if d.scope.Workspace {
		return ParseBindingProfile(s, d.Entry)
	}
	return ParseProfile(s, d.Entry)
}

// ParseSubagent reads a sub-agent reference by the grammar of the directory's layout.
func (d *Directory) ParseSubagent(s string) (Target, error) {
	if d.scope.Workspace {
		return ParseBindingSubagent(s, d.Entry)
	}
	return ParseSubagent(s, d.Entry)
}

// List reads every entry of the folder by the directory's layout.
func (d *Directory) List() ([]Entry, []error) {
	if d.scope.Workspace {
		return ListBindings(d.dir)
	}
	return List(d.dir)
}

// Entry reads the remote's file. In the workspace layout it is a binding, with the key this
// session has for it, if any (an unkeyed binding is returned as it is: Keyed is false).
func (d *Directory) Entry(name string) (Entry, error) {
	if !d.scope.Workspace {
		return Load(d.dir, name)
	}
	entry, err := LoadBinding(d.dir, name)
	if err != nil {
		return Entry{}, err
	}
	if key, ok := d.scope.Keys[name]; ok {
		entry.KeyName, entry.KeyFile = key.Name, key.File
	}
	return entry, nil
}

// Client returns the client of the named remote. The file is read on every call, so an edit
// applies to the next call; the client (and its connections) is reused while the entry is the same.
func (d *Directory) Client(name string) (*Client, error) {
	entry, err := d.Entry(name)
	if err != nil {
		return nil, err
	}
	return d.ClientFor(entry)
}

// ClientFor is Client for an entry the caller has already read and checked: the client is made for that
// entry, not for what the file holds now, so a caller that decided something from the entry (where it
// reaches) connects to what it decided on. Clients are kept per key file, so two sessions with
// different keys for one binding never share a credential.
func (d *Directory) ClientFor(entry Entry) (*Client, error) {
	return d.hub.client(d.dir+"\x00"+entry.Name+"\x00"+entry.KeyFile, entry)
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
