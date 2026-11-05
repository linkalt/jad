package mcp

import (
	"context"
	"fmt"
	"sync"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"
)

// listenProtocolVersion is the first MCP protocol version whose resource subscriptions are
// subscriptions/listen streams (SEP-2575). Before it, resources/subscribe is an ordinary
// request whose reply means the server holds the subscription.
const listenProtocolVersion = "2026-07-28"

// methodSubscriptionsAcknowledged is the notification a server sends first on a
// subscriptions/listen stream, once it holds the subscriptions it lists.
const methodSubscriptionsAcknowledged = "notifications/subscriptions/acknowledged"

// subscriptionAcks lets a subscribe wait until the server acknowledged it. Under the listen
// protocol the SDK's Subscribe only sends the listen request and returns; a change the
// server makes before it handled the request (the server handles requests concurrently)
// reaches nobody, so "subscribed" would be reported for a subscription the server does not
// hold yet.
type subscriptionAcks struct {
	mu      sync.Mutex
	waiters map[string][]chan struct{}
}

func subscriptionAckKey(server, uri string) string { return server + "\x00" + uri }

// wait registers a waiter for the acknowledgement of uri on server; the channel closes when
// it arrives.
func (a *subscriptionAcks) wait(server, uri string) chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.waiters == nil {
		a.waiters = map[string][]chan struct{}{}
	}
	acked := make(chan struct{})
	key := subscriptionAckKey(server, uri)
	a.waiters[key] = append(a.waiters[key], acked)
	return acked
}

// forget drops a waiter that stopped waiting.
func (a *subscriptionAcks) forget(server, uri string, acked chan struct{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := subscriptionAckKey(server, uri)
	waiters := a.waiters[key]
	for index, waiter := range waiters {
		if waiter == acked {
			a.waiters[key] = append(waiters[:index], waiters[index+1:]...)
			break
		}
	}
	if len(a.waiters[key]) == 0 {
		delete(a.waiters, key)
	}
}

// acknowledge wakes the waiters of each uri the server acknowledged.
func (a *subscriptionAcks) acknowledge(server string, uris []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, uri := range uris {
		key := subscriptionAckKey(server, uri)
		for _, acked := range a.waiters[key] {
			close(acked)
		}
		delete(a.waiters, key)
	}
}

// subscriptionAckMiddleware passes the server's subscription acknowledgements to m.acks.
func (m *Manager) subscriptionAckMiddleware(serverID string) mcpSDK.Middleware {
	return func(next mcpSDK.MethodHandler) mcpSDK.MethodHandler {
		return func(ctx context.Context, method string, request mcpSDK.Request) (mcpSDK.Result, error) {
			result, err := next(ctx, method, request)
			if method == methodSubscriptionsAcknowledged {
				if params, ok := request.GetParams().(*mcpSDK.SubscriptionsAcknowledgedParams); ok && params != nil {
					m.acks.acknowledge(serverID, params.Notifications.ResourceSubscriptions)
				}
			}
			return result, err
		}
	}
}

// usesListenSubscriptions reports whether the session's subscriptions are listen streams.
func usesListenSubscriptions(session *mcpSDK.ClientSession) bool {
	result := session.InitializeResult()
	return result != nil && result.ProtocolVersion >= listenProtocolVersion
}

// subscribeAcknowledged subscribes the session to uri and, under the listen protocol, waits
// until the server acknowledged it (or ctx ends), so a change right after the subscribe is
// not lost. A subscription the server does not acknowledge in time is withdrawn and refused.
func (m *Manager) subscribeAcknowledged(ctx context.Context, serverID string, session *mcpSDK.ClientSession, uri string) error {
	if !usesListenSubscriptions(session) {
		return session.Subscribe(ctx, &mcpSDK.SubscribeParams{URI: uri})
	}
	acked := m.acks.wait(serverID, uri)
	if err := session.Subscribe(ctx, &mcpSDK.SubscribeParams{URI: uri}); err != nil {
		m.acks.forget(serverID, uri, acked)
		return err
	}
	select {
	case <-acked:
		return nil
	case <-ctx.Done():
		m.acks.forget(serverID, uri, acked)
		// The SDK keeps a URI it sent a listen for: withdraw it, so a later subscribe sends a new one.
		_ = session.Unsubscribe(context.WithoutCancel(ctx), &mcpSDK.UnsubscribeParams{URI: uri})
		return fmt.Errorf("the server did not acknowledge the subscription: %w", ctx.Err())
	}
}
