package classify

import "testing"

func TestParseGeminiDetectionsParsesJSONInsideMarkdown(t *testing.T) {
	t.Parallel()
	text := "```json\n{\"detections\":[{\"label\":\"fox\",\"category\":\"animal\",\"confidence\":0.82}]}\n```"
	detections, err := parseGeminiDetections(text)
	if err != nil {
		t.Fatalf("parseGeminiDetections() error = %v", err)
	}
	if len(detections) != 1 {
		t.Fatalf("detections = %d, want 1", len(detections))
	}
	if detections[0].Category != "animal" {
		t.Fatalf("category = %q", detections[0].Category)
	}
}
