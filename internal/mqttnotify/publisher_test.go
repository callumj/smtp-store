package mqttnotify

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"smtp-store/internal/classify"
	"smtp-store/internal/config"
)

type publishCall struct {
	topic    string
	retained bool
	payload  string
}

type fakeClient struct {
	connected bool
	calls     []publishCall
}

func (f *fakeClient) Connect() mqtt.Token {
	f.connected = true
	return fakeToken{}
}

func (f *fakeClient) Disconnect(_ uint) {
	f.connected = false
}

func (f *fakeClient) IsConnected() bool {
	return f.connected
}

func (f *fakeClient) Publish(topic string, _ byte, retained bool, payload any) mqtt.Token {
	var text string
	switch v := payload.(type) {
	case []byte:
		text = string(v)
	case string:
		text = v
	default:
		encoded, _ := json.Marshal(v)
		text = string(encoded)
	}
	f.calls = append(f.calls, publishCall{topic: topic, retained: retained, payload: text})
	return fakeToken{}
}

type fakeToken struct{}

func (fakeToken) Wait() bool                     { return true }
func (fakeToken) WaitTimeout(time.Duration) bool { return true }
func (fakeToken) Done() <-chan struct{}          { ch := make(chan struct{}); close(ch); return ch }
func (fakeToken) Error() error                   { return nil }

func TestStartPublishesDiscoveryAndInitialMotionState(t *testing.T) {
	t.Parallel()
	client := &fakeClient{}
	pub := NewWithClient(testConfig(), nil, client)

	if err := pub.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	assertPublished(t, client.calls, "smtp-store/status", "online", true)
	assertPublished(t, client.calls, "homeassistant/event/smtp_store_garden_local/config", "", true)
	assertPublished(t, client.calls, "homeassistant/binary_sensor/smtp_store_garden_local_motion/config", "", true)
	assertPublished(t, client.calls, "smtp-store/garden@local/motion/state", payloadOff, true)
}

func TestPublishClassificationPublishesEventAndMotionForDetections(t *testing.T) {
	t.Parallel()
	client := &fakeClient{connected: true}
	pub := NewWithClient(testConfig(), nil, client)

	sidecar := classify.Sidecar{
		SchemaVersion: classify.SchemaVersion,
		RelativePath:  "garden@local/2026/06/29/195503_1.mp4",
		ThumbnailPath: "garden@local/2026/06/29/195503_1.mp4.thumb.jpg",
		State:         classify.StateSuccess,
		Provider:      "mock",
		Model:         "model",
		UpdatedAt:     "2026-06-29T23:55:03Z",
		HasPerson:     true,
		Detections: []classify.Detection{{
			Category:   "person",
			Label:      "person",
			Confidence: 0.92,
		}},
	}

	if err := pub.PublishClassification(context.Background(), "/tmp/clip.mp4", sidecar); err != nil {
		t.Fatalf("PublishClassification() error = %v", err)
	}

	event := assertPublished(t, client.calls, "smtp-store/garden@local/event", "", false)
	var payload map[string]any
	if err := json.Unmarshal([]byte(event.payload), &payload); err != nil {
		t.Fatalf("decode event payload: %v", err)
	}
	if payload["event_type"] != "person" {
		t.Fatalf("event_type = %v", payload["event_type"])
	}
	if payload["camera"] != "garden@local" {
		t.Fatalf("camera = %v", payload["camera"])
	}
	if payload["thumbnail_url"] == "" {
		t.Fatal("expected thumbnail_url")
	}
	assertPublished(t, client.calls, "smtp-store/garden@local/motion/state", payloadOn, true)
}

func TestPublishClassificationPublishesVehicleEvent(t *testing.T) {
	t.Parallel()
	client := &fakeClient{connected: true}
	cfg := testConfig()
	cfg.MQTT.NotifyCategories = []string{"vehicle"}
	pub := NewWithClient(cfg, nil, client)

	sidecar := classify.Sidecar{
		SchemaVersion: classify.SchemaVersion,
		RelativePath:  "garden@local/2026/06/29/195503_1.mp4",
		ThumbnailPath: "garden@local/2026/06/29/195503_1.mp4.thumb.jpg",
		State:         classify.StateSuccess,
		Provider:      "mock",
		Model:         "model",
		UpdatedAt:     "2026-06-29T23:55:03Z",
		HasVehicle:    true,
		Detections: []classify.Detection{{
			Category:   "vehicle",
			Label:      "car",
			Confidence: 0.92,
		}},
	}

	if err := pub.PublishClassification(context.Background(), "/tmp/clip.mp4", sidecar); err != nil {
		t.Fatalf("PublishClassification() error = %v", err)
	}

	event := assertPublished(t, client.calls, "smtp-store/garden@local/event", "", false)
	var payload map[string]any
	if err := json.Unmarshal([]byte(event.payload), &payload); err != nil {
		t.Fatalf("decode event payload: %v", err)
	}
	if payload["event_type"] != "vehicle" {
		t.Fatalf("event_type = %v", payload["event_type"])
	}
	if payload["has_vehicle"] != true {
		t.Fatalf("has_vehicle = %v", payload["has_vehicle"])
	}
}

func TestPublishClassificationSkipsNoDetections(t *testing.T) {
	t.Parallel()
	client := &fakeClient{connected: true}
	pub := NewWithClient(testConfig(), nil, client)

	err := pub.PublishClassification(context.Background(), "/tmp/clip.mp4", classify.Sidecar{
		SchemaVersion: classify.SchemaVersion,
		RelativePath:  "garden@local/clip.mp4",
		State:         classify.StateSuccess,
	})
	if err != nil {
		t.Fatalf("PublishClassification() error = %v", err)
	}
	if len(client.calls) != 0 {
		t.Fatalf("unexpected publish calls: %#v", client.calls)
	}
}

func testConfig() *config.Config {
	enabled := true
	return &config.Config{
		MQTT: config.MQTTConfig{
			Enabled:          &enabled,
			Host:             "192.168.52.57",
			Port:             1883,
			ClientID:         "smtp-store-test",
			TopicPrefix:      "smtp-store",
			DiscoveryPrefix:  "homeassistant",
			QoS:              1,
			MotionResetAfter: "1h",
			PublicBaseURL:    "https://smtp-store.lake.jonesswimclub.com",
			MediaToken:       "token",
			NotifyCategories: []string{"person", "animal"},
		},
		Users: []config.UserCreds{{
			Username: "garden@local",
			Password: "secret",
		}},
	}
}

func assertPublished(t *testing.T, calls []publishCall, topic, payload string, retained bool) publishCall {
	t.Helper()
	for _, call := range calls {
		if call.topic != topic {
			continue
		}
		if payload != "" && call.payload != payload {
			t.Fatalf("topic %q payload = %q, want %q", topic, call.payload, payload)
		}
		if call.retained != retained {
			t.Fatalf("topic %q retained = %v, want %v", topic, call.retained, retained)
		}
		return call
	}
	t.Fatalf("missing publish to %q; calls=%#v", topic, calls)
	return publishCall{}
}
