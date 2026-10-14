package app_test

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leeclarkuk/openkindred/runtime/internal/llm/llmtest"
)

func openaiConfig(endpoint string, extra string) string {
	return fmt.Sprintf(`{"model":{"provider":"openai-compatible","name":"m","endpoint":%q},"instructions":"x"%s}`, endpoint+"/v1", extra)
}

func waitBodies(t *testing.T, s *llmtest.Server, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for len(s.Bodies()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("model saw %d requests, want %d", len(s.Bodies()), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestConcurrencyIsBoundedAndSlotsAreReleased(t *testing.T) {
	release := make(chan struct{})
	model := llmtest.NewOpenAI(t,
		llmtest.Step{Release: release, Text: "one"},
		llmtest.Step{Release: release, Text: "two"},
		llmtest.Step{Text: "three"},
		llmtest.Step{Text: "four"})
	srv := start(t, openaiConfig(model.URL, `,"limits":{"maxConcurrent":2}`), nil)
	waitReady(t, srv.URL)

	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, _ := invoke(t, srv.URL, `{"input":"hi"}`)
			codes <- code
		}()
	}
	waitBodies(t, model, 2) // both slots are now held

	resp, err := http.Post(srv.URL+"/v1/invoke", "application/json", strings.NewReader(`{"input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("excess request: status %d, Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	code, body := invoke(t, srv.URL, `{"input":"hi"}`)
	if code != 503 || errKind(body) != "overloaded" || !strings.Contains(fmt.Sprint(body), "limit 2") {
		t.Fatalf("want a clear overload response, got %d %v", code, body)
	}
	if len(model.Bodies()) != 2 {
		t.Fatal("a rejected request must not reach the model")
	}
	if code, _ := get(t, srv.URL+"/readyz"); code != 200 {
		t.Fatal("overload must not make the pod unready")
	}

	close(release)
	wg.Wait()
	for i := 0; i < 2; i++ {
		if c := <-codes; c != 200 {
			t.Fatalf("admitted request failed: %d", c)
		}
	}
	// Slots come back after completion.
	if code, _ := invoke(t, srv.URL, `{"input":"hi"}`); code != 200 {
		t.Fatalf("slot not released: %d", code)
	}
}

func TestSlotsAreReleasedWhenRequestsFailOrDisconnect(t *testing.T) {
	model := llmtest.NewOpenAI(t,
		llmtest.Step{Status: 500},
		llmtest.Step{Hang: true},
		llmtest.Step{Text: "fine"})
	srv := start(t, openaiConfig(model.URL, `,"limits":{"maxConcurrent":1,"requestTimeout":"300ms"}`), nil)
	waitReady(t, srv.URL)

	if code, _ := invoke(t, srv.URL, `{"input":"a"}`); code != 502 {
		t.Fatalf("provider failure: %d", code)
	}
	if code, m := invoke(t, srv.URL, `{"input":"b"}`); code != 504 {
		t.Fatalf("timeout: %d %v", code, m)
	}
	if code, m := invoke(t, srv.URL, `{"input":"c"}`); code != 200 {
		t.Fatalf("slot leaked after failure paths: %d %v", code, m)
	}
}

func TestNegativeConcurrencyIsInvalid(t *testing.T) {
	model := llmtest.NewOpenAI(t)
	srv := start(t, openaiConfig(model.URL, `,"limits":{"maxConcurrent":-1}`), nil)
	time.Sleep(100 * time.Millisecond)
	if code, _ := get(t, srv.URL+"/readyz"); code != 503 {
		t.Fatal("negative limit must not start")
	}
}

func TestOversizedRequestBody(t *testing.T) {
	model := llmtest.NewOpenAI(t)
	srv := start(t, openaiConfig(model.URL, ""), nil)
	waitReady(t, srv.URL)
	code, m := invoke(t, srv.URL, `{"input":"`+strings.Repeat("a", 2<<20)+`"}`)
	if code != http.StatusRequestEntityTooLarge || errKind(m) != "bad_request" {
		t.Fatalf("%d %v", code, m)
	}
}

func TestCredentialsAreRedacted(t *testing.T) {
	const key = "sk-test-SUPERSECRET-123456"
	// A provider that echoes the credential back in its error message.
	model := llmtest.NewAnthropic(t, llmtest.Step{Status: 401, ErrorMessage: "invalid key " + key})
	srv := start(t, fmt.Sprintf(`{"model":{"provider":"anthropic","name":"m","endpoint":%q,"apiKeyEnv":"K"},"instructions":"x"}`, model.URL),
		map[string]string{"K": key})
	waitReady(t, srv.URL)
	code, m := invoke(t, srv.URL, `{"input":"hi"}`)
	if code != 502 || errKind(m) != "auth" {
		t.Fatalf("%d %v", code, m)
	}
	if strings.Contains(fmt.Sprint(m), "SUPERSECRET") || !strings.Contains(fmt.Sprint(m), "[REDACTED]") {
		t.Fatalf("credential leaked to caller: %v", m)
	}
}

func TestCredentialsInToolServerURLsAreNotLeaked(t *testing.T) {
	model := llmtest.NewOpenAI(t)
	srv := start(t, fmt.Sprintf(`{"model":{"provider":"openai-compatible","name":"m","endpoint":%q},"instructions":"x",
	  "mcpServers":[{"name":"docs","url":"http://user:hunter2pass@127.0.0.1:1/mcp?token=abcdef123456"}]}`, model.URL+"/v1"), nil)
	time.Sleep(150 * time.Millisecond)
	code, m := get(t, srv.URL+"/readyz")
	text := fmt.Sprint(m)
	if code != 503 || strings.Contains(text, "hunter2pass") || strings.Contains(text, "abcdef123456") {
		t.Fatalf("%d %s", code, text)
	}
}
