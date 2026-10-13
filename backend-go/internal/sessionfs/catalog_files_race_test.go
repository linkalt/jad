package sessionfs

import (
	"errors"
	"io/fs"
	"sync"
	"testing"
)

func TestCatalogCreateAndDeleteRaceOnACategoryDirectory(t *testing.T) {
	layout := Layout{Models: t.TempDir()}
	body := []byte(`{"model":"m"}`)
	var wg sync.WaitGroup
	failures := make(chan error, 4000)
	for worker := 0; worker < 8; worker++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				if _, _, err := WriteCatalogFile(layout, nil, CatalogModels, "cat/a", body, WriteCondition{IfNoneMatch: "*"}, false, nil); err != nil {
					if ce, ok := err.(*CatalogError); ok && ce.Kind == CatalogPreconditionFailed {
						continue
					}
					failures <- err
				}
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				if err := DeleteCatalogFile(layout, nil, CatalogModels, "cat/a", CatalogETag(body)); err != nil {
					if ce, ok := err.(*CatalogError); ok && (ce.Kind == CatalogNotFound || ce.Kind == CatalogPreconditionFailed) {
						continue
					}
					if errors.Is(err, fs.ErrNotExist) {
						continue
					}
					failures <- err
				}
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Errorf("unexpected: %v", err)
		break
	}
}
