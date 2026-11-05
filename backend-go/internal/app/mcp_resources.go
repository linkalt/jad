package app

import (
	"context"
	"errors"
	"net/http"

	"jad/backend-go/internal/httpapi"
	mcpclient "jad/backend-go/internal/tools/mcp"
)

// ListMCPResources implements httpapi.SessionMCPResources.
func (r *Resolver) ListMCPResources(ctx context.Context, reference, profile string) (httpapi.MCPResourceList, error) {
	config, manager, err := r.promptSessionConfig(ctx, reference, profile)
	if err != nil {
		return httpapi.MCPResourceList{}, err
	}
	listed, err := manager.ListUserResources(ctx, config.MCPServers, config.WorkingDir)
	if err != nil {
		return httpapi.MCPResourceList{}, err
	}
	result := httpapi.MCPResourceList{Resources: make([]httpapi.MCPResource, 0, len(listed.Resources))}
	for _, resource := range listed.Resources {
		result.Resources = append(result.Resources, httpapi.MCPResource(resource))
	}
	for _, failure := range listed.Failures {
		result.Failures = append(result.Failures, httpapi.MCPPromptFailure(failure))
	}
	return result, nil
}

// ReadMCPResource implements httpapi.SessionMCPResources. A server outside the profile's
// toolsets is 404, a template that cannot be filled as asked is 400, and a server that
// cannot be reached or refuses the read is 502.
func (r *Resolver) ReadMCPResource(ctx context.Context, reference, profile string, request httpapi.MCPResourceRequest) (httpapi.MCPResourceText, error) {
	config, manager, err := r.promptSessionConfig(ctx, reference, profile)
	if err != nil {
		return httpapi.MCPResourceText{}, err
	}
	read, err := manager.ReadUserResource(ctx, config.MCPServers, config.WorkingDir, mcpclient.UserResourceRead(request))
	if err != nil {
		var userErr *mcpclient.UserPromptError
		if errors.As(err, &userErr) {
			status := http.StatusBadGateway
			switch {
			case userErr.NotFound:
				status = http.StatusNotFound
			case userErr.Invalid:
				status = http.StatusBadRequest
			}
			return httpapi.MCPResourceText{}, &httpapi.StatusError{Code: status, Message: userErr.Message}
		}
		return httpapi.MCPResourceText{}, err
	}
	text := httpapi.MCPResourceText{Server: read.Server, URI: read.URI, MIMEType: read.MIMEType, Text: read.Text}
	for _, attachment := range read.Attachments {
		text.Attachments = append(text.Attachments, httpapi.MCPResourceAttachment(attachment))
	}
	return text, nil
}

// SubscribeMCPResource implements httpapi.SessionMCPResourceWatch: a server outside the
// profile's toolsets is 404, a server that does not offer subscriptions or refuses is 502.
func (r *Resolver) SubscribeMCPResource(ctx context.Context, reference, profile, server, uri string, on bool) error {
	config, manager, err := r.promptSessionConfig(ctx, reference, profile)
	if err != nil {
		return err
	}
	if err := manager.SubscribeUserResource(ctx, config.MCPServers, config.WorkingDir, server, uri, on); err != nil {
		var userErr *mcpclient.UserPromptError
		if errors.As(err, &userErr) {
			status := http.StatusBadGateway
			switch {
			case userErr.NotFound:
				status = http.StatusNotFound
			case userErr.Invalid:
				status = http.StatusBadRequest
			}
			return &httpapi.StatusError{Code: status, Message: userErr.Message}
		}
		return err
	}
	return nil
}

// MCPResourceEvents implements httpapi.SessionMCPResourceWatch. Reading them does not mark
// them told: the profile's next run still hears of them.
func (r *Resolver) MCPResourceEvents(ctx context.Context, reference, profile string, after int64) (httpapi.MCPResourceEvents, error) {
	_, manager, err := r.promptSessionConfig(ctx, reference, profile)
	if err != nil {
		return httpapi.MCPResourceEvents{}, err
	}
	events, latest, subscriptions := manager.ResourceEvents(after)
	result := httpapi.MCPResourceEvents{Latest: latest}
	for _, event := range events {
		result.Events = append(result.Events, httpapi.MCPResourceEvent(event))
	}
	for _, subscription := range subscriptions {
		result.Subscriptions = append(result.Subscriptions, httpapi.MCPResourceSubscription(subscription))
	}
	return result, nil
}
