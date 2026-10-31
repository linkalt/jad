package native

import (
	"fmt"
	"path/filepath"
	"testing"
)

// BenchmarkTaskStoreListManyMatches lists the first page of a large store, where every task matches.
func BenchmarkTaskStoreListManyMatches(b *testing.B) {
	settings := taskTestSettings(filepath.Join(b.TempDir(), "tasks"))
	settings.MaxTasks = 5000
	settings.MaxStorageBytes = 1 << 40
	store := NewTaskStore(settings.StorageDir)
	for index := 0; index < 3000; index++ {
		_, err := store.Create(TaskInput{
			Title: fmt.Sprintf("task-%05d", index), Status: TaskTodo, Priority: TaskPriorityNormal,
			Tags: []string{}, Documents: []string{},
		}, settings)
		if err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := store.List(TaskListFilter{Limit: 10}); err != nil {
			b.Fatal(err)
		}
	}
}
