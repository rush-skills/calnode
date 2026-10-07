package worker

import (
	"testing"
	"time"
)

// The webhook backoff spreads 8 attempts over most of a day: 5m, tripling, capped at 6h.
func TestWebhookBackoff(t *testing.T) {
	want := []time.Duration{5 * time.Minute, 15 * time.Minute, 45 * time.Minute, 135 * time.Minute, 6 * time.Hour, 6 * time.Hour, 6 * time.Hour}
	var total time.Duration
	for i, w := range want {
		if got := webhookBackoff(i + 1); got != w {
			t.Errorf("webhookBackoff(%d) = %v; want %v", i+1, got, w)
		}
		total += w
	}
	if total < 20*time.Hour {
		t.Errorf("retries span %v; want most of a day", total)
	}
}
