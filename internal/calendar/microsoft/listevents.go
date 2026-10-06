package microsoft

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/calnode/calnode/internal/calendar"
)

// eventViewResp is the subset of calendarView we read for the team calendar.
type eventViewResp struct {
	Value []struct {
		ID          string        `json:"id"`
		Subject     string        `json:"subject"`
		IsAllDay    bool          `json:"isAllDay"`
		IsCancelled bool          `json:"isCancelled"`
		Sensitivity string        `json:"sensitivity"` // "normal" | "personal" | "private" | "confidential"
		Location    graphLocation `json:"location"`
		Start       graphDateTime `json:"start"`
		End         graphDateTime `json:"end"`
	} `json:"value"`
	NextLink string `json:"@odata.nextLink"`
}

// ListEvents reads the user's events in [from, to) from the same calendars FreeBusy
// checks, as titled events. calendarView already expands recurrences into instances,
// each with its own id.
func (c *Client) ListEvents(ctx context.Context, userID string, from, to time.Time) ([]calendar.ExternalEvent, error) {
	conns, err := c.freeBusyConnections(ctx, userID)
	if err != nil {
		return nil, err
	}
	var out []calendar.ExternalEvent
	for _, fc := range conns {
		if fc.useDefault {
			evs, err := c.eventView(ctx, fc.hc, c.apiBase+"/me/calendarView", from, to)
			if err != nil {
				return nil, err
			}
			out = append(out, evs...)
			continue
		}
		for _, calID := range fc.calIDs {
			base := c.apiBase + "/me/calendars/" + url.PathEscape(calID) + "/calendarView"
			evs, err := c.eventView(ctx, fc.hc, base, from, to)
			if err != nil {
				return nil, err
			}
			out = append(out, evs...)
		}
	}
	return out, nil
}

// eventView pages through one calendar-view endpoint and returns its events in [from, to).
func (c *Client) eventView(ctx context.Context, hc *http.Client, base string, from, to time.Time) ([]calendar.ExternalEvent, error) {
	q := url.Values{}
	q.Set("startDateTime", from.UTC().Format(time.RFC3339))
	q.Set("endDateTime", to.UTC().Format(time.RFC3339))
	q.Set("$select", "id,subject,location,start,end,isAllDay,isCancelled,sensitivity")
	q.Set("$orderby", "start/dateTime")
	q.Set("$top", "200")
	next := base + "?" + q.Encode()

	var out []calendar.ExternalEvent
	for page := 0; next != "" && page < 25; page++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return nil, fmt.Errorf("microsoft: calendarView request: %w", err)
		}
		req.Header.Set("Prefer", `outlook.timezone="UTC"`)

		resp, err := hc.Do(req)
		if err != nil {
			return nil, fmt.Errorf("microsoft: calendarView call: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			msg := graphErrBody(resp)
			resp.Body.Close() // #nosec G104 -- already returning a more specific error; nothing actionable on close error
			return nil, fmt.Errorf("microsoft: calendarView status %d: %s", resp.StatusCode, msg)
		}
		var ev eventViewResp
		derr := json.NewDecoder(resp.Body).Decode(&ev)
		resp.Body.Close() // #nosec G104 -- body already decoded above; nothing actionable on close error
		if derr != nil {
			return nil, fmt.Errorf("microsoft: calendarView decode: %w", derr)
		}
		for _, it := range ev.Value {
			if it.IsCancelled {
				continue
			}
			s, err1 := it.Start.parse()
			e, err2 := it.End.parse()
			if err1 != nil || err2 != nil {
				c.logger.Warn("microsoft: skipping event with unparseable time", "event", it.ID, "start", it.Start.DateTime, "end", it.End.DateTime)
				continue
			}
			title, loc := it.Subject, it.Location.DisplayName
			// An event the owner marked private is shared with the team as busy time only -
			// the same thing FreeBusy already told them - never by title or place.
			if it.Sensitivity == "private" || it.Sensitivity == "confidential" {
				title, loc = "", ""
			}
			out = append(out, calendar.ExternalEvent{
				ID:       it.ID,
				Title:    title,
				Location: loc,
				Start:    s.UTC(),
				End:      e.UTC(),
				AllDay:   it.IsAllDay,
			})
		}
		next = ev.NextLink
	}
	return out, nil
}
