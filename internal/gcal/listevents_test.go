package gcal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestListEvents_readsSelectedCalendarsWithPaging(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path+"?"+r.URL.RawQuery)
		q := r.URL.Query()
		if q.Get("singleEvents") != "true" || q.Get("orderBy") != "startTime" || q.Get("timeMin") == "" || q.Get("timeMax") == "" {
			t.Errorf("events.list must expand recurrences inside the window; got %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		if q.Get("pageToken") == "" {
			_, _ = w.Write([]byte(`{"nextPageToken":"p2","items":[
				{"id":"e1","status":"confirmed","summary":"Dentist","location":"Main St",
				 "start":{"dateTime":"2026-06-15T09:00:00+02:00"},"end":{"dateTime":"2026-06-15T10:00:00+02:00"}},
				{"id":"gone","status":"cancelled","summary":"Cancelled",
				 "start":{"dateTime":"2026-06-15T11:00:00Z"},"end":{"dateTime":"2026-06-15T12:00:00Z"}}
			]}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[
			{"id":"e2","status":"confirmed","summary":"Offsite",
			 "start":{"date":"2026-06-16"},"end":{"date":"2026-06-17"}},
			{"id":"e3","status":"confirmed","summary":"Oncology appointment","location":"Clinic","visibility":"private",
			 "start":{"dateTime":"2026-06-17T09:00:00Z"},"end":{"dateTime":"2026-06-17T10:00:00Z"}},
			{"id":"broken","status":"confirmed","summary":"No times","start":{},"end":{}}
		]}`))
	}))
	defer srv.Close()

	c := newTestClient(t)
	c.apiBase = srv.URL
	saveAndConnectClient(t, c, "user-1", "primary", "tok")

	from := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	evs, err := c.ListEvents(context.Background(), "user-1", from, from.Add(7*24*time.Hour))
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(evs) != 3 {
		t.Fatalf("got %d events; want 3 (cancelled and timeless skipped): %+v", len(evs), evs)
	}
	// A private event is busy time only: it keeps its id and times, loses title and place.
	if evs[2].ID != "e3" || evs[2].Title != "" || evs[2].Location != "" || evs[2].Start.IsZero() {
		t.Errorf("private event must be stripped to busy time: %+v", evs[2])
	}
	if evs[0].ID != "e1" || evs[0].Title != "Dentist" || evs[0].Location != "Main St" || evs[0].AllDay {
		t.Errorf("e1: %+v", evs[0])
	}
	if want := time.Date(2026, 6, 15, 7, 0, 0, 0, time.UTC); !evs[0].Start.Equal(want) || evs[0].Start.Location() != time.UTC {
		t.Errorf("e1 start = %v; want %v in UTC (offset normalised)", evs[0].Start, want)
	}
	if evs[1].ID != "e2" || !evs[1].AllDay || !evs[1].Start.Equal(time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)) || !evs[1].End.Equal(time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("e2 (all-day): %+v", evs[1])
	}
	if len(paths) != 2 || !strings.HasPrefix(paths[0], "/calendars/primary/events?") || !strings.Contains(paths[1], "pageToken=p2") {
		t.Errorf("expected two pages of /calendars/primary/events; got %v", paths)
	}
}

func TestListEvents_noConflictConnection_returnsNothingWithoutCalling(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer srv.Close()
	c := newTestClient(t)
	c.apiBase = srv.URL
	saveAndConnectClient(t, c, "user-1", "primary", "tok")
	if _, err := c.db.Exec(`UPDATE calendar_connections SET check_conflicts = 0`); err != nil {
		t.Fatal(err)
	}
	evs, err := c.ListEvents(context.Background(), "user-1", time.Now(), time.Now().Add(time.Hour))
	if err != nil || len(evs) != 0 || called {
		t.Errorf("deselected account must contribute nothing: evs=%v err=%v called=%v", evs, err, called)
	}
}

func TestListEvents_apiErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer srv.Close()
	c := newTestClient(t)
	c.apiBase = srv.URL
	saveAndConnectClient(t, c, "user-1", "primary", "tok")
	if _, err := c.ListEvents(context.Background(), "user-1", time.Now(), time.Now().Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("expected a status error; got %v", err)
	}
}
