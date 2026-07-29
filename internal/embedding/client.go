// Package embedding requests and caches tool embeddings against an
// OpenAI-compatible /v1/embeddings gateway.
package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	defaultModel     = "text-embedding-3-small"
	defaultBatchSize = 96
)

// Client posts to a configured gateway's /v1/embeddings endpoint. The base
// URL and API key are always supplied by the caller: neither is ever
// defaulted here, so a caller must source them from config or environment
// rather than this package assuming a provider.
type Client struct {
	baseURL    string
	apiKey     string
	model      string
	batchSize  int
	httpClient *http.Client
}

func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiKey:     apiKey,
		model:      defaultModel,
		batchSize:  defaultBatchSize,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed returns one vector per text, in the same order, batching requests
// rather than issuing one per text. It returns an error rather than
// panicking when the gateway cannot be reached or answers with a
// non-success status, so a caller can degrade to lexical-only ranking.
//
// On a batch failure partway through, Embed stops rather than trying later
// batches, but the returned slice is still length len(texts): batches that
// already succeeded are filled in, and the failed batch onward is left nil.
// A caller must not discard the returned slice just because err is non-nil,
// or it throws away embedding work it already paid for.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	out := make([][]float32, len(texts))
	for start := 0; start < len(texts); start += c.batchSize {
		end := min(start+c.batchSize, len(texts))
		batch, err := c.embedBatch(ctx, texts[start:end])
		if err != nil {
			return out, fmt.Errorf("embed batch %d-%d of %d: %w", start, end, len(texts), err)
		}
		copy(out[start:end], batch)
	}
	return out, nil
}

func (c *Client) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(embedRequest{Model: c.model, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("marshal embeddings request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build embeddings request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embeddings request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("embeddings request: status %d: %s", resp.StatusCode, snippet)
	}

	var parsed embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("parse embeddings response: %w", err)
	}
	vecs := make([][]float32, len(texts))
	for _, d := range parsed.Data {
		if d.Index < 0 || d.Index >= len(vecs) {
			return nil, fmt.Errorf("embeddings response: index %d out of range for %d inputs", d.Index, len(texts))
		}
		vecs[d.Index] = d.Embedding
	}
	return vecs, nil
}
