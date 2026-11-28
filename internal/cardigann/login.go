package cardigann

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// ErrLogin reports credentials or a session the site rejected.
var ErrLogin = errors.New("sign-in failed")

const loginFailureCooldown = 5 * time.Minute

func (r *Runner) lockKey() string { return "cardigann-login:" + r.id }

// session reports the cached session state.
func (r *Runner) session() (bool, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loggedIn, r.version
}

// adopt loads a stored session into the jar.
func (r *Runner) adopt(row sessionRow) {
	r.http.clearCookies(r.site)
	r.http.importCookies(r.site, row.Cookies)
	if row.UserAgent != "" {
		r.http.mu.Lock()
		r.http.ua = row.UserAgent
		r.http.mu.Unlock()
	}
	r.mu.Lock()
	r.version = row.Version
	r.mu.Unlock()
}

func (r *Runner) setLoggedIn(ok bool) {
	r.mu.Lock()
	r.loggedIn = ok
	r.mu.Unlock()
}

// loadSession reads the shared session row. A row that does not open for
// this site (errSessionUnreadable) is treated as no session, so the next
// login replaces it instead of reusing cookies issued to another origin.
func (r *Runner) loadSession(ctx context.Context) (sessionRow, bool, error) {
	row, ok, err := r.sessions.load(ctx, r.id, siteOrigin(r.site))
	if errors.Is(err, errSessionUnreadable) {
		r.log.Warn("cardigann: stored session unusable; logging in again", "indexer", r.id, "err", err)
		return sessionRow{}, false, nil
	}
	return row, ok, err
}

// ensureLogin makes sure the jar holds a working session. The database
// row is the source of truth: a session another node saved is reused
// before logging in again.
func (r *Runner) ensureLogin(ctx context.Context) error {
	if r.def.Login == nil {
		return nil
	}
	loggedIn, version := r.session()
	if loggedIn {
		return nil
	}
	row, ok, err := r.loadSession(ctx)
	if err != nil {
		return fmt.Errorf("load session: %w", err)
	}
	if ok && row.Version != version {
		r.adopt(row)
		if err := r.testSession(ctx); err == nil {
			r.setLoggedIn(true)
			return nil
		}
		version = row.Version
	} else if ok {
		version = row.Version
	}
	return r.login(ctx, version, false)
}

// reauth handles an authentication failure seen with session version
// stale: if another login replaced that session, adopt it; otherwise log
// in again.
func (r *Runner) reauth(ctx context.Context, stale int64) error {
	r.setLoggedIn(false)
	return r.login(ctx, stale, false)
}

// login performs a single-flight login. Inside the lock the stored row is
// re-read: when its version moved past stale someone else already logged
// in, and that session is used instead (unless force is set, as for an
// owner's explicit test).
func (r *Runner) login(ctx context.Context, stale int64, force bool) error {
	if !force {
		r.mu.Lock()
		if !r.loginFailedAt.IsZero() && r.now().Sub(r.loginFailedAt) < loginFailureCooldown {
			err := r.loginFailed
			r.mu.Unlock()
			return err
		}
		r.mu.Unlock()
	}
	unlock, err := r.lock(ctx, r.lockKey())
	if err != nil {
		return err
	}
	defer unlock()
	if !force {
		r.mu.Lock()
		if !r.loginFailedAt.IsZero() && r.now().Sub(r.loginFailedAt) < loginFailureCooldown {
			err := r.loginFailed
			r.mu.Unlock()
			return err
		}
		r.mu.Unlock()
		row, ok, err := r.loadSession(ctx)
		if err != nil {
			return fmt.Errorf("load session: %w", err)
		}
		if ok && row.Version > stale {
			r.adopt(row)
			r.mu.Lock()
			r.loggedIn = true
			r.loginFailedAt = time.Time{}
			r.loginFailed = nil
			r.mu.Unlock()
			return nil
		}
	}
	r.http.clearCookies(r.site)
	if err := r.doLogin(ctx); err != nil {
		r.mu.Lock()
		r.loggedIn = false
		if ctx.Err() == nil {
			r.loginFailedAt = r.now()
			r.loginFailed = err
		}
		r.mu.Unlock()
		return err
	}
	version, err := r.sessions.save(ctx, r.id, siteOrigin(r.site), r.http.exportCookies(r.site), r.http.userAgent())
	if err != nil {
		r.log.Warn("cardigann: could not save session", "indexer", r.id, "err", err)
	}
	r.mu.Lock()
	r.loggedIn, r.version = true, version
	r.loginFailedAt = time.Time{}
	r.loginFailed = nil
	r.mu.Unlock()
	return nil
}

func (r *Runner) doLogin(ctx context.Context) error {
	l := r.def.Login
	if l.Captcha && l.Method != "cookie" {
		return fmt.Errorf("%s: %w", r.def.Name, ErrCaptcha)
	}
	v := r.vars()
	hdr := r.headers(l.Headers, v)
	for _, c := range l.Cookies {
		r.http.setCookieString(r.site, mustRender(c, v))
	}
	var resp *response
	var err error
	switch l.Method {
	case "cookie":
		raw := ""
		for _, in := range l.Inputs {
			if in.Key == "cookie" {
				raw = mustRender(in.Value, v)
			}
		}
		if raw == "" {
			if c, ok := v["Config"].(map[string]any)["cookie"].(string); ok {
				raw = c
			}
		}
		if r.http.setCookieString(r.site, raw) == 0 {
			return fmt.Errorf("%s: %w: no cookie is set; paste the cookie from a signed-in browser", r.def.Name, ErrLogin)
		}
	case "post", "get":
		target, err := r.resolve(mustRender(l.Path, v), nil)
		if err != nil {
			return err
		}
		form, raw, err := r.renderInputs(l.Inputs, v, false)
		if err != nil {
			return err
		}
		method := http.MethodPost
		if l.Method == "get" {
			method = http.MethodGet
		}
		resp, err = r.http.do(ctx, request{method: method, url: target.String(), form: form, raw: raw, headers: hdr, follow: true})
		if err != nil {
			return err
		}
	case "oneurl":
		one := ""
		for _, in := range l.Inputs {
			if in.Key == "oneurl" {
				one = mustRender(in.Value, v)
			}
		}
		target, err := r.resolve(mustRender(l.Path, v)+one, nil)
		if err != nil {
			return err
		}
		resp, err = r.http.do(ctx, request{method: http.MethodGet, url: target.String(), headers: hdr, follow: true})
		if err != nil {
			return err
		}
	case "form":
		resp, err = r.formLogin(ctx, v, hdr)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("%s: login method %q is not supported", r.def.Name, l.Method)
	}
	if resp != nil {
		if resp.status >= 500 {
			return fmt.Errorf("%s: sign-in page returned HTTP %d", r.def.Name, resp.status)
		}
		if err := r.checkErrors(l.Error, resp, v); err != nil {
			return fmt.Errorf("%s: %w: %v", r.def.Name, ErrLogin, err)
		}
	}
	if err := r.testSession(ctx); err != nil {
		return err
	}
	return nil
}

// formLogin loads the login page, fills its form and submits it.
func (r *Runner) formLogin(ctx context.Context, v vars, hdr http.Header) (*response, error) {
	l := r.def.Login
	landingURL, err := r.resolve(mustRender(l.Path, v), nil)
	if err != nil {
		return nil, err
	}
	landing, err := r.http.do(ctx, request{method: http.MethodGet, url: landingURL.String(), headers: hdr, follow: true})
	if err != nil {
		return nil, err
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(r.http.text(landing)))
	if err != nil {
		return nil, err
	}
	formSel := l.Form
	if formSel == "" {
		formSel = "form"
	}
	m, err := compileCSS(formSel)
	if err != nil {
		return nil, err
	}
	form := doc.FindMatcher(m).First()
	if form.Length() == 0 {
		return nil, fmt.Errorf("%s: %w: the login form (%s) was not found on the sign-in page", r.def.Name, ErrLogin, formSel)
	}
	var fields []KV
	set := func(k, val string) {
		for i := range fields {
			if fields[i].Key == k {
				fields[i].Value = val
				return
			}
		}
		fields = append(fields, KV{Key: k, Value: val})
	}
	form.Find("input").Each(func(_ int, in *goquery.Selection) {
		name, ok := in.Attr("name")
		if !ok || name == "" {
			return
		}
		typ := strings.ToLower(in.AttrOr("type", "text"))
		if (typ == "checkbox" || typ == "radio") && in.AttrOr("checked", "\x00") == "\x00" {
			return
		}
		if typ == "submit" || typ == "image" || typ == "button" {
			return
		}
		set(name, in.AttrOr("value", ""))
	})
	env := r.env(v)
	for _, in := range l.Inputs {
		set(in.Key, mustRender(in.Value, v))
	}
	for _, si := range l.SelectorInputs {
		val, ok, err := htmlValue(si.Selector, doc.Selection, env)
		if err != nil || !ok {
			return nil, fmt.Errorf("%s: %w: login input %q was not found on the sign-in page", r.def.Name, ErrLogin, si.Name)
		}
		set(si.Name, val)
	}
	var query []KV
	for _, si := range l.GetSelectorInputs {
		val, ok, err := htmlValue(si.Selector, doc.Selection, env)
		if err != nil || !ok {
			return nil, fmt.Errorf("%s: %w: login input %q was not found on the sign-in page", r.def.Name, ErrLogin, si.Name)
		}
		query = append(query, KV{Key: si.Name, Value: val})
	}
	action := landing.url
	if l.SubmitPath != "" {
		if action, err = r.resolve(mustRender(l.SubmitPath, v), landing.url); err != nil {
			return nil, err
		}
	} else if a := strings.TrimSpace(form.AttrOr("action", "")); a != "" {
		if action, err = r.resolve(a, landing.url); err != nil {
			return nil, err
		}
	}
	if len(query) > 0 {
		q := r.http.encodeForm(query)
		if action.RawQuery != "" {
			action.RawQuery += "&" + q
		} else {
			action.RawQuery = q
		}
	}
	method := strings.ToUpper(form.AttrOr("method", "post"))
	if method != http.MethodGet {
		method = http.MethodPost
	}
	return r.http.do(ctx, request{method: method, url: action.String(), form: fields, headers: hdr, follow: true})
}

// checkErrors applies error blocks to a response.
func (r *Runner) checkErrors(blocks []ErrorBlock, resp *response, v vars) error {
	if len(blocks) == 0 {
		return nil
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(r.http.text(resp)))
	if err != nil {
		return nil
	}
	env := r.env(v)
	for _, e := range blocks {
		if e.Path != "" && (resp.url == nil || !strings.Contains(resp.url.String(), e.Path)) {
			continue
		}
		m, err := compileCSS(mustRender(e.Selector, v))
		if err != nil {
			return err
		}
		hit := doc.FindMatcher(m).First()
		if hit.Length() == 0 {
			continue
		}
		msg := normalizeSpace(hit.Text())
		if e.Message != nil {
			if val, ok, err := htmlValue(*e.Message, doc.Selection, env); err == nil && ok {
				msg = val
			}
		}
		if msg == "" {
			msg = "the site reported an error"
		}
		return errors.New(abbreviate(msg))
	}
	return nil
}

// testSession checks the session with the definition's login test.
func (r *Runner) testSession(ctx context.Context) error {
	t := r.def.Login.Test
	if t == nil {
		return nil
	}
	v := r.vars()
	target, err := r.resolve(mustRender(t.Path, v), nil)
	if err != nil {
		return err
	}
	resp, err := r.http.do(ctx, request{method: http.MethodGet, url: target.String(), headers: r.headers(r.def.Login.Headers, v), follow: r.def.FollowRedirect})
	if err != nil {
		return err
	}
	if resp.redirected() {
		return fmt.Errorf("%s: %w: the site redirected the session check (to %s); check the credentials", r.def.Name, ErrLogin, redactLink(resp.location))
	}
	if resp.status == http.StatusUnauthorized || resp.status == http.StatusForbidden {
		return fmt.Errorf("%s: %w: the session check returned HTTP %d", r.def.Name, ErrLogin, resp.status)
	}
	if t.Selector == "" {
		return nil
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(r.http.text(resp)))
	if err != nil {
		return err
	}
	m, err := compileCSS(mustRender(t.Selector, v))
	if err != nil {
		return err
	}
	if doc.FindMatcher(m).Length() == 0 {
		return fmt.Errorf("%s: %w: signed-in page check (%s) did not match; check the credentials", r.def.Name, ErrLogin, t.Selector)
	}
	return nil
}

// redactLink drops a link's query (it may carry passkeys).
func redactLink(link string) string {
	if i := strings.IndexAny(link, "?#"); i >= 0 {
		return link[:i]
	}
	return link
}
