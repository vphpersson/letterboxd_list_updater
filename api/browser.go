package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/altshiftab/utils_go/pkg/cdp"
	"github.com/altshiftab/utils_go/pkg/cdp/chrome"
	"github.com/altshiftab/utils_go/pkg/cdp/turnstile"
	altshiftErrors "github.com/altshiftab/utils_go/pkg/errors"
	"github.com/altshiftab/utils_go/pkg/errors/types/nil_error"
)

const (
	defaultChromePath = "google-chrome-stable"

	// openTimeout bounds a navigation and the challenge in front of it.
	openTimeout = 45 * time.Second
	// signInTimeout bounds the sign-in, its Turnstile and the redirect after.
	signInTimeout = 120 * time.Second
	// formReadyTimeout is how long the sign-in form is given to be enabled
	// before the page is loaded again: now and then the page's scripts never
	// get round to enabling it (seen 2026-10-09), and a reload does.
	formReadyTimeout = 20 * time.Second
	// signInAttempts is how many times the form is loaded and filled in.
	signInAttempts = 3
	// fetchTimeout bounds one in-page request.
	fetchTimeout = 60 * time.Second
	// diagnosticTimeout bounds what is asked of the page to explain a failure,
	// which may be the page no longer answering.
	diagnosticTimeout = 3 * time.Second
	pollInterval      = 250 * time.Millisecond

	// signedInCookieName is the cookie the site sets, to the username, for a
	// browser that has signed in.
	signedInCookieName = "letterboxd.signed.in.as"
	challengeTitle     = "just a moment"

	// nodeIdParameter names the node a DOM command is about.
	nodeIdParameter = "nodeId"

	signInForm       = "form.js-sign-in-form"
	usernameSelector = signInForm + ` input[name="username"]`
	passwordSelector = signInForm + ` input[name="password"]`
	rememberSelector = signInForm + ` input[name="remember"]`
	submitSelector   = signInForm + ` button[type="submit"]`
)

var reTag = regexp.MustCompile(`<[^>]*>`)

var (
	ErrNavigation  = errors.New("navigation failed")
	ErrSignInStuck = errors.New("sign-in did not complete")
	ErrEvaluation  = errors.New("evaluation failed")
	ErrNoElement   = errors.New("no such element")
)

// fetchScript makes the request from the page with fetch and answers with
// what came back. A multipart body is built as FormData, so the browser picks
// the boundary as it does for the site's own forms.
const fetchScript = `(async (p) => {
	const init = {method: p.method, headers: p.headers ?? {}, credentials: "include"};
	if (p.multipart) {
		const form = new FormData();
		for (const field of p.multipart) {
			if (field.filename) {
				form.append(field.name, new Blob([field.value], {type: field.type || "application/octet-stream"}), field.filename);
			} else {
				form.append(field.name, field.value);
			}
		}
		init.body = form;
	} else if (p.body) {
		init.body = p.body;
	}
	const response = await fetch(p.url, init);
	return {
		status: response.status,
		url: response.url,
		redirected: response.redirected,
		mitigated: response.headers.get("cf-mitigated") ?? "",
		body: await response.text(),
	};
})`

// cdpSession is a Chrome with one page, driven over the DevTools protocol.
type cdpSession struct {
	browser *chrome.Browser
	caller  cdp.Caller
	page    *chrome.Page

	// temporaryProfile is a profile made for this session alone, removed with it.
	temporaryProfile string
	// opened is the page last opened, to go back to after a challenge.
	opened string
	// formReadyTimeout overrides the default; zero means the default.
	formReadyTimeout time.Duration
}

// launchSession starts Chrome on the profile directory, or on a profile of its
// own when there is none, with a blank page to drive.
func launchSession(ctx context.Context, chromePath string, profileDirectory string) (*cdpSession, error) {
	session := &cdpSession{}

	if profileDirectory == "" {
		directory, err := os.MkdirTemp("", "letterboxd-chrome-")
		if err != nil {
			return nil, altshiftErrors.NewWithTrace(fmt.Errorf("os mkdir temp: %w", err))
		}
		profileDirectory = directory
		session.temporaryProfile = directory
	} else {
		if err := os.MkdirAll(profileDirectory, 0o700); err != nil {
			return nil, altshiftErrors.NewWithTrace(fmt.Errorf("os mkdir all: %w", err), profileDirectory)
		}
		// A Chrome killed rather than closed leaves its lock behind, naming a
		// host a restarted container no longer is; nothing else uses the
		// profile while the client's mutex is held.
		removeSingletons(profileDirectory)
	}

	browser, err := chrome.Launch(ctx, chromePath, profileDirectory, "--disk-cache-size=33554432")
	if err != nil {
		session.Close(ctx)
		return nil, fmt.Errorf("chrome launch: %w", err)
	}
	session.browser = browser
	session.caller = browser.Conn

	page, err := browser.NewPage(ctx)
	if err != nil {
		session.Close(ctx)
		return nil, fmt.Errorf("new page: %w", err)
	}
	session.page = page

	return session, nil
}

func removeSingletons(profileDirectory string) {
	paths, _ := filepath.Glob(filepath.Join(profileDirectory, "Singleton*"))
	for _, path := range paths {
		_ = os.Remove(path)
	}
}

func (s *cdpSession) Close(ctx context.Context) {
	if s.browser != nil {
		s.browser.Close(ctx)
	}
	if s.temporaryProfile != "" {
		_ = os.RemoveAll(s.temporaryProfile)
	}
}

func (s *cdpSession) sessionId() string {
	if s.page == nil {
		return ""
	}
	return s.page.SessionId
}

// target returns the page's title and URL, as the browser reports them from
// outside the page.
func (s *cdpSession) target(ctx context.Context) (string, string, error) {
	if s.page == nil {
		return "", "", altshiftErrors.NewWithTrace(nil_error.New("page"))
	}

	var info struct {
		TargetInfo *struct {
			Title string `json:"title"`
			Url   string `json:"url"`
		} `json:"targetInfo"`
	}
	if err := s.caller.Call(ctx, "", "Target.getTargetInfo", map[string]any{"targetId": s.page.TargetId}, &info); err != nil {
		return "", "", fmt.Errorf("get target info: %w", err)
	}
	if info.TargetInfo == nil {
		return "", "", nil
	}
	return info.TargetInfo.Title, info.TargetInfo.Url, nil
}

// onSite reports whether the page is one of the site's own, past any
// challenge: titled, not Cloudflare's "Just a moment...", and on the site.
func (s *cdpSession) onSite(ctx context.Context) (bool, error) {
	title, pageUrl, err := s.target(ctx)
	if err != nil {
		return false, err
	}
	return title != "" && !strings.Contains(strings.ToLower(title), challengeTitle) && strings.HasPrefix(pageUrl, BaseUrl+"/"), nil
}

// Open navigates the page and waits until it is the site's rather than a
// challenge's, clicking Turnstile's checkbox should it ask.
func (s *cdpSession) Open(ctx context.Context, pageUrl string) error {
	if s.caller == nil {
		return altshiftErrors.NewWithTrace(nil_error.New("caller"))
	}

	openCtx, cancel := context.WithTimeout(ctx, openTimeout)
	defer cancel()

	var navigated struct {
		ErrorText string `json:"errorText"`
	}
	if err := s.caller.Call(openCtx, s.sessionId(), "Page.navigate", map[string]any{"url": pageUrl}, &navigated); err != nil {
		return fmt.Errorf("page navigate: %w", err)
	}
	if navigated.ErrorText != "" {
		return altshiftErrors.NewWithTrace(fmt.Errorf("%w: %s", ErrNavigation, navigated.ErrorText), pageUrl)
	}

	if err := turnstile.Solve(openCtx, s.caller, s.sessionId(), s.onSite); err != nil {
		diagnosticCtx, cancel := diagnosticContext(ctx)
		defer cancel()
		title, currentUrl, _ := s.target(diagnosticCtx)
		return altshiftErrors.NewWithTrace(fmt.Errorf("turnstile solve: %w", err), pageUrl, title, currentUrl)
	}

	s.opened = pageUrl
	return nil
}

// SignedIn reports whether the browser holds the site's signed-in cookie.
func (s *cdpSession) SignedIn(ctx context.Context) (bool, error) {
	var cookies struct {
		Cookies []*struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"cookies"`
	}
	if err := s.caller.Call(ctx, s.sessionId(), "Network.getCookies", map[string]any{"urls": []string{BaseUrl + "/"}}, &cookies); err != nil {
		return false, fmt.Errorf("get cookies: %w", err)
	}

	for _, cookie := range cookies.Cookies {
		if cookie != nil && cookie.Name == signedInCookieName && cookie.Value != "" {
			return true, nil
		}
	}
	return false, nil
}

// SignIn fills in the sign-in form as a person would -- remembered, so the
// session outlives this Chrome -- waits for the form to enable the button,
// clicks it, and waits to be sent on. A form that is never enabled is loaded
// again, a few times.
func (s *cdpSession) SignIn(ctx context.Context, username string, password string) error {
	signInCtx, cancel := context.WithTimeout(ctx, signInTimeout)
	defer cancel()

	for attempt := 1; ; attempt++ {
		err := s.fillSignInForm(signInCtx, username, password)
		if err == nil {
			break
		}
		if !errors.Is(err, turnstile.ErrUnsolved) || attempt >= signInAttempts || signInCtx.Err() != nil {
			return err
		}
		slog.WarnContext(ctx, "The sign-in form was not enabled; loading it again.", slog.Int("attempt", attempt))
	}

	if err := s.clickOn(signInCtx, submitSelector); err != nil {
		return fmt.Errorf("click submit: %w", err)
	}

	for {
		_, pageUrl, err := s.target(signInCtx)
		if err != nil {
			return err
		}
		if strings.HasPrefix(pageUrl, BaseUrl+"/") && !strings.Contains(pageUrl, "/sign-in") {
			break
		}

		select {
		case <-signInCtx.Done():
			diagnosticCtx, cancel := diagnosticContext(ctx)
			defer cancel()
			return altshiftErrors.NewWithTrace(
				fmt.Errorf("%w: %w: %s", ErrSignInStuck, signInCtx.Err(), s.formExcerpt(diagnosticCtx)),
				pageUrl,
			)
		case <-time.After(pollInterval):
		}
	}

	signedIn, err := s.SignedIn(signInCtx)
	if err != nil {
		return err
	}
	if !signedIn {
		_, pageUrl, _ := s.target(signInCtx)
		return altshiftErrors.NewWithTrace(fmt.Errorf("%w: no %s cookie", ErrSignInStuck, signedInCookieName), pageUrl)
	}

	slog.InfoContext(ctx, "Signed in to Letterboxd.")
	return nil
}

// fillSignInForm loads the sign-in page, fills in the form and waits for its
// button to be enabled, which the page's scripts do once the form is ready.
// The form's Turnstile asks for its token on submit, and is clicked through
// should it show a checkbox meanwhile.
func (s *cdpSession) fillSignInForm(ctx context.Context, username string, password string) error {
	if err := s.Open(ctx, BaseUrl+"/sign-in/"); err != nil {
		return fmt.Errorf("open sign-in page: %w", err)
	}

	if err := s.typeInto(ctx, usernameSelector, username); err != nil {
		return fmt.Errorf("type username: %w", err)
	}
	if err := s.typeInto(ctx, passwordSelector, password); err != nil {
		return fmt.Errorf("type password: %w", err)
	}

	remember, err := s.waitFor(ctx, rememberSelector)
	if err != nil {
		return fmt.Errorf("wait for remember: %w", err)
	}
	attributes, err := s.attributes(ctx, remember)
	if err != nil {
		return fmt.Errorf("remember attributes: %w", err)
	}
	if _, checked := attributes["checked"]; !checked {
		if err := s.clickOn(ctx, rememberSelector); err != nil {
			return fmt.Errorf("click remember: %w", err)
		}
	}

	timeout := s.formReadyTimeout
	if timeout <= 0 {
		timeout = formReadyTimeout
	}
	readyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := turnstile.Solve(readyCtx, s.caller, s.sessionId(), s.submitEnabled); err != nil {
		diagnosticCtx, cancel := diagnosticContext(ctx)
		defer cancel()
		return fmt.Errorf("turnstile solve: %w: %s", err, s.formExcerpt(diagnosticCtx))
	}
	return nil
}

// diagnosticContext is a context for explaining a failure: one that outlives
// the deadline that failed, briefly.
func diagnosticContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), diagnosticTimeout)
}

// submitEnabled reports whether the sign-in button can be pressed.
func (s *cdpSession) submitEnabled(ctx context.Context) (bool, error) {
	node, err := s.query(ctx, submitSelector)
	if err != nil || node == 0 {
		return false, err
	}
	attributes, err := s.attributes(ctx, node)
	if err != nil {
		return false, err
	}
	_, disabled := attributes["disabled"]
	return !disabled, nil
}

// formExcerpt is the sign-in form's text, which carries the site's message
// when a sign-in fails; never its values, which only ever live in the page.
func (s *cdpSession) formExcerpt(ctx context.Context) string {
	node, err := s.query(ctx, signInForm)
	if err != nil || node == 0 {
		return "(no sign-in form)"
	}

	var outer struct {
		OuterHtml string `json:"outerHTML"`
	}
	if err := s.caller.Call(ctx, s.sessionId(), "DOM.getOuterHTML", map[string]any{nodeIdParameter: node}, &outer); err != nil {
		return "(sign-in form unreadable)"
	}
	return excerpt(reTag.ReplaceAllString(outer.OuterHtml, " "))
}

// query returns the node the selector matches first, or 0 when it matches
// none. Node ids last until the next query, which asks for the document anew.
func (s *cdpSession) query(ctx context.Context, selector string) (int64, error) {
	var document struct {
		Root *struct {
			NodeId int64 `json:"nodeId"`
		} `json:"root"`
	}
	if err := s.caller.Call(ctx, s.sessionId(), "DOM.getDocument", map[string]any{"depth": 0}, &document); err != nil {
		return 0, fmt.Errorf("get document: %w", err)
	}
	if document.Root == nil {
		return 0, nil
	}

	var found struct {
		NodeId int64 `json:"nodeId"`
	}
	if err := s.caller.Call(ctx, s.sessionId(), "DOM.querySelector", map[string]any{nodeIdParameter: document.Root.NodeId, "selector": selector}, &found); err != nil {
		return 0, fmt.Errorf("query selector: %w", err)
	}
	return found.NodeId, nil
}

// waitFor returns the node the selector matches, once the page has one.
func (s *cdpSession) waitFor(ctx context.Context, selector string) (int64, error) {
	for {
		node, err := s.query(ctx, selector)
		// A page between documents has none to query for a moment.
		if err != nil && !cdp.IsTransient(err) {
			return 0, err
		}
		if node != 0 {
			return node, nil
		}

		select {
		case <-ctx.Done():
			return 0, altshiftErrors.NewWithTrace(fmt.Errorf("%w: %w", ErrNoElement, ctx.Err()), selector)
		case <-time.After(pollInterval):
		}
	}
}

func (s *cdpSession) attributes(ctx context.Context, node int64) (map[string]string, error) {
	var found struct {
		Attributes []string `json:"attributes"`
	}
	if err := s.caller.Call(ctx, s.sessionId(), "DOM.getAttributes", map[string]any{nodeIdParameter: node}, &found); err != nil {
		return nil, fmt.Errorf("get attributes: %w", err)
	}

	attributes := make(map[string]string, len(found.Attributes)/2)
	for index := 0; index+1 < len(found.Attributes); index += 2 {
		attributes[found.Attributes[index]] = found.Attributes[index+1]
	}
	return attributes, nil
}

// typeInto focuses the field and enters the text, as a paste would.
func (s *cdpSession) typeInto(ctx context.Context, selector string, text string) error {
	node, err := s.waitFor(ctx, selector)
	if err != nil {
		return err
	}
	if err := s.caller.Call(ctx, s.sessionId(), "DOM.focus", map[string]any{nodeIdParameter: node}, nil); err != nil {
		return fmt.Errorf("focus: %w", err)
	}
	cdp.Pause(ctx, 150*time.Millisecond, 250*time.Millisecond)
	if err := s.caller.Call(ctx, s.sessionId(), "Input.insertText", map[string]any{"text": text}, nil); err != nil {
		return fmt.Errorf("insert text: %w", err)
	}
	cdp.Pause(ctx, 200*time.Millisecond, 300*time.Millisecond)
	return nil
}

// clickOn scrolls the element into view and clicks its middle.
func (s *cdpSession) clickOn(ctx context.Context, selector string) error {
	node, err := s.waitFor(ctx, selector)
	if err != nil {
		return err
	}
	if err := s.caller.Call(ctx, s.sessionId(), "DOM.scrollIntoViewIfNeeded", map[string]any{nodeIdParameter: node}, nil); err != nil {
		return fmt.Errorf("scroll into view: %w", err)
	}

	var model struct {
		Model *struct {
			Content []float64 `json:"content"`
		} `json:"model"`
	}
	if err := s.caller.Call(ctx, s.sessionId(), "DOM.getBoxModel", map[string]any{nodeIdParameter: node}, &model); err != nil {
		return fmt.Errorf("get box model: %w", err)
	}
	if model.Model == nil || len(model.Model.Content) != 8 {
		return altshiftErrors.NewWithTrace(fmt.Errorf("%w: no box", ErrNoElement), selector)
	}

	x, y := quadCenter(model.Model.Content)
	return cdp.Click(ctx, s.caller, s.sessionId(), x, y)
}

// quadCenter is the middle of a box model quad, four corners clockwise.
func quadCenter(quad []float64) (float64, float64) {
	return (quad[0] + quad[2] + quad[4] + quad[6]) / 4, (quad[1] + quad[3] + quad[5] + quad[7]) / 4
}

// Fetch makes the request from the page. A request Cloudflare answers with a
// challenge never reached the site, so after reopening the page past the
// challenge it is made once more.
func (s *cdpSession) Fetch(ctx context.Context, request *FetchRequest) (*FetchResponse, error) {
	response, err := s.evaluateFetch(ctx, request)
	if err != nil || response.Mitigated == "" || s.opened == "" {
		return response, err
	}

	slog.InfoContext(ctx, "A request was challenged; reopening the page.", slog.String("url", request.Url))
	if err := s.Open(ctx, s.opened); err != nil {
		return nil, fmt.Errorf("reopen: %w", err)
	}
	return s.evaluateFetch(ctx, request)
}

func (s *cdpSession) evaluateFetch(ctx context.Context, request *FetchRequest) (*FetchResponse, error) {
	if s.caller == nil {
		return nil, altshiftErrors.NewWithTrace(nil_error.New("caller"))
	}

	expression, err := fetchExpression(request)
	if err != nil {
		return nil, err
	}

	fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	var evaluated json.RawMessage
	params := map[string]any{"expression": expression, "awaitPromise": true, "returnByValue": true}
	if err := s.caller.Call(fetchCtx, s.sessionId(), "Runtime.evaluate", params, &evaluated); err != nil {
		return nil, fmt.Errorf("runtime evaluate: %w", err)
	}

	return decodeFetchResult(evaluated)
}

// fetchExpression is the script making the request. The request goes in as a
// JSON literal, which is a JavaScript expression whatever it holds.
func fetchExpression(request *FetchRequest) (string, error) {
	if request == nil {
		return "", altshiftErrors.NewWithTrace(nil_error.New("request"))
	}

	data, err := json.Marshal(request)
	if err != nil {
		return "", altshiftErrors.NewWithTrace(fmt.Errorf("json marshal: %w", err))
	}
	return fetchScript + "(" + string(data) + ")", nil
}

// decodeFetchResult reads what Runtime.evaluate answered: the response the
// script returned, or the exception it threw.
func decodeFetchResult(data json.RawMessage) (*FetchResponse, error) {
	var evaluated struct {
		Result *struct {
			Type  string          `json:"type"`
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception *struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(data, &evaluated); err != nil {
		return nil, altshiftErrors.NewWithTrace(fmt.Errorf("json unmarshal: %w", err))
	}

	if details := evaluated.ExceptionDetails; details != nil {
		message := details.Text
		if details.Exception != nil && details.Exception.Description != "" {
			message = details.Exception.Description
		}
		return nil, altshiftErrors.NewWithTrace(fmt.Errorf("%w: %s", ErrEvaluation, message))
	}
	if evaluated.Result == nil || evaluated.Result.Type != "object" || len(evaluated.Result.Value) == 0 {
		return nil, altshiftErrors.NewWithTrace(fmt.Errorf("%w: no response object", ErrEvaluation))
	}

	var response FetchResponse
	if err := json.Unmarshal(evaluated.Result.Value, &response); err != nil {
		return nil, altshiftErrors.NewWithTrace(fmt.Errorf("json unmarshal: %w", err))
	}
	return &response, nil
}
