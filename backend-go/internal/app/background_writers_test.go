package app

import "jad/backend-go/internal/httpapi"

// The checkpoint restore handler finds the resolver's writer count through this port.
var _ httpapi.SessionWriters = (*Resolver)(nil)
