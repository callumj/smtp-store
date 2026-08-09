package mqttnotify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"smtp-store/internal/classify"
	"smtp-store/internal/config"
)

const (
	payloadOn  = "ON"
	payloadOff = "OFF"
)

var objectIDRe = regexp.MustCompile(`[^a-z0-9_]+`)

type mqttClient interface {
	Connect() mqtt.Token
	Disconnect(quiesce uint)
	IsConnected() bool
	Publish(topic string, qos byte, retained bool, payload any) mqtt.Token
}

// Publisher sends Home Assistant MQTT discovery and detection events.
type Publisher struct {
	client           mqttClient
	logger           *slog.Logger
	topicPrefix      string
	discoveryPrefix  string
	publicBaseURL    string
	mediaToken       string
	qos              byte
	motionResetAfter time.Duration
	notifyCategories map[string]struct{}
	cameras          []camera
}

type camera struct {
	Name     string
	TopicID  string
	ObjectID string
}

// New creates a publisher from runtime config.
func New(cfg *config.Config, logger *slog.Logger) *Publisher {
	opts := mqtt.NewClientOptions()
	opts.AddBroker(fmt.Sprintf("tcp://%s:%d", strings.TrimSpace(cfg.MQTT.Host), cfg.MQTT.Port))
	opts.SetClientID(strings.TrimSpace(cfg.MQTT.ClientID))
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	opts.SetWill(topic(strings.TrimSpace(cfg.MQTT.TopicPrefix), "status"), "offline", cfg.MQTT.QoS, true)
	if strings.TrimSpace(cfg.MQTT.Username) != "" {
		opts.SetUsername(strings.TrimSpace(cfg.MQTT.Username))
		opts.SetPassword(cfg.MQTT.Password)
	}

	return NewWithClient(cfg, logger, mqtt.NewClient(opts))
}

func NewWithClient(cfg *config.Config, logger *slog.Logger, client mqttClient) *Publisher {
	categories := make(map[string]struct{}, len(cfg.MQTT.NotifyCategories))
	for _, category := range cfg.MQTT.NotifyCategories {
		categories[strings.ToLower(strings.TrimSpace(category))] = struct{}{}
	}

	cameras := make([]camera, 0, len(cfg.Users))
	seen := make(map[string]int, len(cfg.Users))
	for _, user := range cfg.Users {
		name := strings.ToLower(strings.TrimSpace(user.Username))
		if name == "" {
			continue
		}
		topicID := sanitizeTopicPart(name)
		objectID := sanitizeObjectID(name)
		if count := seen[objectID]; count > 0 {
			objectID = fmt.Sprintf("%s_%d", objectID, count+1)
		}
		seen[objectID]++
		cameras = append(cameras, camera{Name: name, TopicID: topicID, ObjectID: objectID})
	}

	return &Publisher{
		client:           client,
		logger:           logger,
		topicPrefix:      strings.Trim(strings.TrimSpace(cfg.MQTT.TopicPrefix), "/"),
		discoveryPrefix:  strings.Trim(strings.TrimSpace(cfg.MQTT.DiscoveryPrefix), "/"),
		publicBaseURL:    strings.TrimRight(strings.TrimSpace(cfg.MQTT.PublicBaseURL), "/"),
		mediaToken:       cfg.MQTT.MediaToken,
		qos:              cfg.MQTT.QoS,
		motionResetAfter: cfg.MQTTMotionResetAfterDuration(),
		notifyCategories: categories,
		cameras:          cameras,
	}
}

// Start connects to the broker and publishes Home Assistant discovery documents.
func (p *Publisher) Start(ctx context.Context) error {
	if token := p.client.Connect(); token.Wait() && token.Error() != nil {
		return token.Error()
	}
	if err := p.publish(topic(p.topicPrefix, "status"), true, "online"); err != nil {
		return err
	}
	if err := p.PublishDiscovery(ctx); err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = p.publish(topic(p.topicPrefix, "status"), true, "offline")
		p.client.Disconnect(250)
	}()
	return nil
}

// PublishDiscovery publishes retained Home Assistant MQTT discovery documents.
func (p *Publisher) PublishDiscovery(_ context.Context) error {
	for _, camera := range p.cameras {
		if err := p.publishDiscoveryForCamera(camera); err != nil {
			return err
		}
		if err := p.publish(topic(p.topicPrefix, camera.TopicID, "motion", "state"), true, payloadOff); err != nil {
			return err
		}
	}
	return nil
}

// PublishClassification publishes a detection event after successful classification.
func (p *Publisher) PublishClassification(ctx context.Context, videoPath string, sidecar classify.Sidecar) error {
	_ = videoPath
	if !p.shouldNotify(sidecar.Detections) {
		return nil
	}
	cameraName := cameraNameFromRelativePath(sidecar.RelativePath)
	camera := p.cameraForName(cameraName)
	eventPayload := map[string]any{
		"event_type":     eventType(sidecar.Detections),
		"camera":         camera.Name,
		"relative_path":  sidecar.RelativePath,
		"video_url":      p.fileURL("view", sidecar.RelativePath),
		"thumbnail_url":  p.thumbnailURL(sidecar.ThumbnailPath),
		"has_person":     sidecar.HasPerson,
		"has_animal":     sidecar.HasAnimal,
		"has_vehicle":    sidecar.HasVehicle,
		"detections":     sidecar.Detections,
		"classified_at":  sidecar.UpdatedAt,
		"provider":       sidecar.Provider,
		"model":          sidecar.Model,
		"schema_version": sidecar.SchemaVersion,
	}

	payload, err := json.Marshal(eventPayload)
	if err != nil {
		return err
	}
	if err := p.publish(topic(p.topicPrefix, camera.TopicID, "event"), false, payload); err != nil {
		return err
	}
	if err := p.publish(topic(p.topicPrefix, camera.TopicID, "motion", "state"), true, payloadOn); err != nil {
		return err
	}
	go p.resetMotion(ctx, camera)
	return nil
}

func (p *Publisher) publishDiscoveryForCamera(camera camera) error {
	device := map[string]any{
		"identifiers":  []string{"smtp_store_" + camera.ObjectID},
		"name":         "SMTP Store " + camera.Name,
		"manufacturer": "smtp-store",
		"model":        "SMTP camera capture",
	}
	availability := []map[string]string{{
		"topic": topic(p.topicPrefix, "status"),
	}}
	eventConfig := map[string]any{
		"name":                  camera.Name + " event",
		"unique_id":             "smtp_store_" + camera.ObjectID + "_event",
		"state_topic":           topic(p.topicPrefix, camera.TopicID, "event"),
		"event_types":           []string{"person", "animal", "vehicle", "person_animal", "person_vehicle", "animal_vehicle", "person_animal_vehicle"},
		"device":                device,
		"availability":          availability,
		"payload_available":     "online",
		"payload_not_available": "offline",
		"qos":                   p.qos,
	}
	if err := p.publishJSON(topic(p.discoveryPrefix, "event", "smtp_store_"+camera.ObjectID, "config"), true, eventConfig); err != nil {
		return err
	}

	motionConfig := map[string]any{
		"name":                  camera.Name + " motion",
		"unique_id":             "smtp_store_" + camera.ObjectID + "_motion",
		"state_topic":           topic(p.topicPrefix, camera.TopicID, "motion", "state"),
		"device_class":          "motion",
		"payload_on":            payloadOn,
		"payload_off":           payloadOff,
		"device":                device,
		"availability":          availability,
		"payload_available":     "online",
		"payload_not_available": "offline",
		"qos":                   p.qos,
	}
	return p.publishJSON(topic(p.discoveryPrefix, "binary_sensor", "smtp_store_"+camera.ObjectID+"_motion", "config"), true, motionConfig)
}

func (p *Publisher) publishJSON(topic string, retained bool, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return p.publish(topic, retained, encoded)
}

func (p *Publisher) publish(topic string, retained bool, payload any) error {
	if !p.client.IsConnected() {
		return fmt.Errorf("mqtt client is not connected")
	}
	token := p.client.Publish(topic, p.qos, retained, payload)
	if token.Wait() && token.Error() != nil {
		return token.Error()
	}
	return nil
}

func (p *Publisher) shouldNotify(detections []classify.Detection) bool {
	for _, detection := range detections {
		if _, ok := p.notifyCategories[strings.ToLower(strings.TrimSpace(detection.Category))]; ok {
			return true
		}
	}
	return false
}

func (p *Publisher) cameraForName(name string) camera {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, camera := range p.cameras {
		if camera.Name == name {
			return camera
		}
	}
	return camera{Name: name, TopicID: sanitizeTopicPart(name), ObjectID: sanitizeObjectID(name)}
}

func (p *Publisher) resetMotion(ctx context.Context, camera camera) {
	timer := time.NewTimer(p.motionResetAfter)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
		if err := p.publish(topic(p.topicPrefix, camera.TopicID, "motion", "state"), true, payloadOff); err != nil && p.logger != nil {
			p.logger.Warn("failed resetting mqtt motion state", "camera", camera.Name, "error", err)
		}
	}
}

func (p *Publisher) fileURL(route, rel string) string {
	if p.publicBaseURL == "" || strings.TrimSpace(rel) == "" {
		return ""
	}
	escaped := escapePath(rel)
	return p.publicBaseURL + "/" + strings.Trim(route, "/") + "/" + escaped
}

func (p *Publisher) thumbnailURL(rel string) string {
	if p.publicBaseURL == "" || strings.TrimSpace(rel) == "" || p.mediaToken == "" {
		return ""
	}
	u, err := url.Parse(p.publicBaseURL + "/media/" + escapePath(rel))
	if err != nil {
		return ""
	}
	q := u.Query()
	q.Set("token", p.mediaToken)
	u.RawQuery = q.Encode()
	return u.String()
}

func eventType(detections []classify.Detection) string {
	seen := map[string]bool{}
	for _, detection := range detections {
		switch strings.ToLower(strings.TrimSpace(detection.Category)) {
		case "person", "animal", "vehicle":
			seen[strings.ToLower(strings.TrimSpace(detection.Category))] = true
		}
	}
	parts := make([]string, 0, 3)
	for _, category := range []string{"person", "animal", "vehicle"} {
		if seen[category] {
			parts = append(parts, category)
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "_")
}

func cameraNameFromRelativePath(rel string) string {
	parts := strings.Split(strings.Trim(strings.TrimSpace(rel), "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return "unknown"
	}
	return strings.ToLower(parts[0])
}

func escapePath(rel string) string {
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func sanitizeTopicPart(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	replacer := strings.NewReplacer("/", "_", "+", "_", "#", "_", " ", "_")
	value = replacer.Replace(value)
	if value == "" {
		return "unknown"
	}
	return value
}

func sanitizeObjectID(value string) string {
	value = objectIDRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(value)), "_")
	value = strings.Trim(value, "_")
	if value == "" {
		return "unknown"
	}
	return value
}

func topic(parts ...string) string {
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.Trim(part, "/")
		if part == "" {
			continue
		}
		clean = append(clean, part)
	}
	return path.Join(clean...)
}
