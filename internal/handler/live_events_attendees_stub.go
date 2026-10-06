package handler

import "context"

// DELETE THIS FILE when merging with the default-participants work (merge 7f11fe7,
// docs/features/default-participants.md), which defines the real
// (*Handler).defaultAttendeeEmails in default_attendees.go. The live-events branch was cut
// from a base that predates it, so this stub keeps the package compiling here; after the
// merge the build fails with "defaultAttendeeEmails redeclared", and removing this file is
// the whole fix - live_events.go then invites the workspace's configured default
// participants with no further change.
//
// defaultAttendeeEmails returns the workspace's default participants. Stub: none.
func (h *Handler) defaultAttendeeEmails(context.Context) ([]string, error) {
	return nil, nil
}
