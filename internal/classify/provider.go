package classify

import "context"

// ProviderRequest is sent to a classifier provider implementation.
type ProviderRequest struct {
	VideoPath string
	Frames    [][]byte
}

// ProviderResponse is a provider's parsed and raw response.
type ProviderResponse struct {
	Detections  []Detection
	RawResponse string
}

// Provider abstracts model vendors so we can swap Gemini later.
type Provider interface {
	Name() string
	ClassifyVideo(ctx context.Context, req ProviderRequest) (ProviderResponse, error)
}

// FrameExtractor extracts a fixed number of representative video frames.
type FrameExtractor interface {
	ExtractFrames(ctx context.Context, videoPath string, frameCount int) ([][]byte, error)
}
