package api

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/altshiftab/utils_go/pkg/cdp"
	"github.com/altshiftab/utils_go/pkg/cdp/chrome"
	"github.com/altshiftab/utils_go/pkg/cdp/turnstile"
	altshiftTestingCmp "github.com/altshiftab/utils_go/pkg/testing/cmp"
)

const (
	usernameNode int64 = 11
	passwordNode int64 = 12
	rememberNode int64 = 13
	submitNode   int64 = 14
	formNode     int64 = 15
)

// fakePage answers the DevTools commands a session sends the way Chrome does
// for a page on the site: a challenge title for a while after a navigation,
// the sign-in form with its button disabled until the form's Turnstile has a
// token, and a press of the button signing in when the password is right.
type fakePage struct {
	// challengePolls is how many title reads a navigation shows the challenge for.
	challengePolls int
	// tokenPolls is how many reads the sign-in button stays disabled for.
	tokenPolls int
	// rememberChecked is the remember box rendered checked.
	rememberChecked bool
	// stuckLoads is how many loads of the sign-in page never enable its button.
	stuckLoads    int
	password      string
	navigateError string
	evaluations   []json.RawMessage
	// gone answers DOM commands as Chrome does for a closed page.
	gone bool

	mutex       sync.Mutex
	title       string
	url         string
	titleReads  int
	submitReads int
	signInLoads int
	signedIn    bool
	focused     int64
	typed       map[int64]string
	presses     [][2]float64
	navigations []string
	sessions    []string
	evaluated   []string
}

func (f *fakePage) Call(_ context.Context, sessionId string, method string, params any, result any) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	if sessionId != "" {
		f.sessions = append(f.sessions, sessionId)
	}
	values, _ := params.(map[string]any)

	var response any
	switch method {
	case "Page.navigate":
		f.url, _ = values["url"].(string)
		f.navigations = append(f.navigations, f.url)
		f.titleReads = 0
		f.title = "Letterboxd"
		if strings.HasSuffix(f.url, "/sign-in/") {
			f.title = "Sign in • Letterboxd"
			f.signInLoads++
			f.submitReads = 0
			f.typed = nil
		}
		response = map[string]any{"frameId": "frame-1", "errorText": f.navigateError}
	case "Target.getTargetInfo":
		f.titleReads++
		title := f.title
		if f.titleReads <= f.challengePolls {
			title = "Just a moment..."
		}
		response = map[string]any{"targetInfo": map[string]any{"title": title, "url": f.url}}
	case "Network.getCookies":
		cookies := []map[string]any{{"name": "cf_clearance", "value": "x"}}
		if f.signedIn {
			cookies = append(cookies, map[string]any{"name": signedInCookieName, "value": "vph"})
		}
		response = map[string]any{"cookies": cookies}
	case "DOM.getDocument":
		if f.gone {
			return &cdp.ProtocolError{Code: -32001, Message: "Session with given id not found."}
		}
		response = map[string]any{"root": map[string]any{"nodeId": 1, "nodeName": "#document"}}
	case "DOM.querySelector":
		nodes := map[string]int64{
			usernameSelector: usernameNode,
			passwordSelector: passwordNode,
			rememberSelector: rememberNode,
			submitSelector:   submitNode,
			signInForm:       formNode,
		}
		node := int64(0)
		if strings.HasSuffix(f.url, "/sign-in/") {
			node = nodes[values["selector"].(string)]
		}
		response = map[string]any{"nodeId": node}
	case "DOM.getAttributes":
		attributes := []string{"class", "x"}
		switch values["nodeId"] {
		case submitNode:
			f.submitReads++
			if f.signInLoads <= f.stuckLoads || f.submitReads <= f.tokenPolls {
				attributes = append(attributes, "disabled", "")
			}
		case rememberNode:
			if f.rememberChecked {
				attributes = append(attributes, "checked", "")
			}
		}
		response = map[string]any{"attributes": attributes}
	case "DOM.focus":
		f.focused, _ = values["nodeId"].(int64)
	case "Input.insertText":
		if f.typed == nil {
			f.typed = make(map[int64]string)
		}
		text, _ := values["text"].(string)
		f.typed[f.focused] += text
	case "DOM.getBoxModel":
		// Each node a 100x20 box, a row apart.
		top := float64(values["nodeId"].(int64)) * 30
		response = map[string]any{"model": map[string]any{"content": []float64{100, top, 200, top, 200, top + 20, 100, top + 20}}}
	case "Input.dispatchMouseEvent":
		if values["type"] == "mousePressed" {
			x, _ := values["x"].(float64)
			y, _ := values["y"].(float64)
			f.presses = append(f.presses, [2]float64{x, y})
			submitTop := float64(submitNode) * 30
			if y >= submitTop && y <= submitTop+20 && f.typed[passwordNode] == f.password {
				f.signedIn = true
				f.url = BaseUrl + "/"
				f.title = "Letterboxd"
			}
		}
	case "DOM.getOuterHTML":
		response = map[string]any{"outerHTML": `<form class="js-sign-in-form"><p class="error">Your credentials don’t match.</p><input name="password" value=""></form>`}
	case "Runtime.evaluate":
		expression, _ := values["expression"].(string)
		f.evaluated = append(f.evaluated, expression)
		if len(f.evaluations) == 0 {
			return &cdp.ProtocolError{Code: -32000, Message: "nothing to evaluate"}
		}
		next := f.evaluations[0]
		f.evaluations = f.evaluations[1:]
		return json.Unmarshal(next, result)
	}

	if result == nil || response == nil {
		return nil
	}
	data, err := json.Marshal(response)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, result)
}

func newTestSession(page *fakePage) *cdpSession {
	return &cdpSession{caller: page, page: &chrome.Page{TargetId: "target-1", SessionId: "session-1"}, formReadyTimeout: 2 * time.Second}
}

func TestOpen(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		page    *fakePage
		timeout time.Duration
		wantIs  error
	}{
		{name: "let through", page: &fakePage{}, timeout: 5 * time.Second},
		{name: "after the challenge", page: &fakePage{challengePolls: 2}, timeout: 10 * time.Second},
		{name: "never past the challenge", page: &fakePage{challengePolls: 1000}, timeout: 2 * time.Second, wantIs: turnstile.ErrUnsolved},
		{name: "navigation failed", page: &fakePage{navigateError: "net::ERR_NAME_NOT_RESOLVED"}, timeout: 5 * time.Second, wantIs: ErrNavigation},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithTimeout(t.Context(), testCase.timeout)
			defer cancel()

			session := newTestSession(testCase.page)
			err := session.Open(ctx, BaseUrl+editPath)
			if !errors.Is(err, testCase.wantIs) {
				t.Fatalf("expected %v, got %v", testCase.wantIs, err)
			}
			if testCase.wantIs == nil && session.opened != BaseUrl+editPath {
				t.Errorf("unexpected opened page: %q", session.opened)
			}
		})
	}
}

func TestSignIn(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name         string
		page         *fakePage
		password     string
		timeout      time.Duration
		wantPresses  int
		wantLoads    int
		wantIs       error
		wantInReason string
	}{
		{
			name:        "remembered, after the form's token",
			page:        &fakePage{password: "secret", tokenPolls: 2},
			password:    "secret",
			timeout:     30 * time.Second,
			wantPresses: 2,
		},
		{
			name:        "remember already checked",
			page:        &fakePage{password: "secret", rememberChecked: true},
			password:    "secret",
			timeout:     30 * time.Second,
			wantPresses: 1,
		},
		{
			name:        "a form not enabled until loaded again",
			page:        &fakePage{password: "secret", stuckLoads: 1},
			password:    "secret",
			timeout:     30 * time.Second,
			wantLoads:   2,
			wantPresses: 3,
		},
		{
			name:     "a form never enabled",
			page:     &fakePage{password: "secret", stuckLoads: 1000},
			password: "secret",
			timeout:  30 * time.Second,
			// The remember box on each load: the button is never pressed disabled.
			wantLoads:   signInAttempts,
			wantPresses: signInAttempts,
			wantIs:      turnstile.ErrUnsolved,
		},
		{
			name:         "refused",
			page:         &fakePage{password: "secret"},
			password:     "wrong",
			timeout:      3 * time.Second,
			wantPresses:  2,
			wantIs:       ErrSignInStuck,
			wantInReason: "credentials don’t match",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithTimeout(t.Context(), testCase.timeout)
			defer cancel()

			page := testCase.page
			err := newTestSession(page).SignIn(ctx, "vph", testCase.password)
			if !errors.Is(err, testCase.wantIs) {
				t.Fatalf("expected %v, got %v", testCase.wantIs, err)
			}
			if testCase.wantInReason != "" && (err == nil || !strings.Contains(err.Error(), testCase.wantInReason)) {
				t.Errorf("the reason is missing from %v", err)
			}
			if err != nil && strings.Contains(err.Error(), testCase.password) {
				t.Errorf("the password is in the error: %v", err)
			}

			page.mutex.Lock()
			defer page.mutex.Unlock()

			expectedTyped := map[int64]string{usernameNode: "vph", passwordNode: testCase.password}
			if diff := altshiftTestingCmp.Diff(expectedTyped, page.typed); diff != "" {
				t.Errorf("typed mismatch (-expected +got):\n%s", diff)
			}
			if len(page.presses) != testCase.wantPresses {
				t.Errorf("unexpected presses: %v", page.presses)
			}
			wantLoads := max(testCase.wantLoads, 1)
			if len(page.navigations) != wantLoads || !slices.ContainsFunc(page.navigations, func(navigation string) bool { return navigation == BaseUrl+"/sign-in/" }) {
				t.Errorf("unexpected navigations: %v", page.navigations)
			}
			for _, session := range page.sessions {
				if session != "session-1" {
					t.Errorf("a command was sent outside the page's session: %q", session)
				}
			}
		})
	}
}

func TestSignedIn(t *testing.T) {
	t.Parallel()

	for _, signedIn := range []bool{true, false} {
		got, err := newTestSession(&fakePage{signedIn: signedIn}).SignedIn(t.Context())
		if err != nil || got != signedIn {
			t.Errorf("signed in %v: got %v %v", signedIn, got, err)
		}
	}
}

func evaluation(t *testing.T, value any) json.RawMessage {
	t.Helper()

	data, err := json.Marshal(map[string]any{"result": map[string]any{"type": "object", "value": value}})
	if err != nil {
		t.Fatalf("json marshal: %v", err)
	}
	return data
}

func TestFetch(t *testing.T) {
	t.Parallel()

	ok := map[string]any{"status": 200, "url": BaseUrl + editPath, "redirected": false, "mitigated": "", "body": "<html>"}
	challenged := map[string]any{"status": 403, "url": BaseUrl + editPath, "mitigated": "challenge", "body": "Just a moment..."}
	expectedOk := &FetchResponse{Status: 200, Url: BaseUrl + editPath, Body: "<html>"}

	testCases := []struct {
		name            string
		opened          string
		evaluations     []any
		expected        *FetchResponse
		wantNavigations int
	}{
		{name: "answered", opened: BaseUrl + editPath, evaluations: []any{ok}, expected: expectedOk},
		{name: "challenged once", opened: BaseUrl + editPath, evaluations: []any{challenged, ok}, expected: expectedOk, wantNavigations: 1},
		{
			name:            "challenged twice",
			opened:          BaseUrl + editPath,
			evaluations:     []any{challenged, challenged},
			expected:        &FetchResponse{Status: 403, Url: BaseUrl + editPath, Mitigated: "challenge", Body: "Just a moment..."},
			wantNavigations: 1,
		},
		{
			name:        "challenged before any page was opened",
			evaluations: []any{challenged},
			expected:    &FetchResponse{Status: 403, Url: BaseUrl + editPath, Mitigated: "challenge", Body: "Just a moment..."},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			page := &fakePage{}
			for _, value := range testCase.evaluations {
				page.evaluations = append(page.evaluations, evaluation(t, value))
			}
			session := newTestSession(page)
			session.opened = testCase.opened

			response, err := session.Fetch(t.Context(), &FetchRequest{Method: "GET", Url: BaseUrl + editPath})
			if err != nil {
				t.Fatalf("fetch: %v", err)
			}
			if diff := altshiftTestingCmp.Diff(testCase.expected, response); diff != "" {
				t.Errorf("response mismatch (-expected +got):\n%s", diff)
			}
			if len(page.navigations) != testCase.wantNavigations {
				t.Errorf("unexpected navigations: %v", page.navigations)
			}
		})
	}
}

func TestFetchExpression(t *testing.T) {
	t.Parallel()

	request := &FetchRequest{
		Method:  "POST",
		Url:     BaseUrl + importPath,
		Headers: map[string]string{"X-CSRF-TOKEN": "t"},
		Multipart: []*FormField{
			{Name: "notes", Value: "</script><script>alert(1)</script>  '\"`${x}`"},
			{Name: "file", Value: "imdbID\ntt1\n", Filename: "import.csv", Type: "text/csv"},
		},
	}

	expression, err := fetchExpression(request)
	if err != nil {
		t.Fatalf("fetch expression: %v", err)
	}
	if !strings.HasPrefix(expression, fetchScript+"(") || !strings.HasSuffix(expression, ")") {
		t.Fatalf("not a call of the script: %q", expression)
	}

	// The argument is JSON, which is a JavaScript expression whatever it holds,
	// and decodes to the request it was made from.
	argument := strings.TrimSuffix(strings.TrimPrefix(expression, fetchScript+"("), ")")
	var decoded FetchRequest
	if err := json.Unmarshal([]byte(argument), &decoded); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	if diff := altshiftTestingCmp.Diff(request, &decoded); diff != "" {
		t.Errorf("request mismatch (-expected +got):\n%s", diff)
	}

	if _, err := fetchExpression(nil); err == nil {
		t.Error("expected an error for a nil request")
	}
}

func TestDecodeFetchResult(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		data     string
		expected *FetchResponse
		wantIs   error
	}{
		{
			name:     "a response",
			data:     `{"result": {"type": "object", "value": {"status": 302, "url": "https://letterboxd.com/x", "redirected": true, "mitigated": "", "body": "b"}}}`,
			expected: &FetchResponse{Status: 302, Url: "https://letterboxd.com/x", Redirected: true, Body: "b"},
		},
		{
			name:   "an exception",
			data:   `{"result": {"type": "object", "subtype": "error"}, "exceptionDetails": {"text": "Uncaught", "exception": {"description": "TypeError: Failed to fetch"}}}`,
			wantIs: ErrEvaluation,
		},
		{name: "not an object", data: `{"result": {"type": "undefined"}}`, wantIs: ErrEvaluation},
		{name: "no result", data: `{}`, wantIs: ErrEvaluation},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			response, err := decodeFetchResult(json.RawMessage(testCase.data))
			if testCase.wantIs != nil {
				if !errors.Is(err, testCase.wantIs) {
					t.Fatalf("expected %v, got %v", testCase.wantIs, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if diff := altshiftTestingCmp.Diff(testCase.expected, response); diff != "" {
				t.Errorf("response mismatch (-expected +got):\n%s", diff)
			}
		})
	}

	// The exception's description is what the error carries.
	_, err := decodeFetchResult(json.RawMessage(testCases[1].data))
	if err == nil || !strings.Contains(err.Error(), "Failed to fetch") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestQuadCenter(t *testing.T) {
	t.Parallel()

	x, y := quadCenter([]float64{100, 420, 200, 420, 200, 440, 100, 440})
	if x != 150 || y != 430 {
		t.Errorf("unexpected center: %v %v", x, y)
	}
}

func TestRemoveSingletons(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	for _, name := range []string{"SingletonLock", "SingletonCookie", "SingletonSocket", "Local State"} {
		if err := os.WriteFile(filepath.Join(directory, name), nil, 0o600); err != nil {
			t.Fatalf("write file: %v", err)
		}
	}

	removeSingletons(directory)

	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var left []string
	for _, entry := range entries {
		left = append(left, entry.Name())
	}
	if !slices.Equal(left, []string{"Local State"}) {
		t.Errorf("unexpected files left: %v", left)
	}
}

// A page that is gone ends the wait for an element at once.
func TestWaitForGone(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	_, err := newTestSession(&fakePage{gone: true}).waitFor(ctx, submitSelector)
	if _, ok := errors.AsType[*cdp.ProtocolError](err); !ok || ctx.Err() != nil {
		t.Fatalf("expected a protocol error at once, got %v", err)
	}
}

// A profile made for a session is removed when Chrome does not start.
func TestLaunchSessionRemovesTemporaryProfile(t *testing.T) {
	temporary := t.TempDir()
	t.Setenv("TMPDIR", temporary)

	if _, err := launchSession(t.Context(), filepath.Join(t.TempDir(), "no-chrome"), ""); err == nil {
		t.Fatal("expected a failure to start chrome")
	}

	entries, err := os.ReadDir(temporary)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the temporary profile was left behind: %v", entries)
	}
}

func TestLaunchSessionFailure(t *testing.T) {
	t.Parallel()

	profile := filepath.Join(t.TempDir(), "profile")
	if _, err := launchSession(t.Context(), filepath.Join(t.TempDir(), "no-chrome"), profile); err == nil {
		t.Fatal("expected a failure to start chrome")
	}
	// The profile directory is made for Chrome before it is started.
	if info, err := os.Stat(profile); err != nil || !info.IsDir() {
		t.Errorf("no profile directory: %v", err)
	}

	if _, err := launchSession(t.Context(), filepath.Join(t.TempDir(), "no-chrome"), ""); err == nil {
		t.Fatal("expected a failure to start chrome")
	}
}
