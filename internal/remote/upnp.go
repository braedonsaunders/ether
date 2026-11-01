package remote

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/egress"
)

// UPnP IGD service types, best first.
const (
	ServiceWANIPConnection2  = "urn:schemas-upnp-org:service:WANIPConnection:2"
	ServiceWANIPConnection1  = "urn:schemas-upnp-org:service:WANIPConnection:1"
	ServiceWANPPPConnection1 = "urn:schemas-upnp-org:service:WANPPPConnection:1"
)

var preferredServices = []string{ServiceWANIPConnection2, ServiceWANIPConnection1, ServiceWANPPPConnection1}

var ssdpSearchTargets = []string{
	"urn:schemas-upnp-org:device:InternetGatewayDevice:2",
	"urn:schemas-upnp-org:device:InternetGatewayDevice:1",
	ServiceWANIPConnection1,
	ServiceWANPPPConnection1,
}

// SSDPMulticast is the standard SSDP group.
const SSDPMulticast = "239.255.255.250:1900"

// IGD is a discovered UPnP Internet Gateway Device connection service.
type IGD struct {
	Location     string // device description URL
	FriendlyName string
	ServiceType  string
	ControlURL   string
	// LocalIP is this host's address on the interface that reaches the
	// gateway: the internal client for port mappings.
	LocalIP net.IP
	Client  *http.Client
}

// UPnPError is a SOAP fault from the gateway.
type UPnPError struct {
	Code        int
	Description string
	HTTPStatus  int
}

func (e *UPnPError) Error() string {
	return fmt.Sprintf("upnp error %d: %s (http %d)", e.Code, e.Description, e.HTTPStatus)
}

// Well-known UPnP error codes.
const (
	UPnPErrConflictInMappingEntry       = 718
	UPnPErrOnlyPermanentLeasesSupported = 725
	UPnPErrExternalPortOnlySupportsWild = 727
)

// DiscoverOptions tunes SSDP discovery.
type DiscoverOptions struct {
	// Target is where M-SEARCH is sent; SSDPMulticast when empty. Tests
	// point it at a loopback responder.
	Target string
	// Wait bounds how long responses are collected (default 3s).
	Wait   time.Duration
	Client *http.Client
}

// ErrNoIGD is returned when no usable gateway answered.
var ErrNoIGD = errors.New("remote: no UPnP internet gateway found")

// DiscoverIGD finds a UPnP gateway with a WAN connection service.
func DiscoverIGD(ctx context.Context, o DiscoverOptions) (*IGD, error) {
	if o.Target == "" {
		o.Target = SSDPMulticast
	}
	if o.Wait <= 0 {
		o.Wait = 3 * time.Second
	}
	if o.Client == nil {
		o.Client = egress.Client(egress.CapDirect, 5*time.Second)
	}
	locations, err := ssdpSearch(ctx, o.Target, o.Wait)
	if err != nil {
		return nil, err
	}
	var errs []error
	for _, loc := range locations {
		igd, err := LoadIGD(ctx, o.Client, loc)
		if err == nil {
			return igd, nil
		}
		errs = append(errs, err)
	}
	return nil, errors.Join(append([]error{ErrNoIGD}, errs...)...)
}

func ssdpSearch(ctx context.Context, target string, wait time.Duration) ([]string, error) {
	raddr, err := net.ResolveUDPAddr("udp4", target)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	deadline := time.Now().Add(wait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()

	for _, st := range ssdpSearchTargets {
		msg := "M-SEARCH * HTTP/1.1\r\n" +
			"HOST: " + SSDPMulticast + "\r\n" +
			"ST: " + st + "\r\n" +
			"MAN: \"ssdp:discover\"\r\n" +
			"MX: 2\r\n\r\n"
		if _, err := conn.WriteToUDP([]byte(msg), raddr); err != nil {
			return nil, fmt.Errorf("ssdp send: %w", err)
		}
	}
	var locs []string
	seen := map[string]bool{}
	buf := make([]byte, 2048)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				break
			}
			return nil, err
		}
		resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(buf[:n])), nil)
		if err != nil {
			continue
		}
		resp.Body.Close()
		loc := resp.Header.Get("Location")
		if resp.StatusCode != http.StatusOK || loc == "" || seen[loc] {
			continue
		}
		seen[loc] = true
		locs = append(locs, loc)
	}
	if len(locs) == 0 {
		return nil, ErrNoIGD
	}
	return locs, nil
}

type upnpRoot struct {
	URLBase string     `xml:"URLBase"`
	Device  upnpDevice `xml:"device"`
}

type upnpDevice struct {
	DeviceType   string        `xml:"deviceType"`
	FriendlyName string        `xml:"friendlyName"`
	Services     []upnpService `xml:"serviceList>service"`
	Devices      []upnpDevice  `xml:"deviceList>device"`
}

type upnpService struct {
	ServiceType string `xml:"serviceType"`
	ControlURL  string `xml:"controlURL"`
}

// LoadIGD fetches a device description and picks its best WAN connection
// service.
func LoadIGD(ctx context.Context, client *http.Client, location string) (*IGD, error) {
	if client == nil {
		client = egress.Client(egress.CapDirect, 5*time.Second)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upnp: %s: status %d", location, resp.StatusCode)
	}
	var root upnpRoot
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&root); err != nil {
		return nil, fmt.Errorf("upnp: %s: %w", location, err)
	}
	var found []struct {
		svc  upnpService
		name string
	}
	var walk func(d upnpDevice)
	walk = func(d upnpDevice) {
		for _, s := range d.Services {
			found = append(found, struct {
				svc  upnpService
				name string
			}{s, d.FriendlyName})
		}
		for _, c := range d.Devices {
			walk(c)
		}
	}
	walk(root.Device)
	base, err := url.Parse(location)
	if err != nil {
		return nil, err
	}
	if root.URLBase != "" {
		if b, err := url.Parse(strings.TrimSpace(root.URLBase)); err == nil {
			base = b
		}
	}
	for _, want := range preferredServices {
		for _, f := range found {
			if strings.TrimSpace(f.svc.ServiceType) != want {
				continue
			}
			ctl, err := base.Parse(strings.TrimSpace(f.svc.ControlURL))
			if err != nil {
				continue
			}
			name := root.Device.FriendlyName
			if name == "" {
				name = f.name
			}
			return &IGD{
				Location:     location,
				FriendlyName: name,
				ServiceType:  want,
				ControlURL:   ctl.String(),
				LocalIP:      localIPFor(ctl.Hostname()),
				Client:       client,
			}, nil
		}
	}
	return nil, fmt.Errorf("upnp: %s: no WAN connection service", location)
}

// localIPFor returns the local address the OS would use to reach host.
func localIPFor(host string) net.IP {
	port := "1900"
	c, err := net.Dial("udp4", net.JoinHostPort(host, port))
	if err != nil {
		return nil
	}
	defer c.Close()
	if a, ok := c.LocalAddr().(*net.UDPAddr); ok {
		if v4 := a.IP.To4(); v4 != nil {
			return v4
		}
		return a.IP
	}
	return nil
}

type soapArg struct{ name, value string }

// soap calls action on the gateway and returns the response's out-args.
func (g *IGD) soap(ctx context.Context, action string, args []soapArg) (map[string]string, error) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?>` + "\r\n")
	b.WriteString(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	fmt.Fprintf(&b, `<u:%s xmlns:u="%s">`, action, g.ServiceType)
	for _, a := range args {
		fmt.Fprintf(&b, "<%s>", a.name)
		_ = xml.EscapeText(&b, []byte(a.value))
		fmt.Fprintf(&b, "</%s>", a.name)
	}
	fmt.Fprintf(&b, `</u:%s></s:Body></s:Envelope>`, action)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.ControlURL, strings.NewReader(b.String()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"`+g.ServiceType+"#"+action+`"`)
	client := g.Client
	if client == nil {
		client = egress.Client(egress.CapDirect, 5*time.Second)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upnp %s: %w", action, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		if fe := parseSOAPFault(body); fe != nil {
			fe.HTTPStatus = resp.StatusCode
			return nil, fe
		}
		return nil, fmt.Errorf("upnp %s: http %d", action, resp.StatusCode)
	}
	return parseSOAPOut(body, action+"Response")
}

func parseSOAPFault(body []byte) *UPnPError {
	var e struct {
		Code int    `xml:"Body>Fault>detail>UPnPError>errorCode"`
		Desc string `xml:"Body>Fault>detail>UPnPError>errorDescription"`
	}
	if xml.Unmarshal(body, &e) != nil || e.Code == 0 {
		return nil
	}
	return &UPnPError{Code: e.Code, Description: e.Desc}
}

// parseSOAPOut collects the leaf elements of the <respName> element.
func parseSOAPOut(body []byte, respName string) (map[string]string, error) {
	dec := xml.NewDecoder(bytes.NewReader(body))
	out := map[string]string{}
	inResp := false
	var cur string
	var text strings.Builder
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("upnp: bad soap response: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == respName {
				inResp = true
			} else if inResp {
				cur = t.Name.Local
				text.Reset()
			}
		case xml.CharData:
			if cur != "" {
				text.Write(t)
			}
		case xml.EndElement:
			if t.Name.Local == respName {
				return out, nil
			}
			if inResp && t.Name.Local == cur {
				out[cur] = strings.TrimSpace(text.String())
				cur = ""
			}
		}
	}
	if !inResp {
		return nil, fmt.Errorf("upnp: soap response without %s", respName)
	}
	return out, nil
}

// ExternalIP asks the gateway for its WAN address.
func (g *IGD) ExternalIP(ctx context.Context) (net.IP, error) {
	out, err := g.soap(ctx, "GetExternalIPAddress", nil)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(out["NewExternalIPAddress"])
	if ip == nil {
		return nil, fmt.Errorf("upnp: gateway returned no external IP")
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip, nil
}

// AddPortMapping maps external TCP/UDP port to internalClient:internalPort
// for lease (0 = permanent).
func (g *IGD) AddPortMapping(ctx context.Context, protocol string, external, internal int, internalClient net.IP, description string, lease time.Duration) error {
	_, err := g.soap(ctx, "AddPortMapping", []soapArg{
		{"NewRemoteHost", ""},
		{"NewExternalPort", strconv.Itoa(external)},
		{"NewProtocol", strings.ToUpper(protocol)},
		{"NewInternalPort", strconv.Itoa(internal)},
		{"NewInternalClient", internalClient.String()},
		{"NewEnabled", "1"},
		{"NewPortMappingDescription", description},
		{"NewLeaseDuration", strconv.Itoa(int(lease / time.Second))},
	})
	return err
}

// DeletePortMapping removes a mapping.
func (g *IGD) DeletePortMapping(ctx context.Context, protocol string, external int) error {
	_, err := g.soap(ctx, "DeletePortMapping", []soapArg{
		{"NewRemoteHost", ""},
		{"NewExternalPort", strconv.Itoa(external)},
		{"NewProtocol", strings.ToUpper(protocol)},
	})
	return err
}

// PortMappingEntry is an existing mapping on the gateway.
type PortMappingEntry struct {
	InternalPort   int
	InternalClient string
	Enabled        bool
	Description    string
	Lease          time.Duration
}

// GetSpecificPortMapping looks up the mapping for an external port.
func (g *IGD) GetSpecificPortMapping(ctx context.Context, protocol string, external int) (*PortMappingEntry, error) {
	out, err := g.soap(ctx, "GetSpecificPortMappingEntry", []soapArg{
		{"NewRemoteHost", ""},
		{"NewExternalPort", strconv.Itoa(external)},
		{"NewProtocol", strings.ToUpper(protocol)},
	})
	if err != nil {
		return nil, err
	}
	ip, _ := strconv.Atoi(out["NewInternalPort"])
	lease, _ := strconv.Atoi(out["NewLeaseDuration"])
	return &PortMappingEntry{
		InternalPort:   ip,
		InternalClient: out["NewInternalClient"],
		Enabled:        out["NewEnabled"] == "1" || strings.EqualFold(out["NewEnabled"], "true"),
		Description:    out["NewPortMappingDescription"],
		Lease:          time.Duration(lease) * time.Second,
	}, nil
}
