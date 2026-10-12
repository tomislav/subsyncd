package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"subsyncd/internal/observability"
	"subsyncd/internal/store"
)

func TestWebhookURLFor(t *testing.T) {
	for _, test := range []struct{ base, instance, token, want string }{
		{"http://subsyncd:8097", "sonarr-main", "ab", "http://subsyncd:8097/webhooks/sonarr-main?token=ab"},
		{"https://h/p", "my show", "ab", "https://h/p/webhooks/my%20show?token=ab"},
		{"https://h/p//", "tv", "ab", "https://h/p/webhooks/tv?token=ab"},
		{"", "radarr", "ab", "/webhooks/radarr?token=ab"},
	} {
		if got := WebhookURLFor(test.base, test.instance, test.token); got != test.want {
			t.Errorf("WebhookURLFor(%q,%q,%q) = %q, want %q", test.base, test.instance, test.token, got, test.want)
		}
	}
}

const (
	syncInstance = "sonarr-main"
	syncBase     = "http://subsyncd:8097"
	syncAPIKey   = "arr-api-key-SENTINEL"
	syncName     = "subsyncd (subsyncd:8097)"
)

var (
	syncToken = strings.Repeat("f00dfeed", 8)
	syncNow   = time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC)

	sonarrSyncFlags = []string{"onGrab", "onDownload", "onUpgrade", "onImportComplete", "onRename", "onSeriesAdd", "onSeriesDelete", "onEpisodeFileDelete", "onEpisodeFileDeleteForUpgrade", "onHealthIssue", "onHealthRestored", "onApplicationUpdate", "onManualInteractionRequired"}
	radarrSyncFlags = []string{"onGrab", "onDownload", "onUpgrade", "onRename", "onMovieAdded", "onMovieDelete", "onMovieFileDelete", "onMovieFileDeleteForUpgrade", "onHealthIssue", "onHealthRestored", "onApplicationUpdate", "onManualInteractionRequired"}
	sonarrRequired  = []string{"onDownload", "onUpgrade", "onRename", "onSeriesDelete", "onEpisodeFileDelete", "onEpisodeFileDeleteForUpgrade"}
	radarrRequired  = []string{"onDownload", "onUpgrade", "onRename", "onMovieDelete", "onMovieFileDelete", "onMovieFileDeleteForUpgrade"}
)

func syncDesiredURL() string { return WebhookURLFor(syncBase, syncInstance, syncToken) }

func syncFlags(names ...string) map[string]bool {
	flags := map[string]bool{}
	for _, name := range names {
		flags[name] = true
	}
	return flags
}

// syncResource builds an Arr notification resource as the list endpoint
// returns it, including a key subsyncd does not know (futureField).
func syncResource(kind string, id int64, name, implementation, hookURL string, on map[string]bool) map[string]any {
	resource := map[string]any{
		"name":                  name,
		"implementation":        implementation,
		"implementationName":    implementation,
		"configContract":        implementation + "Settings",
		"infoLink":              "https://wiki.example/" + implementation,
		"includeHealthWarnings": false,
		"tags":                  []any{3},
		"futureField":           map[string]any{"nested": []any{1, "two"}},
		"fields": []any{
			map[string]any{"order": 0, "name": "url", "label": "URL", "value": hookURL, "type": "url", "advanced": false, "privacy": "normal"},
			map[string]any{"order": 1, "name": "method", "label": "Method", "value": 1, "type": "select"},
			map[string]any{"order": 2, "name": "username", "label": "Username", "value": "", "privacy": "userName"},
			map[string]any{"order": 3, "name": "password", "label": "Password", "value": "********", "privacy": "password"},
			map[string]any{"order": 4, "name": "headers", "label": "Headers", "value": []any{}},
		},
	}
	if id != 0 {
		resource["id"] = id
	}
	flags := sonarrSyncFlags
	if kind == "radarr" {
		flags = radarrSyncFlags
	}
	for _, flag := range flags {
		resource[flag] = on[flag]
		resource["supportsO"+flag[1:]] = true
	}
	return resource
}

// syncOthers are connections subsyncd must never write to. Their settings
// carry sentinels that must never reach a log line or error.
func syncOthers(kind string) []map[string]any {
	altMount := syncResource(kind, 1, "AltMount", "Webhook", "http://altmount:8080/api/arr/webhook?apikey=SENTINEL-OTHER", syncFlags("onDownload"))
	silo := syncResource(kind, 2, "Silo", "Webhook", "http://silo:9000/hooks/"+syncInstance+"?token=SENTINEL-OTHER", syncFlags("onDownload", "onRename"))
	// Same host and path as subsyncd's, but not a Webhook implementation.
	slack := syncResource(kind, 3, "Slack", "Slack", syncBase+"/webhooks/"+syncInstance+"?token=SENTINEL-OTHER", syncFlags("onGrab"))
	return []map[string]any{altMount, silo, slack}
}

type syncRequest struct {
	Method   string
	Path     string
	RawQuery string
	Body     []byte
}

type syncReply struct {
	Status int
	Body   string
}

type fakeSyncArr struct {
	t      *testing.T
	server *httptest.Server

	mu          sync.Mutex
	nextID      int64
	connections []json.RawMessage
	schema      json.RawMessage
	requests    []syncRequest
	listReplies []syncReply
	postReplies []syncReply
	putReplies  []syncReply
}

func newFakeSyncArr(t *testing.T, kind string, connections ...map[string]any) *fakeSyncArr {
	t.Helper()
	arr := &fakeSyncArr{t: t, nextID: 100}
	for _, connection := range connections {
		arr.connections = append(arr.connections, mustJSON(t, connection))
	}
	template := syncResource(kind, 0, "", "Webhook", "", nil)
	delete(template, "futureField")
	template["presets"] = []any{map[string]any{"name": "preset"}}
	discord := syncResource(kind, 0, "", "Discord", "", nil)
	arr.schema = mustJSON(t, []any{discord, template})
	arr.server = httptest.NewServer(http.HandlerFunc(arr.serve))
	t.Cleanup(arr.server.Close)
	return arr
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func (a *fakeSyncArr) setSchema(mutate func(template map[string]any)) {
	var templates []map[string]any
	if err := json.Unmarshal(a.schema, &templates); err != nil {
		a.t.Fatal(err)
	}
	for _, template := range templates {
		if template["implementation"] == "Webhook" {
			mutate(template)
		}
	}
	a.schema = mustJSON(a.t, templates)
}

func (a *fakeSyncArr) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = append(a.requests, syncRequest{Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery, Body: body})
	if r.Header.Get("X-Api-Key") != syncAPIKey {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	reply := func(replies *[]syncReply) bool {
		if len(*replies) == 0 {
			return false
		}
		next := (*replies)[0]
		*replies = (*replies)[1:]
		w.WriteHeader(next.Status)
		_, _ = io.WriteString(w, next.Body)
		return true
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v3/notification":
		if reply(&a.listReplies) {
			return
		}
		list := a.connections
		if list == nil {
			list = []json.RawMessage{}
		}
		_ = json.NewEncoder(w).Encode(list)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v3/notification/schema":
		_, _ = w.Write(a.schema)
	case r.Method == http.MethodPost && r.URL.Path == "/api/v3/notification":
		if reply(&a.postReplies) {
			return
		}
		var resource map[string]any
		if err := json.Unmarshal(body, &resource); err != nil {
			a.t.Errorf("POST body is not a JSON object: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		resource["id"] = a.nextID
		a.nextID++
		created := mustJSON(a.t, resource)
		a.connections = append(a.connections, created)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(created)
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/v3/notification/"):
		if reply(&a.putReplies) {
			return
		}
		pathID, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/api/v3/notification/"), 10, 64)
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var resource struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(body, &resource); err != nil || resource.ID != pathID {
			a.t.Errorf("PUT body id %d does not equal path id %d", resource.ID, pathID)
		}
		for i, connection := range a.connections {
			var existing struct {
				ID int64 `json:"id"`
			}
			_ = json.Unmarshal(connection, &existing)
			if existing.ID == pathID {
				a.connections[i] = append(json.RawMessage(nil), body...)
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write(body)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (a *fakeSyncArr) writes() []syncRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	var writes []syncRequest
	for _, request := range a.requests {
		if request.Method != http.MethodGet {
			writes = append(writes, request)
		}
	}
	return writes
}

func (a *fakeSyncArr) writesBy(method string) []syncRequest {
	var matched []syncRequest
	for _, request := range a.writes() {
		if request.Method == method {
			matched = append(matched, request)
		}
	}
	return matched
}

// assertUntouched checks that none of the given connection IDs received a
// write and that each is still exactly as seeded.
func (a *fakeSyncArr) assertUntouched(seeded []map[string]any, ids ...int64) {
	a.t.Helper()
	for _, request := range a.writes() {
		for _, id := range ids {
			if request.Path == fmt.Sprintf("/api/v3/notification/%d", id) {
				a.t.Errorf("connection %d received %s", id, request.Method)
			}
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, want := range seeded {
		id, _ := want["id"].(int64)
		for _, connection := range a.connections {
			var got map[string]any
			_ = json.Unmarshal(connection, &got)
			if got["id"] == float64(id) && !jsonEqual(a.t, mustJSON(a.t, want), connection) {
				a.t.Errorf("connection %d changed", id)
			}
		}
	}
}

func jsonEqual(t *testing.T, left, right []byte) bool {
	t.Helper()
	var l, r any
	if err := json.Unmarshal(left, &l); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(right, &r); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(l, r)
}

type fakeConnectionStore struct {
	mu      sync.Mutex
	stored  store.WebhookConnection
	records []store.WebhookConnection
}

func (s *fakeConnectionStore) WebhookConnection(_ context.Context, name string) (store.WebhookConnection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name != syncInstance && name != "my show" && name != "radarr-main" {
		return store.WebhookConnection{}, store.ErrUnknownInstance
	}
	return s.stored, nil
}

func (s *fakeConnectionStore) RecordWebhookConnection(_ context.Context, _ string, connection store.WebhookConnection) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, connection)
	s.stored = connection
	return nil
}

type syncHarness struct {
	arr   *fakeSyncArr
	store *fakeConnectionStore
	logs  *bytes.Buffer
	sync  WebhookConnectionSync
}

func newSyncHarness(t *testing.T, arr *fakeSyncArr) *syncHarness {
	t.Helper()
	logs := &bytes.Buffer{}
	events, err := observability.New(logs, observability.Options{Level: "debug", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	connections := &fakeConnectionStore{}
	return &syncHarness{
		arr:   arr,
		store: connections,
		logs:  logs,
		sync: WebhookConnectionSync{
			Instance:     syncInstance,
			InstanceType: "sonarr",
			ArrURL:       arr.server.URL,
			APIKey:       syncAPIKey,
			BaseURL:      syncBase,
			Token:        syncToken,
			Store:        connections,
			Events:       events,
			Now:          func() time.Time { return syncNow },
		},
	}
}

func (h *syncHarness) records(t *testing.T) []map[string]any {
	t.Helper()
	trimmed := strings.TrimSpace(h.logs.String())
	if trimmed == "" {
		return nil
	}
	var records []map[string]any
	for _, line := range strings.Split(trimmed, "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

// expectEvent returns the single record of an event and checks its level,
// instance, and the given fields.
func (h *syncHarness) expectEvent(t *testing.T, event, level string, fields map[string]any) {
	t.Helper()
	var found []map[string]any
	for _, record := range h.records(t) {
		if record["event"] == event {
			found = append(found, record)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s logged %d times, want 1: %s", event, len(found), h.logs.String())
	}
	record := found[0]
	if record["level"] != level || record["component"] != "catalog" || record["instance"] != h.sync.Instance {
		t.Errorf("%s record = %v, want level %s, component catalog, instance %s", event, record, level, h.sync.Instance)
	}
	for key, want := range fields {
		if got := record[key]; !reflect.DeepEqual(got, want) {
			t.Errorf("%s %s = %#v, want %#v", event, key, got, want)
		}
	}
}

func (h *syncHarness) expectNoEvent(t *testing.T, event string) {
	t.Helper()
	for _, record := range h.records(t) {
		if record["event"] == event {
			t.Errorf("unexpected %s: %v", event, record)
		}
	}
}

func (h *syncHarness) expectRecorded(t *testing.T, id int64) {
	t.Helper()
	want := []store.WebhookConnection{{ID: id, ArrURL: h.arr.server.URL, CheckedAt: syncNow}}
	if !reflect.DeepEqual(h.store.records, want) {
		t.Errorf("recorded %+v, want %+v", h.store.records, want)
	}
}

func (h *syncHarness) expectNothingRecorded(t *testing.T) {
	t.Helper()
	if len(h.store.records) != 0 {
		t.Errorf("recorded %+v, want nothing", h.store.records)
	}
}

func expectSyncError(t *testing.T, err error, reason string, status int) {
	t.Helper()
	var syncErr *WebhookSyncError
	if !errors.As(err, &syncErr) {
		t.Fatalf("Sync error = %v (%T), want *WebhookSyncError %s", err, err, reason)
	}
	if syncErr.Reason != reason || syncErr.StatusCode != status {
		t.Fatalf("Sync error = %+v, want reason %s status %d", syncErr, reason, status)
	}
}

func decodeObject(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(payload, &object); err != nil {
		t.Fatalf("decode %s: %v", payload, err)
	}
	return object
}

func testFieldValue(t *testing.T, resource map[string]any, name string) any {
	t.Helper()
	fields, _ := resource["fields"].([]any)
	for _, field := range fields {
		entry, _ := field.(map[string]any)
		if entry["name"] == name {
			return entry["value"]
		}
	}
	t.Fatalf("resource has no %s field", name)
	return nil
}

func setTestFieldValue(resource map[string]any, name string, value any) {
	fields, _ := resource["fields"].([]any)
	for _, field := range fields {
		entry, _ := field.(map[string]any)
		if entry["name"] == name {
			entry["value"] = value
		}
	}
}

// expectPut checks that exactly one PUT went to id and that its body equals
// the seeded resource with only mutate's changes, compared as decoded JSON.
func expectPut(t *testing.T, arr *fakeSyncArr, id int64, seeded map[string]any, mutate func(map[string]any)) {
	t.Helper()
	puts := arr.writesBy(http.MethodPut)
	if len(puts) != 1 || len(arr.writes()) != 1 {
		t.Fatalf("writes = %+v, want one PUT", arr.writes())
	}
	if want := fmt.Sprintf("/api/v3/notification/%d", id); puts[0].Path != want || puts[0].RawQuery != "" {
		t.Fatalf("PUT %s?%s, want %s with no query", puts[0].Path, puts[0].RawQuery, want)
	}
	want := decodeObject(t, mustJSON(t, seeded))
	mutate(want)
	got := decodeObject(t, puts[0].Body)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PUT body\n got %v\nwant %v", got, want)
	}
}

func expectCreated(t *testing.T, request syncRequest, kind, name, hookURL string, required []string, unsupported ...string) {
	t.Helper()
	if request.Path != "/api/v3/notification" || request.RawQuery != "" || strings.Contains(strings.ToLower(string(request.Body)), "forcesave") {
		t.Fatalf("POST %s?%s, want /api/v3/notification without forceSave", request.Path, request.RawQuery)
	}
	body := decodeObject(t, request.Body)
	if body["name"] != name {
		t.Errorf("name = %#v, want %q", body["name"], name)
	}
	if body["implementation"] != "Webhook" || body["configContract"] != "WebhookSettings" {
		t.Errorf("implementation = %#v/%#v", body["implementation"], body["configContract"])
	}
	if _, ok := body["presets"]; ok {
		t.Error("POST body keeps presets")
	}
	if got := testFieldValue(t, body, "url"); got != hookURL {
		t.Errorf("url = %#v, want %q", got, hookURL)
	}
	if got := testFieldValue(t, body, "method"); got != float64(1) {
		t.Errorf("method = %#v, want 1", got)
	}
	flags := sonarrSyncFlags
	if kind == "radarr" {
		flags = radarrSyncFlags
	}
	wantOn := syncFlags(required...)
	for _, flag := range unsupported {
		delete(wantOn, flag)
	}
	for _, flag := range flags {
		if got := body[flag]; got != wantOn[flag] {
			t.Errorf("%s = %#v, want %t", flag, got, wantOn[flag])
		}
	}
}

func TestSyncCreatesWhenNoneExists(t *testing.T) {
	others := syncOthers("sonarr")
	arr := newFakeSyncArr(t, "sonarr", others...)
	h := newSyncHarness(t, arr)

	if err := h.sync.Sync(context.Background()); err != nil {
		t.Fatalf("Sync() = %v", err)
	}
	posts := arr.writesBy(http.MethodPost)
	if len(posts) != 1 || len(arr.writes()) != 1 {
		t.Fatalf("writes = %+v, want one POST", arr.writes())
	}
	expectCreated(t, posts[0], "sonarr", syncName, syncDesiredURL(), sonarrRequired)
	if decodeObject(t, posts[0].Body)["onImportComplete"] != false {
		t.Error("onImportComplete is not false")
	}
	arr.assertUntouched(others, 1, 2, 3)
	h.expectRecorded(t, 100)
	h.expectEvent(t, "webhook.connection_created", "info", map[string]any{"connection_id": float64(100)})
	h.expectNoEvent(t, "webhook.connection_foreign")
}

func TestSyncCreateRadarrUsesRadarrFlags(t *testing.T) {
	arr := newFakeSyncArr(t, "radarr", syncOthers("radarr")...)
	h := newSyncHarness(t, arr)
	h.sync.Instance = "radarr-main"
	h.sync.InstanceType = "radarr"

	if err := h.sync.Sync(context.Background()); err != nil {
		t.Fatalf("Sync() = %v", err)
	}
	posts := arr.writesBy(http.MethodPost)
	if len(posts) != 1 {
		t.Fatalf("writes = %+v, want one POST", arr.writes())
	}
	expectCreated(t, posts[0], "radarr", syncName, WebhookURLFor(syncBase, "radarr-main", syncToken), radarrRequired)
	h.expectRecorded(t, 100)
}

func TestSyncSkipsUnsupportedFlags(t *testing.T) {
	arr := newFakeSyncArr(t, "sonarr")
	arr.setSchema(func(template map[string]any) { template["supportsOnEpisodeFileDeleteForUpgrade"] = false })
	h := newSyncHarness(t, arr)

	if err := h.sync.Sync(context.Background()); err != nil {
		t.Fatalf("Sync() = %v", err)
	}
	posts := arr.writesBy(http.MethodPost)
	if len(posts) != 1 {
		t.Fatalf("writes = %+v, want one POST", arr.writes())
	}
	expectCreated(t, posts[0], "sonarr", syncName, syncDesiredURL(), sonarrRequired, "onEpisodeFileDeleteForUpgrade")

	// An existing connection on such an Arr is not drifted for lacking it.
	on := syncFlags(sonarrRequired...)
	delete(on, "onEpisodeFileDeleteForUpgrade")
	owned := syncResource("sonarr", 7, "subsyncd", "Webhook", syncDesiredURL(), on)
	owned["supportsOnEpisodeFileDeleteForUpgrade"] = false
	repairArr := newFakeSyncArr(t, "sonarr", owned)
	repair := newSyncHarness(t, repairArr)
	if err := repair.sync.Sync(context.Background()); err != nil {
		t.Fatalf("Sync() = %v", err)
	}
	if writes := repairArr.writes(); len(writes) != 0 {
		t.Fatalf("writes = %+v, want none", writes)
	}
	repair.expectEvent(t, "webhook.connection_ok", "debug", map[string]any{"connection_id": float64(7)})
}

func TestSyncAdoptsSameHostOldToken(t *testing.T) {
	others := syncOthers("sonarr")
	handMade := syncResource("sonarr", 7, "subsyncd", "Webhook", syncBase+"/webhooks/"+syncInstance+"?token=old-token", syncFlags(sonarrRequired...))
	arr := newFakeSyncArr(t, "sonarr", append(others, handMade)...)
	h := newSyncHarness(t, arr)

	if err := h.sync.Sync(context.Background()); err != nil {
		t.Fatalf("Sync() = %v", err)
	}
	expectPut(t, arr, 7, handMade, func(want map[string]any) { setTestFieldValue(want, "url", syncDesiredURL()) })
	arr.assertUntouched(others, 1, 2, 3)
	h.expectEvent(t, "webhook.connection_adopted", "info", map[string]any{"connection_id": float64(7)})
	h.expectEvent(t, "webhook.connection_repaired", "info", map[string]any{"connection_id": float64(7), "changed": "url"})
	h.expectRecorded(t, 7)
}

func TestSyncAdoptsOtherHostMatchingToken(t *testing.T) {
	handMade := syncResource("sonarr", 7, "subsyncd", "Webhook", "http://old-host:9000/webhooks/"+syncInstance+"?token="+syncToken, syncFlags(sonarrRequired...))
	arr := newFakeSyncArr(t, "sonarr", handMade)
	h := newSyncHarness(t, arr)

	if err := h.sync.Sync(context.Background()); err != nil {
		t.Fatalf("Sync() = %v", err)
	}
	expectPut(t, arr, 7, handMade, func(want map[string]any) { setTestFieldValue(want, "url", syncDesiredURL()) })
	h.expectEvent(t, "webhook.connection_adopted", "info", map[string]any{"connection_id": float64(7)})
	h.expectEvent(t, "webhook.connection_repaired", "info", map[string]any{"changed": "url"})
	h.expectRecorded(t, 7)
}

func TestSyncLeavesForeignDeploymentAlone(t *testing.T) {
	foreign := syncResource("sonarr", 7, "subsyncd (canary:8097)", "Webhook", "http://canary:8097/webhooks/"+syncInstance+"?token=SENTINEL-OTHER", syncFlags(sonarrRequired...))
	others := append(syncOthers("sonarr"), foreign)
	arr := newFakeSyncArr(t, "sonarr", others...)
	h := newSyncHarness(t, arr)

	if err := h.sync.Sync(context.Background()); err != nil {
		t.Fatalf("Sync() = %v", err)
	}
	posts := arr.writesBy(http.MethodPost)
	if len(posts) != 1 || len(arr.writes()) != 1 {
		t.Fatalf("writes = %+v, want one POST", arr.writes())
	}
	expectCreated(t, posts[0], "sonarr", syncName, syncDesiredURL(), sonarrRequired)
	arr.assertUntouched(others, 1, 2, 3, 7)
	h.expectEvent(t, "webhook.connection_foreign", "info", map[string]any{"count": float64(1)})
	h.expectEvent(t, "webhook.connection_created", "info", map[string]any{"connection_id": float64(100)})
	h.expectNoEvent(t, "webhook.connection_adopted")
	h.expectRecorded(t, 100)
}

func TestSyncAmbiguousWritesNothing(t *testing.T) {
	first := syncResource("sonarr", 7, "subsyncd", "Webhook", syncBase+"/webhooks/"+syncInstance+"?token=old", syncFlags(sonarrRequired...))
	second := syncResource("sonarr", 8, "subsyncd copy", "Webhook", "http://elsewhere/webhooks/"+syncInstance+"?token="+syncToken, nil)
	arr := newFakeSyncArr(t, "sonarr", first, second)
	h := newSyncHarness(t, arr)

	if err := h.sync.Sync(context.Background()); err != nil {
		t.Fatalf("Sync() = %v", err)
	}
	if writes := arr.writes(); len(writes) != 0 {
		t.Fatalf("writes = %+v, want none", writes)
	}
	h.expectEvent(t, "webhook.connection_ambiguous", "warn", map[string]any{"count": float64(2)})
	h.expectNothingRecorded(t)
}

func TestSyncRememberedIDDeletedRecreates(t *testing.T) {
	arr := newFakeSyncArr(t, "sonarr", syncOthers("sonarr")...)
	h := newSyncHarness(t, arr)
	h.store.stored = store.WebhookConnection{ID: 42, ArrURL: arr.server.URL, CheckedAt: syncNow.Add(-time.Hour)}

	if err := h.sync.Sync(context.Background()); err != nil {
		t.Fatalf("Sync() = %v", err)
	}
	if posts := arr.writesBy(http.MethodPost); len(posts) != 1 || len(arr.writes()) != 1 {
		t.Fatalf("writes = %+v, want one POST", arr.writes())
	}
	h.expectEvent(t, "webhook.connection_created", "info", map[string]any{"connection_id": float64(100)})
	h.expectRecorded(t, 100)
}

func TestSyncRememberedIDForOtherArrURLIgnored(t *testing.T) {
	others := syncOthers("sonarr")
	arr := newFakeSyncArr(t, "sonarr", others...)
	h := newSyncHarness(t, arr)
	// AltMount's ID, remembered against a different Arr: not ours here.
	h.store.stored = store.WebhookConnection{ID: 1, ArrURL: "http://other-arr:8989"}

	if err := h.sync.Sync(context.Background()); err != nil {
		t.Fatalf("Sync() = %v", err)
	}
	if posts := arr.writesBy(http.MethodPost); len(posts) != 1 || len(arr.writes()) != 1 {
		t.Fatalf("writes = %+v, want one POST", arr.writes())
	}
	arr.assertUntouched(others, 1, 2, 3)
	h.expectRecorded(t, 100)
}

func TestSyncRememberedNonWebhookIDIgnored(t *testing.T) {
	others := syncOthers("sonarr")
	arr := newFakeSyncArr(t, "sonarr", others...)
	h := newSyncHarness(t, arr)
	h.store.stored = store.WebhookConnection{ID: 3, ArrURL: arr.server.URL}

	if err := h.sync.Sync(context.Background()); err != nil {
		t.Fatalf("Sync() = %v", err)
	}
	if posts := arr.writesBy(http.MethodPost); len(posts) != 1 || len(arr.writes()) != 1 {
		t.Fatalf("writes = %+v, want one POST", arr.writes())
	}
	arr.assertUntouched(others, 1, 2, 3)
}

func TestSyncRepairsUntickedFlagOnly(t *testing.T) {
	on := syncFlags(sonarrRequired...)
	delete(on, "onRename")
	on["onGrab"] = true // a user choice that must survive
	owned := syncResource("sonarr", 7, "renamed by user", "Webhook", syncDesiredURL(), on)
	arr := newFakeSyncArr(t, "sonarr", owned)
	h := newSyncHarness(t, arr)
	h.store.stored = store.WebhookConnection{ID: 7, ArrURL: arr.server.URL}

	if err := h.sync.Sync(context.Background()); err != nil {
		t.Fatalf("Sync() = %v", err)
	}
	expectPut(t, arr, 7, owned, func(want map[string]any) { want["onRename"] = true })
	h.expectEvent(t, "webhook.connection_repaired", "info", map[string]any{"connection_id": float64(7), "changed": "events"})
	h.expectNoEvent(t, "webhook.connection_adopted")
	h.expectRecorded(t, 7)
}

func TestSyncAllFlagsOffRepaired(t *testing.T) {
	owned := syncResource("sonarr", 7, "subsyncd", "Webhook", syncDesiredURL(), nil)
	arr := newFakeSyncArr(t, "sonarr", owned)
	h := newSyncHarness(t, arr)
	h.store.stored = store.WebhookConnection{ID: 7, ArrURL: arr.server.URL}

	if err := h.sync.Sync(context.Background()); err != nil {
		t.Fatalf("Sync() = %v", err)
	}
	expectPut(t, arr, 7, owned, func(want map[string]any) {
		for _, flag := range sonarrRequired {
			want[flag] = true
		}
	})
	h.expectEvent(t, "webhook.connection_repaired", "info", map[string]any{"changed": "events"})
	h.expectRecorded(t, 7)
}

func TestSyncRepairReportsEveryChangedKey(t *testing.T) {
	owned := syncResource("sonarr", 7, "subsyncd", "Webhook", syncBase+"/webhooks/"+syncInstance+"?token=old", syncFlags("onDownload"))
	setTestFieldValue(owned, "method", 2)
	arr := newFakeSyncArr(t, "sonarr", owned)
	h := newSyncHarness(t, arr)

	if err := h.sync.Sync(context.Background()); err != nil {
		t.Fatalf("Sync() = %v", err)
	}
	expectPut(t, arr, 7, owned, func(want map[string]any) {
		setTestFieldValue(want, "url", syncDesiredURL())
		setTestFieldValue(want, "method", float64(1))
		for _, flag := range sonarrRequired {
			want[flag] = true
		}
	})
	h.expectEvent(t, "webhook.connection_repaired", "info", map[string]any{"changed": "url,method,events"})
}

func TestSyncNoDriftNoWrite(t *testing.T) {
	owned := syncResource("sonarr", 7, "subsyncd", "Webhook", syncDesiredURL(), syncFlags(sonarrRequired...))
	arr := newFakeSyncArr(t, "sonarr", owned)
	h := newSyncHarness(t, arr)
	h.store.stored = store.WebhookConnection{ID: 7, ArrURL: arr.server.URL}

	if err := h.sync.Sync(context.Background()); err != nil {
		t.Fatalf("Sync() = %v", err)
	}
	if writes := arr.writes(); len(writes) != 0 {
		t.Fatalf("writes = %+v, want none", writes)
	}
	h.expectEvent(t, "webhook.connection_ok", "debug", map[string]any{"connection_id": float64(7)})
	h.expectNoEvent(t, "webhook.connection_repaired")
	h.expectRecorded(t, 7)
}

const nameTakenBody = `[{"propertyName":"Name","errorMessage":"Should be unique","attemptedValue":"subsyncd (subsyncd:8097)","severity":"error"}]`

func TestSyncNameTakenRetriesSuffix(t *testing.T) {
	t.Run("second name", func(t *testing.T) {
		arr := newFakeSyncArr(t, "sonarr")
		arr.postReplies = []syncReply{{Status: http.StatusBadRequest, Body: nameTakenBody}}
		h := newSyncHarness(t, arr)

		if err := h.sync.Sync(context.Background()); err != nil {
			t.Fatalf("Sync() = %v", err)
		}
		posts := arr.writesBy(http.MethodPost)
		if len(posts) != 2 {
			t.Fatalf("writes = %+v, want two POSTs", arr.writes())
		}
		expectCreated(t, posts[0], "sonarr", syncName, syncDesiredURL(), sonarrRequired)
		expectCreated(t, posts[1], "sonarr", syncName+" 2", syncDesiredURL(), sonarrRequired)
		h.expectRecorded(t, 100)
	})
	t.Run("all names taken", func(t *testing.T) {
		arr := newFakeSyncArr(t, "sonarr")
		for range 9 {
			arr.postReplies = append(arr.postReplies, syncReply{Status: http.StatusBadRequest, Body: nameTakenBody})
		}
		h := newSyncHarness(t, arr)

		err := h.sync.Sync(context.Background())
		expectSyncError(t, err, "name_unavailable", 0)
		posts := arr.writesBy(http.MethodPost)
		if len(posts) != 9 {
			t.Fatalf("POSTs = %d, want 9", len(posts))
		}
		if got := decodeObject(t, posts[8].Body)["name"]; got != syncName+" 9" {
			t.Errorf("last name = %#v", got)
		}
		h.expectNothingRecorded(t)
		h.expectNoEvent(t, "webhook.connection_created")
	})
}

const (
	testRejectedBody    = `[{"propertyName":"Url","errorMessage":"Unable to send test message: Unauthorized at [http://subsyncd:8097/webhooks/sonarr-main?token=SENTINEL]","severity":"error"}]`
	testUnreachableBody = `{"message":"Connection refused (subsyncd:8097) SENTINEL","description":"System.Net.Http.HttpRequestException at http://subsyncd:8097/webhooks/sonarr-main?token=SENTINEL"}`
)

func TestSyncTestRejected(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		arr := newFakeSyncArr(t, "sonarr")
		arr.postReplies = []syncReply{{Status: http.StatusBadRequest, Body: testRejectedBody}}
		h := newSyncHarness(t, arr)

		expectSyncError(t, h.sync.Sync(context.Background()), "arr_test_rejected", http.StatusBadRequest)
		if posts := arr.writesBy(http.MethodPost); len(posts) != 1 {
			t.Fatalf("POSTs = %d, want 1 (no retry)", len(posts))
		}
		h.expectNothingRecorded(t)
	})
	t.Run("repair", func(t *testing.T) {
		owned := syncResource("sonarr", 7, "subsyncd", "Webhook", syncBase+"/webhooks/"+syncInstance+"?token=old", syncFlags(sonarrRequired...))
		arr := newFakeSyncArr(t, "sonarr", owned)
		arr.putReplies = []syncReply{{Status: http.StatusBadRequest, Body: testRejectedBody}}
		h := newSyncHarness(t, arr)

		expectSyncError(t, h.sync.Sync(context.Background()), "arr_test_rejected", http.StatusBadRequest)
		h.expectNothingRecorded(t)
		h.expectNoEvent(t, "webhook.connection_repaired")
	})
	t.Run("no properties", func(t *testing.T) {
		arr := newFakeSyncArr(t, "sonarr")
		arr.postReplies = []syncReply{{Status: http.StatusBadRequest, Body: `[]`}}
		h := newSyncHarness(t, arr)

		expectSyncError(t, h.sync.Sync(context.Background()), "arr_test_rejected", http.StatusBadRequest)
	})
	t.Run("name and url", func(t *testing.T) {
		arr := newFakeSyncArr(t, "sonarr")
		arr.postReplies = []syncReply{{Status: http.StatusBadRequest, Body: `[{"propertyName":"Name"},{"propertyName":"Url"}]`}}
		h := newSyncHarness(t, arr)

		expectSyncError(t, h.sync.Sync(context.Background()), "arr_test_rejected", http.StatusBadRequest)
		if posts := arr.writesBy(http.MethodPost); len(posts) != 1 {
			t.Fatalf("POSTs = %d, want 1 (no retry)", len(posts))
		}
	})
}

func TestSyncTestUnreachable(t *testing.T) {
	arr := newFakeSyncArr(t, "sonarr")
	arr.postReplies = []syncReply{{Status: http.StatusInternalServerError, Body: testUnreachableBody}}
	h := newSyncHarness(t, arr)

	expectSyncError(t, h.sync.Sync(context.Background()), "arr_test_unreachable", http.StatusInternalServerError)
	h.expectNothingRecorded(t)
}

func TestSyncReadFailures(t *testing.T) {
	t.Run("list status", func(t *testing.T) {
		arr := newFakeSyncArr(t, "sonarr")
		arr.listReplies = []syncReply{{Status: http.StatusServiceUnavailable, Body: "SENTINEL"}}
		h := newSyncHarness(t, arr)
		expectSyncError(t, h.sync.Sync(context.Background()), "arr_status", http.StatusServiceUnavailable)
	})
	t.Run("list invalid", func(t *testing.T) {
		arr := newFakeSyncArr(t, "sonarr")
		arr.listReplies = []syncReply{{Status: http.StatusOK, Body: `{"not":"a list"}`}}
		h := newSyncHarness(t, arr)
		expectSyncError(t, h.sync.Sync(context.Background()), "arr_response_invalid", 0)
	})
	t.Run("schema without webhook", func(t *testing.T) {
		arr := newFakeSyncArr(t, "sonarr")
		arr.schema = json.RawMessage(`[{"implementation":"Discord"}]`)
		h := newSyncHarness(t, arr)
		expectSyncError(t, h.sync.Sync(context.Background()), "schema_missing_webhook", 0)
		if writes := arr.writes(); len(writes) != 0 {
			t.Fatalf("writes = %+v, want none", writes)
		}
	})
	t.Run("put status", func(t *testing.T) {
		owned := syncResource("sonarr", 7, "subsyncd", "Webhook", syncBase+"/webhooks/"+syncInstance+"?token=old", nil)
		arr := newFakeSyncArr(t, "sonarr", owned)
		arr.putReplies = []syncReply{{Status: http.StatusConflict}}
		h := newSyncHarness(t, arr)
		expectSyncError(t, h.sync.Sync(context.Background()), "arr_status", http.StatusConflict)
	})
}

func TestSyncArrDown(t *testing.T) {
	arr := newFakeSyncArr(t, "sonarr")
	h := newSyncHarness(t, arr)
	arr.server.Close()

	expectSyncError(t, h.sync.Sync(context.Background()), "arr_unreachable", 0)
	h.expectNothingRecorded(t)
}

func TestSyncCancellation(t *testing.T) {
	arr := newFakeSyncArr(t, "sonarr")
	h := newSyncHarness(t, arr)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := h.sync.Sync(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Sync() = %v, want context.Canceled", err)
	}
	var syncErr *WebhookSyncError
	if errors.As(err, &syncErr) {
		t.Fatalf("Sync() wrapped cancellation into %+v", syncErr)
	}
	h.expectNothingRecorded(t)
}

func TestSyncPathPrefixAndEscapedName(t *testing.T) {
	const base = "http://h/subsyncd/"
	want := "http://h/subsyncd/webhooks/my%20show?token=" + syncToken
	t.Run("create", func(t *testing.T) {
		arr := newFakeSyncArr(t, "sonarr")
		h := newSyncHarness(t, arr)
		h.sync.Instance, h.sync.BaseURL = "my show", base

		if err := h.sync.Sync(context.Background()); err != nil {
			t.Fatalf("Sync() = %v", err)
		}
		posts := arr.writesBy(http.MethodPost)
		if len(posts) != 1 {
			t.Fatalf("writes = %+v, want one POST", arr.writes())
		}
		expectCreated(t, posts[0], "sonarr", "subsyncd (h)", want, sonarrRequired)
	})
	t.Run("adopt", func(t *testing.T) {
		handMade := syncResource("sonarr", 7, "subsyncd", "Webhook", "http://h/subsyncd/webhooks/my%20show?token=old", syncFlags(sonarrRequired...))
		arr := newFakeSyncArr(t, "sonarr", handMade)
		h := newSyncHarness(t, arr)
		h.sync.Instance, h.sync.BaseURL = "my show", base

		if err := h.sync.Sync(context.Background()); err != nil {
			t.Fatalf("Sync() = %v", err)
		}
		expectPut(t, arr, 7, handMade, func(resource map[string]any) { setTestFieldValue(resource, "url", want) })
		h.expectEvent(t, "webhook.connection_adopted", "info", map[string]any{"connection_id": float64(7)})
	})
}

func TestSyncNeverLeaksSecrets(t *testing.T) {
	handMade := func() map[string]any {
		return syncResource("sonarr", 7, "subsyncd", "Webhook", syncBase+"/webhooks/"+syncInstance+"?token=SENTINEL-OTHER", syncFlags(sonarrRequired...))
	}
	scenarios := map[string]func(*fakeSyncArr){
		"create": func(*fakeSyncArr) {},
		"adopt":  func(arr *fakeSyncArr) { arr.connections = append(arr.connections, mustJSON(t, handMade())) },
		"rejected": func(arr *fakeSyncArr) {
			arr.postReplies = []syncReply{{Status: http.StatusBadRequest, Body: testRejectedBody}}
		},
		"unreachable": func(arr *fakeSyncArr) {
			arr.postReplies = []syncReply{{Status: http.StatusInternalServerError, Body: testUnreachableBody}}
		},
		"repair rejected": func(arr *fakeSyncArr) {
			arr.connections = append(arr.connections, mustJSON(t, handMade()))
			arr.putReplies = []syncReply{{Status: http.StatusBadRequest, Body: testRejectedBody}}
		},
		"repair unreachable": func(arr *fakeSyncArr) {
			arr.connections = append(arr.connections, mustJSON(t, handMade()))
			arr.putReplies = []syncReply{{Status: http.StatusInternalServerError, Body: testUnreachableBody}}
		},
		"foreign": func(arr *fakeSyncArr) {
			arr.connections = append(arr.connections, mustJSON(t, syncResource("sonarr", 8, "other", "Webhook", "http://canary/webhooks/"+syncInstance+"?token=SENTINEL-OTHER", nil)))
		},
	}
	for name, setup := range scenarios {
		t.Run(name, func(t *testing.T) {
			arr := newFakeSyncArr(t, "sonarr", syncOthers("sonarr")...)
			setup(arr)
			h := newSyncHarness(t, arr)
			err := h.sync.Sync(context.Background())
			output := h.logs.String()
			if err != nil {
				output += "\n" + err.Error()
			}
			if strings.TrimSpace(h.logs.String()) == "" && err == nil {
				t.Fatal("scenario produced neither logs nor an error")
			}
			for _, forbidden := range []string{syncToken, "SENTINEL-OTHER", "SENTINEL", "http://", "https://", arr.server.URL, "old-token"} {
				if strings.Contains(output, forbidden) {
					t.Errorf("output contains %q:\n%s", forbidden, output)
				}
			}
		})
	}
}
