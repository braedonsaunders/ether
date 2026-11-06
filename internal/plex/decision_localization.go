package plex

import (
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/plex/model"
)

// These translations are maintained independently of client or server assets.
// Unknown diagnostic sentences remain intact so source-specific details survive.
var decisionTranslations = map[string][3]string{
	"af":    {"Direkte afspeel is gereed.", "Direkte afspeel is nie beskikbaar nie; omskakeling is gereed.", "Die toepassing kan nie hierdie item direk speel nie. Direkte afspeel is afgeskakel."},
	"ar":    {"التشغيل المباشر متاح.", "التشغيل المباشر غير متاح؛ التحويل متاح.", "لا يستطيع التطبيق تشغيل هذا العنصر مباشرة. التشغيل المباشر معطل."},
	"cs":    {"Přímé přehrávání je dostupné.", "Přímé přehrávání není dostupné; převod je dostupný.", "Aplikace nemůže tuto položku přehrát přímo. Přímé přehrávání je vypnuté."},
	"da":    {"Direkte afspilning er klar.", "Direkte afspilning er ikke tilgængelig; konvertering er klar.", "Appen kan ikke afspille dette element direkte. Direkte afspilning er slået fra."},
	"de":    {"Direkte Wiedergabe ist möglich.", "Direkte Wiedergabe ist nicht verfügbar; Konvertierung ist möglich.", "Die App kann diesen Inhalt nicht direkt wiedergeben. Direkte Wiedergabe ist deaktiviert."},
	"el":    {"Η άμεση αναπαραγωγή είναι διαθέσιμη.", "Η άμεση αναπαραγωγή δεν είναι διαθέσιμη· η μετατροπή είναι διαθέσιμη.", "Η εφαρμογή δεν μπορεί να αναπαράγει απευθείας αυτό το στοιχείο. Η άμεση αναπαραγωγή είναι απενεργοποιημένη."},
	"es":    {"La reproducción directa está disponible.", "La reproducción directa no está disponible; la conversión está disponible.", "La aplicación no puede reproducir este elemento directamente. La reproducción directa está desactivada."},
	"fi":    {"Suora toisto on käytettävissä.", "Suora toisto ei ole käytettävissä; muuntaminen on käytettävissä.", "Sovellus ei voi toistaa tätä kohdetta suoraan. Suora toisto on poistettu käytöstä."},
	"fr":    {"La lecture directe est disponible.", "La lecture directe est indisponible ; la conversion est disponible.", "L’application ne peut pas lire cet élément directement. La lecture directe est désactivée."},
	"he":    {"ניגון ישיר זמין.", "ניגון ישיר אינו זמין; המרה זמינה.", "היישום אינו יכול לנגן פריט זה ישירות. הניגון הישיר מושבת."},
	"hr":    {"Izravna reprodukcija je dostupna.", "Izravna reprodukcija nije dostupna; pretvorba je dostupna.", "Aplikacija ne može izravno reproducirati ovu stavku. Izravna reprodukcija je isključena."},
	"hu":    {"A közvetlen lejátszás elérhető.", "A közvetlen lejátszás nem érhető el; az átalakítás elérhető.", "Az alkalmazás nem tudja közvetlenül lejátszani ezt az elemet. A közvetlen lejátszás ki van kapcsolva."},
	"it":    {"La riproduzione diretta è disponibile.", "La riproduzione diretta non è disponibile; la conversione è disponibile.", "L’app non può riprodurre direttamente questo elemento. La riproduzione diretta è disattivata."},
	"ja":    {"直接再生を利用できます。", "直接再生は利用できません。変換を利用できます。", "このアプリはこの項目を直接再生できません。直接再生は無効です。"},
	"ko":    {"직접 재생을 사용할 수 있습니다.", "직접 재생을 사용할 수 없습니다. 변환을 사용할 수 있습니다.", "앱에서 이 항목을 직접 재생할 수 없습니다. 직접 재생이 비활성화되어 있습니다."},
	"nl":    {"Direct afspelen is beschikbaar.", "Direct afspelen is niet beschikbaar; conversie is beschikbaar.", "De app kan dit item niet direct afspelen. Direct afspelen is uitgeschakeld."},
	"no":    {"Direkteavspilling er tilgjengelig.", "Direkteavspilling er ikke tilgjengelig; konvertering er tilgjengelig.", "Appen kan ikke spille av dette elementet direkte. Direkteavspilling er deaktivert."},
	"pl":    {"Odtwarzanie bezpośrednie jest dostępne.", "Odtwarzanie bezpośrednie jest niedostępne; konwersja jest dostępna.", "Aplikacja nie może odtworzyć tego elementu bezpośrednio. Odtwarzanie bezpośrednie jest wyłączone."},
	"pt":    {"A reprodução direta está disponível.", "A reprodução direta não está disponível; a conversão está disponível.", "A aplicação não consegue reproduzir este item diretamente. A reprodução direta está desativada."},
	"pt-br": {"A reprodução direta está disponível.", "A reprodução direta não está disponível; a conversão está disponível.", "O aplicativo não pode reproduzir este item diretamente. A reprodução direta está desativada."},
	"ro":    {"Redarea directă este disponibilă.", "Redarea directă nu este disponibilă; conversia este disponibilă.", "Aplicația nu poate reda direct acest element. Redarea directă este dezactivată."},
	"ru":    {"Прямое воспроизведение доступно.", "Прямое воспроизведение недоступно; преобразование доступно.", "Приложение не может воспроизвести этот элемент напрямую. Прямое воспроизведение отключено."},
	"sk":    {"Priame prehrávanie je dostupné.", "Priame prehrávanie nie je dostupné; konverzia je dostupná.", "Aplikácia nemôže prehrať túto položku priamo. Priame prehrávanie je vypnuté."},
	"sr":    {"Директна репродукција је доступна.", "Директна репродукција није доступна; конверзија је доступна.", "Апликација не може директно да репродукује ову ставку. Директна репродукција је искључена."},
	"sv":    {"Direktuppspelning är tillgänglig.", "Direktuppspelning är inte tillgänglig; konvertering är tillgänglig.", "Appen kan inte spela upp det här objektet direkt. Direktuppspelning är avstängd."},
	"tr":    {"Doğrudan oynatma kullanılabilir.", "Doğrudan oynatma kullanılamıyor; dönüştürme kullanılabilir.", "Uygulama bu öğeyi doğrudan oynatamıyor. Doğrudan oynatma devre dışı."},
	"uk":    {"Пряме відтворення доступне.", "Пряме відтворення недоступне; перетворення доступне.", "Застосунок не може відтворити цей елемент безпосередньо. Пряме відтворення вимкнено."},
	"zh":    {"可以直接播放。", "无法直接播放；可以转换。", "应用无法直接播放此项目。直接播放已禁用。"},
}

func decisionCatalog(raw string) (string, bool) {
	raw = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(raw), "_", "-"))
	if strings.HasPrefix(raw, "zh-hant") || raw == "zh-tw" || raw == "zh-hk" || raw == "zh-mo" {
		return "", false
	}
	switch raw {
	case "pt-br", "pt-pt":
		return "pt-br", true
	case "nb", "nb-no", "no", "no-no":
		return "no", true
	case "ca", "ca-es":
		return "es", false
	case "nn", "nn-no":
		return "no", false
	}
	base, _, _ := strings.Cut(raw, "-")
	if base == "en" {
		return "en", true
	}
	if _, ok := decisionTranslations[base]; ok {
		return base, true
	}
	return "", false
}

func decisionLanguage(r *http.Request) string {
	if raw := FromContext(r.Context()).Language; raw != "" {
		lang, _ := decisionCatalog(raw)
		return lang
	}
	type candidate struct {
		raw string
		q   float64
	}
	var candidates []candidate
	for _, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		raw, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		q := 1.0
		if params != "" {
			var err error
			q, err = strconv.ParseFloat(strings.TrimPrefix(strings.TrimSpace(params), "q="), 64)
			if err != nil || math.IsNaN(q) || q <= 0 || q > 1 {
				continue
			}
		}
		candidates = append(candidates, candidate{raw, q})
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].q > candidates[j].q })
	fallback := ""
	for _, candidate := range candidates {
		lang, exact := decisionCatalog(candidate.raw)
		if exact {
			return lang
		}
		if fallback == "" {
			fallback = lang
		}
	}
	return fallback
}

// localizeDecisionResponse copies only decision attributes, preserving shared
// model values and every numeric code, media row and stream selection.
func localizeDecisionResponse(r *http.Request, value any) any {
	if !strings.HasSuffix(r.URL.Path, "/:/transcode/universal/decision") {
		return value
	}
	mc, ok := value.(*model.MediaContainer)
	if !ok {
		return value
	}
	lang := decisionLanguage(r)
	translated, ok := decisionTranslations[lang]
	if !ok {
		return value
	}
	copyContainer := *mc
	copyContainer.Extra = append([]model.Attr(nil), mc.Extra...)
	for i, attr := range copyContainer.Extra {
		switch attr.Name {
		case "mdeDecisionText", "directPlayDecisionText", "generalDecisionText", "transcodeDecisionText":
		default:
			continue
		}
		switch attr.Value {
		case textDirectPlayOK:
			copyContainer.Extra[i].Value = translated[0]
		case textConversionOK:
			copyContainer.Extra[i].Value = translated[1]
		case textDirectPlayDisable:
			copyContainer.Extra[i].Value = translated[2]
		default:
			if text, ok := attr.Value.(string); ok {
				copyContainer.Extra[i].Value = localizeDecisionDiagnostic(lang, text)
			}
		}
	}
	return &copyContainer
}
