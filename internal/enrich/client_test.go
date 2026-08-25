package enrich

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient wires a Client to a fake OpenRouter: /key answers
// keyStatus, /chat/completions goes to handler. Sleeps are recorded,
// never real.
func newTestClient(t *testing.T, keyStatus int, handler http.HandlerFunc) (*Client, *[]time.Duration) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/key", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(keyStatus)
	})
	if handler != nil {
		mux.HandleFunc("/chat/completions", handler)
	}
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	var slept []time.Duration
	c := &Client{
		APIKey:  "test-key",
		BaseURL: ts.URL,
		Sleep: func(ctx context.Context, d time.Duration) bool {
			slept = append(slept, d)
			return ctx.Err() == nil
		},
	}
	return c, &slept
}

// chatOK writes a 200 chat response with the given assistant content.
func chatOK(w http.ResponseWriter, content string) {
	fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`, content)
}

func TestClassify(t *testing.T) {
	moderation := []byte(`{"error":{"code":403,"message":"flagged","metadata":{"reasons":["nudity"],"flagged_input":"…"}}}`)
	guardrail := []byte(`{"error":{"code":403,"message":"blocked","metadata":{"patterns":["x"]}}}`)
	cases := []struct {
		name   string
		status int
		body   []byte
		want   Class
	}{
		{"401 bad key", 401, nil, ClassHard},
		{"402 no credits", 402, nil, ClassHard},
		{"403 bare (permissions)", 403, []byte(`{"error":{"code":403,"message":"forbidden"}}`), ClassHard},
		{"403 moderation", 403, moderation, ClassPermanent},
		{"403 guardrail", 403, guardrail, ClassPermanent},
		{"404 unknown model", 404, nil, ClassHard},
		{"408 timeout", 408, nil, ClassTransient},
		{"429 rate limit", 429, nil, ClassTransient},
		{"500", 500, nil, ClassTransient},
		{"502 provider down", 502, nil, ClassTransient},
		{"503 no provider", 503, nil, ClassTransient},
		{"504", 504, nil, ClassTransient},
		{"400 invalid_image", 400, []byte(`{"error":{"code":400,"message":"invalid image","metadata":{"error_type":"invalid_image"}}}`), ClassPermanent},
		{"422", 422, nil, ClassPermanent},
		{"413 too large", 413, nil, ClassPermanent},
	}
	for _, c := range cases {
		if got := ClassOf(classify(c.status, c.body)); got != c.want {
			t.Errorf("%s: class = %v, want %v", c.name, got, c.want)
		}
	}
	// 413 must additionally be recognizable for split-and-retry.
	var tooLarge *errTooLarge
	if !errors.As(classify(413, nil), &tooLarge) {
		t.Error("413 should be an errTooLarge")
	}
}

func TestBackoffRetriesThenSucceeds(t *testing.T) {
	var calls atomic.Int64
	c, slept := newTestClient(t, 200, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(429)
			return
		}
		chatOK(w, "hello")
	})
	cr, err := c.complete(context.Background(), "m", nil, nil, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if cr.Choices[0].Message.Content != "hello" {
		t.Errorf("content = %q", cr.Choices[0].Message.Content)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", calls.Load())
	}
	if len(*slept) != 2 || (*slept)[0] != 1*time.Second || (*slept)[1] != 2*time.Second {
		t.Errorf("backoff = %v, want [1s 2s]", *slept)
	}
}

func TestRetryAfterHonored(t *testing.T) {
	var calls atomic.Int64
	c, slept := newTestClient(t, 200, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(429)
			return
		}
		chatOK(w, "ok")
	})
	if _, err := c.complete(context.Background(), "m", nil, nil, 10, 0); err != nil {
		t.Fatal(err)
	}
	if len(*slept) != 1 || (*slept)[0] != 7*time.Second {
		t.Errorf("slept = %v, want [7s]", *slept)
	}
}

func TestTransientExhaustion(t *testing.T) {
	var calls atomic.Int64
	c, _ := newTestClient(t, 200, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(503)
	})
	c.MaxRetries = 3
	_, err := c.complete(context.Background(), "m", nil, nil, 10, 0)
	if err == nil || ClassOf(err) != ClassTransient {
		t.Fatalf("err = %v, want transient", err)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", calls.Load())
	}
}

func TestHardErrorNoRetry(t *testing.T) {
	var calls atomic.Int64
	c, _ := newTestClient(t, 200, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(402)
	})
	_, err := c.complete(context.Background(), "m", nil, nil, 10, 0)
	if ClassOf(err) != ClassHard {
		t.Fatalf("err = %v, want hard", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1 (hard errors must not retry)", calls.Load())
	}
}

// Errors can arrive inside a 200 body; they classify by their code.
func TestInBandErrors(t *testing.T) {
	var calls atomic.Int64
	c, _ := newTestClient(t, 200, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			fmt.Fprint(w, `{"error":{"code":429,"message":"rate limited"}}`)
			return
		}
		chatOK(w, "recovered")
	})
	cr, err := c.complete(context.Background(), "m", nil, nil, 10, 0)
	if err != nil || cr.Choices[0].Message.Content != "recovered" {
		t.Fatalf("in-band transient should retry: %v", err)
	}

	c2, _ := newTestClient(t, 200, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"error":{"code":400,"message":"invalid image","metadata":{"error_type":"invalid_image"}}}`)
	})
	_, err = c2.complete(context.Background(), "m", nil, nil, 10, 0)
	if ClassOf(err) != ClassPermanent {
		t.Errorf("in-band 400 should be permanent, got %v", err)
	}
}

func TestPreflight(t *testing.T) {
	c, _ := newTestClient(t, 401, nil)
	err := c.Preflight(context.Background())
	if err == nil || ClassOf(err) != ClassHard {
		t.Errorf("bad key: err = %v, want hard", err)
	}

	c2, _ := newTestClient(t, 200, nil)
	if err := c2.Preflight(context.Background()); err != nil {
		t.Errorf("good key: %v", err)
	}

	c3 := &Client{APIKey: " "}
	err = c3.Preflight(context.Background())
	if err == nil || !strings.Contains(err.Error(), APIKeyEnv) {
		t.Errorf("empty key must name %s: %v", APIKeyEnv, err)
	}
}

func TestSplitTextDescription(t *testing.T) {
	ocr, desc := splitTextDescription("TEXT: total 45 AED\n\nDESCRIPTION: a receipt on a table")
	if ocr != "total 45 AED" || desc != "a receipt on a table" {
		t.Errorf("got (%q, %q)", ocr, desc)
	}
	ocr, desc = splitTextDescription("TEXT: none\n\nDESCRIPTION: a sunset")
	if ocr != "" || desc != "a sunset" {
		t.Errorf("'none' should normalize: (%q, %q)", ocr, desc)
	}
	ocr, desc = splitTextDescription("just a plain reply")
	if ocr != "" || desc != "just a plain reply" {
		t.Errorf("markerless: (%q, %q)", ocr, desc)
	}
}
