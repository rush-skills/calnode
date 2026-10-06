package microsoft

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestListEvents_parsesCalendarViewWithPaging(t *testing.T) {
	c := newTestClient(t)
	connect(t, c, "u1")

	var srv *httptest.Server
	var hits []string
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		if !strings.Contains(r.URL.Path, "/me/calendarView") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Prefer") != `outlook.timezone="UTC"` {
			t.Errorf("missing UTC Prefer header")
		}
		if sel := r.URL.Query().Get("$select"); r.URL.Query().Get("$skiptoken") == "" && !strings.Contains(sel, "subject") {
			t.Errorf("$select must ask for titles; got %q", sel)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("$skiptoken") == "" {
			_, _ = w.Write([]byte(`{"@odata.nextLink":"` + srv.URL + `/me/calendarView?$skiptoken=p2","value":[
				{"id":"m1","subject":"Standup","isAllDay":false,"isCancelled":false,"location":{"displayName":"Teams"},
				 "start":{"dateTime":"2026-06-22T21:00:00.0000000","timeZone":"UTC"},
				 "end":{"dateTime":"2026-06-22T21:30:00.0000000","timeZone":"UTC"}},
				{"id":"m-cancelled","subject":"Nope","isCancelled":true,
				 "start":{"dateTime":"2026-06-22T22:00:00.0000000","timeZone":"UTC"},
				 "end":{"dateTime":"2026-06-22T22:30:00.0000000","timeZone":"UTC"}}
			]}`))
			return
		}
		_, _ = w.Write([]byte(`{"value":[
			{"id":"m2","subject":"Offsite","isAllDay":true,
			 "start":{"dateTime":"2026-06-23T00:00:00.0000000","timeZone":"UTC"},
			 "end":{"dateTime":"2026-06-24T00:00:00.0000000","timeZone":"UTC"}},
			{"id":"m3","subject":"Therapy","sensitivity":"confidential","location":{"displayName":"Clinic"},
			 "start":{"dateTime":"2026-06-23T09:00:00.0000000","timeZone":"UTC"},
			 "end":{"dateTime":"2026-06-23T10:00:00.0000000","timeZone":"UTC"}}
		]}`))
	}))
	defer srv.Close()
	c.apiBase = srv.URL

	evs, err := c.ListEvents(context.Background(), "u1", time.Now(), time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(evs) != 3 {
		t.Fatalf("got %d events; want 3 (cancelled skipped): %+v", len(evs), evs)
	}
	// A confidential event is busy time only: it keeps its id and times, loses title and place.
	if evs[2].ID != "m3" || evs[2].Title != "" || evs[2].Location != "" {
		t.Errorf("confidential event must be stripped to busy time: %+v", evs[2])
	}
	if evs[0].ID != "m1" || evs[0].Title != "Standup" || evs[0].Location != "Teams" || evs[0].AllDay {
		t.Errorf("m1: %+v", evs[0])
	}
	if want := time.Date(2026, 6, 22, 21, 0, 0, 0, time.UTC); !evs[0].Start.Equal(want) {
		t.Errorf("m1 start = %v; want %v", evs[0].Start, want)
	}
	if evs[1].ID != "m2" || !evs[1].AllDay {
		t.Errorf("m2 (all-day): %+v", evs[1])
	}
	if len(hits) != 2 {
		t.Errorf("expected two pages; got %v", hits)
	}
}

func TestListEvents_readsPickedCalendars(t *testing.T) {
	c := newTestClient(t)
	connect(t, c, "u1")
	// A saved selection switches the read from /me/calendarView to each picked calendar.
	if _, err := c.db.Exec(`INSERT INTO connection_calendars (id,user_id,provider,account_email,calendar_id,name,check_conflicts,is_destination)
		VALUES ('s1','u1','microsoft','','cal-A','Work',1,0), ('s2','u1','microsoft','','cal-B','Personal',0,0)`); err != nil {
		t.Fatal(err)
	}
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value":[]}`))
	}))
	defer srv.Close()
	c.apiBase = srv.URL
	if _, err := c.ListEvents(context.Background(), "u1", time.Now(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "/me/calendars/cal-A/calendarView" {
		t.Errorf("only the conflict-checked calendar must be read; got %v", paths)
	}
}
