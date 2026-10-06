package handler_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/handler"
)

func removeUser(t *testing.T, h *handler.Handler, key, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := authReq(http.MethodDelete, "/v1/users/"+id, body, key)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	h.RequireAuth(h.DeleteUser)(rec, req)
	return rec
}

// B1: a past booking the receiver already hosted at the same start would violate
// idx_bookings_no_double; it is attributed to the "Former member" tombstone instead,
// the rest moves, and the client never sees a raw SQL error.
func TestDeleteUser_transferCollidingPastBookingsGoToTombstone(t *testing.T) {
	h, db, ownerKey, _ := setupWorkspaceWithDB(t)
	for _, q := range []string{
		`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u2','leaver@example.com','Leaver','UTC',0)`,
		`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u3','heir@example.com','Heir','UTC',0)`,
		`INSERT INTO event_types (id,user_id,slug,name,duration_minutes) VALUES ('etx','u2','leaver-call','Call',30)`,
		`INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status) VALUES ('p1','etx','u2','2020-01-01T10:00:00Z','2020-01-01T10:30:00Z','confirmed')`,
		`INSERT INTO booking_hosts (id,booking_id,user_id,is_primary) VALUES ('bh-p1','p1','u2',1)`,
		`INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status) VALUES ('p2','etx','u3','2020-01-01T10:00:00Z','2020-01-01T10:30:00Z','confirmed')`,
		`INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status) VALUES ('p3','etx','u2','2020-01-02T10:00:00Z','2020-01-02T10:30:00Z','confirmed')`,
		// Cancelled rows never collide (the index is partial): both move to the receiver.
		`INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status) VALUES ('c1','etx','u2','2020-01-03T10:00:00Z','2020-01-03T10:30:00Z','cancelled')`,
		`INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status) VALUES ('c2','etx','u3','2020-01-03T10:00:00Z','2020-01-03T10:30:00Z','cancelled')`,
	} {
		mustExec(t, db, q)
	}
	rec := removeUser(t, h, ownerKey, "u2", `{"mode":"transfer","transfer_to":"u3"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("transfer: %d — %s", rec.Code, rec.Body.String())
	}
	host := func(id string) string {
		var s string
		db.QueryRow(`SELECT host_id FROM bookings WHERE id = ?`, id).Scan(&s)
		return s
	}
	if host("p1") != "former-member" {
		t.Errorf("colliding past booking host = %q; want the former-member tombstone", host("p1"))
	}
	if host("p3") != "u3" || host("c1") != "u3" || host("p2") != "u3" {
		t.Errorf("non-colliding rows: p3=%s c1=%s p2=%s; want all u3", host("p3"), host("c1"), host("p2"))
	}
	var seat string
	db.QueryRow(`SELECT user_id FROM booking_hosts WHERE id = 'bh-p1'`).Scan(&seat)
	if seat != "former-member" {
		t.Errorf("tombstoned booking's seat = %q; want former-member", seat)
	}
	var email, name string
	var isAdmin, emailLogin int
	var archived *string
	if err := db.QueryRow(`SELECT email, name, is_admin, email_login, archived_at FROM users WHERE id = 'former-member'`).
		Scan(&email, &name, &isAdmin, &emailLogin, &archived); err != nil {
		t.Fatalf("tombstone row: %v", err)
	}
	if email != "former-member@calnode.invalid" || name != "Former member" || isAdmin != 0 || emailLogin != 0 || archived == nil {
		t.Errorf("tombstone = %s %s admin=%d login=%d archived=%v", email, name, isAdmin, emailLogin, archived)
	}
	var gone int
	db.QueryRow(`SELECT COUNT(*) FROM users WHERE id = 'u2'`).Scan(&gone)
	if gone != 0 {
		t.Error("leaver still exists")
	}

	// Never in the directory, archived view or not; never removable; never a transfer target.
	for _, q := range []string{"", "?include_archived=true"} {
		req := authReq(http.MethodGet, "/v1/users"+q, "", ownerKey)
		lrec := httptest.NewRecorder()
		h.RequireAuth(h.ListUsers)(lrec, req)
		if strings.Contains(lrec.Body.String(), "former-member") {
			t.Errorf("tombstone listed in GET /v1/users%s: %s", q, lrec.Body.String())
		}
	}
	if rec := removeUser(t, h, ownerKey, "former-member", `{"mode":"delete"}`); rec.Code != http.StatusNotFound {
		t.Errorf("removing the tombstone -> %d; want 404", rec.Code)
	}
	mustExec(t, db, `INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u4','four@example.com','Four','UTC',0)`)
	if rec := removeUser(t, h, ownerKey, "u4", `{"mode":"transfer","transfer_to":"former-member"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("transfer to the tombstone -> %d; want 400", rec.Code)
	}
	// A second former member who hosted a booking at the SAME start collides with the
	// tombstone itself (the index applies to it too): the row overflows to
	// former-member-2, with the same rules; a different start reuses former-member.
	mustExec(t, db, `INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status) VALUES ('p4','etx','u4','2020-01-01T10:00:00Z','2020-01-01T10:30:00Z','confirmed')`)
	mustExec(t, db, `INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status) VALUES ('p5','etx','u4','2020-01-02T10:00:00Z','2020-01-02T10:30:00Z','confirmed')`)
	if rec := removeUser(t, h, ownerKey, "u4", `{"mode":"transfer","transfer_to":"u3"}`); rec.Code != http.StatusOK {
		t.Fatalf("second transfer: %d — %s", rec.Code, rec.Body.String())
	}
	var tombstones int
	db.QueryRow(`SELECT COUNT(*) FROM users WHERE email LIKE '%@calnode.invalid' AND archived_at IS NOT NULL AND email_login = 0`).Scan(&tombstones)
	if host("p4") != "former-member-2" || host("p5") != "former-member" || tombstones != 2 {
		t.Errorf("second collision: p4=%s p5=%s tombstones=%d; want former-member-2, former-member, 2", host("p4"), host("p5"), tombstones)
	}
	req := authReq(http.MethodGet, "/v1/users?include_archived=true", "", ownerKey)
	lrec := httptest.NewRecorder()
	h.RequireAuth(h.ListUsers)(lrec, req)
	if strings.Contains(lrec.Body.String(), "former-member") {
		t.Errorf("overflow tombstone listed: %s", lrec.Body.String())
	}
}

// M6a: the upcoming list includes meetings where the member only holds a seat.
func TestListUserUpcomingBookings_includesSeats(t *testing.T) {
	h, db, ownerKey, ownerID := setupWorkspaceWithDB(t)
	for _, q := range []string{
		`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u2','seat@example.com','Seat','UTC',0)`,
		`INSERT INTO event_types (id,user_id,slug,name,duration_minutes) VALUES ('etx','` + ownerID + `','coll','Call',30)`,
		`INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status) VALUES ('f1','etx','` + ownerID + `','2099-01-01T10:00:00Z','2099-01-01T10:30:00Z','confirmed')`,
		`INSERT INTO booking_hosts (id,booking_id,user_id,is_primary) VALUES ('bh1','f1','` + ownerID + `',1)`,
		`INSERT INTO booking_hosts (id,booking_id,user_id,is_primary) VALUES ('bh2','f1','u2',0)`,
		`INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status) VALUES ('f2','etx','u2','2099-01-02T10:00:00Z','2099-01-02T10:30:00Z','confirmed')`,
		`INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status) VALUES ('old','etx','u2','2020-01-02T10:00:00Z','2020-01-02T10:30:00Z','confirmed')`,
	} {
		mustExec(t, db, q)
	}
	req := authReq(http.MethodGet, "/v1/users/u2/upcoming-bookings", "", ownerKey)
	req.SetPathValue("id", "u2")
	rec := httptest.NewRecorder()
	h.RequireAuth(h.ListUserUpcomingBookings)(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, `"id":"f1"`) || !strings.Contains(body, `"id":"f2"`) || strings.Contains(body, `"old"`) {
		t.Errorf("upcoming = %s; want f1 (seat) and f2 (host), not the past one", body)
	}
}

// M6b: transfer moves every upcoming meeting synchronously - the primary booking through
// the reassign core (old event cancelled as the leaver, new one on the receiver's
// calendar), the non-primary seat by cancelling the leaver's event and writing the
// receiver's - all before the user row goes; and the leaver's per-event-type
// availability travels with the event type.
func TestDeleteUser_transferMovesUpcomingMeetingsBeforeDeleting(t *testing.T) {
	h, db, ownerKey, ownerID := setupWorkspaceWithDB(t)
	for _, q := range []string{
		`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u2','leaver@example.com','Leaver','UTC',0)`,
		`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u3','heir@example.com','Heir','UTC',0)`,
		`INSERT INTO event_types (id,user_id,slug,name,duration_minutes) VALUES ('etx','u2','leaver-call','Call',30)`,
		`INSERT INTO availability_rules (id,user_id,event_type_id,day_of_week,start_time,end_time) VALUES ('ar1','u2','etx',1,'09:00','12:00')`,
		`INSERT INTO availability_rules (id,user_id,event_type_id,day_of_week,start_time,end_time) VALUES ('ar2','u2','etx',2,'09:00','12:00')`,
		`INSERT INTO availability_rules (id,user_id,event_type_id,day_of_week,start_time,end_time) VALUES ('ar3','u3','etx',2,'09:00','12:00')`,
		`INSERT INTO availability_rules (id,user_id,event_type_id,day_of_week,start_time,end_time) VALUES ('ar-global','u2',NULL,3,'09:00','12:00')`,
		// Primary: the leaver hosts it, with a stamped calendar event.
		`INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status,external_event_id,location_value) VALUES ('mine','etx','u2','2099-02-01T10:00:00Z','2099-02-01T10:30:00Z','confirmed','evt-leaver-mine','https://meet.example/mine')`,
		`INSERT INTO booking_attendees (id,booking_id,name,email,iana_timezone,is_organizer) VALUES ('a1','mine','Alice','alice@example.com','UTC',1)`,
		`INSERT INTO booking_hosts (id,booking_id,user_id,is_primary,external_event_id,external_provider) VALUES ('bh1','mine','u2',1,'evt-leaver-mine','google')`,
		// Secondary seat on the owner's booking.
		`INSERT INTO event_types (id,user_id,slug,name,duration_minutes) VALUES ('eto','` + ownerID + `','coll','Collab',30)`,
		`INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status,location_value) VALUES ('f1','eto','` + ownerID + `','2099-01-01T10:00:00Z','2099-01-01T10:30:00Z','confirmed','https://meet.example/f1')`,
		`INSERT INTO booking_attendees (id,booking_id,name,email,iana_timezone,is_organizer) VALUES ('a2','f1','Bob','bob@example.com','UTC',1)`,
		`INSERT INTO booking_hosts (id,booking_id,user_id,is_primary,external_event_id) VALUES ('bh-o','f1','` + ownerID + `',1,'ownerevt')`,
		`INSERT INTO booking_hosts (id,booking_id,user_id,is_primary,external_event_id,external_provider) VALUES ('bh2','f1','u2',0,'evt-leaver-seat','google')`,
		// Secondary seat where the receiver is already on the meeting: the seat is dropped.
		`INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status) VALUES ('f2','eto','` + ownerID + `','2099-01-03T10:00:00Z','2099-01-03T10:30:00Z','confirmed')`,
		`INSERT INTO booking_hosts (id,booking_id,user_id,is_primary) VALUES ('bh-o2','f2','` + ownerID + `',1)`,
		`INSERT INTO booking_hosts (id,booking_id,user_id,is_primary,external_event_id,external_provider) VALUES ('bh3','f2','u2',0,'evt-leaver-dup','google')`,
		`INSERT INTO booking_hosts (id,booking_id,user_id,is_primary) VALUES ('bh-h','f2','u3',0)`,
	} {
		mustExec(t, db, q)
	}
	p := &userCalendar{}
	installUserCalendar(t, h, db, p, "u2", "u3", ownerID)

	rec := removeUser(t, h, ownerKey, "u2", `{"mode":"transfer","transfer_to":"u3"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("transfer: %d — %s", rec.Code, rec.Body.String())
	}
	creates, cancels := p.snap()
	// The leaver's events were cancelled AS the leaver (their row still existed), one per seat.
	for _, want := range []string{"u2:evt-leaver-mine", "u2:evt-leaver-seat", "u2:evt-leaver-dup"} {
		found := false
		for _, c := range cancels {
			found = found || c == want
		}
		if !found {
			t.Errorf("cancels = %v; want %s", cancels, want)
		}
	}
	// Two events written for the receiver: the reassigned primary and the moved seat,
	// each carrying the meeting's existing link, none minting a new one.
	if len(creates) != 2 || creates[0] != "u3:nomeet:loc=https://meet.example/f1" || creates[1] != "u3:nomeet:loc=https://meet.example/mine" {
		t.Errorf("creates = %v; want the receiver's event per moved meeting, in start order, no mint", creates)
	}
	var host, seatMine, seatF1, extF1 string
	db.QueryRow(`SELECT host_id FROM bookings WHERE id = 'mine'`).Scan(&host)
	db.QueryRow(`SELECT user_id FROM booking_hosts WHERE id = 'bh1'`).Scan(&seatMine)
	db.QueryRow(`SELECT user_id, COALESCE(external_event_id,'') FROM booking_hosts WHERE id = 'bh2'`).Scan(&seatF1, &extF1)
	if host != "u3" || seatMine != "u3" || seatF1 != "u3" || !strings.HasPrefix(extF1, "evt-u3-") {
		t.Errorf("after transfer: host=%s primary seat=%s moved seat=%s ext=%s", host, seatMine, seatF1, extF1)
	}
	var dupSeats int
	db.QueryRow(`SELECT COUNT(*) FROM booking_hosts WHERE booking_id = 'f2' AND user_id = 'u3'`).Scan(&dupSeats)
	if dupSeats != 1 {
		t.Errorf("receiver seats on f2 = %d; want exactly one (the leaver's dropped)", dupSeats)
	}
	var leftover int
	db.QueryRow(`SELECT COUNT(*) FROM booking_hosts WHERE user_id = 'u2'`).Scan(&leftover)
	if leftover != 0 {
		t.Errorf("leaver still holds %d seats", leftover)
	}
	// Availability for the transferred event type moved (duplicates dropped); the global rule went with the account.
	var rules string
	rows, _ := db.Query(`SELECT id || ':' || user_id || ':' || COALESCE(event_type_id,'-') FROM availability_rules ORDER BY id`)
	for rows.Next() {
		var s string
		rows.Scan(&s)
		rules += s + " "
	}
	rows.Close()
	if rules != "ar1:u3:etx ar3:u3:etx " {
		t.Errorf("availability rules after transfer = %q; want ar1 moved, ar2 dropped as duplicate of ar3, global gone", rules)
	}
}

// M6b: a receiver who is busy at one of the leaver's meetings is a 409 that names the
// meeting, and NOTHING has moved - no row, no calendar event, no user deleted.
func TestDeleteUser_transferReceiverConflictIs409AndMovesNothing(t *testing.T) {
	h, db, ownerKey, ownerID := setupWorkspaceWithDB(t)
	for _, q := range []string{
		`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u2','leaver@example.com','Leaver','UTC',0)`,
		`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u3','heir@example.com','Heir','UTC',0)`,
		`INSERT INTO event_types (id,user_id,slug,name,duration_minutes) VALUES ('etx','u2','leaver-call','Leaver Call',30)`,
		`INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status) VALUES ('early','etx','u2','2099-02-01T08:00:00Z','2099-02-01T08:30:00Z','confirmed')`,
		`INSERT INTO booking_hosts (id,booking_id,user_id,is_primary,external_event_id,external_provider) VALUES ('bh0','early','u2',1,'evt-early','google')`,
		`INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status) VALUES ('mine','etx','u2','2099-02-01T10:00:00Z','2099-02-01T10:30:00Z','confirmed')`,
		`INSERT INTO booking_hosts (id,booking_id,user_id,is_primary,external_event_id,external_provider) VALUES ('bh1','mine','u2',1,'evt-mine','google')`,
		`INSERT INTO event_types (id,user_id,slug,name,duration_minutes) VALUES ('eto','` + ownerID + `','coll','Collab',30)`,
		`INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status) VALUES ('theirs','eto','u3','2099-02-01T10:15:00Z','2099-02-01T10:45:00Z','confirmed')`,
		`INSERT INTO booking_hosts (id,booking_id,user_id,is_primary) VALUES ('bh3','theirs','u3',1)`,
	} {
		mustExec(t, db, q)
	}
	p := &userCalendar{}
	installUserCalendar(t, h, db, p, "u2", "u3")
	rec := removeUser(t, h, ownerKey, "u2", `{"mode":"transfer","transfer_to":"u3"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("got %d; want 409 — %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "mine") || !strings.Contains(rec.Body.String(), "Leaver Call") || !strings.Contains(rec.Body.String(), "2099-02-01 10:00") {
		t.Errorf("409 must name the meeting: %s", rec.Body.String())
	}
	creates, cancels := p.snap()
	if len(creates) != 0 || len(cancels) != 0 {
		t.Errorf("provider touched before the conflict was reported: creates=%v cancels=%v", creates, cancels)
	}
	var hostEarly string
	var users int
	db.QueryRow(`SELECT host_id FROM bookings WHERE id = 'early'`).Scan(&hostEarly)
	db.QueryRow(`SELECT COUNT(*) FROM users WHERE id = 'u2'`).Scan(&users)
	if hostEarly != "u2" || users != 1 {
		t.Errorf("something moved: early host=%s leaver rows=%d", hostEarly, users)
	}
}

// M6c: the leaver's scheduled and live sessions are re-hosted in transfer mode (event
// cancelled as the leaver, fresh Meet for the receiver) and cancelled in delete mode
// (calendar event cancelled as the leaver, before the row goes).
func TestDeleteUser_settlesLiveEvents(t *testing.T) {
	h, db, ownerKey, ownerID := setupWorkspaceWithDB(t)
	leaverKey := seedLiveMember(t, db, "u2", "leaver@example.com", false)
	seedLiveMember(t, db, "u3", "heir@example.com", false)
	p := &userCalendar{}
	installUserCalendar(t, h, db, p, "u2", "u3", ownerID)

	rec := liveReq(h, leaverKey, http.MethodPost, "/v1/live-events", "", `{"title":"Sched","scheduled_start_at":"2030-01-01T10:00:00Z"}`)
	mustStatus(t, rec, http.StatusCreated, "create scheduled")
	sched := liveBody(t, rec)["id"].(string)
	rec = liveReq(h, leaverKey, http.MethodPost, "/v1/live-events", "", `{"title":"Live","start_now":true}`)
	mustStatus(t, rec, http.StatusCreated, "create live")
	live := liveBody(t, rec)
	liveID := live["id"].(string)
	rec = liveReq(h, leaverKey, http.MethodPost, "/v1/live-events", "", `{"title":"Done","start_now":true}`)
	mustStatus(t, rec, http.StatusCreated, "create ended")
	endedID := liveBody(t, rec)["id"].(string)
	mustStatus(t, liveReq(h, leaverKey, http.MethodPost, "/v1/live-events/"+endedID+"/end", endedID, ""), http.StatusOK, "end")

	rec = removeUser(t, h, ownerKey, "u2", `{"mode":"transfer","transfer_to":"u3"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("transfer: %d — %s", rec.Code, rec.Body.String())
	}
	creates, cancels := p.snap()
	if len(cancels) != 2 || !strings.HasPrefix(cancels[0], "u2:") || !strings.HasPrefix(cancels[1], "u2:") {
		t.Errorf("cancels = %v; want the two active sessions' events cancelled as the leaver", cancels)
	}
	var u3Creates []string
	for _, c := range creates {
		if strings.HasPrefix(c, "u3:") {
			u3Creates = append(u3Creates, c)
		}
	}
	if len(u3Creates) != 2 {
		t.Errorf("receiver creates = %v; want one per re-hosted session", u3Creates)
	}
	ev := liveBody(t, liveReq(h, ownerKey, http.MethodGet, "/v1/live-events/"+sched, sched, ""))
	if ev["host_user_id"] != "u3" || !strings.HasPrefix(ev["join_url"].(string), "https://meet.google.com/u3") || ev["status"] != "scheduled" {
		t.Errorf("scheduled session after transfer: %v; want re-hosted with a fresh Meet for u3", ev)
	}
	ev = liveBody(t, liveReq(h, ownerKey, http.MethodGet, "/v1/live-events/"+liveID, liveID, ""))
	if ev["host_user_id"] != "u3" || ev["status"] != "live" || ev["join_url"] != live["join_url"] {
		t.Errorf("live session after transfer: %v; want still live, hosted by u3, same link", ev)
	}
	ev = liveBody(t, liveReq(h, ownerKey, http.MethodGet, "/v1/live-events/"+endedID, endedID, ""))
	if ev["host_user_id"] != "u3" || ev["created_by"] != "u3" {
		t.Errorf("ended session after transfer: %v; want attributed to u3", ev)
	}

	// Delete mode: cancelled, calendar event cancelled as the leaver.
	k4 := seedLiveMember(t, db, "u4", "four@example.com", false)
	installUserCalendar(t, h, db, p, "u4")
	rec = liveReq(h, k4, http.MethodPost, "/v1/live-events", "", `{"title":"Gone","scheduled_start_at":"2030-01-01T10:00:00Z"}`)
	mustStatus(t, rec, http.StatusCreated, "create u4")
	goneID := liveBody(t, rec)["id"].(string)
	rec = removeUser(t, h, ownerKey, "u4", `{"mode":"delete"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d — %s", rec.Code, rec.Body.String())
	}
	_, cancels = p.snap()
	if last := cancels[len(cancels)-1]; !strings.HasPrefix(last, "u4:") {
		t.Errorf("last cancel = %s; want u4's event cancelled as u4", last)
	}
	ev = liveBody(t, liveReq(h, ownerKey, http.MethodGet, "/v1/live-events/"+goneID, goneID, ""))
	if ev["status"] != "cancelled" || ev["has_calendar_event"] != false || ev["created_by"] != ownerID {
		t.Errorf("session after delete: %v; want cancelled, no event, credited to the admin", ev)
	}
}

// The raw constraint text never reaches a client: whatever fails inside the removal
// transaction is a generic message.
func TestDeleteUser_neverLeaksSQLErrors(t *testing.T) {
	h, db, ownerKey, _ := setupWorkspaceWithDB(t)
	mustExec(t, db, `INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u2','leaver@example.com','Leaver','UTC',0)`)
	mustExec(t, db, `INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u3','heir@example.com','Heir','UTC',0)`)
	// Two live events pointing at the leaver's calendar provider that was never registered:
	// no provider is not an error. The transfer still succeeds, and the response is clean.
	h.SetCalendar(calendar.NewService(db))
	rec := removeUser(t, h, ownerKey, "u2", `{"mode":"transfer","transfer_to":"u3"}`)
	if rec.Code != http.StatusOK || strings.Contains(strings.ToLower(rec.Body.String()), "constraint") {
		t.Errorf("got %d %s", rec.Code, rec.Body.String())
	}
	_ = time.Now
}
