package plex

import (
	"fmt"
	"regexp"
	"strings"
)

// Diagnostic phrases are maintained here independently. Codec names, property
// keys, comparison operators and numeric values remain technical identifiers.
// Columns: direct refusal, audio selection, sidecar, embedded selection,
// conversion profile, color format, constraint, direct-play profile.
var decisionDiagnosticPhrases = map[string][8]string{
	"af":    {"Direkte afspeel is nie moontlik nie", "Oudioseleksie word nie ondersteun nie", "Eksterne onderskrif is onversoenbaar", "Ingebedde onderskrif is nie eerste nie", "Geen omskakelingsprofiel nie", "Kleurformaat word nie ondersteun nie", "Beperking oorskry", "Geen versoenbare afspeelprofiel nie"},
	"ar":    {"التشغيل المباشر غير ممكن", "اختيار الصوت غير مدعوم", "الترجمة الخارجية غير متوافقة", "الترجمة المضمنة ليست الأولى", "لا يوجد ملف تحويل", "تنسيق الألوان غير مدعوم", "تم تجاوز القيد", "لا يوجد ملف تشغيل متوافق"},
	"cs":    {"Přímé přehrávání není možné", "Výběr zvuku není podporován", "Externí titulky nejsou kompatibilní", "Vložené titulky nejsou první", "Chybí profil převodu", "Barevný formát není podporován", "Překročené omezení", "Chybí kompatibilní profil přehrávání"},
	"da":    {"Direkte afspilning er ikke mulig", "Lydvalg understøttes ikke", "Eksterne undertekster er inkompatible", "Indlejrede undertekster er ikke først", "Ingen konverteringsprofil", "Farveformat understøttes ikke", "Begrænsning overskredet", "Ingen kompatibel afspilningsprofil"},
	"de":    {"Direkte Wiedergabe ist nicht möglich", "Audioauswahl wird nicht unterstützt", "Externe Untertitel sind inkompatibel", "Eingebettete Untertitel stehen nicht an erster Stelle", "Kein Konvertierungsprofil", "Farbformat wird nicht unterstützt", "Grenzwert überschritten", "Kein kompatibles Wiedergabeprofil"},
	"el":    {"Η άμεση αναπαραγωγή δεν είναι δυνατή", "Η επιλογή ήχου δεν υποστηρίζεται", "Οι εξωτερικοί υπότιτλοι είναι ασύμβατοι", "Οι ενσωματωμένοι υπότιτλοι δεν είναι πρώτοι", "Δεν υπάρχει προφίλ μετατροπής", "Η μορφή χρώματος δεν υποστηρίζεται", "Υπέρβαση περιορισμού", "Δεν υπάρχει συμβατό προφίλ αναπαραγωγής"},
	"es":    {"La reproducción directa no es posible", "La selección de audio no es compatible", "Los subtítulos externos son incompatibles", "Los subtítulos integrados no son los primeros", "Falta el perfil de conversión", "El formato de color no es compatible", "Límite superado", "Falta un perfil de reproducción compatible"},
	"fi":    {"Suora toisto ei ole mahdollista", "Äänen valintaa ei tueta", "Ulkoiset tekstitykset eivät ole yhteensopivia", "Upotetut tekstitykset eivät ole ensimmäisenä", "Muunnosprofiili puuttuu", "Värimuotoa ei tueta", "Rajoitus ylitetty", "Yhteensopiva toistoprofiili puuttuu"},
	"fr":    {"La lecture directe est impossible", "La sélection audio n’est pas prise en charge", "Le sous-titre externe est incompatible", "Le sous-titre intégré n’est pas le premier", "Aucun profil de conversion", "Le format de couleur n’est pas pris en charge", "Limite dépassée", "Aucun profil de lecture compatible"},
	"he":    {"ניגון ישיר אינו אפשרי", "בחירת שמע אינה נתמכת", "הכתוביות החיצוניות אינן תואמות", "הכתוביות המוטמעות אינן הראשונות", "אין פרופיל המרה", "פורמט הצבע אינו נתמך", "חריגה מהמגבלה", "אין פרופיל ניגון תואם"},
	"hr":    {"Izravna reprodukcija nije moguća", "Odabir zvuka nije podržan", "Vanjski titlovi nisu kompatibilni", "Ugrađeni titlovi nisu prvi", "Nema profila pretvorbe", "Format boje nije podržan", "Ograničenje je prekoračeno", "Nema kompatibilnog profila reprodukcije"},
	"hu":    {"A közvetlen lejátszás nem lehetséges", "A hangválasztás nem támogatott", "A külső felirat nem kompatibilis", "A beágyazott felirat nem az első", "Nincs átalakítási profil", "A színformátum nem támogatott", "Korlát túllépve", "Nincs kompatibilis lejátszási profil"},
	"it":    {"La riproduzione diretta non è possibile", "La selezione audio non è supportata", "Il sottotitolo esterno è incompatibile", "Il sottotitolo incorporato non è il primo", "Nessun profilo di conversione", "Il formato colore non è supportato", "Limite superato", "Nessun profilo di riproduzione compatibile"},
	"ja":    {"直接再生できません", "音声の選択に対応していません", "外部字幕は互換性がありません", "埋め込み字幕が先頭ではありません", "変換プロファイルがありません", "色形式に対応していません", "制限を超えています", "互換性のある再生プロファイルがありません"},
	"ko":    {"직접 재생할 수 없습니다", "오디오 선택을 지원하지 않습니다", "외부 자막이 호환되지 않습니다", "내장 자막이 첫 번째가 아닙니다", "변환 프로필이 없습니다", "색상 형식을 지원하지 않습니다", "제한을 초과했습니다", "호환되는 재생 프로필이 없습니다"},
	"nl":    {"Direct afspelen is niet mogelijk", "Audioselectie wordt niet ondersteund", "Externe ondertitels zijn niet compatibel", "Ingebedde ondertitels staan niet als eerste", "Geen conversieprofiel", "Kleurformaat wordt niet ondersteund", "Limiet overschreden", "Geen compatibel afspeelprofiel"},
	"no":    {"Direkteavspilling er ikke mulig", "Lydvalg støttes ikke", "Eksterne undertekster er inkompatible", "Innebygde undertekster er ikke først", "Ingen konverteringsprofil", "Fargeformat støttes ikke", "Grense overskredet", "Ingen kompatibel avspillingsprofil"},
	"pl":    {"Odtwarzanie bezpośrednie nie jest możliwe", "Wybór dźwięku nie jest obsługiwany", "Zewnętrzne napisy są niezgodne", "Osadzone napisy nie są pierwsze", "Brak profilu konwersji", "Format kolorów nie jest obsługiwany", "Przekroczono ograniczenie", "Brak zgodnego profilu odtwarzania"},
	"pt":    {"A reprodução direta não é possível", "A seleção de áudio não é suportada", "A legenda externa é incompatível", "A legenda incorporada não é a primeira", "Não existe perfil de conversão", "O formato de cor não é suportado", "Limite excedido", "Não existe perfil de reprodução compatível"},
	"pt-br": {"A reprodução direta não é possível", "A seleção de áudio não é compatível", "A legenda externa é incompatível", "A legenda incorporada não é a primeira", "Nenhum perfil de conversão", "O formato de cor não é compatível", "Limite excedido", "Nenhum perfil de reprodução compatível"},
	"ro":    {"Redarea directă nu este posibilă", "Selectarea sunetului nu este acceptată", "Subtitrarea externă este incompatibilă", "Subtitrarea încorporată nu este prima", "Lipsește profilul de conversie", "Formatul de culoare nu este acceptat", "Limită depășită", "Lipsește un profil de redare compatibil"},
	"ru":    {"Прямое воспроизведение невозможно", "Выбор звука не поддерживается", "Внешние субтитры несовместимы", "Встроенные субтитры не первые", "Нет профиля преобразования", "Формат цвета не поддерживается", "Ограничение превышено", "Нет совместимого профиля воспроизведения"},
	"sk":    {"Priame prehrávanie nie je možné", "Výber zvuku nie je podporovaný", "Externé titulky nie sú kompatibilné", "Vložené titulky nie sú prvé", "Chýba profil konverzie", "Farebný formát nie je podporovaný", "Prekročené obmedzenie", "Chýba kompatibilný profil prehrávania"},
	"sr":    {"Директна репродукција није могућа", "Избор звука није подржан", "Спољни титлови нису компатибилни", "Уграђени титлови нису први", "Нема профила конверзије", "Формат боје није подржан", "Ограничење је прекорачено", "Нема компатибилног профила репродукције"},
	"sv":    {"Direktuppspelning är inte möjlig", "Ljudval stöds inte", "Externa undertexter är inkompatibla", "Inbäddade undertexter är inte först", "Ingen konverteringsprofil", "Färgformat stöds inte", "Gräns överskriden", "Ingen kompatibel uppspelningsprofil"},
	"tr":    {"Doğrudan oynatma mümkün değil", "Ses seçimi desteklenmiyor", "Harici altyazı uyumsuz", "Gömülü altyazı ilk sırada değil", "Dönüştürme profili yok", "Renk biçimi desteklenmiyor", "Sınır aşıldı", "Uyumlu oynatma profili yok"},
	"uk":    {"Пряме відтворення неможливе", "Вибір звуку не підтримується", "Зовнішні субтитри несумісні", "Вбудовані субтитри не перші", "Немає профілю перетворення", "Формат кольору не підтримується", "Перевищено обмеження", "Немає сумісного профілю відтворення"},
	"zh":    {"无法直接播放", "不支持选择音轨", "外部字幕不兼容", "内嵌字幕不是第一条", "没有转换配置", "不支持此色彩格式", "超出限制", "没有兼容的播放配置"},
}

var directProfileDiagnostic = regexp.MustCompile(`^App cannot direct play this item\. No direct play video profile exists for protocol ([^,]+), with container ([^,]+), (?:and |using )?video codec ([^,.]+)(?:, (?:and )?audio codec ([^,.]+))?(?:, and subtitle codec ([^.]+))?\.$`)
var limitProfileDiagnostic = regexp.MustCompile(`^This app cannot play this item\. The reason is: (\S+) limitation applies: (.+)\.$`)

func localizeDecisionDiagnostic(lang, message string) string {
	c, ok := decisionDiagnosticPhrases[lang]
	if !ok {
		return message
	}
	if match := directProfileDiagnostic.FindStringSubmatch(message); match != nil {
		details := fmt.Sprintf("protocol=%s; container=%s; video=%s", match[1], match[2], match[3])
		if match[4] != "" {
			details += "; audio=" + match[4]
		}
		if match[5] != "" {
			details += "; subtitle=" + match[5]
		}
		return c[0] + ". " + c[7] + " (" + details + ")."
	}
	if match := limitProfileDiagnostic.FindStringSubmatch(message); match != nil {
		return c[0] + ". " + c[6] + ": " + match[1] + ": " + match[2] + "."
	}
	switch message {
	case "App cannot direct play this item.":
		return c[0] + "."
	case "App cannot direct play this item. Selected audio stream is not the first audio stream and app cannot select audio streams in direct play.":
		return c[0] + ". " + c[1] + "."
	case "App cannot direct play this item. Selected sidecar subtitle stream cannot be direct played.":
		return c[0] + ". " + c[2] + "."
	case "App cannot direct play this item. Selected embedded subtitle stream is not the first embedded subtitle stream.":
		return c[0] + ". " + c[3] + "."
	case "File is unplayable. DoVi (Profile 5) color space is not supported.":
		return c[0] + ". " + c[5] + ": DoVi (Profile 5)."
	}
	for _, prefix := range []string{"Cannot convert this item. No conversion profile found for protocol ", "No transcoding profile exists for protocol "} {
		if strings.HasPrefix(message, prefix) && strings.HasSuffix(message, ".") {
			protocol := strings.TrimSuffix(strings.TrimPrefix(message, prefix), ".")
			if protocol != "" && !strings.ContainsAny(protocol, " \t\r\n") {
				return c[4] + ": protocol=" + protocol + "."
			}
		}
	}
	return message
}
