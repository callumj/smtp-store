package classify

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var jsonObjectRe = regexp.MustCompile(`\{[\s\S]*\}`)

// GeminiProvider classifies sampled frames via Gemini's generateContent endpoint.
type GeminiProvider struct {
	apiKey     string
	model      string
	httpClient *http.Client
}

// NewGeminiProvider constructs a Gemini provider implementation.
func NewGeminiProvider(apiKey, model string) *GeminiProvider {
	return &GeminiProvider{
		apiKey: apiKey,
		model:  model,
		httpClient: &http.Client{
			Timeout: 45 * time.Second,
		},
	}
}

func (p *GeminiProvider) Name() string {
	return "gemini"
}

func (p *GeminiProvider) ClassifyVideo(ctx context.Context, req ProviderRequest) (ProviderResponse, error) {
	if len(req.Frames) == 0 {
		return ProviderResponse{}, fmt.Errorf("no frames provided")
	}

	payload, err := p.buildRequestPayload(req.Frames)
	if err != nil {
		return ProviderResponse{}, err
	}

	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s", p.model, p.apiKey)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return ProviderResponse{}, fmt.Errorf("create gemini request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return ProviderResponse{}, fmt.Errorf("send gemini request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return ProviderResponse{}, fmt.Errorf("read gemini response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return ProviderResponse{}, fmt.Errorf("gemini api status %d: %s", resp.StatusCode, string(respBody))
	}

	var parsed geminiResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return ProviderResponse{}, fmt.Errorf("decode gemini response: %w", err)
	}

	text := parsed.primaryText()
	if strings.TrimSpace(text) == "" {
		return ProviderResponse{}, fmt.Errorf("gemini returned empty text response")
	}

	detections, err := parseGeminiDetections(text)
	if err != nil {
		return ProviderResponse{}, fmt.Errorf("parse gemini detections: %w", err)
	}

	return ProviderResponse{
		Detections:  detections,
		RawResponse: string(respBody),
	}, nil
}

func (p *GeminiProvider) buildRequestPayload(frames [][]byte) ([]byte, error) {
	parts := make([]map[string]any, 0, len(frames)+1)
	parts = append(parts, map[string]any{
		"text": "You are a detection classifier for security camera clips. Identify people, animals, and vehicles visible in any frame. Respond ONLY as JSON with shape: {\"detections\":[{\"label\":\"string\",\"category\":\"person|animal|vehicle\",\"confidence\":0-1}]}. Use category \"vehicle\" for cars, trucks, vans, SUVs, motorcycles, bicycles, and similar road vehicles. If none, return {\"detections\":[]}.",
	})
	for _, frame := range frames {
		parts = append(parts, map[string]any{
			"inline_data": map[string]any{
				"mime_type": "image/jpeg",
				"data":      base64.StdEncoding.EncodeToString(frame),
			},
		})
	}

	body := map[string]any{
		"contents": []map[string]any{{
			"role":  "user",
			"parts": parts,
		}},
		"generationConfig": map[string]any{
			"temperature":      0,
			"responseMimeType": "application/json",
		},
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal gemini request: %w", err)
	}
	return payload, nil
}

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
}

func (r geminiResponse) primaryText() string {
	if len(r.Candidates) == 0 {
		return ""
	}
	parts := r.Candidates[0].Content.Parts
	if len(parts) == 0 {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

type geminiDetectionsEnvelope struct {
	Detections []Detection `json:"detections"`
}

func parseGeminiDetections(text string) ([]Detection, error) {
	text = strings.TrimSpace(text)
	match := jsonObjectRe.FindString(text)
	if match != "" {
		text = match
	}
	var env geminiDetectionsEnvelope
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		return nil, err
	}
	return env.Detections, nil
}
