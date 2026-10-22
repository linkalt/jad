package asyncmgr

import (
	"encoding/json"
	"sync/atomic"
)

type blockingDriver struct {
	kind    Kind
	release chan struct{}
	entered atomic.Bool
}

func (d *blockingDriver) Kind() Kind { return d.kind }

func (d *blockingDriver) Stop(id, reason string) {
	d.entered.Store(true)
	<-d.release
}

func mustJSON(v any) []byte {
	data, _ := json.Marshal(v)
	return data
}
