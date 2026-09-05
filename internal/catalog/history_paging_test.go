package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cplieger/arrapi/v2"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"subsyncd/internal/domain"
	"testing"
	"time"
)

type pagedHistoryFake struct {
	pages []arrapi.HistoryPage
	calls []arrapi.HistoryOptions
	errAt int
}

func (f *pagedHistoryFake) History(_ context.Context, o arrapi.HistoryOptions) (arrapi.HistoryPage, error) {
	f.calls = append(f.calls, o)
	if o.Page == f.errAt {
		return arrapi.HistoryPage{}, errors.New("failed")
	}
	if o.Page > len(f.pages) {
		return arrapi.HistoryPage{Page: o.Page, PageSize: o.PageSize}, nil
	}
	page := f.pages[o.Page-1]
	page.Page = o.Page
	page.PageSize = o.PageSize
	return page, nil
}
func TestHistoryPagesBoundOverlapAndConcurrentDuplicates(t *testing.T) {
	since := time.Date(2026, 9, 5, 12, 0, 0, 500000000, time.UTC)
	through := since.Add(time.Hour)
	record := func(id int, at time.Time) arrapi.HistoryRecord {
		return arrapi.HistoryRecord{ID: id, MovieID: id, EventType: arrapi.EventFileDeleted, Date: at}
	}
	a := record(1, through.Add(time.Second))
	b := record(100, since.Add(time.Minute))
	c := record(101, since.Truncate(time.Second))
	d := record(102, since.Add(-time.Second))
	first := []arrapi.HistoryRecord{a}
	for id := 2; id < 100; id++ {
		first = append(first, arrapi.HistoryRecord{ID: id, Date: b.Date, EventType: arrapi.EventGrabbed})
	}
	first = append(first, b)
	// One concurrent insertion moves b from offset 99 to offset 100.
	f := &pagedHistoryFake{pages: []arrapi.HistoryPage{{Records: first, TotalRecords: 102}, {Records: []arrapi.HistoryRecord{b, c, d}, TotalRecords: 103}}}
	records, err := readHistoryWindow(t.Context(), f, since, through)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].ID != 100 || records[1].ID != 101 || len(f.calls) != 2 {
		t.Fatalf("records=%+v calls=%v", records, f.calls)
	}
	for i, o := range f.calls {
		if o.Page != i+1 || o.PageSize != 100 {
			t.Fatalf("options=%+v", o)
		}
	}
}
func TestHistoryPagesZeroCursorAndFailure(t *testing.T) {
	now := time.Now()
	first := historyFullPage(now)
	f := &pagedHistoryFake{pages: []arrapi.HistoryPage{{Records: first, TotalRecords: 101}, {Records: []arrapi.HistoryRecord{{ID: 101, MovieID: 101, EventType: arrapi.EventFileDeleted, Date: now.Add(-time.Hour)}}, TotalRecords: 101}}}
	got, err := readHistoryWindow(t.Context(), f, time.Time{}, now)
	if err != nil || len(got) != 101 {
		t.Fatalf("records=%v err=%v", got, err)
	}
	f.calls = nil
	f.errAt = 2
	got, err = readHistoryWindow(t.Context(), f, time.Time{}, now)
	if err == nil || got != nil {
		t.Fatalf("partial success=%v err=%v", got, err)
	}
}

func writeHistoryFixture(t *testing.T, w http.ResponseWriter, payload []byte) {
	t.Helper()
	var records []map[string]any
	if err := json.Unmarshal(payload, &records); err != nil {
		t.Error(err)
		return
	}
	writeHistoryRecords(t, w, records)
}
func writeHistoryRecords(t *testing.T, w http.ResponseWriter, records []map[string]any) {
	t.Helper()
	sort.SliceStable(records, func(i, j int) bool { return fmt.Sprint(records[i]["date"]) > fmt.Sprint(records[j]["date"]) })
	if err := json.NewEncoder(w).Encode(map[string]any{"records": records, "totalRecords": len(records), "page": 1, "pageSize": 100}); err != nil {
		t.Error(err)
	}
}

func TestHistoryPagesFailClosedOnInconsistentPages(t *testing.T) {
	now := time.Now()
	r := arrapi.HistoryRecord{ID: 1, Date: now, EventType: arrapi.EventFileDeleted, MovieID: 1}
	first := historyFullPage(now)
	for _, tc := range []struct {
		name  string
		pages []arrapi.HistoryPage
	}{
		{"shrinking total", []arrapi.HistoryPage{{Records: first, TotalRecords: 102}, {Records: []arrapi.HistoryRecord{{ID: 101, Date: now.Add(-time.Second)}}, TotalRecords: 101}}},
		{"changed duplicate", []arrapi.HistoryPage{{Records: first, TotalRecords: 101}, {Records: []arrapi.HistoryRecord{{ID: 100, Date: now.Add(-time.Second)}, {ID: 101, Date: now.Add(-time.Second)}}, TotalRecords: 102}}},
		{"no progress", []arrapi.HistoryPage{{Records: first, TotalRecords: 101}, {Records: []arrapi.HistoryRecord{first[99]}, TotalRecords: 101}}},
		{"unordered", []arrapi.HistoryPage{{Records: []arrapi.HistoryRecord{r, {ID: 2, Date: now.Add(time.Second)}}, TotalRecords: 2}}},
		{"missing date", []arrapi.HistoryPage{{Records: []arrapi.HistoryRecord{{ID: 2}}, TotalRecords: 1}}},
		{"empty early", []arrapi.HistoryPage{{TotalRecords: 1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &pagedHistoryFake{pages: tc.pages}
			got, err := readHistoryWindow(t.Context(), f, time.Time{}, now)
			if err == nil || got != nil {
				t.Fatalf("accepted malformed page: %v %v", got, err)
			}
		})
	}
}

func TestPagedHistoryUsesArrapiAndKeepsCursorOnLaterPageFailure(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v3/history" || r.URL.Query().Get("pageSize") != "100" || r.URL.Query().Get("sortKey") != "date" || r.URL.Query().Get("sortDirection") != "descending" {
					t.Errorf("unexpected request %s", r.URL)
					w.WriteHeader(400)
					return
				}
				calls++
				page, _ := strconv.Atoi(r.URL.Query().Get("page"))
				if page == 2 && fail {
					w.WriteHeader(400)
					return
				}
				var records []map[string]any
				start, end := 0, 100
				if page == 2 {
					start, end = 100, 101
				}
				for i := start; i < end; i++ {
					records = append(records, map[string]any{"id": i + 1, "movieId": 400, "eventType": "movieFileDeleted", "date": now.Add(-time.Duration(i) * time.Second)})
				}
				json.NewEncoder(w).Encode(map[string]any{"page": page, "pageSize": 100, "totalRecords": 101, "records": records})
			}))
			defer server.Close()
			client, err := newRadarrEntityClient("main", server.URL, "secret", nil)
			if err != nil {
				t.Fatal(err)
			}
			records, err := readHistoryWindow(t.Context(), client, time.Time{}, now)
			if fail {
				if err == nil || records != nil {
					t.Fatalf("partial success=%v err=%v", records, err)
				}
			} else {
				if err != nil || len(records) != 101 {
					t.Fatalf("count=%d err=%v", len(records), err)
				}
				changes, err := reduceHistoryByEntity(records, domain.MediaMovie, now, func(r arrapi.HistoryRecord) int64 { return int64(r.MovieID) })
				if err != nil || len(changes) != 1 || changes[0].HistoryID != 1 {
					t.Fatalf("reduction=%v %v", changes, err)
				}
			}
			if calls != 2 {
				t.Fatalf("page requests=%d", calls)
			}
			if fail {
				backend := &fakeReconcileStore{cursor: now.Add(-time.Hour)}
				cat := &Radarr{client: &arrClient{instance: "main"}, entity: client}
				r := Reconciler{Instance: "main", Catalog: cat, Store: backend, Now: func() time.Time { return now }}
				if err := r.Run(t.Context()); err == nil || !backend.committed.IsZero() {
					t.Fatalf("cursor advanced after page failure: %v", err)
				}
			}
		})
	}
}

type fixedHistoryPage arrapi.HistoryPage

func (f fixedHistoryPage) History(context.Context, arrapi.HistoryOptions) (arrapi.HistoryPage, error) {
	return arrapi.HistoryPage(f), nil
}
func TestHistoryPagesRejectMissingMetadata(t *testing.T) {
	for _, page := range []arrapi.HistoryPage{{}, {Page: 1}, {Page: 1, PageSize: 99}, {Page: 1, PageSize: 100, TotalRecords: 0, Records: []arrapi.HistoryRecord{{ID: 1, Date: time.Now()}}}} {
		if _, err := readHistoryWindow(t.Context(), fixedHistoryPage(page), time.Time{}, time.Now()); err == nil {
			t.Fatalf("accepted page %+v", page)
		}
	}
}

type pagedRadarrFake struct{ pagedHistoryFake }

func (f *pagedRadarrFake) MovieByID(_ context.Context, id int) (arrapi.Movie, error) {
	return arrapi.Movie{ID: id, HasFile: false}, nil
}
func TestHistoryShortNonfinalPageCannotAdvanceCursor(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	since := now.Add(-100 * time.Second)
	var first []arrapi.HistoryRecord
	for i := 0; i < 99; i++ {
		first = append(first, arrapi.HistoryRecord{ID: i + 1, MovieID: i + 1, EventType: arrapi.EventFileDeleted, Date: now.Add(-time.Duration(i) * time.Second)})
	}
	// Offset 100 on page two skips the omitted 100th record, whose timestamp
	// would still be inside the requested window. The next record is below it.
	client := &pagedRadarrFake{pagedHistoryFake{pages: []arrapi.HistoryPage{
		{Records: first, TotalRecords: 101},
		{Records: []arrapi.HistoryRecord{{ID: 101, MovieID: 101, EventType: arrapi.EventFileDeleted, Date: since.Add(-time.Second)}}, TotalRecords: 101},
	}}}
	backend := &fakeReconcileStore{cursor: since}
	cat := &Radarr{client: &arrClient{instance: "main"}, entity: client}
	r := Reconciler{Instance: "main", Catalog: cat, Store: backend, Now: func() time.Time { return now }}
	err := r.Run(t.Context())
	if err == nil || !backend.committed.IsZero() || len(backend.mutations) != 0 {
		t.Fatalf("short page committed: err=%v cursor=%v mutations=%d", err, backend.committed, len(backend.mutations))
	}
	if len(client.calls) != 1 {
		t.Fatalf("continued past incomplete page: calls=%d", len(client.calls))
	}
}

func historyFullPage(at time.Time) []arrapi.HistoryRecord {
	records := make([]arrapi.HistoryRecord, 100)
	for i := range records {
		records[i] = arrapi.HistoryRecord{ID: i + 1, MovieID: i + 1, EventType: arrapi.EventFileDeleted, Date: at}
	}
	return records
}
