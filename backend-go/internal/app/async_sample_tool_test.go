package app

import (
	"path/filepath"
	"testing"
)

func asyncSampleToolPath(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "..", "sample", "tools", "native", "async.json"))
	if err != nil {
		t.Fatal(err)
	}
	return path
}
