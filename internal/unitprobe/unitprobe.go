// Package unitprobe reads systemd unit state over D-Bus.
package unitprobe

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"
)

// ActiveState values systemd can report for a unit. Exported so callers can
// emit a metric series per state rather than only the current one: a query for
// "active" that goes absent during an outage is a query that cannot alert.
var ActiveStates = []string{
	"active",
	"reloading",
	"inactive",
	"failed",
	"activating",
	"deactivating",
}

// Status is a point-in-time reading of one unit.
type Status struct {
	Name        string
	ActiveState string
	SubState    string
	// ActiveSince is the zero time when the unit has never been active.
	ActiveSince time.Time
	// Err is set when the unit could not be read at all, which is a different
	// condition from a unit that is legitimately inactive.
	Err error
}

// Healthy reports whether the unit is up and running. Both halves matter:
// ActiveState alone is "active" for a unit whose main process has exited when
// RemainAfterExit is set, and SubState is what distinguishes running from
// start-pre, reload, and the rest.
func (s Status) Healthy() bool {
	return s.Err == nil && s.ActiveState == "active" && s.SubState == "running"
}

// Prober reads the state of the named units.
type Prober interface {
	Probe(ctx context.Context, units []string) []Status
	Close() error
}

// dbusProber holds one system-bus connection for the process lifetime and
// reconnects when it breaks.
type dbusProber struct {
	mu   sync.Mutex
	conn *dbus.Conn
}

// New returns a Prober backed by the D-Bus system bus.
//
// The connection is established lazily, so the service starts and serves 503s
// with a per-unit error rather than refusing to boot when D-Bus is briefly
// unavailable.
func New() Prober { return &dbusProber{} }

// connect returns a usable connection, dialing one if needed.
//
// NewSystemConnectionContext talks to the shared /run/dbus/system_bus_socket.
// The alternative, NewSystemdConnectionContext, uses /run/systemd/private and
// requires root; reading unit properties over the system bus needs no
// privileges at all, since only start/stop go through polkit.
func (p *dbusProber) connect(ctx context.Context) (*dbus.Conn, error) {
	if p.conn != nil && p.conn.Connected() {
		return p.conn, nil
	}
	if p.conn != nil {
		p.conn.Close()
		p.conn = nil
	}
	conn, err := dbus.NewSystemConnectionContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect to the D-Bus system bus: %w", err)
	}
	p.conn = conn
	return conn, nil
}

// Probe reads every named unit. Errors are reported per unit rather than
// returned, so one unreadable unit does not hide the state of the others.
func (p *dbusProber) Probe(ctx context.Context, units []string) []Status {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([]Status, len(units))

	conn, err := p.connect(ctx)
	if err != nil {
		for i, name := range units {
			out[i] = Status{Name: name, Err: err}
		}
		return out
	}

	for i, name := range units {
		out[i] = p.probeOne(ctx, conn, name)
	}
	return out
}

func (p *dbusProber) probeOne(ctx context.Context, conn *dbus.Conn, name string) Status {
	st := Status{Name: name}

	props, err := conn.GetUnitPropertiesContext(ctx, name)
	if err != nil {
		// A broken connection must not persist: without this, a dbus.service
		// restart leaves every probe failing until this process is restarted,
		// which reads as "everything is down" while everything is fine.
		if p.conn != nil {
			p.conn.Close()
			p.conn = nil
		}
		st.Err = fmt.Errorf("read properties of %s: %w", name, err)
		return st
	}

	st.ActiveState, _ = props["ActiveState"].(string)
	st.SubState, _ = props["SubState"].(string)

	// ActiveEnterTimestamp is microseconds since the epoch, and 0 for a unit
	// that has never been active.
	if usec, ok := props["ActiveEnterTimestamp"].(uint64); ok && usec > 0 {
		st.ActiveSince = time.UnixMicro(int64(usec)).UTC()
	}

	if st.ActiveState == "" {
		st.Err = fmt.Errorf("%s reported no ActiveState; is the unit name correct?", name)
	}
	return st
}

// Close releases the D-Bus connection.
func (p *dbusProber) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conn != nil {
		p.conn.Close()
		p.conn = nil
	}
	return nil
}
