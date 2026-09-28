package handler_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A host who attends a booking but is not its primary (bookings.host_id) — a required
// host beside a rotation pick, or any Group member — sees it under "My bookings" but
// got "booking not found" on reschedule and cancel, which gated on the primary alone.
// Both now accept any attending host; a non-admin who is not on the booking still 404s.
func TestSecondaryHost_canRescheduleAndCancel_strangerCannot(t *testing.T) {
	h, database, ownerKey, ownerID := setupWorkspaceWithDB(t)
	for _, u := range [][2]string{{"u-second", "Second Host"}, {"u-stranger", "Not Invited"}} {
		if _, err := database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES (?,?,?,'UTC',0)`, u[0], u[0]+"@example.com", u[1]); err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}
	const secondKey, strangerKey = "cno_secondkey", "cno_strangerkey"
	database.Exec(`INSERT INTO api_keys (id,user_id,name,key_hash,created_at) VALUES ('k-second','u-second','t',?,'2024-01-01')`, sha256HexForTest(secondKey))       //nolint:errcheck
	database.Exec(`INSERT INTO api_keys (id,user_id,name,key_hash,created_at) VALUES ('k-stranger','u-stranger','t',?,'2024-01-01')`, sha256HexForTest(strangerKey)) //nolint:errcheck

	slug, _ := seedEventTypeHTTP(t, h, ownerKey)
	// Reschedule validates the new time against every attending host's hours.
	seedFullAvailabilityDB(t, database, "u-second")
	// Two weeks out: past the minimum notice, inside the booking window, whatever today is.
	day := time.Now().UTC().AddDate(0, 0, 14).Truncate(24 * time.Hour)
	for day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
		day = day.AddDate(0, 0, 1)
	}
	bookingID := createBookingViaHTTP(t, h, slug, day.Add(9*time.Hour).Format(time.RFC3339))
	newStart := day.Add(10 * time.Hour).Format(time.RFC3339)
	// The owner is the primary; add the second host as an attending non-primary.
	if _, err := database.Exec(`INSERT INTO booking_hosts (id, booking_id, user_id, is_primary) VALUES ('bh-second', ?, 'u-second', 0)`, bookingID); err != nil {
		t.Fatalf("seed secondary host: %v", err)
	}
	var primary string
	if err := database.QueryRow(`SELECT host_id FROM bookings WHERE id = ?`, bookingID).Scan(&primary); err != nil || primary != ownerID {
		t.Fatalf("precondition: primary = %q (err %v); want owner %q", primary, err, ownerID)
	}

	reschedule := func(key string) *httptest.ResponseRecorder {
		req := authReq(http.MethodPatch, "/v1/bookings/"+bookingID+"/reschedule", `{"start_at":"`+newStart+`"}`, key)
		req.SetPathValue("id", bookingID)
		rec := httptest.NewRecorder()
		h.RequireAuth(h.RescheduleBooking)(rec, req)
		return rec
	}
	cancel := func(key string) *httptest.ResponseRecorder {
		req := authReq(http.MethodPost, "/v1/bookings/"+bookingID+"/cancel", `{}`, key)
		req.SetPathValue("id", bookingID)
		rec := httptest.NewRecorder()
		h.RequireAuth(h.CancelBooking)(rec, req)
		return rec
	}

	if rec := reschedule(strangerKey); rec.Code != http.StatusNotFound {
		t.Errorf("stranger reschedule: status = %d; want 404 — %s", rec.Code, rec.Body.String())
	}
	if rec := cancel(strangerKey); rec.Code != http.StatusNotFound {
		t.Errorf("stranger cancel: status = %d; want 404 — %s", rec.Code, rec.Body.String())
	}
	if rec := reschedule(secondKey); rec.Code != http.StatusOK {
		t.Fatalf("secondary host reschedule: status = %d; want 200 — %s", rec.Code, rec.Body.String())
	}
	if rec := cancel(secondKey); rec.Code != http.StatusOK {
		t.Fatalf("secondary host cancel: status = %d; want 200 — %s", rec.Code, rec.Body.String())
	}
	var status string
	if err := database.QueryRow(`SELECT status FROM bookings WHERE id = ?`, bookingID).Scan(&status); err != nil || status != "cancelled" {
		t.Errorf("status after cancel = %q (err %v); want cancelled", status, err)
	}
}
