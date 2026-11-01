// Package mediabrowser implements the Jellyfin and Emby protocol surfaces.
package mediabrowser

import (
	"context"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

// PublicInfo is supplied by the host, so the server identity remains stable
// across restarts and is distinct for each protocol surface.
type PublicInfo struct {
	ID, ServerName, LocalAddress, Version, ProductName string
}

// PublicRoutes registers connection endpoints that do not require a grant.
// A user picker is intentionally not exposed until the host can supply its
// authorized list of visible viewers.
func PublicRoutes(r *route.Router, info PublicInfo) error {
	return publicRoutes(r, func() PublicInfo { return info }, nil, nil)
}
func publicRoutes(r *route.Router, getInfo func() PublicInfo, picker http.HandlerFunc, clientIP func(*http.Request) (netip.Addr, bool)) error {
	if picker == nil {
		picker = func(w http.ResponseWriter, _ *http.Request) { jsonReply(w, []any{}) }
	}
	handlers := []struct {
		method, path string
		handler      http.HandlerFunc
	}{
		{"GET", "/System/Info/Public", func(w http.ResponseWriter, r *http.Request) {
			info := getInfo()
			address := info.LocalAddress
			if address == "" && r.Host != "" {
				scheme := "http"
				if r.TLS != nil {
					scheme = "https"
				}
				address = scheme + "://" + r.Host
			}
			if prefix, ok := r.Context().Value(prefixKey{}).(string); ok && address != "" {
				address = strings.TrimRight(address, "/") + prefix
			}
			if info.ProductName == "Emby Server" {
				jsonReply(w, map[string]any{"Id": info.ID, "ServerName": info.ServerName, "Version": info.Version, "LocalAddress": address, "LocalAddresses": []string{address}, "RemoteAddresses": []string{address}, "WanAddress": address})
				return
			}
			jsonReply(w, struct {
				ID                     string `json:"Id"`
				ServerName             string `json:"ServerName"`
				LocalAddress           string `json:"LocalAddress"`
				Version                string `json:"Version"`
				ProductName            string `json:"ProductName"`
				OperatingSystem        string `json:"OperatingSystem"`
				StartupWizardCompleted bool   `json:"StartupWizardCompleted"`
			}{info.ID, info.ServerName, address, info.Version, info.ProductName, "", true})
		}},
		{"GET", "/Branding/Configuration", func(w http.ResponseWriter, _ *http.Request) { jsonReply(w, map[string]any{}) }},
		{"POST", "/System/Ping", func(w http.ResponseWriter, _ *http.Request) { jsonReply(w, getInfo().ProductName) }},
		{"GET", "/web", webPage},
		{"GET", "/web/{path...}", func(w http.ResponseWriter, r *http.Request) {
			if getInfo().ProductName == "Emby Server" && r.PathValue("path") != "" {
				http.NotFound(w, r)
				return
			}
			webPage(w, r)
		}},
		{"GET", "/Users/Public", picker},
		{"GET", "/System/Endpoint", func(w http.ResponseWriter, r *http.Request) {
			jsonReply(w, endpointInfo(r, clientIP))
		}},
		{"GET", "/System/Ping", func(w http.ResponseWriter, _ *http.Request) {
			jsonReply(w, getInfo().ProductName)
		}},
	}
	for _, h := range handlers {
		if err := r.Handle(h.method, h.path, h.handler); err != nil {
			return err
		}
	}
	return nil
}

// endpointInfo reports where the caller sits relative to the server. Emby for
// tvOS and other native clients call it before signing in and treat a 404 as
// an unreachable server. IsLocal is a loopback peer and IsInNetwork a
// loopback, private or link-local peer, as in Jellyfin. The peer comes from
// the host's trusted-proxy resolution, so forwarding headers cannot claim
// the LAN.
func endpointInfo(r *http.Request, clientIP func(*http.Request) (netip.Addr, bool)) map[string]bool {
	var ip netip.Addr
	ok := false
	if clientIP != nil {
		ip, ok = clientIP(r)
	} else if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		ip, ok = ap.Addr(), true
	}
	ip = ip.Unmap()
	local := ok && ip.IsLoopback()
	network := local || ok && (ip.IsPrivate() || ip.IsLinkLocalUnicast())
	return map[string]bool{"IsLocal": local, "IsInNetwork": network}
}

type prefixKey struct{}

// StripPrefix accepts the optional protocol prefix while preserving the rest
// of the request for the router. It does not mutate the incoming request.
func StripPrefix(prefix string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) >= len(prefix) && strings.EqualFold(r.URL.Path[:len(prefix)], prefix) && (len(r.URL.Path) == len(prefix) || r.URL.Path[len(prefix)] == '/') {
			clone := r.Clone(context.WithValue(r.Context(), prefixKey{}, prefix))
			u := *r.URL
			clone.URL = &u
			clone.URL.Path = r.URL.Path[len(prefix):]
			if clone.URL.Path == "" {
				clone.URL.Path = "/"
			}
			clone.URL.RawPath = ""
			r = clone
		}
		next.ServeHTTP(w, r)
	})
}

func webPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.Header().Set("Vary", "Accept-Language")
	type copy struct{ heading, instruction string }
	translations := map[string]copy{
		"en":    {"Connect to Ether", "Open a native Jellyfin or Emby app, add this server's address, and sign in with your Ether viewer account or Quick Connect."},
		"fr":    {"Se connecter à Ether", "Ouvrez une application Jellyfin ou Emby native, ajoutez l’adresse de ce serveur et connectez-vous avec votre compte Ether ou Quick Connect."},
		"es":    {"Conectar a Ether", "Abra una aplicación nativa de Jellyfin o Emby, agregue la dirección de este servidor e inicie sesión con su cuenta de Ether o Quick Connect."},
		"de":    {"Mit Ether verbinden", "Öffnen Sie eine native Jellyfin- oder Emby-App, fügen Sie die Serveradresse hinzu und melden Sie sich mit Ihrem Ether-Konto oder Quick Connect an."},
		"cs":    {"Připojit k Etheru", "Otevřete nativní aplikaci Jellyfin nebo Emby, přidejte adresu tohoto serveru a přihlaste se svým účtem diváka Ether nebo pomocí rychlého připojení."},
		"da":    {"Opret forbindelse til Ether", "Åbn en indbygget Jellyfin- eller Emby-app, tilføj denne servers adresse, og log ind med din Ether-seerkonto eller hurtig forbindelse."},
		"fi":    {"Yhdistä Etheriin", "Avaa natiivi Jellyfin- tai Emby-sovellus, lisää tämän palvelimen osoite ja kirjaudu Ether-katselijatililläsi tai pikayhteydellä."},
		"it":    {"Connettiti a Ether", "Apri un’app nativa Jellyfin o Emby, aggiungi l’indirizzo di questo server e accedi con il tuo account spettatore Ether o con la connessione rapida."},
		"ja":    {"Ether に接続", "ネイティブの Jellyfin または Emby アプリを開き、このサーバーのアドレスを追加して、Ether の視聴者アカウントまたはクイック接続でログインしてください。"},
		"ko":    {"Ether에 연결", "Jellyfin 또는 Emby 네이티브 앱을 열고 이 서버의 주소를 추가한 다음 Ether 시청자 계정이나 빠른 연결로 로그인하세요."},
		"nb":    {"Koble til Ether", "Åpne en innebygd Jellyfin- eller Emby-app, legg til denne serverens adresse og logg inn med Ether-seerkontoen din eller hurtigtilkobling."},
		"nl":    {"Verbinding maken met Ether", "Open een native Jellyfin- of Emby-app, voeg het adres van deze server toe en meld je aan met je Ether-kijkersaccount of Snel verbinden."},
		"pl":    {"Połącz z Ether", "Otwórz natywną aplikację Jellyfin lub Emby, dodaj adres tego serwera i zaloguj się za pomocą konta widza Ether lub szybkiego łączenia."},
		"pt-BR": {"Conectar ao Ether", "Abra um aplicativo nativo do Jellyfin ou Emby, adicione o endereço deste servidor e entre com sua conta de espectador Ether ou com a conexão rápida."},
		"ru":    {"Подключиться к Ether", "Откройте нативное приложение Jellyfin или Emby, добавьте адрес этого сервера и войдите с учётной записью зрителя Ether или через быстрое подключение."},
		"sv":    {"Anslut till Ether", "Öppna en inbyggd Jellyfin- eller Emby-app, lägg till den här serverns adress och logga in med ditt Ether-tittarkonto eller snabbanslutning."},
		"tr":    {"Ether’e bağlan", "Yerel bir Jellyfin veya Emby uygulamasını açın, bu sunucunun adresini ekleyin ve Ether izleyici hesabınızla ya da hızlı bağlantıyla oturum açın."},
		"uk":    {"Підключитися до Ether", "Відкрийте нативний застосунок Jellyfin або Emby, додайте адресу цього сервера й увійдіть з обліковим записом глядача Ether або через швидке підключення."},
		"zh-CN": {"连接到 Ether", "打开原生 Jellyfin 或 Emby 应用，添加此服务器的地址，然后使用 Ether 观众账户或快速连接登录。"},
		"zh-TW": {"連線至 Ether", "開啟原生 Jellyfin 或 Emby 應用程式，新增此伺服器的位址，然後使用 Ether 觀眾帳號或快速連線登入。"},
	}
	lang := "en"
	for _, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		tag := strings.ToLower(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]))
		if strings.HasPrefix(tag, "zh-tw") || strings.HasPrefix(tag, "zh-hant") {
			tag = "zh-TW"
		} else if strings.HasPrefix(tag, "zh") {
			tag = "zh-CN"
		} else if strings.HasPrefix(tag, "pt") {
			tag = "pt-BR"
		} else {
			tag = strings.SplitN(tag, "-", 2)[0]
		}
		if _, ok := translations[tag]; ok {
			lang = tag
			break
		}
	}
	c := translations[lang]
	address := webAddress(r)
	addressHTML := ""
	if address != "" {
		addressHTML = fmt.Sprintf(`<p>%s</p><p><code>%s</code></p>`, html.EscapeString(addressLabels[lang]), html.EscapeString(address))
	}
	_, _ = fmt.Fprintf(w, `<!doctype html><html lang="%s"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Ether</title><style>body{margin:0;background:#0e1116;color:#e7e9ee;font:16px/1.45 system-ui,sans-serif}main{max-width:36rem;margin:4rem auto;padding:0 1.25rem}h1{font-size:1.5rem;font-weight:650}p,li{color:#b7bdc9}code{color:#e7e9ee}a{color:#8eb6ff}</style><main><h1>%s</h1><p>%s</p>%s<p>%s</p><ul><li><a href="https://github.com/jarnedemeulemeester/findroid">Findroid</a></li><li><a href="https://github.com/jellyfin/swiftfin">Swiftfin</a></li><li><a href="https://github.com/jellyfin/jellyfin-media-player">Jellyfin Media Player</a></li></ul></main></html>`, lang, html.EscapeString(c.heading), html.EscapeString(c.instruction), addressHTML, html.EscapeString(appLabels[lang]))
}

var addressLabels = map[string]string{
	"en": "Server address", "fr": "Adresse du serveur", "es": "Dirección del servidor",
	"de": "Serveradresse", "cs": "Adresa serveru", "da": "Serveradresse",
	"fi": "Palvelimen osoite", "it": "Indirizzo del server", "ja": "サーバーアドレス",
	"ko": "서버 주소", "nb": "Serveradresse", "nl": "Serveradres",
	"pl": "Adres serwera", "pt-BR": "Endereço do servidor", "ru": "Адрес сервера",
	"sv": "Serveradress", "tr": "Sunucu adresi", "uk": "Адреса сервера",
	"zh-CN": "服务器地址", "zh-TW": "伺服器位址",
}

var appLabels = map[string]string{
	"en": "Apps to try", "fr": "Applications à essayer", "es": "Aplicaciones para probar",
	"de": "Apps zum Ausprobieren", "cs": "Aplikace k vyzkoušení", "da": "Apps du kan prøve",
	"fi": "Kokeiltavia sovelluksia", "it": "App da provare", "ja": "試せるアプリ",
	"ko": "써 볼 앱", "nb": "Apper du kan prøve", "nl": "Apps om te proberen",
	"pl": "Aplikacje do wypróbowania", "pt-BR": "Apps para experimentar", "ru": "Приложения",
	"sv": "Appar att prova", "tr": "Denenecek uygulamalar", "uk": "Застосунки",
	"zh-CN": "可以试试的应用", "zh-TW": "可以試試的應用程式",
}

func webAddress(r *http.Request) string {
	if r.Host == "" {
		return ""
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil && netip.MustParsePrefix("172.16.0.0/12").Contains(ip.Unmap()) {
		return ""
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	path := ""
	if prefix, ok := r.Context().Value(prefixKey{}).(string); ok {
		path = prefix
	}
	return scheme + "://" + r.Host + path
}
