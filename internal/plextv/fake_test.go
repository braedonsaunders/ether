package plextv

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"encoding/xml"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePlexTV is an in-memory plex.tv covering the endpoints Ether uses.
type fakePlexTV struct {
	t   *testing.T
	srv *httptest.Server

	mu            sync.Mutex
	users         map[string]Account // token -> account
	pins          map[int64]*PIN
	nextPIN       int64
	claims        map[string]int64 // claim token -> account id
	exchangeErr   int              // status to fail claim exchange with
	machineID     string
	published     []string
	httpsRequired string
	putCount      int
	registered    []string // servers.xml bodies
	connectivity  int
	certHash      string
	csrStatus     int    // status to fail CSR upload with
	csrUploads    int    // CSR PUTs received
	subjectAttr   string // overrides the base64 DER "subject" attribute
	lastCSR       *x509.CertificateRequest
	pendingDL     int // 202s before the certificate is ready
	issued        []byte
	caKey         *ecdsa.PrivateKey
	caCert        *x509.Certificate
	certTTL       time.Duration
	accessXML     string
	accessFail    int // fail access_tokens.xml this many times with 500
	sharedXML     string
	usersXML      string
	userXML       string
	userJSON      string
	features      []Feature
	featuresErr   int
	requests      []string
	headers       map[string]http.Header // path -> last headers
	resources     map[string][]Resource  // token -> extra resources
	// deviceOwner is the account holding this server's device record (0 =
	// none). Like plex.tv, publishing for another account is refused.
	deviceOwner      int64
	modelAssociation bool
	associated       bool
	deviceDeletes    int
}

const fakeHash = "0a1b2c3d4e5f60718293a4b5c6d7e8f9"

func newFakePlexTV(t *testing.T, machineID string) *fakePlexTV {
	f := &fakePlexTV{
		t:         t,
		users:     map[string]Account{},
		pins:      map[int64]*PIN{},
		nextPIN:   1000,
		claims:    map[string]int64{},
		machineID: machineID,
		certHash:  fakeHash,
		certTTL:   90 * 24 * time.Hour,
		headers:   map[string]http.Header{},
		resources: map[string][]Resource{},
		features:  []Feature{},
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Fake Plex CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(10 * 365 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	f.caKey = key
	f.caCert, _ = x509.ParseCertificate(der)
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

// subjectDER is the X.509 Name plex.tv returns (base64) in the subject
// response; the structure, not the values, mirrors the real response.
func (f *fakePlexTV) subjectDER() []byte {
	der, err := asn1.Marshal(pkix.Name{Country: []string{"US"}, Organization: []string{"Example Org"},
		CommonName: "*." + f.certHash + ".plex.direct"}.ToRDNSequence())
	if err != nil {
		f.t.Fatal(err)
	}
	return der
}

func (f *fakePlexTV) addUser(token string, a Account) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[token] = a
}

func (f *fakePlexTV) approvePIN(id int64, token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pins[id].AuthToken = token
}

func (f *fakePlexTV) header(path string) http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.headers[path]
}

func (f *fakePlexTV) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func (f *fakePlexTV) jsonErr(w http.ResponseWriter, status, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"errors":[{"code":%d,"message":%q,"status":%d}]}`, code, msg, status)
}

func (f *fakePlexTV) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	f.headers[r.URL.Path] = r.Header.Clone()
	token := r.Header.Get("X-Plex-Token")
	if token == "" {
		token = r.URL.Query().Get("auth_token")
	}
	acct, authed := f.users[token]
	devPrefix := "/api/v2/devices/" + f.machineID + "/certificate/"

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v2/pins":
		if r.URL.Query().Get("strong") != "true" || r.Header.Get("X-Plex-Client-Identifier") == "" {
			f.jsonErr(w, 400, 1000, "bad pin request")
			return
		}
		f.nextPIN++
		p := &PIN{ID: f.nextPIN, Code: "strongcode" + strconv.FormatInt(f.nextPIN, 10),
			ClientIdentifier: r.Header.Get("X-Plex-Client-Identifier"), ExpiresIn: 1800}
		f.pins[p.ID] = p
		w.WriteHeader(201)
		fmt.Fprintf(w, `{"id":%d,"code":%q,"product":"Ether","trusted":false,"clientIdentifier":%q,"expiresIn":1800,"createdAt":"2026-10-18T10:00:00Z","expiresAt":%q,"authToken":null}`,
			p.ID, p.Code, p.ClientIdentifier, time.Now().Add(30*time.Minute).UTC().Format(time.RFC3339))
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v2/pins/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/api/v2/pins/"), 10, 64)
		p, ok := f.pins[id]
		if !ok || p.Code != r.URL.Query().Get("code") {
			f.jsonErr(w, 404, 1020, "Code not found or expired")
			return
		}
		if p.ClientIdentifier != r.Header.Get("X-Plex-Client-Identifier") {
			f.jsonErr(w, 400, 1000, "client identifier mismatch")
			return
		}
		tok := "null"
		if p.AuthToken != "" {
			tok = strconv.Quote(p.AuthToken)
		}
		fmt.Fprintf(w, `{"id":%d,"code":%q,"clientIdentifier":%q,"expiresIn":1700,"authToken":%s}`, p.ID, p.Code, p.ClientIdentifier, tok)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v2/features":
		if !authed {
			f.jsonErr(w, 401, 1001, "User could not be authenticated")
			return
		}
		if f.featuresErr != 0 {
			f.jsonErr(w, f.featuresErr, 1000, "Feature lookup unavailable")
			return
		}
		json.NewEncoder(w).Encode(f.features)
	case r.URL.Path == "/api/v2/user":
		if !authed {
			f.jsonErr(w, 401, 1001, "User could not be authenticated")
			return
		}
		if r.URL.Query().Get("includeSubscriptions") != "1" || r.URL.Query().Get("includeProviders") != "1" {
			f.jsonErr(w, 400, 1000, "missing user includes")
			return
		}
		if f.userXML != "" {
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, f.userXML)
			return
		}
		if f.userJSON != "" {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, f.userJSON)
			return
		}
		json.NewEncoder(w).Encode(acct)
	case r.URL.Path == "/api/claim/token.json":
		if !authed {
			f.jsonErr(w, 401, 1001, "User could not be authenticated")
			return
		}
		ct := "claim-" + strconv.FormatInt(acct.ID, 10)
		f.claims[ct] = acct.ID
		fmt.Fprintf(w, `{"token":%q}`, ct)
	case r.Method == http.MethodPost && r.URL.Path == "/api/claim/exchange":
		if f.exchangeErr != 0 {
			w.WriteHeader(f.exchangeErr)
			return
		}
		id, ok := f.claims[r.URL.Query().Get("token")]
		if !ok || r.Header.Get("X-Plex-Provides") != "server" || r.Header.Get("X-Plex-Client-Identifier") != f.machineID {
			w.WriteHeader(400)
			io.WriteString(w, `<errors><error>Invalid claim token</error></errors>`)
			return
		}
		delete(f.claims, r.URL.Query().Get("token"))
		var owner Account
		for _, a := range f.users {
			if a.ID == id {
				owner = a
			}
		}
		st := "server-token-" + strconv.FormatInt(id, 10)
		f.users[st] = owner
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<user email="%s" id="%d" uuid="%s" username="%s" title="%s">
  <subscription active="1" status="Active" plan="lifetime"/>
  <authentication-token>%s</authentication-token>
</user>`, owner.Email, owner.ID, owner.UUID, owner.Username, owner.Title, st)
	case r.Method == http.MethodPut && r.URL.Path == "/devices/"+f.machineID:
		if !authed || (f.deviceOwner != 0 && f.deviceOwner != acct.ID) {
			w.WriteHeader(401)
			io.WriteString(w, `<errors><error>Invalid authentication token.</error></errors>`)
			return
		}
		f.deviceOwner = acct.ID
		f.putCount++
		f.published = r.URL.Query()["Connection[][uri]"]
		f.httpsRequired = r.URL.Query().Get("httpsRequired")
		io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Device/>`)
	case r.Method == http.MethodGet && r.URL.Path == "/devices.xml":
		if !authed {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		io.WriteString(w, `<MediaContainer><Device id="77" name="Phone" clientIdentifier="phone-1" provides="client"/>`)
		if f.deviceOwner == acct.ID {
			fmt.Fprintf(w, `<Device id="1822812611" name="Ether" product="Plex Media Server" clientIdentifier=%q provides="server" token="secret"/>`, f.machineID)
		}
		io.WriteString(w, `</MediaContainer>`)
	case r.Method == http.MethodDelete && r.URL.Path == "/devices/1822812611.xml":
		if !authed || f.deviceOwner != acct.ID {
			w.WriteHeader(401)
			return
		}
		f.deviceOwner = 0
		if f.modelAssociation {
			f.associated = false
		}
		f.deviceDeletes++
	case r.Method == http.MethodPost && r.URL.Path == "/servers.xml":
		// PMS: token only as auth_token, no other query parameters, XML body.
		if !authed || r.Header.Get("X-Plex-Token") != "" || len(r.URL.Query()) != 1 || r.Header.Get("Content-Type") != "application/xml" {
			w.WriteHeader(401)
			return
		}
		b, _ := io.ReadAll(r.Body)
		f.registered = append(f.registered, string(b))
		var payload struct {
			XMLName xml.Name `xml:"MediaContainer"`
			Server  struct {
				Name              string `xml:"name,attr"`
				Address           string `xml:"address,attr"`
				Port              int    `xml:"port,attr"`
				MachineIdentifier string `xml:"machineIdentifier,attr"`
				LocalAddresses    string `xml:"localAddresses,attr"`
			}
		}
		// plex.tv answers 422 without a port (observed live).
		if xml.Unmarshal(b, &payload) != nil || payload.Server.Name == "" || payload.Server.Port == 0 ||
			payload.Server.MachineIdentifier != f.machineID {
			w.WriteHeader(422)
			return
		}
		w.WriteHeader(201)
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><MediaContainer size="1"><Server address=%q port="%d"/></MediaContainer>`, payload.Server.Address, payload.Server.Port)
	case r.Method == http.MethodPut && r.URL.Path == "/api/servers/"+f.machineID+"/connectivity":
		if !authed || r.URL.Query().Get("X-Plex-Token") == "" || r.URL.Query().Get("asyncIdentifier") == "" {
			w.WriteHeader(401)
			return
		}
		f.connectivity++
		w.WriteHeader(200)
	case r.URL.Path == "/api/v2/resources":
		if !authed {
			f.jsonErr(w, 401, 1001, "User could not be authenticated")
			return
		}
		q := r.URL.Query()
		if q.Get("includeHttps") != "1" || q.Get("includeRelay") != "1" {
			f.jsonErr(w, 400, 1000, "missing includes")
			return
		}
		out := append([]Resource{}, f.resources[token]...)
		if strings.HasPrefix(token, "server-token-") || token == "owner-token" {
			var conns []ResourceConnection
			for _, u := range f.published {
				if u == "" {
					continue
				}
				if strings.HasPrefix(u, "https://") {
					conns = append(conns, ResourceConnection{Protocol: "https", URI: u})
					continue
				}
				hostport := strings.TrimPrefix(u, "http://")
				host, port, _ := strings.Cut(hostport, ":")
				p, _ := strconv.Atoi(port)
				if !f.modelAssociation || f.associated {
					conns = append(conns, ResourceConnection{Protocol: "https", Address: host, Port: p, Local: strings.HasPrefix(host, "192.168."),
						URI: "https://" + strings.ReplaceAll(host, ".", "-") + "." + f.certHash + ".plex.direct:" + port})
				}
				conns = append(conns, ResourceConnection{Protocol: "http", Address: host, Port: p, URI: u, Local: strings.HasPrefix(host, "192.168.")})
			}
			out = append(out, Resource{Name: "Ether", Product: ServerProduct, ClientIdentifier: f.machineID, Provides: "server",
				Owned: true, PublicAddress: "203.0.113.7", Connections: conns})
		}
		json.NewEncoder(w).Encode(out)
	case r.URL.Path == "/api/v2/server/access_tokens":
		if !authed || r.URL.Query().Get("auth_token") == "" {
			w.WriteHeader(401)
			return
		}
		if f.accessFail > 0 {
			f.accessFail--
			w.WriteHeader(500)
			return
		}
		io.WriteString(w, f.accessXML)
	case r.URL.Path == "/api/servers/"+f.machineID+"/shared_servers":
		if !authed {
			w.WriteHeader(401)
			return
		}
		io.WriteString(w, f.sharedXML)
	case r.URL.Path == "/api/v2/server/users":
		if !authed {
			w.WriteHeader(401)
			return
		}
		if f.usersXML == "" {
			w.WriteHeader(500)
			return
		}
		io.WriteString(w, f.usersXML)
	case r.URL.Path == "/api/v2/server/users/subscriptions":
		if !authed {
			w.WriteHeader(401)
			return
		}
		io.WriteString(w, `<users><user id="42"><subscriptions/></user></users>`)
	case r.URL.Path == "/api/v2/server/users/services":
		if !authed {
			w.WriteHeader(401)
			return
		}
		io.WriteString(w, `<users><user id="42"><services><service identifier="example" endpoint="https://service.invalid" status="active" token="must-not-be-returned" secret="must-not-be-returned"/></services></user></users>`)
	case r.URL.Path == devPrefix+"subject":
		if !authed {
			w.WriteHeader(401)
			return
		}
		if r.Header.Get("Accept") != "application/xml" {
			f.jsonErr(w, 406, 1006, "Format not acceptable")
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		subject := f.subjectAttr
		if subject == "" {
			subject = base64.StdEncoding.EncodeToString(f.subjectDER())
		}
		fmt.Fprintf(w, `<subject commonName="*.%s.plex.direct" subject=%q/>`, f.certHash, subject)
	case r.URL.Path == devPrefix+"csr" && r.Method == http.MethodPut:
		if !authed {
			w.WriteHeader(401)
			return
		}
		if f.csrStatus != 0 {
			f.jsonErr(w, f.csrStatus, 1003, "API rate limit exceeded")
			return
		}
		if q := r.URL.Query(); q.Get("reason") == "" || q.Get("invalidIn") == "" {
			w.WriteHeader(400)
			return
		}
		// PMS uploads multipart/form-data with one part "file" (no filename,
		// Content-Type application/x-pem-file) holding the PEM CSR. Like the
		// live service, anything else (raw PEM, form, JSON, and a part with
		// filename="", which form parsers drop as an empty file input) is
		// answered with 422 code 1008 "Blank CSR".
		f.csrUploads++
		mr, err := r.MultipartReader()
		if err != nil {
			f.jsonErr(w, 422, 1008, "Blank CSR")
			return
		}
		var b []byte
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			if p.FormName() == "file" && p.Header.Get("Content-Type") == "application/x-pem-file" &&
				!strings.Contains(p.Header.Get("Content-Disposition"), "filename") {
				b, _ = io.ReadAll(p)
			}
		}
		blk, _ := pem.Decode(b)
		if blk == nil || blk.Type != "NEW CERTIFICATE REQUEST" {
			f.jsonErr(w, 422, 1008, "Blank CSR")
			return
		}
		csr, err := x509.ParseCertificateRequest(blk.Bytes)
		if err != nil || csr.CheckSignature() != nil || csr.Subject.CommonName != "*."+f.certHash+".plex.direct" || len(csr.Extensions) != 0 {
			w.WriteHeader(400)
			return
		}
		if f.subjectAttr == "" && !bytes.Equal(csr.RawSubject, f.subjectDER()) {
			w.WriteHeader(400)
			return
		}
		f.lastCSR = csr
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: csr.Subject, DNSNames: []string{csr.Subject.CommonName},
			NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(f.certTTL),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, f.caCert, csr.PublicKey, f.caKey)
		if err != nil {
			w.WriteHeader(500)
			return
		}
		f.issued = append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
			pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.caCert.Raw})...)
		if f.modelAssociation {
			f.associated = true
		}
		w.WriteHeader(204)
	case r.URL.Path == devPrefix+"download":
		if !authed {
			w.WriteHeader(401)
			return
		}
		if f.issued == nil {
			w.WriteHeader(404)
			return
		}
		if f.pendingDL > 0 {
			f.pendingDL--
			w.WriteHeader(202)
			return
		}
		w.Write(f.issued)
	default:
		w.WriteHeader(404)
	}
}
