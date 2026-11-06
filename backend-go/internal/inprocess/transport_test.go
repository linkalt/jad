package inprocess

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestTransportReturnsTheHandlersStatusHeaderAndBody(t *testing.T) {
	client := &http.Client{Transport: &Transport{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization")+" "+string(data))
	})}}
	request, _ := http.NewRequest(http.MethodPost, "http://model.invalid/v1/responses", strings.NewReader("hello"))
	request.Header.Set("Authorization", "Bearer k")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	if response.StatusCode != 429 || response.Header.Get("Retry-After") != "3" || string(data) != "POST /v1/responses Bearer k hello" {
		t.Fatalf("response = %d %v %q", response.StatusCode, response.Header, data)
	}
}

// The caller reads each part as the handler flushes it, before the handler returns.
func TestTransportStreamsWhileTheHandlerRuns(t *testing.T) {
	release := make(chan struct{})
	client := &http.Client{Transport: &Transport{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "second\n")
	})}}
	response, err := client.Get("http://model.invalid/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	buffer := make([]byte, 6)
	if _, err := io.ReadFull(response.Body, buffer); err != nil || string(buffer) != "first\n" {
		t.Fatalf("first part = %q %v", buffer, err)
	}
	close(release)
	rest, _ := io.ReadAll(response.Body)
	if string(rest) != "second\n" {
		t.Fatalf("rest = %q", rest)
	}
}

// Closing the body cancels the handler's context, so a translator stops its upstream call.
func TestTransportCloseCancelsTheHandler(t *testing.T) {
	stopped := make(chan error, 1)
	client := &http.Client{Transport: &Transport{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		<-r.Context().Done()
		stopped <- r.Context().Err()
	})}}
	response, err := client.Get("http://model.invalid/")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	select {
	case err := <-stopped:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("handler context error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler was not cancelled")
	}
}

func TestTransportCancelledBeforeTheStatus(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &http.Client{Transport: &Transport{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	})}}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://model.invalid/", nil)
	if _, err := client.Do(request); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestTransportReportsAPanic(t *testing.T) {
	client := &http.Client{Transport: &Transport{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})}}
	if _, err := client.Get("http://model.invalid/"); err == nil || !strings.Contains(err.Error(), "panicked: boom") {
		t.Fatalf("error = %v", err)
	}
	// After the status was sent, the panic reaches the body reader.
	client = &http.Client{Transport: &Transport{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "partial")
		panic("late")
	})}}
	response, err := client.Get("http://model.invalid/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.ReadAll(response.Body); err == nil || !strings.Contains(err.Error(), "late") {
		t.Fatalf("body error = %v", err)
	}
}

func TestTransportHandlerWithoutAWriteAnswers200(t *testing.T) {
	client := &http.Client{Transport: &Transport{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})}}
	response, err := client.Get("http://model.invalid/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if data, _ := io.ReadAll(response.Body); response.StatusCode != 200 || len(data) != 0 {
		t.Fatalf("response = %d %q", response.StatusCode, data)
	}
}

// cancelledWithoutDone reports a cancelled context but never closes Done, so RoundTrip
// wakes on the handler's status alone and then sees the cancel: both are ready at once.
type cancelledWithoutDone struct{ context.Context }

func (cancelledWithoutDone) Done() <-chan struct{} { return nil }
func (cancelledWithoutDone) Err() error            { return context.Canceled }

func TestTransportCancelledWhileTheHandlerPanicsKeepsBoth(t *testing.T) {
	transport := &Transport{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})}
	request, _ := http.NewRequestWithContext(cancelledWithoutDone{context.Background()}, http.MethodGet, "http://model.invalid/", nil)
	_, err := transport.RoundTrip(request)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, errHandlerPanicked) || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error = %v, want the cancel and the panic", err)
	}
}

// A handler that panics after the request was cancelled reaches no caller: RoundTrip has
// returned the cancel. The panic is logged, with its stack.
func TestTransportLogsAPanicAfterTheCancel(t *testing.T) {
	logged := make(chan slog.Record, 1)
	ctx, cancel := context.WithCancel(context.Background())
	transport := &Transport{Logger: slog.New(recordSink{logged}), Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
		panic("after the cancel")
	})}
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://model.invalid/v1/responses", nil)
	if _, err := transport.RoundTrip(request); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	select {
	case record := <-logged:
		attrs := map[string]string{}
		record.Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value.String(); return true })
		if record.Level != slog.LevelError || attrs["panic"] != "after the cancel" || attrs["path"] != "/v1/responses" ||
			attrs["cancelled"] != "true" || !strings.Contains(attrs["stack"], "TestTransportLogsAPanicAfterTheCancel") {
			t.Fatalf("logged %v %q %v", record.Level, record.Message, attrs)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the panic was not logged")
	}
}

// recordSink is a slog handler that sends every record to a channel.
type recordSink struct{ records chan<- slog.Record }

func (s recordSink) Enabled(context.Context, slog.Level) bool { return true }
func (s recordSink) Handle(_ context.Context, record slog.Record) error {
	s.records <- record.Clone()
	return nil
}
func (s recordSink) WithAttrs([]slog.Attr) slog.Handler { return s }
func (s recordSink) WithGroup(string) slog.Handler      { return s }
