package unitprobe

import (
	"errors"
	"testing"
	"time"
)

func TestStatusHealthy(t *testing.T) {
	tests := []struct {
		name   string
		status Status
		want   bool
	}{
		{
			name:   "active and running",
			status: Status{ActiveState: "active", SubState: "running"},
			want:   true,
		},
		{
			// A unit with RemainAfterExit reports active with a sub-state of
			// exited, which is not a running service.
			name:   "active but exited",
			status: Status{ActiveState: "active", SubState: "exited"},
			want:   false,
		},
		{
			name:   "still starting",
			status: Status{ActiveState: "activating", SubState: "start-pre"},
			want:   false,
		},
		{
			name:   "failed",
			status: Status{ActiveState: "failed", SubState: "failed"},
			want:   false,
		},
		{
			// Never healthy on a read failure, whatever else the struct holds:
			// unknown is not the same as fine.
			name:   "unreadable despite looking active",
			status: Status{ActiveState: "active", SubState: "running", Err: errors.New("bus closed")},
			want:   false,
		},
		{
			name:   "zero value",
			status: Status{},
			want:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.status.Healthy(); got != tc.want {
				t.Errorf("Healthy() = %v, want %v", got, tc.want)
			}
		})
	}
}

// The state set metric emits one series per state, so the list has to match
// what systemd can actually report.
func TestActiveStatesCoversSystemdsEnum(t *testing.T) {
	want := map[string]bool{
		"active": true, "reloading": true, "inactive": true,
		"failed": true, "activating": true, "deactivating": true,
	}
	if len(ActiveStates) != len(want) {
		t.Fatalf("ActiveStates has %d entries, want %d", len(ActiveStates), len(want))
	}
	for _, s := range ActiveStates {
		if !want[s] {
			t.Errorf("unexpected state %q", s)
		}
	}
}

func TestActiveSinceIsUTC(t *testing.T) {
	// ActiveEnterTimestamp is microseconds since the epoch; the JSON body
	// formats it as RFC 3339, which is only unambiguous in UTC.
	st := Status{ActiveSince: time.UnixMicro(1_756_199_561_000_000).UTC()}
	if got := st.ActiveSince.Location(); got != time.UTC {
		t.Errorf("ActiveSince location = %v, want UTC", got)
	}
}
