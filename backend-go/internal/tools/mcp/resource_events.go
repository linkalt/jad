package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"

	"jad/backend-go/internal/sessionfs"
)

// Resource change notifications. A server that offers resources may tell its clients that
// its list changed (notifications/resources/list_changed) and, for a resource a client
// subscribed to (resources/subscribe), that the resource changed
// (notifications/resources/updated). The manager keeps the newest of these per server, at
// most the server file's resource_events_max, and the subscriptions it made, which it makes
// again when it connects to the server anew.

const (
	// ResourceEventUpdated is a subscribed resource that changed; ResourceEventListChanged is
	// a server whose resource list changed.
	ResourceEventUpdated     = "updated"
	ResourceEventListChanged = "list_changed"
)

// ResourceEvent is one change notification. Seq grows by one per event of the manager.
type ResourceEvent struct {
	Seq    int64     `json:"seq"`
	Server string    `json:"server"`
	Kind   string    `json:"kind"`
	URI    string    `json:"uri,omitempty"`
	At     time.Time `json:"at"`
}

// ResourceSubscription is one resource the manager is subscribed to.
type ResourceSubscription struct {
	Server string `json:"server"`
	URI    string `json:"uri"`
}

type resourceEvents struct {
	mu       sync.Mutex
	seq      int64
	byServer map[string][]ResourceEvent
	// subscribed is server -> URI -> true.
	subscribed map[string]map[string]bool
	// noted is the last Seq a run was told about (TakeResourceChanges).
	noted int64
}

func (e *resourceEvents) record(server, kind, uri string, limit int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.byServer == nil {
		e.byServer = map[string][]ResourceEvent{}
	}
	e.seq++
	events := append(e.byServer[server], ResourceEvent{Seq: e.seq, Server: server, Kind: kind, URI: uri, At: time.Now().UTC()})
	if limit > 0 && len(events) > limit {
		events = append([]ResourceEvent(nil), events[len(events)-limit:]...)
	}
	e.byServer[server] = events
}

// since is every kept event after seq, oldest first, and the newest Seq.
func (e *resourceEvents) since(after int64) ([]ResourceEvent, int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var events []ResourceEvent
	for _, kept := range e.byServer {
		for _, event := range kept {
			if event.Seq > after {
				events = append(events, event)
			}
		}
	}
	sort.Slice(events, func(i, j int) bool { return events[i].Seq < events[j].Seq })
	return events, e.seq
}

func (e *resourceEvents) isSubscribed(server, uri string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.subscribed[server][uri]
}

func (e *resourceEvents) setSubscribed(server, uri string, on bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.subscribed == nil {
		e.subscribed = map[string]map[string]bool{}
	}
	if on {
		if e.subscribed[server] == nil {
			e.subscribed[server] = map[string]bool{}
		}
		e.subscribed[server][uri] = true
		return
	}
	delete(e.subscribed[server], uri)
}

func (e *resourceEvents) subscriptions(server string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	uris := make([]string, 0, len(e.subscribed[server]))
	for uri := range e.subscribed[server] {
		uris = append(uris, uri)
	}
	sort.Strings(uris)
	return uris
}

// clientOptions are the options of a server's client: its change notifications go to the
// manager's event log.
func (m *Manager) clientOptions(serverID string, limit int) *mcpSDK.ClientOptions {
	return &mcpSDK.ClientOptions{
		ResourceUpdatedHandler: func(_ context.Context, request *mcpSDK.ResourceUpdatedNotificationRequest) {
			if request != nil && request.Params != nil {
				m.events.record(serverID, ResourceEventUpdated, request.Params.URI, limit)
			}
		},
		ResourceListChangedHandler: func(context.Context, *mcpSDK.ResourceListChangedRequest) {
			m.events.record(serverID, ResourceEventListChanged, "", limit)
		},
	}
}

// resubscribe makes the subscriptions of a server again on a new connection. A server that
// refuses one is left without it, and the subscription is dropped.
func (m *Manager) resubscribe(ctx context.Context, current *connection) {
	for _, uri := range m.events.subscriptions(current.serverID) {
		err := current.request(ctx, func(ctx context.Context, session *mcpSDK.ClientSession) error {
			return m.subscribeAcknowledged(ctx, current.serverID, session, uri)
		})
		if err != nil {
			m.events.setSubscribed(current.serverID, uri, false)
		}
	}
}

// ResourceEvents are the change notifications kept after seq, oldest first, the newest Seq
// (the next call's after) and the subscriptions held now.
func (m *Manager) ResourceEvents(after int64) ([]ResourceEvent, int64, []ResourceSubscription) {
	events, latest := m.events.since(after)
	m.events.mu.Lock()
	servers := make([]string, 0, len(m.events.subscribed))
	for server := range m.events.subscribed {
		servers = append(servers, server)
	}
	m.events.mu.Unlock()
	sort.Strings(servers)
	var subscriptions []ResourceSubscription
	for _, server := range servers {
		for _, uri := range m.events.subscriptions(server) {
			subscriptions = append(subscriptions, ResourceSubscription{Server: server, URI: uri})
		}
	}
	return events, latest, subscriptions
}

// TakeResourceChanges returns the notifications no run was told about yet and marks them
// told: the next run of the profile hears of each change once.
func (m *Manager) TakeResourceChanges() []ResourceEvent {
	m.events.mu.Lock()
	after := m.events.noted
	m.events.mu.Unlock()
	events, latest := m.events.since(after)
	m.events.mu.Lock()
	if latest > m.events.noted {
		m.events.noted = latest
	}
	m.events.mu.Unlock()
	return events
}

// ResourceChangesNote words changes for the model, one line each, or "" for none.
func ResourceChangesNote(events []ResourceEvent) string {
	if len(events) == 0 {
		return ""
	}
	lines := []string{"MCP resources changed since your last turn (read them again if they matter):"}
	for _, event := range events {
		at := event.At.Format(time.RFC3339)
		switch event.Kind {
		case ResourceEventListChanged:
			lines = append(lines, fmt.Sprintf("- server %s: its resource list changed (%s)", event.Server, at))
			continue
		case ResourceEventToolsChanged:
			lines = append(lines, fmt.Sprintf("- server %s: its tool list changed; the tools of this turn are listed anew (%s)", event.Server, at))
			continue
		case ResourceEventPromptsChanged:
			lines = append(lines, fmt.Sprintf("- server %s: its prompt list changed (%s)", event.Server, at))
			continue
		}
		lines = append(lines, fmt.Sprintf("- server %s: %s changed (%s)", event.Server, event.URI, at))
	}
	return strings.Join(lines, "\n")
}

// SubscribeUserResource subscribes to (on) or unsubscribes from one resource of a server with
// resources: true that offers subscriptions. A server that does not is refused.
func (m *Manager) SubscribeUserResource(ctx context.Context, servers []sessionfs.MCPServerReference, cwd, serverID, uri string, on bool) error {
	var server *sessionfs.MCPServerReference
	for _, candidate := range resourceServers(servers) {
		if candidate.ID == serverID {
			server = &candidate
			break
		}
	}
	if server == nil {
		return &UserPromptError{NotFound: true, Message: fmt.Sprintf("no MCP server %q with resources: true in the profile's toolsets", serverID)}
	}
	if strings.TrimSpace(uri) == "" {
		return &UserPromptError{Invalid: true, Message: "uri must be a non-empty string"}
	}
	current, err := m.resourceConnection(ctx, *server, cwd)
	if err != nil {
		return &UserPromptError{Message: err.Error()}
	}
	if err := subscribe(ctx, m, current, uri, on); err != nil {
		return &UserPromptError{Message: fmt.Sprintf("MCP server %s: %s", server.ID, err.Error())}
	}
	return nil
}

func subscribe(ctx context.Context, m *Manager, current *connection, uri string, on bool) error {
	capabilities := current.capabilities()
	if capabilities == nil || capabilities.Resources == nil || !capabilities.Resources.Subscribe {
		return fmt.Errorf("the server does not offer resource subscriptions")
	}
	if !current.receivesNotifications() {
		return fmt.Errorf("resource subscriptions work with stdio servers only for now: a streamable_http connection does not open the stream the server's notifications travel on")
	}
	if on && m.events.isSubscribed(current.serverID, uri) {
		// The session already holds it; under the listen protocol a second Subscribe sends
		// nothing, so no acknowledgement would come.
		return nil
	}
	err := current.request(ctx, func(ctx context.Context, session *mcpSDK.ClientSession) error {
		if on {
			return m.subscribeAcknowledged(ctx, current.serverID, session, uri)
		}
		return session.Unsubscribe(ctx, &mcpSDK.UnsubscribeParams{URI: uri})
	})
	if err != nil {
		return fmt.Errorf("resource %s: %s", uri, userRequestError(err, current))
	}
	m.events.setSubscribed(current.serverID, uri, on)
	return nil
}

// receivesNotifications reports whether the server's own messages reach the client. A stdio
// connection carries them on its one stream. A streamable_http one does not: the SDK opens
// the stream they travel on (the standalone GET) from a hook of its own connection type,
// which the structured-content wrapper around the connection hides.
func (c *connection) receivesNotifications() bool {
	return c.stdioCommand != nil
}
