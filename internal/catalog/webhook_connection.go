package catalog

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"subsyncd/internal/observability"
	"subsyncd/internal/store"
)

// WebhookURLFor returns the URL Arr must call for an instance's webhooks:
// base (trailing slashes trimmed) + /webhooks/<escaped name>?token=<token>.
// An empty base yields only the path and query.
func WebhookURLFor(base, instance, token string) string {
	return strings.TrimRight(base, "/") + "/webhooks/" + url.PathEscape(instance) + "?token=" + token
}

// WebhookConnectionStore remembers which Arr connection subsyncd owns for an
// instance.
type WebhookConnectionStore interface {
	WebhookConnection(ctx context.Context, name string) (store.WebhookConnection, error)
	RecordWebhookConnection(ctx context.Context, name string, connection store.WebhookConnection) error
}

// WebhookConnectionSync finds, adopts, creates or repairs the one Sonarr or
// Radarr webhook connection subsyncd owns for an instance. It writes only to
// that connection and never forces a save past Arr's own test.
type WebhookConnectionSync struct {
	Instance, InstanceType, ArrURL, APIKey, BaseURL, Token string
	Store                                                  WebhookConnectionStore
	Client                                                 *http.Client // nil → newArrClient default
	Events                                                 *observability.Emitter
	Now                                                    func() time.Time
}

// WebhookSyncError is a sync failure reduced to a safe reason and, where one
// applies, Arr's HTTP status. It never carries URLs, tokens or Arr bodies.
type WebhookSyncError struct {
	Reason     string
	StatusCode int
}

func (e *WebhookSyncError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("webhook connection sync failed: reason=%s status=%d", e.Reason, e.StatusCode)
	}
	return "webhook connection sync failed: reason=" + e.Reason
}

const (
	notificationPath       = "/api/v3/notification"
	notificationSchemaPath = "/api/v3/notification/schema"
	webhookImplementation  = "Webhook"
	webhookMethodPOST      = 1
	maxWebhookNameSuffix   = 9
)

var requiredWebhookFlags = map[string][]string{
	"sonarr": {"onDownload", "onUpgrade", "onRename", "onSeriesDelete", "onEpisodeFileDelete", "onEpisodeFileDeleteForUpgrade"},
	"radarr": {"onDownload", "onUpgrade", "onRename", "onMovieDelete", "onMovieFileDelete", "onMovieFileDeleteForUpgrade"},
}

// arrResource is an Arr notification resource kept as raw JSON so keys
// subsyncd does not know round-trip unchanged.
type arrResource map[string]json.RawMessage

// Sync brings the owned connection to the desired state. It returns nil when
// the connection was created, adopted, repaired, already correct, or when the
// match was ambiguous (which only warns). Cancellation is returned as-is;
// every other failure is a *WebhookSyncError.
func (s WebhookConnectionSync) Sync(ctx context.Context) error {
	base, err := url.Parse(s.BaseURL)
	if err != nil || base.Host == "" {
		return errors.New("webhook connection sync: invalid webhook base URL")
	}
	required, ok := requiredWebhookFlags[s.InstanceType]
	if !ok {
		return errors.New("webhook connection sync: unsupported instance type")
	}
	if s.Store == nil {
		return errors.New("webhook connection sync: no connection store")
	}
	client, err := newArrClient(s.Instance, s.ArrURL, s.APIKey, s.Client)
	if err != nil {
		return err
	}
	events := s.Events.For("catalog")
	instance := slog.String("instance", s.Instance)
	desiredURL := WebhookURLFor(s.BaseURL, s.Instance, s.Token)

	var connections []arrResource
	if err := client.getJSON(ctx, notificationPath, nil, &connections); err != nil {
		return webhookReadError(ctx, err)
	}
	stored, err := s.Store.WebhookConnection(ctx, s.Instance)
	if err != nil {
		return err
	}

	owned := rememberedConnection(connections, stored, s.ArrURL)
	adopted := false
	if owned == nil {
		candidates, ours := s.matchConnections(connections, base)
		switch {
		case len(ours) > 1:
			events.Log(ctx, slog.LevelWarn, "webhook.connection_ambiguous",
				"more than one Arr webhook connection matches this subsyncd; delete the extras",
				instance, slog.Int("count", len(ours)))
			return nil
		case len(ours) == 1:
			owned, adopted = ours[0], true
		default:
			if candidates > 0 {
				events.Log(ctx, slog.LevelInfo, "webhook.connection_foreign",
					"left Arr webhook connections for another subsyncd deployment unchanged",
					instance, slog.Int("count", candidates))
			}
			id, err := s.create(ctx, client, base, desiredURL, required)
			if err != nil {
				return err
			}
			events.Log(ctx, slog.LevelInfo, "webhook.connection_created", "created the Arr webhook connection",
				instance, slog.Int64("connection_id", id))
			return s.record(ctx, id)
		}
	}

	id, ok := resourceID(owned)
	if !ok {
		return &WebhookSyncError{Reason: "arr_response_invalid"}
	}
	changed, err := repairWebhookResource(owned, desiredURL, required)
	if err != nil {
		return &WebhookSyncError{Reason: "arr_response_invalid"}
	}
	if len(changed) > 0 {
		path := notificationPath + "/" + strconv.FormatInt(id, 10)
		if err := client.sendJSON(ctx, http.MethodPut, path, owned, nil, "notification_update"); err != nil {
			return webhookWriteError(ctx, err)
		}
	}
	if adopted {
		events.Log(ctx, slog.LevelInfo, "webhook.connection_adopted", "adopted an existing Arr webhook connection",
			instance, slog.Int64("connection_id", id))
	}
	if len(changed) > 0 {
		events.Log(ctx, slog.LevelInfo, "webhook.connection_repaired", "repaired the Arr webhook connection",
			instance, slog.Int64("connection_id", id), slog.String("changed", strings.Join(changed, ",")))
	} else {
		events.Log(ctx, slog.LevelDebug, "webhook.connection_ok", "the Arr webhook connection is up to date",
			instance, slog.Int64("connection_id", id))
	}
	return s.record(ctx, id)
}

func (s WebhookConnectionSync) record(ctx context.Context, id int64) error {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	return s.Store.RecordWebhookConnection(ctx, s.Instance, store.WebhookConnection{ID: id, ArrURL: s.ArrURL, CheckedAt: now()})
}

// rememberedConnection returns the stored connection when it was recorded
// against this Arr and still exists as a Webhook.
func rememberedConnection(connections []arrResource, stored store.WebhookConnection, arrURL string) arrResource {
	if stored.ID == 0 || stored.ArrURL != arrURL {
		return nil
	}
	for _, connection := range connections {
		if id, ok := resourceID(connection); ok && id == stored.ID && isWebhook(connection) {
			return connection
		}
	}
	return nil
}

// matchConnections returns how many Webhook connections point at this
// instance's webhook path, and those among them that are positively ours:
// same scheme and host as the base URL, or carrying the stored token.
func (s WebhookConnectionSync) matchConnections(connections []arrResource, base *url.URL) (int, []arrResource) {
	candidates := 0
	var ours []arrResource
	for _, connection := range connections {
		if !isWebhook(connection) {
			continue
		}
		raw, ok := fieldValue(connection, "url")
		if !ok {
			continue
		}
		var value string
		if json.Unmarshal(raw, &value) != nil {
			continue
		}
		hook, err := url.Parse(value)
		if err != nil || !strings.HasSuffix(hook.Path, "/webhooks/"+s.Instance) {
			continue
		}
		candidates++
		// The same origin is ours only at exactly our path: another
		// deployment behind the same proxy differs only by path prefix.
		sameEndpoint := sameWebhookOrigin(hook, base) && hook.Path == strings.TrimRight(base.Path, "/")+"/webhooks/"+s.Instance
		tokens := hook.Query()["token"]
		if sameEndpoint || len(tokens) == 1 && sameWebhookSecret(tokens[0], s.Token) {
			ours = append(ours, connection)
		}
	}
	return candidates, ours
}

// sameWebhookOrigin compares scheme and host name case-insensitively and the
// effective port, so http://h and http://h:80 are the same origin.
func sameWebhookOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Hostname(), b.Hostname()) && effectivePort(a) == effectivePort(b)
}

func effectivePort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	}
	return ""
}

// sameWebhookSecret compares in constant time, as httpapi does.
func sameWebhookSecret(provided, expected string) bool {
	if expected == "" {
		return false
	}
	providedHash := sha256.Sum256([]byte(provided))
	expectedHash := sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(providedHash[:], expectedHash[:]) == 1
}

// create posts a new connection built from Arr's Webhook template, retrying
// with a numbered name while the name is taken.
func (s WebhookConnectionSync) create(ctx context.Context, client *arrClient, base *url.URL, desiredURL string, required []string) (int64, error) {
	var templates []arrResource
	if err := client.getJSON(ctx, notificationSchemaPath, nil, &templates); err != nil {
		return 0, webhookReadError(ctx, err)
	}
	var template arrResource
	for _, candidate := range templates {
		if isWebhook(candidate) {
			template = candidate
			break
		}
	}
	if template == nil {
		return 0, &WebhookSyncError{Reason: "schema_missing_webhook"}
	}
	delete(template, "presets")
	for _, key := range flagKeys(template) {
		template[key] = json.RawMessage("false")
	}
	for _, flag := range required {
		if boolKey(template, supportsKey(flag)) {
			template[flag] = json.RawMessage("true")
		}
	}
	if setFieldValue(template, "url", desiredURL) != nil || setFieldValue(template, "method", webhookMethodPOST) != nil {
		return 0, &WebhookSyncError{Reason: "arr_response_invalid"}
	}
	name := "subsyncd (" + base.Host + ")"
	for attempt := 1; attempt <= maxWebhookNameSuffix; attempt++ {
		candidate := name
		if attempt > 1 {
			candidate = name + " " + strconv.Itoa(attempt)
		}
		encoded, err := json.Marshal(candidate)
		if err != nil {
			return 0, &WebhookSyncError{Reason: "arr_response_invalid"}
		}
		template["name"] = encoded
		var created arrResource
		err = client.sendJSON(ctx, http.MethodPost, notificationPath, template, &created, "notification_create")
		if err == nil {
			id, ok := resourceID(created)
			if !ok {
				return 0, &WebhookSyncError{Reason: "arr_response_invalid"}
			}
			return id, nil
		}
		var validation *arrValidationError
		if errors.As(err, &validation) && len(validation.Properties) == 1 && validation.Properties[0] == "Name" {
			continue
		}
		return 0, webhookWriteError(ctx, err)
	}
	return 0, &WebhookSyncError{Reason: "name_unavailable"}
}

// repairWebhookResource sets the managed keys of resource to their desired
// values in place and reports which groups changed, in the order url,
// method, events. Every other key is left untouched.
func repairWebhookResource(resource arrResource, desiredURL string, required []string) ([]string, error) {
	var changed []string
	var current string
	if raw, ok := fieldValue(resource, "url"); !ok || json.Unmarshal(raw, &current) != nil || current != desiredURL {
		if err := setFieldValue(resource, "url", desiredURL); err != nil {
			return nil, err
		}
		changed = append(changed, "url")
	}
	var method float64
	if raw, ok := fieldValue(resource, "method"); !ok || json.Unmarshal(raw, &method) != nil || method != webhookMethodPOST {
		if err := setFieldValue(resource, "method", webhookMethodPOST); err != nil {
			return nil, err
		}
		changed = append(changed, "method")
	}
	events := false
	for _, flag := range required {
		if boolKey(resource, supportsKey(flag)) && !boolKey(resource, flag) {
			resource[flag] = json.RawMessage("true")
			events = true
		}
	}
	if events {
		changed = append(changed, "events")
	}
	return changed, nil
}

func resourceID(resource arrResource) (int64, bool) {
	var id int64
	if json.Unmarshal(resource["id"], &id) != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func isWebhook(resource arrResource) bool {
	var implementation string
	return json.Unmarshal(resource["implementation"], &implementation) == nil && implementation == webhookImplementation
}

func boolKey(resource arrResource, key string) bool {
	var value bool
	return json.Unmarshal(resource[key], &value) == nil && value
}

// supportsKey maps an event flag such as onDownload to supportsOnDownload.
func supportsKey(flag string) string { return "supportsO" + strings.TrimPrefix(flag, "o") }

// flagKeys returns the boolean event flags (on<Event>) of a resource.
func flagKeys(resource arrResource) []string {
	var keys []string
	for key, raw := range resource {
		if len(key) > 2 && strings.HasPrefix(key, "on") && key[2] >= 'A' && key[2] <= 'Z' {
			var value bool
			if json.Unmarshal(raw, &value) == nil {
				keys = append(keys, key)
			}
		}
	}
	return keys
}

func resourceFields(resource arrResource) ([]map[string]json.RawMessage, error) {
	var fields []map[string]json.RawMessage
	if raw, ok := resource["fields"]; ok {
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, err
		}
	}
	return fields, nil
}

func fieldName(field map[string]json.RawMessage) string {
	var name string
	_ = json.Unmarshal(field["name"], &name)
	return name
}

func fieldValue(resource arrResource, name string) (json.RawMessage, bool) {
	fields, err := resourceFields(resource)
	if err != nil {
		return nil, false
	}
	for _, field := range fields {
		if fieldName(field) == name {
			value, ok := field["value"]
			return value, ok
		}
	}
	return nil, false
}

// setFieldValue sets one settings field's value, keeping every other key of
// that field and every other field, and appends the field when it is absent.
func setFieldValue(resource arrResource, name string, value any) error {
	fields, err := resourceFields(resource)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	found := false
	for _, field := range fields {
		if fieldName(field) == name {
			field["value"] = encoded
			found = true
		}
	}
	if !found {
		encodedName, _ := json.Marshal(name)
		fields = append(fields, map[string]json.RawMessage{"name": encodedName, "value": encoded})
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	resource["fields"] = raw
	return nil
}

func webhookCanceled(ctx context.Context, err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return ctx.Err()
	}
	return nil
}

// webhookReadError maps a failed GET to a safe reason.
func webhookReadError(ctx context.Context, err error) error {
	if canceled := webhookCanceled(ctx, err); canceled != nil {
		return canceled
	}
	var api *arrAPIError
	if errors.As(err, &api) {
		switch api.Kind {
		case "transport", "timeout", "deadline":
			return &WebhookSyncError{Reason: "arr_unreachable"}
		case "status":
			return &WebhookSyncError{Reason: "arr_status", StatusCode: api.StatusCode}
		}
	}
	return &WebhookSyncError{Reason: "arr_response_invalid"}
}

// webhookWriteError maps a failed POST or PUT. A 400 means Arr's save-time
// test reached an address that answered with an error; a 500 means it could
// not connect at all.
func webhookWriteError(ctx context.Context, err error) error {
	if canceled := webhookCanceled(ctx, err); canceled != nil {
		return canceled
	}
	var validation *arrValidationError
	if errors.As(err, &validation) {
		return &WebhookSyncError{Reason: "arr_test_rejected", StatusCode: validation.StatusCode}
	}
	var api *arrAPIError
	if errors.As(err, &api) {
		switch api.Kind {
		case "transport", "timeout", "deadline":
			return &WebhookSyncError{Reason: "arr_unreachable"}
		case "status":
			switch api.StatusCode {
			case http.StatusBadRequest:
				return &WebhookSyncError{Reason: "arr_test_rejected", StatusCode: api.StatusCode}
			case http.StatusInternalServerError:
				return &WebhookSyncError{Reason: "arr_test_unreachable", StatusCode: api.StatusCode}
			default:
				return &WebhookSyncError{Reason: "arr_status", StatusCode: api.StatusCode}
			}
		}
	}
	return &WebhookSyncError{Reason: "arr_response_invalid"}
}
