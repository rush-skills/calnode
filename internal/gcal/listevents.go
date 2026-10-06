package gcal

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/calnode/calnode/internal/calendar"
)

// listEventsResp is the subset of events.list we read for the team calendar.
type listEventsResp struct {
	Items []struct {
		ID         string `json:"id"`
		Status     string `json:"status"`
		Summary    string `json:"summary"`
		Location   string `json:"location"`
		Visibility string `json:"visibility"` // "default" | "public" | "private" | "confidential"
		Start      struct {
			DateTime string `json:"dateTime"`
			Date     string `json:"date"`
		} `json:"start"`
		End struct {
			DateTime string `json:"dateTime"`
			Date     string `json:"date"`
		} `json:"end"`
	} `json:"items"`
	NextPageToken string `json:"nextPageToken"`
}

// ListEvents reads the user's events in [from, to) from the same calendars FreeBusy
// checks (every conflict-check account's selected calendars), as titled events.
// singleEvents=true expands recurrences into instances so no RRULE is interpreted
// here, and each instance carries its own id, so a platform-created event (always a
// single event) can still be matched by id for de-duplication.
func (c *Client) ListEvents(ctx context.Context, userID string, from, to time.Time) ([]calendar.ExternalEvent, error) {
	conns, err := c.freeBusyConnections(ctx, userID)
	if err != nil {
		return nil, err
	}
	var out []calendar.ExternalEvent
	for _, conn := range conns {
		for _, calID := range conn.calIDs {
			evs, err := c.listEventsForCalendar(ctx, conn.hc, calID, from, to)
			if err != nil {
				return nil, err
			}
			out = append(out, evs...)
		}
	}
	return out, nil
}

// listEventsForCalendar pages through events.list for one calendar.
func (c *Client) listEventsForCalendar(ctx context.Context, hc *http.Client, calID string, from, to time.Time) ([]calendar.ExternalEvent, error) {
	q := url.Values{}
	q.Set("singleEvents", "true")
	q.Set("orderBy", "startTime")
	q.Set("timeMin", from.UTC().Format(time.RFC3339))
	q.Set("timeMax", to.UTC().Format(time.RFC3339))
	q.Set("maxResults", "250")
	q.Set("fields", "items(id,status,summary,location,visibility,start,end),nextPageToken")
	base := c.apiBase + "/calendars/" + url.PathEscape(calID) + "/events"

	var out []calendar.ExternalEvent
	for page := 0; page < 20; page++ { // 5000 events in a 6-week window is a runaway, not a calendar
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"?"+q.Encode(), nil)
		if err != nil {
			return nil, fmt.Errorf("gcal: events.list request: %w", err)
		}
		resp, err := hc.Do(req)
		if err != nil {
			return nil, fmt.Errorf("gcal: events.list call: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close() // #nosec G104 -- already returning a more specific error; nothing actionable on close error
			return nil, fmt.Errorf("gcal: events.list status %d", resp.StatusCode)
		}
		var lr listEventsResp
		derr := json.NewDecoder(resp.Body).Decode(&lr)
		resp.Body.Close() // #nosec G104 -- body already decoded above; nothing actionable on close error
		if derr != nil {
			return nil, fmt.Errorf("gcal: events.list decode: %w", derr)
		}
		for _, it := range lr.Items {
			if it.Status == "cancelled" {
				continue
			}
			ev := calendar.ExternalEvent{ID: it.ID, Title: it.Summary, Location: it.Location}
			// An event the owner marked private is shared with the team as busy time only -
			// the same thing FreeBusy already told them - never by title or place.
			if it.Visibility == "private" || it.Visibility == "confidential" {
				ev.Title, ev.Location = "", ""
			}
			var err error
			ev.Start, ev.AllDay, err = parseEventTime(it.Start.DateTime, it.Start.Date)
			if err != nil {
				c.logger.Warn("gcal: skipping event with unparseable start", "event", it.ID, "error", err)
				continue
			}
			ev.End, _, err = parseEventTime(it.End.DateTime, it.End.Date)
			if err != nil {
				c.logger.Warn("gcal: skipping event with unparseable end", "event", it.ID, "error", err)
				continue
			}
			out = append(out, ev)
		}
		if lr.NextPageToken == "" {
			break
		}
		q.Set("pageToken", lr.NextPageToken)
	}
	return out, nil
}

// parseEventTime reads a Google event boundary: a timed event carries dateTime
// (RFC3339 with offset), an all-day one carries date (YYYY-MM-DD, interpreted as UTC
// midnight so the day survives the trip through the UTC-only API).
func parseEventTime(dateTime, date string) (t time.Time, allDay bool, err error) {
	switch {
	case dateTime != "":
		t, err = time.Parse(time.RFC3339, dateTime)
		return t.UTC(), false, err
	case date != "":
		t, err = time.Parse("2006-01-02", date)
		return t.UTC(), true, err
	default:
		return time.Time{}, false, fmt.Errorf("gcal: event without start/end")
	}
}
