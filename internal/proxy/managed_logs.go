package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

type requestLog struct {
	ID                string    `json:"id"`
	RequestID         string    `json:"request_id"`
	JobID             string    `json:"job_id,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	CompletedAt       time.Time `json:"completed_at"`
	DurationMS        int64     `json:"duration_ms"`
	QueueMS           int64     `json:"queue_ms"`
	Route             string    `json:"route"`
	KeyID             string    `json:"key_id"`
	KeyName           string    `json:"key_name"`
	ClientIP          string    `json:"client_ip,omitempty"`
	SourceAccountID   string    `json:"source_account_id"`
	AccountID         string    `json:"account_id,omitempty"`
	AccountName       string    `json:"account_name,omitempty"`
	Provider          string    `json:"provider,omitempty"`
	UpstreamHost      string    `json:"upstream_host,omitempty"`
	Model             string    `json:"model,omitempty"`
	Action            string    `json:"action,omitempty"`
	Width             int       `json:"width,omitempty"`
	Height            int       `json:"height,omitempty"`
	Steps             int       `json:"steps,omitempty"`
	Samples           int       `json:"n_samples,omitempty"`
	Strength          *float64  `json:"strength,omitempty"`
	UpscaledEnhance   bool      `json:"upscaled_enhance"`
	Stream            bool      `json:"stream"`
	Multipart         bool      `json:"multipart"`
	UseNewSharedTrial *bool     `json:"use_new_shared_trial,omitempty"`
	Status            int       `json:"status"`
	UpstreamStatus    int       `json:"upstream_status,omitempty"`
	StreamErrorCode   int       `json:"stream_error_code,omitempty"`
	Outcome           string    `json:"outcome"`
	ErrorSource       string    `json:"error_source,omitempty"`
	Error             string    `json:"error,omitempty"`
	Interrupted       bool      `json:"interrupted"`
	ResponseBytes     int64     `json:"response_bytes"`
	ReservedAnlas     int64     `json:"reserved_anlas"`
	ReservedOpus      int64     `json:"reserved_opus"`
	FormulaAnlas      int64     `json:"formula_anlas"`
	BillingState      string    `json:"billing_state"`
	KeyFixedRemaining int64     `json:"key_fixed_remaining"`
	KeyPaidRemaining  int64     `json:"key_purchased_remaining"`
	KeyOpusRemaining  int64     `json:"key_opus_remaining"`
}

type requestTraceKey struct{}
type requestLogQueuedAtKey struct{}

type requestTrace struct {
	entry    requestLog
	secrets  []string
	upstream bool
}

type requestLogWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
	error  []byte
}

func (w *requestLogWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *requestLogWriter) WriteHeader(status int) {
	if status >= 200 && w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *requestLogWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.status >= 400 && len(w.error) < 4096 {
		w.error = append(w.error, data[:min(len(data), 4096-len(w.error))]...)
	}
	n, err := w.ResponseWriter.Write(data)
	w.bytes += int64(n)
	return n, err
}

func traceFor(r *http.Request) *requestTrace {
	trace, _ := r.Context().Value(requestTraceKey{}).(*requestTrace)
	return trace
}

func (h *ManagedHandler) traceRequest(w http.ResponseWriter, r *http.Request, key clientKey) (http.ResponseWriter, *http.Request, func()) {
	if traceFor(r) != nil || !h.settings.snapshot().LogsEnabled {
		return w, r, func() {}
	}
	now := time.Now().UTC()
	created := now
	if queued, ok := r.Context().Value(requestLogQueuedAtKey{}).(time.Time); ok {
		created = queued
	}
	random, err := randomHex(8)
	if err != nil {
		return w, r, func() {}
	}
	trace := &requestTrace{entry: requestLog{
		CreatedAt: created, Route: r.URL.Path, KeyID: key.ID, KeyName: key.Name, SourceAccountID: keyAccountID(key),
		ClientIP: h.clientIP(r),
		Stream:   strings.HasSuffix(r.URL.Path, "generate-image-stream"), Multipart: strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/"),
		Outcome: "rejected", BillingState: "not_reserved", KeyFixedRemaining: displayRemaining(fixedRemaining(key)),
		KeyPaidRemaining: displayRemaining(purchasedRemaining(key)), KeyOpusRemaining: displayRemaining(opusRemaining(key)),
	}}
	trace.entry.ID = now.Format("20060102-150405.000000000-") + random
	trace.entry.RequestID = trace.entry.ID
	trace.entry.JobID, _ = r.Context().Value(archiveJobIDKey{}).(string)
	if raw, ok := bearerToken(r); ok {
		trace.secrets = append(trace.secrets, raw)
	}
	w.Header().Set("X-Proxy-Request-Id", trace.entry.ID)
	writer := &requestLogWriter{ResponseWriter: w}
	r = r.WithContext(context.WithValue(r.Context(), requestTraceKey{}, trace))
	finish := func() {
		value := recover()
		trace.entry.CompletedAt = time.Now().UTC()
		trace.entry.DurationMS = max(0, trace.entry.CompletedAt.Sub(created).Milliseconds())
		trace.entry.Status, trace.entry.ResponseBytes = writer.status, writer.bytes
		trace.entry.Interrupted = trace.entry.Interrupted || value != nil || r.Context().Err() != nil
		if trace.entry.Status == 0 {
			trace.entry.Status = http.StatusBadGateway
			if r.Context().Err() != nil {
				trace.entry.Status = 499
			}
		}
		if trace.entry.Error == "" && len(writer.error) > 0 {
			trace.entry.Error = trace.sanitize(logErrorMessage(writer.error))
		}
		if trace.entry.Outcome != "success" && trace.entry.ErrorSource == "" {
			trace.entry.ErrorSource = "proxy"
			if trace.upstream {
				trace.entry.ErrorSource = "upstream"
			}
		}
		if trace.entry.Interrupted && trace.entry.Error == "" {
			trace.entry.Error = "request or response interrupted"
		}
		trace.entry.KeyName = trace.sanitize(trace.entry.KeyName)
		trace.entry.AccountName = trace.sanitize(trace.entry.AccountName)
		if trace.entry.BillingState == "not_reserved" {
			trace.entry.ReservedAnlas, trace.entry.ReservedOpus = 0, 0
		}
		_ = h.logs.append(trace.entry) // Debug logging must never change the request outcome.
		if value != nil {
			panic(value)
		}
	}
	return writer, r, finish
}

func (trace *requestTrace) metadata(body []byte, contentType string) {
	if trace == nil {
		return
	}
	data, err := jobRequestJSON(body, contentType)
	if err != nil {
		return
	}
	type caption struct {
		Base       string `json:"base_caption"`
		Characters []struct {
			Caption string `json:"char_caption"`
		} `json:"char_captions"`
	}
	type prompt struct {
		Caption caption `json:"caption"`
	}
	var payload struct {
		Model      string `json:"model"`
		Action     string `json:"action"`
		Input      string `json:"input"`
		Trial      *bool  `json:"use_new_shared_trial"`
		Width      int    `json:"width"`
		Height     int    `json:"height"`
		Type       string `json:"req_type"`
		Parameters struct {
			Width          int      `json:"width"`
			Height         int      `json:"height"`
			Steps          int      `json:"steps"`
			Samples        int      `json:"n_samples"`
			Strength       *float64 `json:"strength"`
			Upscaled       bool     `json:"upscaled_enhance"`
			Negative       string   `json:"negative_prompt"`
			Prompt         prompt   `json:"v4_prompt"`
			NegativePrompt prompt   `json:"v4_negative_prompt"`
		} `json:"parameters"`
	}
	_ = json.Unmarshal(data, &payload)
	trace.secrets = append(trace.secrets, payload.Input, payload.Parameters.Negative)
	for _, prompt := range []prompt{payload.Parameters.Prompt, payload.Parameters.NegativePrompt} {
		trace.secrets = append(trace.secrets, prompt.Caption.Base)
		for _, character := range prompt.Caption.Characters {
			trace.secrets = append(trace.secrets, character.Caption)
		}
	}
	if allowedGenerationModel(payload.Model) {
		trace.entry.Model = payload.Model
	} else if payload.Model != "" {
		trace.entry.Model = "unsupported"
	}
	for _, action := range []string{"generate", "img2img", "enhance", "inpaint", "emotion", "bg-removal", "colorize", "declutter", "lineart", "sketch"} {
		if payload.Action == action || payload.Type == action {
			trace.entry.Action = action
		}
	}
	p := payload.Parameters
	trace.entry.Width, trace.entry.Height, trace.entry.Steps, trace.entry.Samples = p.Width, p.Height, p.Steps, p.Samples
	if trace.entry.Width == 0 && trace.entry.Height == 0 {
		trace.entry.Width, trace.entry.Height = payload.Width, payload.Height
	}
	trace.entry.Strength, trace.entry.UpscaledEnhance, trace.entry.UseNewSharedTrial = p.Strength, p.Upscaled, payload.Trial
}

func (trace *requestTrace) selected(account upstreamAccount, host, provider, token string, hold reservation, cost jobCost) {
	if trace == nil {
		return
	}
	trace.entry.AccountID, trace.entry.AccountName = account.ID, account.Name
	trace.entry.UpstreamHost, trace.entry.Provider = host, provider
	trace.entry.ReservedAnlas, trace.entry.ReservedOpus, trace.entry.FormulaAnlas = hold.Fixed+hold.Purchased, hold.Opus, cost.FormulaAnlas
	trace.secrets = append(trace.secrets, token)
}

func (trace *requestTrace) settlement(tracked *statusWriter, generated, refund, uncertain, charge, interrupted bool) {
	if trace == nil {
		return
	}
	trace.entry.UpstreamStatus, trace.entry.Interrupted = tracked.status, interrupted
	trace.entry.Outcome, trace.entry.BillingState = "success", "charged"
	if refund {
		trace.entry.Outcome, trace.entry.BillingState = "rejected", "refunded"
	} else if uncertain {
		trace.entry.Outcome, trace.entry.BillingState = "uncertain", "pending"
		if charge {
			trace.entry.BillingState = "charged_uncertain"
		}
	} else if !generated && trace.entry.Samples > 0 {
		trace.entry.Outcome = "uncertain"
	}
	if trace.entry.Outcome == "uncertain" && tracked.status >= 200 && tracked.status < 300 {
		trace.entry.Error = "upstream response did not contain a recognizable generated image"
		if tracked.stream != nil {
			trace.entry.Error = "upstream stream ended without a valid final image"
		}
	}
	if tracked.stream != nil && tracked.stream.errorCode != 0 {
		trace.entry.StreamErrorCode = tracked.stream.errorCode
		trace.entry.Error = trace.sanitize(tracked.stream.errorMessage)
		if trace.entry.Error == "" {
			trace.entry.Error = fmt.Sprintf("upstream stream error %d", tracked.stream.errorCode)
		}
	}
}

var logCredentialPattern = regexp.MustCompile(`(?i)\b(?:bearer\s+|sk-|pst-)[A-Za-z0-9._-]+`)
var logBinaryPattern = regexp.MustCompile(`[A-Za-z0-9+/=_-]{80,}`)

func (trace *requestTrace) sanitize(message string) string {
	for _, secret := range trace.secrets {
		if secret == "" {
			continue
		}
		message = strings.ReplaceAll(message, secret, "[redacted]")
		// Error capture may end partway through an echoed prompt or credential.
		if len(secret) >= 8 {
			if index := strings.Index(message, secret[:8]); index >= 0 {
				count := 8
				for count < len(secret) && index+count < len(message) && message[index+count] == secret[count] {
					count++
				}
				message = message[:index] + "[redacted]" + message[index+count:]
			}
		}
	}
	message = logCredentialPattern.ReplaceAllString(message, "[redacted]")
	message = logBinaryPattern.ReplaceAllString(message, "[redacted binary]")
	message = strings.Join(strings.Fields(strings.ToValidUTF8(message, "")), " ")
	if len(message) > 2048 {
		message = message[:2048]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
		message += "…"
	}
	return message
}

func logErrorMessage(data []byte) string {
	var payload map[string]json.RawMessage
	if json.Unmarshal(data, &payload) == nil {
		for _, field := range []string{"message", "error"} {
			var message string
			if json.Unmarshal(payload[field], &message) == nil && message != "" {
				return message
			}
			var nested struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(payload[field], &nested) == nil && nested.Message != "" {
				return nested.Message
			}
		}
		return "upstream error response (no message)"
	}
	if bytes.ContainsAny(data, "\x00\x01\x02") {
		return "upstream returned a binary error response"
	}
	return string(data)
}
