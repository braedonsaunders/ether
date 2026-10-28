package remote

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"sync"
	"time"
)

// Port mapping methods.
const (
	MethodUPnP   = "upnp"
	MethodNATPMP = "nat-pmp"
)

type portMappingConflict struct {
	external int
	client   string
	internal int
}

func (e *portMappingConflict) Error() string {
	return fmt.Sprintf("upnp: external port %d already mapped to %s:%d; give each service its own public port", e.external, e.client, e.internal)
}

// MappingStatus is the port mapper's state for the admin dashboard.
type MappingStatus struct {
	Active       bool      `json:"active"`
	Method       string    `json:"method,omitempty"`  // "upnp" or "nat-pmp"
	Gateway      string    `json:"gateway,omitempty"` // IGD friendly name or NAT-PMP address
	ExternalIP   string    `json:"externalIp,omitempty"`
	ExternalPort int       `json:"externalPort,omitempty"`
	InternalIP   string    `json:"internalIp,omitempty"`
	InternalPort int       `json:"internalPort,omitempty"`
	Lease        string    `json:"lease,omitempty"` // "permanent" or a duration
	RenewAt      time.Time `json:"renewAt,omitempty"`
	LastError    string    `json:"lastError,omitempty"`
	UpdatedAt    time.Time `json:"updatedAt,omitempty"`
}

// PortMapperOptions configures a PortMapper.
type PortMapperOptions struct {
	// Description appears in the router's mapping table.
	Description string
	// Lease requested from the gateway (default 2h). Renewed at half-life.
	Lease time.Duration
	// DiscoverIGD finds a UPnP gateway; nil uses SSDP multicast discovery.
	DiscoverIGD func(ctx context.Context) (*IGD, error)
	// Gateway returns the NAT-PMP gateway; nil uses DefaultGateway. Return
	// an error to disable NAT-PMP.
	Gateway func() (net.IP, error)
	// NATPMPPort overrides the NAT-PMP port (tests).
	NATPMPPort int
	Logger     *slog.Logger
	Now        func() time.Time
}

// PortMapper keeps one TCP port mapping alive on the LAN gateway, via UPnP
// IGD first and NAT-PMP second. Ensure is idempotent and cheap to call on
// an interval; Close removes the mapping.
type PortMapper struct {
	o PortMapperOptions

	opMu   sync.Mutex // serialises gateway operations
	status MappingStatus

	pubMu    sync.Mutex
	pub      MappingStatus // snapshot for Status, never blocked by network I/O
	igd      *IGD
	natpmp   *NATPMP
	internal int
	external int
	renewAt  time.Time
	backoff  time.Time
}

// NewPortMapper returns a mapper; nothing happens until Ensure.
func NewPortMapper(o PortMapperOptions) *PortMapper {
	if o.Description == "" {
		o.Description = "Ether"
	}
	if o.Lease <= 0 {
		o.Lease = 2 * time.Hour
	}
	if o.DiscoverIGD == nil {
		o.DiscoverIGD = func(ctx context.Context) (*IGD, error) { return DiscoverIGD(ctx, DiscoverOptions{}) }
	}
	if o.Gateway == nil {
		o.Gateway = DefaultGateway
	}
	if o.NATPMPPort == 0 {
		o.NATPMPPort = NATPMPPort
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &PortMapper{o: o}
}

// Status returns a snapshot of the mapping state.
func (m *PortMapper) Status() MappingStatus {
	m.pubMu.Lock()
	defer m.pubMu.Unlock()
	return m.pub
}

func (m *PortMapper) publishLocked() {
	m.pubMu.Lock()
	m.pub = m.status
	m.pubMu.Unlock()
}

// Ensure makes sure external TCP port maps to this host's internal port,
// creating or renewing the mapping when needed. It returns the current
// status. After a failure it waits a minute before trying again.
func (m *PortMapper) Ensure(ctx context.Context, internal, external int) (MappingStatus, error) {
	if external == 0 {
		external = internal
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	defer m.publishLocked()
	now := m.o.Now()
	changed := internal != m.internal || external != m.external
	if changed && m.status.Active {
		m.deleteLocked(ctx)
	}
	if !changed && m.status.Active && now.Before(m.renewAt) {
		return m.status, nil
	}
	if !changed && !m.status.Active && now.Before(m.backoff) {
		return m.status, errors.New(m.status.LastError)
	}
	m.internal, m.external = internal, external

	err := m.mapLocked(ctx, now)
	if err != nil {
		m.status = MappingStatus{LastError: err.Error(), UpdatedAt: now, InternalPort: internal, ExternalPort: external}
		m.backoff = now.Add(time.Minute)
		m.igd, m.natpmp = nil, nil
		return m.status, err
	}
	return m.status, nil
}

func (m *PortMapper) mapLocked(ctx context.Context, now time.Time) error {
	var errs []error
	var conflict *portMappingConflict
	// Reuse a known gateway first; rediscover if it stopped answering.
	if m.igd != nil {
		if err := m.mapUPnP(ctx, m.igd, now); err == nil {
			return nil
		} else {
			if errors.As(err, &conflict) {
				return err
			}
			errs = append(errs, err)
			m.igd = nil
		}
	}
	if m.natpmp != nil {
		if err := m.mapNATPMP(ctx, m.natpmp, now); err == nil {
			return nil
		} else {
			errs = append(errs, err)
			m.natpmp = nil
		}
	}
	dctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	igd, err := m.o.DiscoverIGD(dctx)
	cancel()
	if err == nil {
		if err := m.mapUPnP(ctx, igd, now); err == nil {
			m.igd = igd
			return nil
		} else {
			if errors.As(err, &conflict) {
				return err
			}
			errs = append(errs, err)
		}
	} else {
		errs = append(errs, fmt.Errorf("upnp: %w", err))
	}
	if gw, err := m.o.Gateway(); err == nil {
		c := &NATPMP{Gateway: net.JoinHostPort(gw.String(), fmt.Sprint(m.o.NATPMPPort))}
		if err := m.mapNATPMP(ctx, c, now); err == nil {
			m.natpmp = c
			return nil
		} else {
			errs = append(errs, err)
		}
	} else {
		errs = append(errs, fmt.Errorf("nat-pmp: %w", err))
	}
	return errors.Join(errs...)
}

func (m *PortMapper) mapUPnP(ctx context.Context, igd *IGD, now time.Time) error {
	// Re-resolve this host's address towards the gateway on every
	// (re)mapping: a cached gateway keeps the address found at discovery,
	// and after a DHCP change renewing with it would keep forwarding the
	// port to the old address while reporting success.
	staleIP := igd.LocalIP
	if u, err := url.Parse(igd.ControlURL); err == nil && u.Hostname() != "" {
		if ip := localIPFor(u.Hostname()); ip != nil {
			igd.LocalIP = ip
		}
	}
	if staleIP.Equal(igd.LocalIP) {
		staleIP = nil
	}
	if igd.LocalIP == nil {
		return errors.New("upnp: cannot determine local address towards gateway")
	}
	owned := func(e *PortMappingEntry) bool {
		return e.InternalPort == m.internal &&
			(net.ParseIP(e.InternalClient).Equal(igd.LocalIP) ||
				(staleIP != nil && net.ParseIP(e.InternalClient).Equal(staleIP)))
	}
	// Some gateways silently replace a mapping for the same LAN host even
	// when it points at another service. Inspect before adding, not just
	// after a conflict response, and do not bypass a known conflict via NAT-PMP.
	if e, err := igd.GetSpecificPortMapping(ctx, "TCP", m.external); err == nil && !owned(e) {
		return &portMappingConflict{m.external, e.InternalClient, e.InternalPort}
	}
	lease := m.o.Lease
	err := igd.AddPortMapping(ctx, "TCP", m.external, m.internal, igd.LocalIP, m.o.Description, lease)
	var ue *UPnPError
	if errors.As(err, &ue) && ue.Code == UPnPErrOnlyPermanentLeasesSupported {
		lease = 0
		err = igd.AddPortMapping(ctx, "TCP", m.external, m.internal, igd.LocalIP, m.o.Description, 0)
	}
	adopted := false
	if errors.As(err, &ue) && ue.Code == UPnPErrConflictInMappingEntry {
		// Another host (or a stale entry of ours) holds the port. If it is
		// ours, replace it; otherwise report the conflict.
		// Our own entry from before an address change counts as ours too.
		if e, gerr := igd.GetSpecificPortMapping(ctx, "TCP", m.external); gerr == nil && owned(e) {
			_ = igd.DeletePortMapping(ctx, "TCP", m.external)
			err = igd.AddPortMapping(ctx, "TCP", m.external, m.internal, igd.LocalIP, m.o.Description, lease)
		} else if gerr == nil {
			err = &portMappingConflict{m.external, e.InternalClient, e.InternalPort}
		}
	}
	if err != nil && errors.As(err, &ue) {
		// Some gateways refuse to add, replace or delete an entry that
		// already exists, even one of ours (left by an earlier run of this
		// node, or a crash). An entry that already forwards the external
		// port to this host and port is exactly what we want: adopt it.
		if e, gerr := igd.GetSpecificPortMapping(ctx, "TCP", m.external); gerr == nil &&
			net.ParseIP(e.InternalClient).Equal(igd.LocalIP) && e.InternalPort == m.internal {
			err, adopted, lease = nil, true, e.Lease
		}
	}
	if err != nil {
		return fmt.Errorf("upnp %s: %w", igd.FriendlyName, err)
	}
	st := MappingStatus{
		Active: true, Method: MethodUPnP, Gateway: igd.FriendlyName,
		ExternalPort: m.external, InternalIP: igd.LocalIP.String(), InternalPort: m.internal,
		UpdatedAt: now,
	}
	if ip, err := igd.ExternalIP(ctx); err == nil {
		st.ExternalIP = ip.String()
	}
	switch {
	case adopted:
		// Not ours to renew on the usual schedule: check it again soon.
		st.Lease = "existing"
		if lease > 0 {
			st.Lease = lease.String()
		}
		m.renewAt = now.Add(15 * time.Minute)
		if lease > 0 {
			m.renewAt = now.Add(min(15*time.Minute, max(lease/2, time.Minute)))
		}
	case lease == 0:
		st.Lease = "permanent"
		m.renewAt = now.Add(time.Hour) // re-assert periodically; routers reboot
	default:
		st.Lease = lease.String()
		m.renewAt = now.Add(lease / 2)
	}
	st.RenewAt = m.renewAt
	m.status = st
	if adopted {
		m.o.Logger.Info("port mapping already present on the gateway; using it", "gateway", igd.FriendlyName, "external", m.external, "internal", fmt.Sprintf("%s:%d", igd.LocalIP, m.internal))
		return nil
	}
	m.o.Logger.Info("port mapped", "method", MethodUPnP, "gateway", igd.FriendlyName, "external", m.external, "internal", fmt.Sprintf("%s:%d", igd.LocalIP, m.internal), "lease", st.Lease)
	return nil
}

func (m *PortMapper) mapNATPMP(ctx context.Context, c *NATPMP, now time.Time) error {
	ext, granted, err := c.Map(ctx, "tcp", m.internal, m.external, m.o.Lease)
	if err != nil {
		return fmt.Errorf("nat-pmp %s: %w", c.Gateway, err)
	}
	if granted <= 0 {
		granted = m.o.Lease
	}
	st := MappingStatus{
		Active: true, Method: MethodNATPMP, Gateway: c.Gateway,
		ExternalPort: ext, InternalPort: m.internal, Lease: granted.String(), UpdatedAt: now,
	}
	if host, _, err := net.SplitHostPort(c.Gateway); err == nil {
		if ip := localIPFor(host); ip != nil {
			st.InternalIP = ip.String()
		}
	}
	if ip, err := c.ExternalIP(ctx); err == nil {
		st.ExternalIP = ip.String()
	}
	m.renewAt = now.Add(granted / 2)
	st.RenewAt = m.renewAt
	m.status = st
	m.o.Logger.Info("port mapped", "method", MethodNATPMP, "gateway", c.Gateway, "external", ext, "internal", m.internal, "lease", granted)
	return nil
}

func (m *PortMapper) deleteLocked(ctx context.Context) {
	switch {
	case m.status.Method == MethodUPnP && m.igd != nil:
		entry, err := m.igd.GetSpecificPortMapping(ctx, "TCP", m.status.ExternalPort)
		if err != nil {
			m.o.Logger.Warn("upnp unmap skipped: cannot verify mapping ownership", "err", err)
		} else if entry.InternalPort != m.status.InternalPort || !net.ParseIP(entry.InternalClient).Equal(net.ParseIP(m.status.InternalIP)) {
			m.o.Logger.Warn("upnp unmap skipped: mapping now belongs to another service", "external", m.status.ExternalPort)
		} else if err := m.igd.DeletePortMapping(ctx, "TCP", m.status.ExternalPort); err != nil {
			m.o.Logger.Warn("upnp unmap failed", "err", err)
		}
	case m.status.Method == MethodNATPMP && m.natpmp != nil:
		if _, _, err := m.natpmp.Map(ctx, "tcp", m.internal, 0, 0); err != nil {
			m.o.Logger.Warn("nat-pmp unmap failed", "err", err)
		}
	}
	m.status = MappingStatus{UpdatedAt: m.o.Now()}
	m.renewAt = time.Time{}
}

// Close removes the mapping from the gateway (on shutdown or when remote
// access is turned off).
func (m *PortMapper) Close(ctx context.Context) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	defer m.publishLocked()
	if m.status.Active {
		m.deleteLocked(ctx)
	}
	m.internal, m.external = 0, 0
}
