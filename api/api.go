// Package api adds films to a Letterboxd list the way the site's own importer
// does, from inside a real Chrome.
//
// Letterboxd sits behind Cloudflare bot management that scores every request,
// not only the first: a clearance earned by a browser does not carry a Go or
// curl client through (measured 2026-10-08). So every request is made by the
// browser itself, as an in-page fetch from a letterboxd.com page, and Go only
// orchestrates and reads the answers.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"

	altshiftErrors "github.com/altshiftab/utils_go/pkg/errors"
	"github.com/altshiftab/utils_go/pkg/errors/types/empty_error"
	"github.com/altshiftab/utils_go/pkg/errors/types/nil_error"
	"github.com/vphpersson/letterboxd_list_updater/api/types"
)

const (
	BaseUrl = "https://letterboxd.com"

	// maxExcerptBytes is how much of an unexpected page an error carries.
	maxExcerptBytes = 300

	// sharingPublic is the editor's sharing option for a published list.
	sharingPublic = "Public"

	headerContentType = "Content-Type"

	// The paths the site has used, for when a page stops naming them.
	defaultImportPath = "/import/list/"
	defaultMatchPath  = "/import/watchlist/match-import-film/"
	defaultStagePath  = "/list/add-films/"
)

var (
	// ErrNotSignedIn is the site answering as it does a visitor with no session.
	ErrNotSignedIn = errors.New("not signed in")
	// ErrChallenged is Cloudflare answering instead of the site.
	ErrChallenged = errors.New("cloudflare challenge")
	// ErrUnexpectedStatus is the site refusing or failing a request.
	ErrUnexpectedStatus = errors.New("unexpected status")
	// ErrUnexpectedPage is a page without what the import needs from it, which
	// is what a change on Letterboxd's side looks like.
	ErrUnexpectedPage = errors.New("unexpected page")
	ErrBadListPath    = errors.New("bad list path")
	// ErrSaveRefused is the API answering a save with an error message.
	ErrSaveRefused = errors.New("save refused")
	// ErrBusy is an update asked for while another is running.
	ErrBusy = errors.New("an update is running")
)

// FormField is one field of a multipart form; one with a filename is a file.
type FormField struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Filename string `json:"filename,omitzero"`
	Type     string `json:"type,omitzero"`
}

// FetchRequest is a request for the page to make with fetch.
type FetchRequest struct {
	Method  string            `json:"method"`
	Url     string            `json:"url"`
	Headers map[string]string `json:"headers,omitzero"`
	Body    string            `json:"body,omitzero"`
	// Multipart is sent as FormData, the browser choosing the boundary.
	Multipart []*FormField `json:"multipart,omitzero"`
}

// FetchResponse is what the page's fetch came back with.
type FetchResponse struct {
	Status     int    `json:"status"`
	Url        string `json:"url"`
	Redirected bool   `json:"redirected"`
	// Mitigated is Cloudflare's cf-mitigated header: "challenge" when it, not
	// the site, answered.
	Mitigated string `json:"mitigated"`
	Body      string `json:"body"`
}

type pageFetcher interface {
	Fetch(ctx context.Context, request *FetchRequest) (*FetchResponse, error)
}

// browserSession is a Chrome with a page on the site.
type browserSession interface {
	pageFetcher
	// Open navigates the page and waits out any challenge in front of it.
	Open(ctx context.Context, pageUrl string) error
	SignedIn(ctx context.Context) (bool, error)
	SignIn(ctx context.Context, username string, password string) error
	Close(ctx context.Context)
}

type Options struct {
	Username string
	Password string
	// ChromePath is the Chrome binary; empty means google-chrome-stable.
	ChromePath string
	// ProfileDirectory keeps Chrome's profile, and with it the session,
	// between updates. Empty means a profile of its own for every update,
	// which signs in every time.
	ProfileDirectory string
}

type Client struct {
	username string
	password string

	// mutex keeps updates, and so Chromes on the one profile, to one at a time.
	mutex sync.Mutex
	// openSession starts a browser; a launched Chrome outside tests.
	openSession func(ctx context.Context) (browserSession, error)
}

func NewClient(options *Options) (*Client, error) {
	if options == nil {
		return nil, altshiftErrors.NewWithTrace(nil_error.New("options"))
	}
	if options.Username == "" {
		return nil, altshiftErrors.NewWithTrace(empty_error.New("username"))
	}
	if options.Password == "" {
		return nil, altshiftErrors.NewWithTrace(empty_error.New("password"))
	}

	chromePath := options.ChromePath
	if chromePath == "" {
		chromePath = defaultChromePath
	}
	profileDirectory := options.ProfileDirectory

	return &Client{
		username: options.Username,
		password: options.Password,
		openSession: func(ctx context.Context) (browserSession, error) {
			session, err := launchSession(ctx, chromePath, profileDirectory)
			if err != nil {
				return nil, err
			}
			return session, nil
		},
	}, nil
}

// ParseListPath splits "user/slug" into its parts.
func ParseListPath(listPath string) (string, string, error) {
	user, slug, found := strings.Cut(strings.Trim(listPath, "/"), "/")
	if !found || user == "" || slug == "" || strings.Contains(slug, "/") {
		return "", "", altshiftErrors.NewWithTrace(
			fmt.Errorf("%w: %w: want \"user/slug\"", altshiftErrors.ErrValidationError, ErrBadListPath),
			listPath,
		)
	}
	return user, slug, nil
}

// UpdateList imports the CSV into the list at "user/slug": signing in first
// when the browser has no session, and once more when the site turns out not
// to know the one it has. A dry run stops before the request that commits.
func (c *Client) UpdateList(ctx context.Context, listPath string, csv []byte, dryRun bool) (*types.UpdateResult, error) {
	user, slug, err := ParseListPath(listPath)
	if err != nil {
		return nil, err
	}
	if c.openSession == nil {
		return nil, altshiftErrors.NewWithTrace(nil_error.New("open session"))
	}

	// One at a time, and a second is refused rather than kept waiting: its
	// time would run out while the first runs.
	if !c.mutex.TryLock() {
		return nil, altshiftErrors.NewWithTrace(ErrBusy)
	}
	defer c.mutex.Unlock()

	session, err := c.openSession(ctx)
	if err != nil {
		return nil, fmt.Errorf("open session: %w", err)
	}
	if session == nil {
		return nil, altshiftErrors.NewWithTrace(nil_error.New("session"))
	}
	defer session.Close(ctx)

	editUrl := editPageUrl(user, slug)
	if err := session.Open(ctx, editUrl); err != nil {
		return nil, fmt.Errorf("open edit page: %w", err)
	}

	signedIn, err := session.SignedIn(ctx)
	if err != nil {
		return nil, fmt.Errorf("signed in: %w", err)
	}

	signIn := func() error {
		slog.InfoContext(ctx, "Signing in to Letterboxd.", slog.String("username", c.username))
		if err := session.SignIn(ctx, c.username, c.password); err != nil {
			return fmt.Errorf("sign in: %w", err)
		}
		if err := session.Open(ctx, editUrl); err != nil {
			return fmt.Errorf("open edit page: %w", err)
		}
		return nil
	}

	if !signedIn {
		if err := signIn(); err != nil {
			return nil, err
		}
	}

	result, err := updateList(ctx, session, user, slug, csv, dryRun)
	// Only the first request, reading the edit page, can find the session
	// gone, so nothing has been staged and running again is safe.
	if signedIn && errors.Is(err, ErrNotSignedIn) {
		slog.InfoContext(ctx, "The Letterboxd session was not recognised.")
		if err := signIn(); err != nil {
			return nil, err
		}
		result, err = updateList(ctx, session, user, slug, csv, dryRun)
	}
	return result, err
}

func editPageUrl(user string, slug string) string {
	return fmt.Sprintf("%s/%s/list/%s/edit/", BaseUrl, url.PathEscape(user), url.PathEscape(slug))
}

// listEditor is the list as the site renders its editor with it, in the
// data attributes of the element the editor is drawn into.
type listEditor struct {
	Lid         string
	Version     int
	Name        string
	Description string
	Ranked      bool
	// Sharing is the editor's sharing option: Public, Anyone, Friends or You.
	Sharing    string
	Tags       []string
	ImportPath string
	// Updates are the changes staged in the editor and not yet saved.
	Updates []*listUpdate
}

// listUpdate is one change to a list's entries, as the editor stages it and
// the API takes it: an ADD of a film by its LID, or an UPDATE or DELETE of the
// entry at a position.
type listUpdate struct {
	Action   string `json:"action"`
	Listable string `json:"listable,omitzero"`
	Position *int   `json:"position,omitzero"`
}

func parseListEditor(page string) (*listEditor, error) {
	tag := listEditorTag(page)
	if tag == nil {
		return nil, altshiftErrors.NewWithTrace(fmt.Errorf("%w: no list editor", ErrUnexpectedPage), excerpt(page))
	}
	attributes := tag.Attributes

	editor := &listEditor{
		Lid:         attributes["data-list-lid"],
		Name:        attributes["data-list-name"],
		Description: attributes["data-list-description"],
		ImportPath:  attributes["data-list-import-url"],
	}
	if editor.Lid == "" {
		return nil, altshiftErrors.NewWithTrace(fmt.Errorf("%w: no data-list-lid", ErrUnexpectedPage))
	}
	if !strings.HasPrefix(editor.ImportPath, "/") {
		editor.ImportPath = defaultImportPath
	}

	version, err := strconv.Atoi(attributes["data-list-version"])
	if err != nil {
		return nil, altshiftErrors.NewWithTrace(fmt.Errorf("%w: data-list-version: %w", ErrUnexpectedPage, err), attributes["data-list-version"])
	}
	editor.Version = version

	// As the editor reads it: "true" or "false", and absent or empty is false.
	switch ranked := attributes["data-list-is-ranked"]; ranked {
	case "true":
		editor.Ranked = true
	case "", "false":
	default:
		return nil, altshiftErrors.NewWithTrace(fmt.Errorf("%w: data-list-is-ranked", ErrUnexpectedPage), ranked)
	}

	// A public list is the editor's Public whatever its share policy says.
	editor.Sharing = attributes["data-list-share-policy"]
	if attributes["data-list-type"] == "public" {
		editor.Sharing = sharingPublic
	}
	if editor.Sharing == "" {
		editor.Sharing = "You"
	}

	if tags := attributes["data-list-tags"]; tags != "" {
		if err := json.Unmarshal([]byte(tags), &editor.Tags); err != nil {
			return nil, altshiftErrors.NewWithTrace(fmt.Errorf("%w: data-list-tags: %w", ErrUnexpectedPage, err), tags)
		}
	}
	if updates := attributes["data-initial-list-updates"]; updates != "" {
		if err := json.Unmarshal([]byte(updates), &editor.Updates); err != nil {
			return nil, altshiftErrors.NewWithTrace(fmt.Errorf("%w: data-initial-list-updates: %w", ErrUnexpectedPage, err), updates)
		}
	}

	return editor, nil
}

// publicList and sharePolicy are how the site's forms and its API put the
// editor's sharing: Public is a published list shared with You.
func (e *listEditor) publicList() bool {
	return e.Sharing == sharingPublic
}

func (e *listEditor) sharePolicy() string {
	if e.Sharing == sharingPublic {
		return "You"
	}
	return e.Sharing
}

// lidAlphabet is the base-62 alphabet of LIDs (boxd.it codes).
const lidAlphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// lidListType is the last decimal digit of a decoded LID that names a list,
// as 0 names a film and 1 a viewing.
const lidListType = 2

// decodeListLid returns the numeric id of the list a LID names, as the
// site's editor computes the filmListId its import form posts.
func decodeListLid(lid string) (int64, error) {
	if lid == "" || len(lid) > 10 {
		return 0, altshiftErrors.NewWithTrace(fmt.Errorf("%w: lid length", ErrUnexpectedPage), lid)
	}

	var value int64
	for _, character := range lid {
		index := strings.IndexRune(lidAlphabet, character)
		if index < 0 {
			return 0, altshiftErrors.NewWithTrace(fmt.Errorf("%w: lid character %q", ErrUnexpectedPage, character), lid)
		}
		value = value*int64(len(lidAlphabet)) + int64(index)
	}

	if value%10 != lidListType {
		return 0, altshiftErrors.NewWithTrace(fmt.Errorf("%w: not a list lid", ErrUnexpectedPage), lid)
	}
	return value / 10, nil
}

// importer is the importer page's matched form: where the rows are matched,
// where the matched films are staged, and the fields it posts besides them.
type importer struct {
	Rows      []string
	MatchPath string
	StagePath string
	Fields    [][2]string
}

// itemFields are the form's per-row inputs, which the site's importer removes
// and replaces with one entries field before posting.
var itemFields = []string{"importProductionId", "importViewingId", "shouldImportProduction", "importReview", "importRating"}

func parseImporter(page string) (*importer, error) {
	form, contents := extractFormById(page, "importer-matched-form")
	if form == nil {
		return nil, altshiftErrors.NewWithTrace(fmt.Errorf("%w: no #importer-matched-form", ErrUnexpectedPage), excerpt(page))
	}

	parsed := &importer{
		Rows:      importFilmObjects(contents),
		MatchPath: form.Attributes["data-url"],
		StagePath: form.Attributes["action"],
	}
	if !strings.HasPrefix(parsed.MatchPath, "/") {
		parsed.MatchPath = defaultMatchPath
	}
	if !strings.HasPrefix(parsed.StagePath, "/") {
		parsed.StagePath = defaultStagePath
	}

	for _, input := range startTags(contents, "input") {
		name := input.Attributes["name"]
		if name == "" || !strings.EqualFold(input.Attributes["type"], "hidden") || slices.Contains(itemFields, name) {
			continue
		}
		parsed.Fields = append(parsed.Fields, [2]string{name, input.Attributes["value"]})
	}

	if len(parsed.Rows) == 0 {
		return nil, altshiftErrors.NewWithTrace(fmt.Errorf("%w: no import-film rows", ErrUnexpectedPage), excerpt(contents))
	}
	for _, row := range parsed.Rows {
		if !json.Valid([]byte(row)) {
			return nil, altshiftErrors.NewWithTrace(fmt.Errorf("%w: an import-film row that is not json", ErrUnexpectedPage), excerpt(row))
		}
	}
	return parsed, nil
}

// fetchPage makes the request and returns the page it answered with.
func fetchPage(ctx context.Context, fetcher pageFetcher, request *FetchRequest) (string, error) {
	response, err := fetcher.Fetch(ctx, request)
	if err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}
	return checkResponse(request, response)
}

// checkResponse returns the page a request answered with, refusing
// Cloudflare's answers and the site's errors.
func checkResponse(request *FetchRequest, response *FetchResponse) (string, error) {
	if response == nil {
		return "", altshiftErrors.NewWithTrace(nil_error.New("fetch response"))
	}

	if response.Mitigated != "" {
		return "", altshiftErrors.NewWithTrace(
			fmt.Errorf("%w: %s %s: cf-mitigated %s", ErrChallenged, request.Method, request.Url, response.Mitigated),
		)
	}
	if response.Status < 200 || response.Status > 299 {
		return "", altshiftErrors.NewWithTrace(
			fmt.Errorf("%w: %s %s: %d", ErrUnexpectedStatus, request.Method, request.Url, response.Status),
			excerpt(response.Body),
		)
	}
	return response.Body, nil
}

// readEditPage returns the list's edit page, telling a missing session apart
// from other failures.
func readEditPage(ctx context.Context, fetcher pageFetcher, user string, slug string) (string, error) {
	request := &FetchRequest{Method: http.MethodGet, Url: editPageUrl(user, slug), Headers: map[string]string{"Accept": "text/html,*/*;q=0.9"}}

	response, err := fetcher.Fetch(ctx, request)
	if err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}

	// A visitor without a session is sent to sign in, or refused outright.
	if response != nil && response.Mitigated == "" &&
		(strings.Contains(response.Url, "/sign-in") || response.Status == http.StatusUnauthorized || response.Status == http.StatusForbidden) {
		return "", altshiftErrors.NewWithTrace(fmt.Errorf("%w: %d %s", ErrNotSignedIn, response.Status, response.Url))
	}

	return checkResponse(request, response)
}

// updateList walks the site's import as its list editor does (2026-10):
//
//  1. GET the list's edit page for the list as the editor is rendered with it.
//  2. POST the CSV to the editor's import URL, as the editor's import form
//     does, which answers with the importer page and the rows it read.
//  3. POST the rows to the match URL, which resolves them to films.
//  4. POST the films the importer would keep to the matched form's action,
//     which stages them and redirects to the editor with the staged changes.
//  5. PATCH /api/v0/list/<lid> with the films the staging adds, which saves.
//
// No notes are written: a film already on the list is staged as an UPDATE of
// its entry, which is left out, so its note stays as it is.
func updateList(ctx context.Context, fetcher pageFetcher, user string, slug string, csv []byte, dryRun bool) (*types.UpdateResult, error) {
	if fetcher == nil {
		return nil, altshiftErrors.NewWithTrace(nil_error.New("fetcher"))
	}
	listPath := user + "/" + slug

	editPage, err := readEditPage(ctx, fetcher, user, slug)
	if err != nil {
		return nil, fmt.Errorf("read edit page: %w", err)
	}
	editor, err := parseListEditor(editPage)
	if err != nil {
		return nil, fmt.Errorf("parse list editor: %w", err)
	}
	csrf := csrfToken(editPage)
	if csrf == "" {
		return nil, altshiftErrors.NewWithTrace(fmt.Errorf("%w: no csrf token", ErrUnexpectedPage))
	}
	filmListId, err := decodeListLid(editor.Lid)
	if err != nil {
		return nil, fmt.Errorf("decode list lid: %w", err)
	}

	importFields := []*FormField{
		{Name: "filmListId", Value: strconv.FormatInt(filmListId, 10)},
		{Name: "__csrf", Value: csrf},
		{Name: "name", Value: editor.Name},
		{Name: "notes", Value: editor.Description},
		{Name: "publicList", Value: strconv.FormatBool(editor.publicList())},
		{Name: "sharePolicy", Value: editor.sharePolicy()},
		{Name: "numberedList", Value: strconv.FormatBool(editor.Ranked)},
	}
	for _, tag := range editor.Tags {
		importFields = append(importFields, &FormField{Name: "tag", Value: tag})
	}
	importFields = append(importFields, &FormField{Name: "file", Value: string(csv), Filename: "import.csv", Type: "text/csv"})

	importPage, err := fetchPage(ctx, fetcher, &FetchRequest{Method: http.MethodPost, Url: BaseUrl + editor.ImportPath, Multipart: importFields})
	if err != nil {
		return nil, fmt.Errorf("import: %w", err)
	}
	importer, err := parseImporter(importPage)
	if err != nil {
		return nil, fmt.Errorf("parse importer: %w", err)
	}

	// The rows go back as the importer embedded them, in the importer's own
	// words.
	matchPayload := `{ importType: "list", importProductions: [ ` + strings.Join(importer.Rows, ", ") + ` ]}`
	matchPage, err := fetchPage(ctx, fetcher, &FetchRequest{
		Method: http.MethodPost,
		Url:    BaseUrl + importer.MatchPath,
		Headers: map[string]string{
			headerContentType:  "application/x-www-form-urlencoded; charset=UTF-8",
			"Accept":           "text/html, */*; q=0.01",
			"X-Requested-With": "XMLHttpRequest",
		},
		Body: url.Values{"json": {matchPayload}, "__csrf": {csrf}}.Encode(),
	})
	if err != nil {
		return nil, fmt.Errorf("match: %w", err)
	}

	productions := matchedProductions(matchPage)
	if len(productions) == 0 {
		return nil, altshiftErrors.NewWithTrace(fmt.Errorf("%w: no film matched", ErrUnexpectedPage), excerpt(matchPage))
	}

	entries := make([]*stagedEntry, 0, len(productions))
	for _, production := range productions {
		entries = append(entries, &stagedEntry{Production: production})
	}
	entriesJson, err := json.Marshal(entries)
	if err != nil {
		return nil, altshiftErrors.NewWithTrace(fmt.Errorf("json marshal: %w", err), entries)
	}

	stageForm := url.Values{}
	for _, field := range importer.Fields {
		stageForm.Add(field[0], field[1])
	}
	stageForm.Set("entries", string(entriesJson))

	stagedPage, err := fetchPage(ctx, fetcher, &FetchRequest{
		Method:  http.MethodPost,
		Url:     BaseUrl + importer.StagePath,
		Headers: map[string]string{headerContentType: "application/x-www-form-urlencoded"},
		Body:    stageForm.Encode(),
	})
	if err != nil {
		return nil, fmt.Errorf("stage: %w", err)
	}
	staged, err := parseListEditor(stagedPage)
	if err != nil {
		return nil, fmt.Errorf("parse staged list editor: %w", err)
	}
	// Every matched film comes back staged, as an addition or as an update of
	// the entry already there; none at all is the page having changed, not the
	// list having every film.
	if len(staged.Updates) == 0 {
		return nil, altshiftErrors.NewWithTrace(fmt.Errorf("%w: no staged updates", ErrUnexpectedPage), excerpt(stagedPage))
	}
	if stagedCsrf := csrfToken(stagedPage); stagedCsrf != "" {
		csrf = stagedCsrf
	}

	var additions []*listUpdate
	for _, update := range staged.Updates {
		if update != nil && update.Action == "ADD" && update.Listable != "" {
			additions = append(additions, &listUpdate{Action: "ADD", Listable: update.Listable})
		}
	}

	result := &types.UpdateResult{List: listPath, Matched: len(productions), Added: len(additions), DryRun: dryRun}

	if len(additions) == 0 {
		slog.InfoContext(ctx, "The Letterboxd list already has every film.", slog.String("list", listPath), slog.Int("matched", len(productions)))
		return result, nil
	}
	if dryRun {
		lids := make([]string, 0, len(additions))
		for _, addition := range additions {
			lids = append(lids, addition.Listable)
		}
		slog.InfoContext(
			ctx,
			"Dry run: the Letterboxd list was left as it was.",
			slog.String("list", listPath),
			slog.Int("matched", len(productions)),
			slog.Any("lids", lids),
		)
		return result, nil
	}

	payload, err := json.Marshal(newApiListBody(staged, additions))
	if err != nil {
		return nil, altshiftErrors.NewWithTrace(fmt.Errorf("json marshal: %w", err))
	}
	saved, err := fetchPage(ctx, fetcher, &FetchRequest{
		Method: http.MethodPatch,
		Url:    BaseUrl + "/api/v0/list/" + url.PathEscape(staged.Lid),
		Headers: map[string]string{
			headerContentType: "application/json; charset=UTF-8",
			"X-CSRF-TOKEN":    csrf,
		},
		Body: string(payload),
	})
	if err != nil {
		return nil, fmt.Errorf("save: %w", err)
	}
	if err := checkSaveMessages(saved); err != nil {
		return nil, fmt.Errorf("save: %w", err)
	}

	slog.InfoContext(ctx, "The Letterboxd list was updated.", slog.String("list", listPath), slog.Int("matched", len(productions)), slog.Int("added", len(additions)))
	return result, nil
}

// stagedEntry is a film for the staging, keyed by production since 2026-07; a
// review is left out when there is none, as the site does.
type stagedEntry struct {
	Production string `json:"production"`
}

type apiListBody struct {
	Version     int           `json:"version"`
	Published   bool          `json:"published"`
	Name        string        `json:"name"`
	SharePolicy string        `json:"sharePolicy"`
	Ranked      bool          `json:"ranked"`
	Description string        `json:"description"`
	Tags        []string      `json:"tags"`
	Entries     []*listUpdate `json:"entries"`
}

// newApiListBody is the save of the list as the editor has it, with the
// additions, as the editor's own save sends it.
func newApiListBody(editor *listEditor, additions []*listUpdate) *apiListBody {
	tags := editor.Tags
	if tags == nil {
		tags = []string{}
	}
	return &apiListBody{
		Version:     editor.Version,
		Published:   editor.publicList(),
		Name:        editor.Name,
		SharePolicy: editor.sharePolicy(),
		Ranked:      editor.Ranked,
		Description: editor.Description,
		Tags:        tags,
		Entries:     additions,
	}
}

// checkSaveMessages refuses a save the API answered with an error message,
// which it does with a 200.
func checkSaveMessages(body string) error {
	var answer struct {
		Messages []*struct {
			Type  string `json:"type"`
			Code  string `json:"code"`
			Title string `json:"title"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		return altshiftErrors.NewWithTrace(fmt.Errorf("%w: json unmarshal: %w", ErrUnexpectedPage, err), excerpt(body))
	}

	var problems []string
	for _, message := range answer.Messages {
		if message != nil && message.Type == "Error" {
			problems = append(problems, strings.TrimSpace(message.Code+" "+message.Title))
		}
	}
	if len(problems) > 0 {
		return altshiftErrors.NewWithTrace(fmt.Errorf("%w: %s", ErrSaveRefused, strings.Join(problems, "; ")))
	}
	return nil
}

// excerpt is the start of a page, for an error to show what came instead.
func excerpt(page string) string {
	page = strings.Join(strings.Fields(page), " ")
	if len(page) <= maxExcerptBytes {
		return page
	}
	return page[:maxExcerptBytes] + "…"
}
