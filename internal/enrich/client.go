// Package enrich turns the media files queued in .unenriched/ into
// searchable text via OpenRouter: image OCR + descriptions, voice-note
// transcripts, and PDF body text. The queue directory is the whole
// state — a file is deleted the moment its text is committed to the
// DB, a permanently-failing file is quarantined in .unenriched/failed/,
// and everything else stays queued for the next run.
package enrich

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// APIKeyEnv is where the enrich command reads the OpenRouter key from.
const APIKeyEnv = "OPENROUTER_API_KEY"

const defaultBaseURL = "https://openrouter.ai/api/v1"

// Class buckets every failure by what retrying would achieve.
// The classification follows OpenRouter's documented error semantics
// (openrouter.ai/docs/api-reference/errors), not just the status family:
// notably 403 is a MODERATION flag when its metadata says so (per-item,
// permanent) and an auth/permission failure otherwise (hard).
type Class int

const (
	// ClassTransient: the world had a problem (rate limit, outage,
	// timeout). Retry is always legitimate — in-run with backoff, then
	// again on the next run.
	ClassTransient Class = iota
	// ClassPermanent: the item has a problem (flagged content, bad
	// bytes, over limits). No retry will ever help — quarantine.
	ClassPermanent
	// ClassHard: the run has a problem (bad key, no credits, unknown
	// model). Abort everything; nothing item-level will succeed.
	ClassHard
)

// Error is a classified OpenRouter failure.
type Error struct {
	Class  Class
	Status int
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

// ClassOf classifies any error for the runner: a typed *Error keeps its
// class; everything else (local validation, decode garbage) counts as
// permanent — the bytes were readable but unusable, retrying won't help.
// Context cancellation is transient (the run is stopping, not the item
// failing).
func ClassOf(err error) Class {
	var e *Error
	if errors.As(err, &e) {
		return e.Class
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ClassTransient
	}
	return ClassPermanent
}

// errTooLarge marks an HTTP 413 so the document engine can split the
// PDF and retry instead of quarantining. Any other caller unwraps to
// the permanent *Error.
type errTooLarge struct{ e *Error }

func (t *errTooLarge) Error() string { return t.e.Msg }
func (t *errTooLarge) Unwrap() error { return t.e }

// errorMetadata is the moderation/guardrail metadata OpenRouter attaches
// to 403s that are content decisions rather than auth decisions.
type errorBody struct {
	Error *struct {
		Code     int    `json:"code"`
		Message  string `json:"message"`
		Metadata *struct {
			Reasons      []string `json:"reasons"`
			FlaggedInput string   `json:"flagged_input"`
			Patterns     []string `json:"patterns"`
			ErrorType    string   `json:"error_type"`
		} `json:"metadata"`
	} `json:"error"`
}

// classify maps one OpenRouter response (status + body) to a typed error.
func classify(status int, body []byte) error {
	var eb errorBody
	_ = json.Unmarshal(body, &eb)
	msg := fmt.Sprintf("openrouter http %d", status)
	if eb.Error != nil && eb.Error.Message != "" {
		msg += ": " + clip(eb.Error.Message, 200)
	} else if len(body) > 0 {
		msg += ": " + clip(string(body), 200)
	}

	switch status {
	case http.StatusTooManyRequests, http.StatusRequestTimeout,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return &Error{Class: ClassTransient, Status: status, Msg: msg}
	case http.StatusUnauthorized, http.StatusPaymentRequired:
		return &Error{Class: ClassHard, Status: status,
			Msg: msg + " — check your API key and credit balance"}
	case http.StatusForbidden:
		// Moderation/guardrail metadata → this item's content was
		// flagged: per-item, permanent. Bare 403 → key permissions:
		// global, hard.
		if m := eb.errMetadata(); m != nil &&
			(len(m.Reasons) > 0 || m.FlaggedInput != "" || len(m.Patterns) > 0) {
			return &Error{Class: ClassPermanent, Status: status, Msg: msg + " (content flagged)"}
		}
		return &Error{Class: ClassHard, Status: status,
			Msg: msg + " — the key lacks permission for this request"}
	case http.StatusNotFound:
		// Unknown model slug / endpoint: every request would fail.
		return &Error{Class: ClassHard, Status: status, Msg: msg}
	case http.StatusRequestEntityTooLarge:
		return &errTooLarge{&Error{Class: ClassPermanent, Status: status, Msg: msg}}
	default:
		// 400, 422, and anything unrecognised: the request/content is
		// the problem.
		return &Error{Class: ClassPermanent, Status: status, Msg: msg}
	}
}

func (eb *errorBody) errMetadata() *struct {
	Reasons      []string `json:"reasons"`
	FlaggedInput string   `json:"flagged_input"`
	Patterns     []string `json:"patterns"`
	ErrorType    string   `json:"error_type"`
} {
	if eb.Error == nil {
		return nil
	}
	return eb.Error.Metadata
}

// Client is the OpenRouter transport: one POST per model call, with
// transient-error retries that honour Retry-After. BaseURL and Sleep are
// injectable so tests run against httptest with zero real waiting.
type Client struct {
	APIKey     string
	BaseURL    string                                          // default: the real OpenRouter API
	HTTPClient *http.Client                                    // default: 5-minute timeout (voice/PDF calls are slow)
	Sleep      func(ctx context.Context, d time.Duration) bool // default: real sleep; false = ctx cancelled
	MaxRetries int                                             // attempts per call, default 5
}

func (c *Client) baseURL() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return defaultBaseURL
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

func (c *Client) sleep(ctx context.Context, d time.Duration) bool {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (c *Client) maxRetries() int {
	if c.MaxRetries > 0 {
		return c.MaxRetries
	}
	return 5
}

// Preflight verifies the key with a token-free GET /key, so a bad key
// fails the run in one request instead of after burning the queue.
func (c *Client) Preflight(ctx context.Context) error {
	if strings.TrimSpace(c.APIKey) == "" {
		return &Error{Class: ClassHard, Msg: "set " + APIKeyEnv + " to your OpenRouter API key"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL()+"/key", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return &Error{Class: ClassHard, Status: resp.StatusCode,
			Msg: fmt.Sprintf("OpenRouter rejected the key (HTTP %d): %s", resp.StatusCode, clip(string(body), 160))}
	}
	return nil
}

// Balance returns the key's remaining OpenRouter credits in USD
// (total_credits - total_usage, via GET /credits).
func (c *Client) Balance(ctx context.Context) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL()+"/credits", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return 0, fmt.Errorf("credits endpoint: http %d: %s", resp.StatusCode, clip(string(body), 160))
	}
	var out struct {
		Data struct {
			TotalCredits float64 `json:"total_credits"`
			TotalUsage   float64 `json:"total_usage"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("decode credits: %w", err)
	}
	return out.Data.TotalCredits - out.Data.TotalUsage, nil
}

// complete POSTs one chat completion and returns the decoded response.
// Transient failures are retried with backoff (honouring Retry-After);
// every other failure returns its classified error immediately.
func (c *Client) complete(ctx context.Context, model string, parts []contentPart, plugins []pdfPlugin, maxTokens int, temperature float64) (*chatResponse, error) {
	body, err := json.Marshal(chatRequest{
		Model:       model,
		Messages:    []chatMessage{{Role: "user", Content: parts}},
		MaxTokens:   maxTokens,
		Temperature: temperature,
		Plugins:     plugins,
		Usage:       &usageRequest{Include: true},
	})
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt < c.maxRetries(); attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL()+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Title", "whatskept")

		resp, err := c.httpClient().Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = &Error{Class: ClassTransient, Msg: "openrouter: " + err.Error()}
			if !c.sleep(ctx, backoffDelay(attempt, 0)) {
				return nil, ctx.Err()
			}
			continue
		}
		respBody, _ := io.ReadAll(resp.Body)
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			cErr := classify(resp.StatusCode, respBody)
			if ClassOf(cErr) != ClassTransient {
				return nil, cErr
			}
			lastErr = cErr
			if !c.sleep(ctx, backoffDelay(attempt, retryAfter)) {
				return nil, ctx.Err()
			}
			continue
		}

		var cr chatResponse
		if err := json.Unmarshal(respBody, &cr); err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}
		// Errors can arrive inside a 200 body; classify by their code
		// exactly like a status line.
		if cr.Error != nil && cr.Error.Message != "" {
			code := cr.Error.Code
			if code == 0 {
				code = http.StatusInternalServerError
			}
			cErr := classify(code, respBody)
			if ClassOf(cErr) != ClassTransient {
				return nil, cErr
			}
			lastErr = cErr
			if !c.sleep(ctx, backoffDelay(attempt, retryAfter)) {
				return nil, ctx.Err()
			}
			continue
		}
		if len(cr.Choices) == 0 {
			return nil, &Error{Class: ClassPermanent, Msg: "openrouter: response had no choices"}
		}
		return &cr, nil
	}
	return nil, lastErr
}

// backoffDelay is 1s, 2s, 4s, … unless the server named a Retry-After.
func backoffDelay(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	return time.Duration(int64(1)<<uint(attempt)) * time.Second
}

// parseRetryAfter reads a Retry-After header (delta-seconds form only —
// the HTTP-date form is rare from APIs and not worth the parse).
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// --- OpenRouter wire types (OpenAI-compatible chat completions) -------

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens"`
	Temperature float64       `json:"temperature"`
	Plugins     []pdfPlugin   `json:"plugins,omitempty"`
	Usage       *usageRequest `json:"usage,omitempty"`
}

// usageRequest asks OpenRouter to report the request's exact cost in
// the response body — the basis of the run's cost accounting.
type usageRequest struct {
	Include bool `json:"include"`
}

// pdfPlugin selects OpenRouter's file-parser plugin and its PDF engine
// ("pdf-text" for the native text layer, "mistral-ocr" for scanned pages).
type pdfPlugin struct {
	ID  string          `json:"id"`
	PDF pdfPluginConfig `json:"pdf"`
}

type pdfPluginConfig struct {
	Engine string `json:"engine"`
}

type chatMessage struct {
	Role    string        `json:"role"`
	Content []contentPart `json:"content"`
}

type contentPart struct {
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	ImageURL   *imageURLPart   `json:"image_url,omitempty"`
	InputAudio *inputAudioPart `json:"input_audio,omitempty"`
	File       *filePart       `json:"file,omitempty"`
}

type imageURLPart struct {
	URL string `json:"url"`
}

// filePart carries a base64 data-URI PDF for the file-parser plugin.
type filePart struct {
	Filename string `json:"filename"`
	FileData string `json:"file_data"`
}

// inputAudioPart carries base64 audio; Format is the container hint
// ("ogg" for WhatsApp's Ogg/Opus voice notes).
type inputAudioPart struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content     string           `json:"content"`
			Annotations []fileAnnotation `json:"annotations"`
		} `json:"message"`
	} `json:"choices"`
	Usage *struct {
		Cost float64 `json:"cost"` // USD; present because we set usage.include
	} `json:"usage"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// cost returns the response's reported USD cost, 0 when absent.
func (cr *chatResponse) cost() float64 {
	if cr == nil || cr.Usage == nil {
		return 0
	}
	return cr.Usage.Cost
}

// fileAnnotation is the file-parser plugin's parsed-document result; the
// extracted text lives in File.Content[].Text (the model's prose reply
// can truncate a long document — the annotation is the faithful source).
type fileAnnotation struct {
	Type string `json:"type"`
	File struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"file"`
}
