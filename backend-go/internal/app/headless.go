package app

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"jad/backend-go/internal/headless"
)

// headlessTokenBytes is the size of the random API token a headless service accepts. The
// token never leaves the process: no listener serves the handler.
const headlessTokenBytes = 32

// NewHeadless builds a service for "jad run" and "jad eval": the same handler and resolver
// as the server, without a listener. Its differences from the server are all about being a
// short-lived process beside a server that may be running:
//
//   - The API token is random and in-process only, so JAD_API_TOKEN is not needed.
//   - The remote API is off: nothing listens, and its token file is not read.
//   - The start-time recovery scans are off: they inspect every session's processes and
//     sub-agent runs, which is the long-running server's job, not a one-turn process's.
//   - server.triggers are off: schedules and webhooks belong to the long-running server.
//
// Close the service when done; it stops the runs and child processes it started.
func NewHeadless(config Config) (*Service, *headless.Runner, error) {
	token, err := randomHeadlessToken()
	if err != nil {
		return nil, nil, err
	}
	config.HTTP.APIToken = token
	config.HTTP.AllowedOrigins = nil
	config.RemoteAPI.Enabled = false
	config.RecoverSubagentRunsAtStart = false
	config.Triggers.Enabled = false
	service, err := NewService(config)
	if err != nil {
		return nil, nil, err
	}
	return service, headless.NewRunner(service.Handler, token), nil
}

func randomHeadlessToken() (string, error) {
	raw := make([]byte, headlessTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("create the in-process API token: %w", err)
	}
	return hex.EncodeToString(raw), nil
}
