package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	altshiftErrors "github.com/altshiftab/utils_go/pkg/errors"
	"github.com/altshiftab/utils_go/pkg/errors/types/empty_error"
	"github.com/altshiftab/utils_go/pkg/errors/types/nil_error"
	altshiftTestingCmp "github.com/altshiftab/utils_go/pkg/testing/cmp"
	"github.com/vphpersson/letterboxd_list_updater/api/types"
)

var errBrowserCrashed = errors.New("browser crashed")

const (
	testUser     = "vph"
	testSlug     = "collected"
	testLid      = "TZx9e"
	testListId   = "82736391"
	testCsrf     = "c45ff41db9e6df185c8a"
	stagedCsrf   = "d56aa52eac7fe0296d9b"
	testCsv      = "imdbID\ntt6751668\ntt0081505\n"
	editPath     = "/vph/list/collected/edit/"
	importPath   = "/import/list/"
	matchPath    = "/import/watchlist/match-import-film/"
	stagePath    = "/list/add-films/"
	savePath     = "/api/v0/list/TZx9e"
	savedAnswer  = `{"data": {"id": "TZx9e", "version": 25}, "messages": []}`
	matchedItems = `<div class="matched-item"><input type="hidden" name="importProductionId" value="hTha"/><div class="import-match"><input type="hidden" name="shouldImportProduction" value="true" /></div></div>` +
		`<div class="matched-item"><input type="hidden" name="importProductionId" value="ZpZu"/><div class="import-match"><input type="hidden" name="shouldImportProduction" value="true" /></div></div>`
)

// editorPage is the edit page as the site renders it, the editor's state in
// the attributes of the element it is drawn into.
func editorPage(csrf string, updates string, extra string) string {
	return fmt.Sprintf(`<!DOCTYPE html><html><head><script>
	var x = {
		supermodelCSRF = "",
	};
supermodelCSRF = '%s'; geolocation.country = 'SE';</script></head><body>
<div
	data-component-class="ListEditor"
	class="react-component list-editor-wrapper"
		data-list-lid="%s"
		data-list-url="/vph/list/collected/"
		data-list-version="24"
	data-list-name="Collected"
	data-list-description=""
	data-list-type="private"
	data-list-share-policy="You"
	data-list-tags='[]'
	%s
	data-initial-list-updates='%s'
	data-member-lists-url="/vph/lists/"
	data-list-import-url="/import/list/"
>
</div></body></html>`, csrf, testLid, extra, updates)
}

const importerPage = `<form method="POST" action="/list/add-films/" id="importer-matched-form" style="display: none" data-url="/import/watchlist/match-import-film/"><input type="hidden" name="__csrf" value="` + testCsrf + `">
	<input type="hidden" name="importListDetails" value="true">
	<input type="hidden" name="filmListId" value="` + testListId + `">
	<input type="hidden" name="name" value="Collected">
	<input type="hidden" name="notes" value="">
	<input type="hidden" name="publicList" value="false">
	<input type="hidden" name="sharePolicy" value="You">
	<input type="hidden" name="numberedList" value="false">
	<input type="hidden" name="cancelled" value="false">
	<ul id="import-films" class="import-activity import-with-ratings">
		<li class="import-film" data-json="{&quot;imdbId&quot;:&quot;tt6751668&quot;}"><input type="hidden" name="importRating" value=""><input type="hidden" name="importReview" value=""></li>
		<li class="import-film" data-json="{&quot;imdbId&quot;:&quot;tt0081505&quot;}"><input type="hidden" name="importRating" value=""><input type="hidden" name="importReview" value=""></li>
	</ul>
	<input type="checkbox" name="shouldReplaceOriginal" id="replace-original" class="checkbox" value="true">
</form>`

// fakeSite answers fetches as Letterboxd does, by method and path, and keeps
// every request it was sent.
type fakeSite struct {
	responses map[string]*FetchResponse

	mutex    sync.Mutex
	requests []*FetchRequest
}

func newFakeSite() *fakeSite {
	ok := func(body string) *FetchResponse { return &FetchResponse{Status: http.StatusOK, Body: body} }
	return &fakeSite{responses: map[string]*FetchResponse{
		"GET " + editPath:    ok(editorPage(testCsrf, "[]", "")),
		"POST " + importPath: ok(importerPage),
		"POST " + matchPath:  ok(matchedItems),
		// The film already on the list is staged as an update of its entry.
		"POST " + stagePath: {
			Status:     http.StatusOK,
			Url:        "https://letterboxd.com" + editPath + "?__requestid=x",
			Redirected: true,
			Body:       editorPage(stagedCsrf, html.EscapeString(`[{"action":"UPDATE","position":0},{"listable":"ZpZu","action":"ADD"}]`), ""),
		},
		"PATCH " + savePath: ok(savedAnswer),
	}}
}

func (f *fakeSite) Fetch(_ context.Context, request *FetchRequest) (*FetchResponse, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	f.requests = append(f.requests, request)

	requestUrl, err := url.Parse(request.Url)
	if err != nil {
		return nil, err
	}
	if response, ok := f.responses[request.Method+" "+requestUrl.Path]; ok {
		return response, nil
	}
	return &FetchResponse{Status: http.StatusNotFound, Body: "not found"}, nil
}

func (f *fakeSite) request(method string, path string) *FetchRequest {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	for _, request := range f.requests {
		if request.Method == method && strings.HasSuffix(request.Url, path) {
			return request
		}
	}
	return nil
}

func (f *fakeSite) sequence() []string {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	var sequence []string
	for _, request := range f.requests {
		sequence = append(sequence, request.Method+" "+strings.TrimPrefix(request.Url, BaseUrl))
	}
	return sequence
}

func TestUpdateList(t *testing.T) {
	t.Parallel()

	fullSequence := []string{"GET " + editPath, "POST " + importPath, "POST " + matchPath, "POST " + stagePath, "PATCH " + savePath}

	testCases := []struct {
		name         string
		dryRun       bool
		change       func(site *fakeSite)
		expected     *types.UpdateResult
		wantSequence []string
		wantIs       error
	}{
		{
			name:         "adds the films not on the list",
			expected:     &types.UpdateResult{List: "vph/collected", Matched: 2, Added: 1},
			wantSequence: fullSequence,
		},
		{
			name:         "a dry run does not save",
			dryRun:       true,
			expected:     &types.UpdateResult{List: "vph/collected", Matched: 2, Added: 1, DryRun: true},
			wantSequence: fullSequence[:4],
		},
		{
			name: "nothing new",
			change: func(site *fakeSite) {
				site.responses["POST "+stagePath].Body = editorPage(stagedCsrf, html.EscapeString(`[{"action":"UPDATE","position":0},{"action":"UPDATE","position":3}]`), "")
			},
			expected:     &types.UpdateResult{List: "vph/collected", Matched: 2, Added: 0},
			wantSequence: fullSequence[:4],
		},
		{
			name:   "nothing staged",
			change: func(site *fakeSite) { site.responses["POST "+stagePath].Body = editorPage(stagedCsrf, "[]", "") },
			wantIs: ErrUnexpectedPage,
		},
		{
			name: "no staged updates at all",
			change: func(site *fakeSite) {
				site.responses["POST "+stagePath].Body = strings.Replace(editorPage(stagedCsrf, "[]", ""), "data-initial-list-updates='[]'", "", 1)
			},
			wantIs: ErrUnexpectedPage,
		},
		{
			name:   "refused as a visitor",
			change: func(site *fakeSite) { site.responses["GET "+editPath] = &FetchResponse{Status: http.StatusForbidden} },
			wantIs: ErrNotSignedIn,
		},
		{
			name: "sent to sign in",
			change: func(site *fakeSite) {
				site.responses["GET "+editPath] = &FetchResponse{Status: http.StatusOK, Url: BaseUrl + "/sign-in/", Redirected: true}
			},
			wantIs: ErrNotSignedIn,
		},
		{
			name: "challenged",
			change: func(site *fakeSite) {
				site.responses["POST "+importPath] = &FetchResponse{Status: http.StatusForbidden, Mitigated: "challenge"}
			},
			wantIs: ErrChallenged,
		},
		{
			name:   "an edit page without the editor",
			change: func(site *fakeSite) { site.responses["GET "+editPath].Body = "<html></html>" },
			wantIs: ErrUnexpectedPage,
		},
		{
			name: "an edit page without a token",
			change: func(site *fakeSite) {
				site.responses["GET "+editPath].Body = strings.Replace(editorPage(testCsrf, "[]", ""), testCsrf, "", 1)
			},
			wantIs: ErrUnexpectedPage,
		},
		{
			name:   "nothing matched",
			change: func(site *fakeSite) { site.responses["POST "+matchPath].Body = `<div class="matched-item"></div>` },
			wantIs: ErrUnexpectedPage,
		},
		{
			name: "the save refused",
			change: func(site *fakeSite) {
				site.responses["PATCH "+savePath].Body = `{"messages": [{"type": "Error", "code": "DuplicateEntry", "title": "Duplicate"}]}`
			},
			wantIs: ErrSaveRefused,
		},
		{
			name: "the save failing",
			change: func(site *fakeSite) {
				site.responses["PATCH "+savePath] = &FetchResponse{Status: http.StatusBadRequest, Body: "{}"}
			},
			wantIs: ErrUnexpectedStatus,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			site := newFakeSite()
			if testCase.change != nil {
				testCase.change(site)
			}

			result, err := updateList(t.Context(), site, testUser, testSlug, []byte(testCsv), testCase.dryRun)
			if testCase.wantIs != nil {
				if !errors.Is(err, testCase.wantIs) {
					t.Fatalf("expected %v, got %v", testCase.wantIs, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("update list: %v", err)
			}
			if diff := altshiftTestingCmp.Diff(testCase.expected, result); diff != "" {
				t.Errorf("result mismatch (-expected +got):\n%s", diff)
			}
			if diff := altshiftTestingCmp.Diff(testCase.wantSequence, site.sequence()); diff != "" {
				t.Errorf("request sequence mismatch (-expected +got):\n%s", diff)
			}
		})
	}
}

// The requests are the ones the site's own editor and importer send.
func TestUpdateListRequests(t *testing.T) {
	t.Parallel()

	site := newFakeSite()
	if _, err := updateList(t.Context(), site, testUser, testSlug, []byte(testCsv), false); err != nil {
		t.Fatalf("update list: %v", err)
	}

	importRequest := site.request(http.MethodPost, importPath)
	if importRequest == nil {
		t.Fatal("no import request")
	}
	expectedImport := []*FormField{
		{Name: "filmListId", Value: testListId},
		{Name: "__csrf", Value: testCsrf},
		{Name: "name", Value: "Collected"},
		{Name: "notes", Value: ""},
		{Name: "publicList", Value: "false"},
		{Name: "sharePolicy", Value: "You"},
		{Name: "numberedList", Value: "false"},
		{Name: "file", Value: testCsv, Filename: "import.csv", Type: "text/csv"},
	}
	if diff := altshiftTestingCmp.Diff(expectedImport, importRequest.Multipart); diff != "" {
		t.Errorf("import fields mismatch (-expected +got):\n%s", diff)
	}

	matchRequest := site.request(http.MethodPost, matchPath)
	if matchRequest == nil {
		t.Fatal("no match request")
	}
	matchForm, err := url.ParseQuery(matchRequest.Body)
	if err != nil {
		t.Fatalf("parse match body: %v", err)
	}
	expectedPayload := `{ importType: "list", importProductions: [ {"imdbId":"tt6751668"}, {"imdbId":"tt0081505"} ]}`
	if matchForm.Get("json") != expectedPayload || matchForm.Get("__csrf") != testCsrf {
		t.Errorf("unexpected match form: %v", matchForm)
	}

	stageRequest := site.request(http.MethodPost, stagePath)
	if stageRequest == nil {
		t.Fatal("no stage request")
	}
	stageForm, err := url.ParseQuery(stageRequest.Body)
	if err != nil {
		t.Fatalf("parse stage body: %v", err)
	}
	expectedStage := url.Values{
		"__csrf":            {testCsrf},
		"importListDetails": {"true"},
		"filmListId":        {testListId},
		"name":              {"Collected"},
		"notes":             {""},
		"publicList":        {"false"},
		"sharePolicy":       {"You"},
		"numberedList":      {"false"},
		"cancelled":         {"false"},
		"entries":           {`[{"production":"hTha"},{"production":"ZpZu"}]`},
	}
	if diff := altshiftTestingCmp.Diff(expectedStage, stageForm); diff != "" {
		t.Errorf("stage form mismatch (-expected +got):\n%s", diff)
	}

	saveRequest := site.request(http.MethodPatch, savePath)
	if saveRequest == nil {
		t.Fatal("no save request")
	}
	// The token is the one the staged editor was rendered with.
	if saveRequest.Headers["X-CSRF-TOKEN"] != stagedCsrf {
		t.Errorf("unexpected token: %q", saveRequest.Headers["X-CSRF-TOKEN"])
	}
	var saved map[string]any
	if err := json.Unmarshal([]byte(saveRequest.Body), &saved); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	expectedSave := map[string]any{
		"version":     float64(24),
		"published":   false,
		"name":        "Collected",
		"sharePolicy": "You",
		"ranked":      false,
		"description": "",
		"tags":        []any{},
		// Only the addition: the update of an entry already there is left out.
		"entries": []any{map[string]any{"action": "ADD", "listable": "ZpZu"}},
	}
	if diff := altshiftTestingCmp.Diff(expectedSave, saved); diff != "" {
		t.Errorf("save body mismatch (-expected +got):\n%s", diff)
	}
}

func TestUpdateListNilFetcher(t *testing.T) {
	t.Parallel()

	_, err := updateList(t.Context(), nil, testUser, testSlug, []byte(testCsv), false)
	altshiftTestingCmp.CompareErr(t, err, nil_error.New("fetcher"))
}

func TestParseListEditor(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		page     string
		expected *listEditor
		wantIs   error
	}{
		{
			name: "private",
			page: editorPage(testCsrf, html.EscapeString(`[{"listable":"ZpZu","action":"ADD"},{"action":"UPDATE","position":0}]`), ""),
			expected: &listEditor{
				Lid:        testLid,
				Version:    24,
				Name:       "Collected",
				Sharing:    "You",
				Tags:       []string{},
				ImportPath: "/import/list/",
				Updates:    []*listUpdate{{Action: "ADD", Listable: "ZpZu"}, {Action: "UPDATE", Position: new(0)}},
			},
		},
		{
			name: "public, ranked and tagged",
			page: strings.NewReplacer(`data-list-type="private"`, `data-list-type="public"`, `data-list-tags='[]'`, `data-list-tags='["nyt","critics"]'`).
				Replace(editorPage(testCsrf, "[]", `data-list-is-ranked="true"`)),
			expected: &listEditor{
				Lid:        testLid,
				Version:    24,
				Name:       "Collected",
				Ranked:     true,
				Sharing:    "Public",
				Tags:       []string{"nyt", "critics"},
				ImportPath: "/import/list/",
				Updates:    []*listUpdate{},
			},
		},
		{name: "no editor", page: `<div class="list-editor"></div>`, wantIs: ErrUnexpectedPage},
		{name: "no lid", page: strings.Replace(editorPage(testCsrf, "[]", ""), testLid, "", 1), wantIs: ErrUnexpectedPage},
		{name: "a bad version", page: strings.Replace(editorPage(testCsrf, "[]", ""), `"24"`, `"x"`, 1), wantIs: ErrUnexpectedPage},
		{name: "a bad ranked", page: editorPage(testCsrf, "[]", `data-list-is-ranked="yes"`), wantIs: ErrUnexpectedPage},
		{name: "bad tags", page: strings.Replace(editorPage(testCsrf, "[]", ""), `'[]'`, `'nyt'`, 1), wantIs: ErrUnexpectedPage},
		{name: "bad updates", page: editorPage(testCsrf, "{", ""), wantIs: ErrUnexpectedPage},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			editor, err := parseListEditor(testCase.page)
			if testCase.wantIs != nil {
				if !errors.Is(err, testCase.wantIs) {
					t.Fatalf("expected %v, got %v", testCase.wantIs, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse list editor: %v", err)
			}
			if diff := altshiftTestingCmp.Diff(testCase.expected, editor); diff != "" {
				t.Errorf("editor mismatch (-expected +got):\n%s", diff)
			}
		})
	}
}

func TestListEditorSharing(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		sharing         string
		wantPublicList  bool
		wantSharePolicy string
	}{
		{sharing: "Public", wantPublicList: true, wantSharePolicy: "You"},
		{sharing: "Anyone", wantSharePolicy: "Anyone"},
		{sharing: "Friends", wantSharePolicy: "Friends"},
		{sharing: "You", wantSharePolicy: "You"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.sharing, func(t *testing.T) {
			t.Parallel()

			editor := &listEditor{Sharing: testCase.sharing}
			if editor.publicList() != testCase.wantPublicList || editor.sharePolicy() != testCase.wantSharePolicy {
				t.Errorf("unexpected: %v %q", editor.publicList(), editor.sharePolicy())
			}

			body := newApiListBody(editor, nil)
			if body.Published != testCase.wantPublicList || body.SharePolicy != testCase.wantSharePolicy || body.Tags == nil {
				t.Errorf("unexpected body: %+v", body)
			}
		})
	}
}

func TestDecodeListLid(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		lid      string
		expected int64
		wantIs   error
	}{
		// As measured against the site's editor on 2026-10-08.
		{name: "a list", lid: testLid, expected: 82736391},
		{name: "a film", lid: "hTha", wantIs: ErrUnexpectedPage},
		{name: "not base 62", lid: "TZ-9e", wantIs: ErrUnexpectedPage},
		{name: "empty", lid: "", wantIs: ErrUnexpectedPage},
		{name: "too long to be a lid", lid: "TZx9eTZx9eTZx9e", wantIs: ErrUnexpectedPage},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			id, err := decodeListLid(testCase.lid)
			if testCase.wantIs != nil {
				if !errors.Is(err, testCase.wantIs) {
					t.Fatalf("expected %v, got %v", testCase.wantIs, err)
				}
				return
			}
			if err != nil || id != testCase.expected {
				t.Errorf("unexpected: %d %v", id, err)
			}
		})
	}
}

func TestParseImporter(t *testing.T) {
	t.Parallel()

	parsed, err := parseImporter(importerPage)
	if err != nil {
		t.Fatalf("parse importer: %v", err)
	}
	expected := &importer{
		Rows:      []string{`{"imdbId":"tt6751668"}`, `{"imdbId":"tt0081505"}`},
		MatchPath: matchPath,
		StagePath: stagePath,
		Fields: [][2]string{
			{"__csrf", testCsrf},
			{"importListDetails", "true"},
			{"filmListId", testListId},
			{"name", "Collected"},
			{"notes", ""},
			{"publicList", "false"},
			{"sharePolicy", "You"},
			{"numberedList", "false"},
			{"cancelled", "false"},
		},
	}
	if diff := altshiftTestingCmp.Diff(expected, parsed); diff != "" {
		t.Errorf("importer mismatch (-expected +got):\n%s", diff)
	}

	// A form that stops naming its paths is posted to the ones it had.
	unnamed := strings.NewReplacer(`action="/list/add-films/"`, "", `data-url="/import/watchlist/match-import-film/"`, "").Replace(importerPage)
	if parsed, err := parseImporter(unnamed); err != nil || parsed.MatchPath != defaultMatchPath || parsed.StagePath != defaultStagePath {
		t.Errorf("unexpected defaults: %+v %v", parsed, err)
	}

	for name, page := range map[string]string{
		"no form":        "<html></html>",
		"no rows":        `<form id="importer-matched-form"></form>`,
		"a row not json": `<form id="importer-matched-form"><li class="import-film" data-json="{nope"></li></form>`,
	} {
		if _, err := parseImporter(page); !errors.Is(err, ErrUnexpectedPage) {
			t.Errorf("%s: expected an unexpected page, got %v", name, err)
		}
	}
}

func TestCheckSaveMessages(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name   string
		body   string
		wantIs error
	}{
		{name: "saved", body: savedAnswer},
		{name: "a warning only", body: `{"messages": [{"type": "Success", "code": "Saved"}]}`},
		{name: "refused", body: `{"messages": [{"type": "Error", "code": "DuplicateEntry"}]}`, wantIs: ErrSaveRefused},
		{name: "not json", body: "<html>", wantIs: ErrUnexpectedPage},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if err := checkSaveMessages(testCase.body); !errors.Is(err, testCase.wantIs) {
				t.Errorf("expected %v, got %v", testCase.wantIs, err)
			}
		})
	}
}

func TestParseListPath(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		path     string
		wantUser string
		wantSlug string
		wantErr  bool
	}{
		{path: "vph/collected", wantUser: "vph", wantSlug: "collected"},
		{path: "/vph/collected/", wantUser: "vph", wantSlug: "collected"},
		{path: "vph", wantErr: true},
		{path: "vph/", wantErr: true},
		{path: "/collected", wantErr: true},
		{path: "vph/list/collected", wantErr: true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.path, func(t *testing.T) {
			t.Parallel()

			user, slug, err := ParseListPath(testCase.path)
			if testCase.wantErr {
				if !errors.Is(err, ErrBadListPath) || !errors.Is(err, altshiftErrors.ErrValidationError) {
					t.Fatalf("expected a validation error, got %v", err)
				}
				return
			}
			if err != nil || user != testCase.wantUser || slug != testCase.wantSlug {
				t.Errorf("unexpected: %q %q %v", user, slug, err)
			}
		})
	}
}

func TestExcerpt(t *testing.T) {
	t.Parallel()

	if got := excerpt("a\n\t b"); got != "a b" {
		t.Errorf("unexpected excerpt: %q", got)
	}
	if got := excerpt(strings.Repeat("x", maxExcerptBytes+10)); len(got) != maxExcerptBytes+len("…") {
		t.Errorf("unexpected length: %d", len(got))
	}
}

// fakeSession is a browser on the fake site that signs in or not as told.
type fakeSession struct {
	*fakeSite

	signedIn bool
	// recognised is whether the site knows the session; a sign-in makes it so.
	recognised bool
	signInErr  error

	signIns int
	opened  []string
	closed  bool
}

func (f *fakeSession) Fetch(ctx context.Context, request *FetchRequest) (*FetchResponse, error) {
	if !f.recognised && strings.HasSuffix(request.Url, editPath) {
		return &FetchResponse{Status: http.StatusForbidden}, nil
	}
	return f.fakeSite.Fetch(ctx, request)
}

func (f *fakeSession) Open(_ context.Context, pageUrl string) error {
	f.opened = append(f.opened, pageUrl)
	return nil
}

func (f *fakeSession) SignedIn(context.Context) (bool, error) {
	return f.signedIn, nil
}

func (f *fakeSession) SignIn(context.Context, string, string) error {
	f.signIns++
	if f.signInErr != nil {
		return f.signInErr
	}
	f.signedIn = true
	f.recognised = true
	return nil
}

func (f *fakeSession) Close(context.Context) {
	f.closed = true
}

func TestClientUpdateList(t *testing.T) {
	t.Parallel()

	editUrl := BaseUrl + editPath

	testCases := []struct {
		name        string
		session     *fakeSession
		wantSignIns int
		wantOpened  []string
		wantIs      error
	}{
		{
			name:       "signed in",
			session:    &fakeSession{signedIn: true, recognised: true},
			wantOpened: []string{editUrl},
		},
		{
			name:        "signs in first",
			session:     &fakeSession{},
			wantSignIns: 1,
			wantOpened:  []string{editUrl, editUrl},
		},
		{
			name:        "signs in again when the session is not recognised",
			session:     &fakeSession{signedIn: true},
			wantSignIns: 1,
			wantOpened:  []string{editUrl, editUrl},
		},
		{
			name:        "a failed sign-in",
			session:     &fakeSession{signInErr: ErrSignInStuck},
			wantSignIns: 1,
			wantOpened:  []string{editUrl},
			wantIs:      ErrSignInStuck,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			session := testCase.session
			session.fakeSite = newFakeSite()
			client := &Client{
				username:    "vph",
				password:    "secret",
				openSession: func(context.Context) (browserSession, error) { return session, nil },
			}

			result, err := client.UpdateList(t.Context(), "vph/collected", []byte(testCsv), true)
			if testCase.wantIs != nil {
				if !errors.Is(err, testCase.wantIs) {
					t.Fatalf("expected %v, got %v", testCase.wantIs, err)
				}
			} else if err != nil || result == nil || result.Added != 1 {
				t.Fatalf("update list: %+v %v", result, err)
			}

			if session.signIns != testCase.wantSignIns {
				t.Errorf("unexpected sign-ins: %d", session.signIns)
			}
			if diff := altshiftTestingCmp.Diff(testCase.wantOpened, session.opened); diff != "" {
				t.Errorf("opened mismatch (-expected +got):\n%s", diff)
			}
			if !session.closed {
				t.Error("the session was not closed")
			}
		})
	}
}

// A session the site keeps refusing is signed in once, not over and over.
func TestClientUpdateListSignsInOnce(t *testing.T) {
	t.Parallel()

	for _, signedIn := range []bool{true, false} {
		t.Run(fmt.Sprintf("signed in %v", signedIn), func(t *testing.T) {
			t.Parallel()

			site := newFakeSite()
			site.responses["GET "+editPath] = &FetchResponse{Status: http.StatusForbidden}
			session := &fakeSession{fakeSite: site, signedIn: signedIn, recognised: true}

			client := &Client{username: "vph", password: "secret", openSession: func(context.Context) (browserSession, error) { return session, nil }}
			if _, err := client.UpdateList(t.Context(), "vph/collected", []byte(testCsv), true); !errors.Is(err, ErrNotSignedIn) {
				t.Fatalf("expected not signed in, got %v", err)
			}
			if session.signIns != 1 {
				t.Errorf("unexpected sign-ins: %d", session.signIns)
			}
		})
	}
}

// A second update while one runs is refused, not kept waiting.
func TestClientUpdateListBusy(t *testing.T) {
	t.Parallel()

	opened := false
	client := &Client{username: "vph", password: "secret", openSession: func(context.Context) (browserSession, error) {
		opened = true
		return nil, errBrowserCrashed
	}}

	client.mutex.Lock()
	_, err := client.UpdateList(t.Context(), "vph/collected", []byte(testCsv), true)
	client.mutex.Unlock()

	if !errors.Is(err, ErrBusy) || opened {
		t.Fatalf("expected busy before any browser, got %v (opened %v)", err, opened)
	}
}

func TestClientUpdateListFailures(t *testing.T) {
	t.Parallel()

	opened := false
	client := &Client{username: "vph", password: "secret", openSession: func(context.Context) (browserSession, error) {
		opened = true
		return nil, errBrowserCrashed
	}}

	if _, err := client.UpdateList(t.Context(), "collected", []byte(testCsv), true); !errors.Is(err, ErrBadListPath) || opened {
		t.Errorf("expected a bad path before any browser, got %v (opened %v)", err, opened)
	}
	if _, err := client.UpdateList(t.Context(), "vph/collected", []byte(testCsv), true); !errors.Is(err, errBrowserCrashed) {
		t.Errorf("expected the browser failure, got %v", err)
	}

	_, err := (&Client{}).UpdateList(t.Context(), "vph/collected", nil, true)
	altshiftTestingCmp.CompareErr(t, err, nil_error.New("open session"))
}

func TestNewClient(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		options *Options
		wantErr error
	}{
		{name: "nil", wantErr: nil_error.New("options")},
		{name: "no username", options: &Options{Password: "secret"}, wantErr: empty_error.New("username")},
		{name: "no password", options: &Options{Username: "vph"}, wantErr: empty_error.New("password")},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			_, err := NewClient(testCase.options)
			altshiftTestingCmp.CompareErr(t, err, testCase.wantErr)
		})
	}

	client, err := NewClient(&Options{Username: "vph", Password: "secret"})
	if err != nil || client == nil || client.openSession == nil {
		t.Fatalf("new client: %v", err)
	}
}
