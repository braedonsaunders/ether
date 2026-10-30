package dlna

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
)

type viewerKey struct{}

func (i *instance) serverName() string {
	if i.host.Config != nil {
		if name, ok := i.host.Config()["serverName"].(string); ok && strings.TrimSpace(name) != "" {
			return name
		}
	}
	return "Ether"
}
func (i *instance) uuid() string {
	return fmt.Sprintf("%s-%s-%s-%s-%s", i.id[:8], i.id[8:12], i.id[12:16], i.id[16:20], i.id[20:])
}

func writeXML(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/xml; charset=\"utf-8\"")
	_, _ = io.WriteString(w, xml.Header+body)
}
func escape(value string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(value))
	return b.String()
}
func (i *instance) description(w http.ResponseWriter, _ *http.Request) {
	writeXML(w, `<root xmlns="urn:schemas-upnp-org:device-1-0"><specVersion><major>1</major><minor>0</minor></specVersion><device><deviceType>urn:schemas-upnp-org:device:MediaServer:1</deviceType><friendlyName>`+escape(i.serverName())+`</friendlyName><manufacturer>Ether</manufacturer><modelName>Ether</modelName><UDN>uuid:`+i.uuid()+`</UDN><serviceList><service><serviceType>urn:schemas-upnp-org:service:ContentDirectory:1</serviceType><serviceId>urn:upnp-org:serviceId:ContentDirectory</serviceId><SCPDURL>/dlna/ContentDirectory/description.xml</SCPDURL><controlURL>/dlna/ContentDirectory/control</controlURL><eventSubURL>/dlna/ContentDirectory/events</eventSubURL></service><service><serviceType>urn:schemas-upnp-org:service:ConnectionManager:1</serviceType><serviceId>urn:upnp-org:serviceId:ConnectionManager</serviceId><SCPDURL>/dlna/ConnectionManager/description.xml</SCPDURL><controlURL>/dlna/ConnectionManager/control</controlURL><eventSubURL>/dlna/ConnectionManager/events</eventSubURL></service></serviceList></device></root>`)
}
func (i *instance) contentDescription(w http.ResponseWriter, _ *http.Request) {
	writeService(w, contentService())
}
func (i *instance) connectionDescription(w http.ResponseWriter, _ *http.Request) {
	writeService(w, connectionService())
}

func action(r *http.Request) string {
	value := strings.Trim(r.Header.Get("SOAPAction"), `" `)
	_, name, ok := strings.Cut(value, "#")
	if !ok {
		return ""
	}
	return name
}
func soap(w http.ResponseWriter, service, name, body string) {
	writeXML(w, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body><u:`+name+`Response xmlns:u="urn:schemas-upnp-org:service:`+service+`:1">`+body+`</u:`+name+`Response></s:Body></s:Envelope>`)
}
func soapError(w http.ResponseWriter, code int) {
	w.Header().Set("Content-Type", "text/xml; charset=\"utf-8\"")
	w.WriteHeader(500)
	description := map[int]string{401: "Invalid Action", 402: "Invalid Args", 501: "Action Failed", 701: "No Such Object", 709: "Unsupported or Invalid Sort Criteria"}[code]
	_, _ = fmt.Fprintf(w, `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><s:Fault><faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring><detail><UPnPError xmlns="urn:schemas-upnp-org:control-1-0"><errorCode>%d</errorCode><errorDescription>%s</errorDescription></UPnPError></detail></s:Fault></s:Body></s:Envelope>`, code, description)
}
func (i *instance) connectionControl(w http.ResponseWriter, r *http.Request) {
	switch action(r) {
	case "GetProtocolInfo":
		soap(w, "ConnectionManager", "GetProtocolInfo", `<Source>http-get:*:video/x-matroska:*,http-get:*:video/mp4:*</Source><Sink></Sink>`)
	case "GetCurrentConnectionIDs":
		soap(w, "ConnectionManager", "GetCurrentConnectionIDs", `<ConnectionIDs>0</ConnectionIDs>`)
	case "GetCurrentConnectionInfo":
		soap(w, "ConnectionManager", "GetCurrentConnectionInfo", `<RcsID>-1</RcsID><AVTransportID>-1</AVTransportID><ProtocolInfo></ProtocolInfo><PeerConnectionManager></PeerConnectionManager><PeerConnectionID>-1</PeerConnectionID><Direction>Output</Direction><Status>Unknown</Status>`)
	default:
		soapError(w, 401)
	}
}

type browseArgs struct {
	ObjectID       string `xml:"ObjectID"`
	ContainerID    string `xml:"ContainerID"`
	BrowseFlag     string `xml:"BrowseFlag"`
	SearchCriteria string `xml:"SearchCriteria"`
	SortCriteria   string `xml:"SortCriteria"`
	StartingIndex  int    `xml:"StartingIndex"`
	RequestedCount int    `xml:"RequestedCount"`
}

func parseArgs(r *http.Request, expected string) (browseArgs, error) {
	var args browseArgs
	decoder := xml.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return args, err
		}
		if start, ok := token.(xml.StartElement); ok && start.Name.Local == expected {
			if err = decoder.DecodeElement(&args, &start); err != nil {
				return args, err
			}
			return args, nil
		}
	}
	return args, fmt.Errorf("missing action")
}

type didlObject struct {
	XMLName    xml.Name
	ID         string        `xml:"id,attr"`
	ParentID   string        `xml:"parentID,attr"`
	Restricted string        `xml:"restricted,attr"`
	Title      string        `xml:"dc:title"`
	Class      string        `xml:"upnp:class"`
	Resource   *didlResource `xml:"res,omitempty"`
}
type didlResource struct {
	ProtocolInfo string `xml:"protocolInfo,attr"`
	URL          string `xml:",chardata"`
}
type didl struct {
	XMLName xml.Name     `xml:"DIDL-Lite"`
	XMLNS   string       `xml:"xmlns,attr"`
	DC      string       `xml:"xmlns:dc,attr"`
	UPNP    string       `xml:"xmlns:upnp,attr"`
	Objects []didlObject `xml:",any"`
}

func (i *instance) contentControl(w http.ResponseWriter, r *http.Request) {
	name := action(r)
	switch name {
	case "GetSearchCapabilities":
		soap(w, "ContentDirectory", name, `<SearchCaps>dc:title</SearchCaps>`)
		return
	case "GetSortCapabilities":
		soap(w, "ContentDirectory", name, `<SortCaps></SortCaps>`)
		return
	case "GetSystemUpdateID":
		soap(w, "ContentDirectory", name, `<Id>0</Id>`)
		return
	case "Browse", "Search":
	default:
		soapError(w, 401)
		return
	}
	args, err := parseArgs(r, name)
	if err != nil || args.StartingIndex < 0 || args.RequestedCount < 0 {
		soapError(w, 402)
		return
	}
	if strings.TrimSpace(args.SortCriteria) != "" {
		soapError(w, 709)
		return
	}
	if args.RequestedCount == 0 || args.RequestedCount > 200 {
		args.RequestedCount = 200
	}
	viewer, _ := r.Context().Value(viewerKey{}).(*frontend.Viewer)
	if i.host.ClientIP == nil {
		soapError(w, 501)
		return
	}
	peer, ok := i.host.ClientIP(r)
	if !ok {
		soapError(w, 501)
		return
	}
	device := frontend.LANDevice(peer)
	id := args.ObjectID
	if name == "Search" {
		id = args.ContainerID
	}
	objects, total, err := i.browse(r, viewer, device, name, id, args)
	if err != nil {
		soapError(w, 701)
		return
	}
	i.addResources(r, viewer, device, peer, objects)
	document := didl{XMLNS: "urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/", DC: "http://purl.org/dc/elements/1.1/", UPNP: "urn:schemas-upnp-org:metadata-1-0/upnp/", Objects: objects}
	encoded, err := xml.Marshal(document)
	if err != nil {
		soapError(w, 501)
		return
	}
	body := `<Result>` + escape(string(encoded)) + `</Result><NumberReturned>` + strconv.Itoa(len(objects)) + `</NumberReturned><TotalMatches>` + strconv.Itoa(total) + `</TotalMatches><UpdateID>0</UpdateID>`
	soap(w, "ContentDirectory", name, body)
}

func (i *instance) addResources(r *http.Request, viewer *frontend.Viewer, device frontend.Device, peer netip.Addr, objects []didlObject) {
	if i.host.LANStreams == nil || i.host.Capabilities == nil || i.host.SurfaceURL == nil {
		return
	}
	ctx := frontend.WithClientIP(r.Context(), peer)
	profile, err := i.host.Capabilities.Build(ctx, frontend.CapabilityRequest{Viewer: viewer, Device: caps.DeviceInfo{Protocol: "dlna", ClientID: device.ID, Product: device.Product}, UserAgent: r.UserAgent()})
	if err != nil {
		return
	}
	base, err := i.host.SurfaceURL(ctx, r)
	if err != nil {
		return
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return
	}
	for n := range objects {
		if objects[n].XMLName.Local != "item" {
			continue
		}
		key, err := objectKey(objects[n].ID)
		if err != nil {
			continue
		}
		path, err := i.host.LANStreams.IssueLAN(ctx, r, frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "dlna", Item: key, Caps: profile.Client, Intent: frontend.PlayIntent{Options: frontend.DecideOptions{DirectPlay: true}}})
		if err != nil || !validLANCapability(path) {
			continue
		}
		itemURL := *u
		itemURL.Path = strings.TrimRight(u.Path, "/") + path
		itemURL.RawPath = ""
		objects[n].Resource = &didlResource{ProtocolInfo: "http-get:*:application/octet-stream:*", URL: itemURL.String()}
	}
}

func validLANCapability(path string) bool {
	rest, ok := strings.CutPrefix(path, "/sessions/lazy/")
	if !ok {
		return false
	}
	token, ok := strings.CutSuffix(rest, "/file")
	if !ok || token == "" || len(token) > 128 {
		return false
	}
	for _, r := range token {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}
