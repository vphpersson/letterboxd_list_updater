package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

const (
	BaseURL        = "https://letterboxd.com"
	csrfCookieName = "com.xk72.webparts.csrf"
	userAgent      = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36"
)

type Options struct {
	Username   string
	Password   string
	CookiePath string
	DumpDir    string
}

type Client struct {
	http       *http.Client
	username   string
	password   string
	cookiePath string
	dumpDir    string

	mu       sync.Mutex
	loggedIn bool
}

func NewClient(opts Options) (*Client, error) {
	if opts.Username == "" || opts.Password == "" {
		return nil, errors.New("letterboxd: username and password are required")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("cookiejar new: %w", err)
	}
	c := &Client{
		http:       &http.Client{Jar: jar},
		username:   opts.Username,
		password:   opts.Password,
		cookiePath: opts.CookiePath,
		dumpDir:    opts.DumpDir,
	}
	if c.cookiePath != "" {
		if err := c.loadCookies(); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("load cookies: %w", err)
		}
		if c.hasSessionCookie() {
			c.loggedIn = true
		}
	}
	return c, nil
}

func (c *Client) Close() {}

// UpdateList logs in (once per process) then walks the full import flow:
//
//  1. GET the list's edit page to capture list metadata (list LID, version,
//     name, sharing policy, ranked, description, tags, CSRF).
//  2. POST the CSV to /import/list/ (multipart).
//  3. POST /list/add-films/ — the response embeds the staged films with
//     their Letterboxd short IDs (LIDs) on .js-new-film-list-entry nodes.
//  4. PATCH /api/v0/list/<list_lid> (JSON, X-Csrf-Token header) to commit.
//
// No notes are written (step 4 leaves entries without review).
func (c *Client) UpdateList(ctx context.Context, listURL string, csv []byte) error {
	if err := c.ensureLogin(ctx); err != nil {
		return err
	}
	user, slug, err := parseListURL(listURL)
	if err != nil {
		return fmt.Errorf("parse list url: %w", err)
	}
	info, err := c.getListInfo(ctx, user, slug)
	if err != nil {
		return fmt.Errorf("get list info: %w", err)
	}
	filmLIDs, err := c.stageFilms(ctx, info, csv)
	if err != nil {
		return fmt.Errorf("stage films: %w", err)
	}
	if len(filmLIDs) == 0 {
		slog.InfoContext(ctx, "Letterboxd list unchanged (all films already present).", "list", user+"/"+slug)
		_ = c.saveCookies()
		return nil
	}
	if err := c.patchList(ctx, info, filmLIDs); err != nil {
		return fmt.Errorf("patch list: %w", err)
	}
	slog.InfoContext(ctx, "Letterboxd list updated.", "list", user+"/"+slug, "filmCount", len(filmLIDs))
	_ = c.saveCookies()
	return nil
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", userAgent)
	}
	if req.Header.Get("Accept-Language") == "" {
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	}
	return c.http.Do(req)
}

func (c *Client) csrf() string {
	u, _ := url.Parse(BaseURL)
	for _, ck := range c.http.Jar.Cookies(u) {
		if ck.Name == csrfCookieName {
			return ck.Value
		}
	}
	return ""
}

func (c *Client) hasSessionCookie() bool {
	u, _ := url.Parse(BaseURL)
	for _, ck := range c.http.Jar.Cookies(u) {
		if ck.Name != csrfCookieName {
			return true
		}
	}
	return false
}

func (c *Client) ensureLogin(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loggedIn {
		return nil
	}
	if err := c.login(ctx); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	c.loggedIn = true
	_ = c.saveCookies()
	return nil
}

func (c *Client) login(ctx context.Context) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, BaseURL+"/", nil)
	resp, err := c.do(req)
	if err != nil {
		return fmt.Errorf("prime: %w", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	csrf := c.csrf()
	if csrf == "" {
		return errors.New("csrf cookie missing after prime")
	}

	form := url.Values{
		"__csrf":   {csrf},
		"username": {c.username},
		"password": {c.password},
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodPost, BaseURL+"/user/login.do", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Referer", BaseURL+"/")
	resp, err = c.do(req)
	if err != nil {
		return fmt.Errorf("login post: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read login body: %w", err)
	}
	if !bytes.Contains(body, []byte(`"result": "success"`)) && !bytes.Contains(body, []byte(`"result":"success"`)) {
		return fmt.Errorf("login failed (status %d): %s", resp.StatusCode, truncate(string(body), 200))
	}
	return nil
}

// listInfo mirrors the edit-page form fields that feed the PATCH API body.
type listInfo struct {
	User        string
	Slug        string
	FilmListId  string // numeric, used by /import/list/ and /list/add-films/
	ListLid     string // short alphanumeric, the target of PATCH /api/v0/list/<lid>
	Version     int
	CSRF        string
	Name        string
	SharePolicy string // "Public" | "Anyone" | "Friends" | "You"
	Ranked      bool
	Description string
	Tags        []string
}

func (c *Client) getListInfo(ctx context.Context, user, slug string) (*listInfo, error) {
	u := fmt.Sprintf("%s/%s/list/%s/edit/", BaseURL, user, slug)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := c.do(req)
	if err != nil {
		return nil, fmt.Errorf("get edit: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get edit: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read edit body: %w", err)
	}
	return c.parseListInfo(user, slug, body)
}

func (c *Client) parseListInfo(user, slug string, body []byte) (*listInfo, error) {
	src := string(body)
	info := &listInfo{
		User:        user,
		Slug:        slug,
		FilmListId:  findInputValue(src, "filmListId"),
		ListLid:     findInputValue(src, "filmListLid"),
		CSRF:        findInputValue(src, "__csrf"),
		Name:        findInputValue(src, "name"),
		SharePolicy: findSelectedOption(src, "sharing"),
		Ranked:      hasCheckedAttr(src, "numberedList"),
		Description: findTextareaValue(src, "notes"),
	}
	if v := findInputValue(src, "version"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			info.Version = n
		}
	}
	if t := findInputValue(src, "tags"); t != "" {
		for _, p := range strings.Split(t, ",") {
			if p = strings.TrimSpace(p); p != "" {
				info.Tags = append(info.Tags, p)
			}
		}
	}
	if info.CSRF == "" {
		info.CSRF = c.csrf()
	}
	if info.ListLid == "" {
		c.dumpHTML("edit-page", body)
		return nil, errors.New("filmListLid not found in edit page")
	}
	return info, nil
}

var (
	reImportFilmDataJSON = regexp.MustCompile(`<li class="import-film" data-json="([^"]*)"`)
	reImportFilmIDNum    = regexp.MustCompile(`<input[^>]*\bname="importFilmId"[^>]*\bvalue="(\d+)"`)
	reNewFilmLID         = regexp.MustCompile(`<li[^>]*\bclass="[^"]*\bjs-new-film-list-entry\b[^"]*"[^>]*\bdata-film-id="([^"]+)"`)
)

// stageFilms walks the /import/list/ → /import/watchlist/match-import-film/
// → /list/add-films/ pipeline. The final response HTML carries the LIDs of
// the newly staged films in <li class="js-new-film-list-entry"> nodes.
func (c *Client) stageFilms(ctx context.Context, info *listInfo, csv []byte) ([]string, error) {
	// Step 1: upload CSV to /import/list/. The response contains
	// <li class="import-film" data-json="..."> per row but no numeric IDs.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "import.csv")
	if err != nil {
		return nil, fmt.Errorf("create form file: %w", err)
	}
	if _, err := fw.Write(csv); err != nil {
		return nil, fmt.Errorf("write csv: %w", err)
	}
	if err := mw.WriteField("__csrf", info.CSRF); err != nil {
		return nil, fmt.Errorf("write csrf: %w", err)
	}
	if err := mw.WriteField("filmListId", info.FilmListId); err != nil {
		return nil, fmt.Errorf("write filmListId: %w", err)
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("close multipart: %w", err)
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, BaseURL+"/import/list/", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Referer", fmt.Sprintf("%s/%s/list/%s/edit/", BaseURL, info.User, info.Slug))
	resp, err := c.do(req)
	if err != nil {
		return nil, fmt.Errorf("post import: %w", err)
	}
	importBody, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read import body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("import: status %d", resp.StatusCode)
	}

	dataJSONMatches := reImportFilmDataJSON.FindAllStringSubmatch(string(importBody), -1)
	if len(dataJSONMatches) == 0 {
		c.dumpHTML("import-response", importBody)
		return nil, errors.New("no import-film nodes in /import/list/ response")
	}
	filmObjects := make([]string, 0, len(dataJSONMatches))
	for _, m := range dataJSONMatches {
		filmObjects = append(filmObjects, html.UnescapeString(m[1]))
	}

	// Step 2: ask /import/watchlist/match-import-film/ to resolve each row
	// to a Letterboxd numeric film id.
	matchPayload := fmt.Sprintf(`{"importType":"list","importFilms":[%s]}`, strings.Join(filmObjects, ","))
	matchForm := url.Values{
		"json":   {matchPayload},
		"__csrf": {info.CSRF},
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodPost, BaseURL+"/import/watchlist/match-import-film/", strings.NewReader(matchForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("Accept", "text/html, */*; q=0.01")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Referer", BaseURL+"/import/list/")
	resp, err = c.do(req)
	if err != nil {
		return nil, fmt.Errorf("post match-import-film: %w", err)
	}
	matchBodyBytes, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read match body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("match-import-film: status %d", resp.StatusCode)
	}

	numericMatches := reImportFilmIDNum.FindAllStringSubmatch(string(matchBodyBytes), -1)
	if len(numericMatches) == 0 {
		c.dumpHTML("match-response", matchBodyBytes)
		return nil, errors.New("no numeric film ids in match response")
	}
	type stagedEntry struct {
		Film   string `json:"film"`
		Review string `json:"review"`
	}
	entries := make([]stagedEntry, 0, len(numericMatches))
	for _, m := range numericMatches {
		entries = append(entries, stagedEntry{Film: m[1]})
	}
	entriesJSON, err := json.Marshal(entries)
	if err != nil {
		return nil, fmt.Errorf("marshal staged entries: %w", err)
	}

	// Step 3: POST /list/add-films/ — response embeds LIDs of staged films
	// (the ones we need to feed to the v0 API patch).
	stageForm := url.Values{
		"__csrf":            {info.CSRF},
		"importListDetails": {"true"},
		"filmListId":        {info.FilmListId},
		"name":              {info.Name},
		"notes":             {info.Description},
		"publicList":        {""},
		"sharePolicy":       {""},
		"numberedList":      {""},
		"cancelled":         {"false"},
		"entries":           {string(entriesJSON)},
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodPost, BaseURL+"/list/add-films/", strings.NewReader(stageForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", BaseURL+"/import/list/")
	resp, err = c.do(req)
	if err != nil {
		return nil, fmt.Errorf("post add-films: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("add-films: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read add-films body: %w", err)
	}

	// Refresh list metadata (version, csrf) — the staging form bumps the
	// served version exposed in the edit HTML.
	if refreshed, err := c.parseListInfo(info.User, info.Slug, body); err == nil {
		info.Version = refreshed.Version
		info.CSRF = refreshed.CSRF
		info.ListLid = refreshed.ListLid
	}

	lidMatches := reNewFilmLID.FindAllStringSubmatch(string(body), -1)
	lids := make([]string, 0, len(lidMatches))
	for _, m := range lidMatches {
		lids = append(lids, m[1])
	}
	return lids, nil
}

type apiListEntry struct {
	Film             string `json:"film"`
	Action           string `json:"action"`
	ContainsSpoilers bool   `json:"containsSpoilers"`
}

type apiListBody struct {
	Version     int            `json:"version"`
	Published   bool           `json:"published"`
	Name        string         `json:"name"`
	SharePolicy string         `json:"sharePolicy"`
	Ranked      bool           `json:"ranked"`
	Description string         `json:"description"`
	Tags        []string       `json:"tags"`
	Entries     []apiListEntry `json:"entries"`
}

func (c *Client) patchList(ctx context.Context, info *listInfo, filmLIDs []string) error {
	if info.ListLid == "" {
		return errors.New("patch list: missing list LID")
	}
	entries := make([]apiListEntry, 0, len(filmLIDs))
	for _, lid := range filmLIDs {
		entries = append(entries, apiListEntry{Film: lid, Action: "ADD"})
	}

	body := apiListBody{
		Version:     info.Version,
		Published:   info.SharePolicy == "Public",
		Name:        info.Name,
		SharePolicy: info.SharePolicy,
		Ranked:      info.Ranked,
		Description: info.Description,
		Tags:        info.Tags,
		Entries:     entries,
	}
	if body.SharePolicy == "" {
		// The HTML default when no option is selected; API expects "You".
		body.SharePolicy = "You"
	}
	if body.Tags == nil {
		body.Tags = []string{}
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal body: %w", err)
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodPatch,
		fmt.Sprintf("%s/api/v0/list/%s", BaseURL, info.ListLid),
		bytes.NewReader(payload),
	)
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("X-Csrf-Token", info.CSRF)
	req.Header.Set("Referer", fmt.Sprintf("%s/%s/list/%s/edit/", BaseURL, info.User, info.Slug))
	resp, err := c.do(req)
	if err != nil {
		return fmt.Errorf("patch: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		c.dumpHTML("patch-response", respBody)
		return fmt.Errorf("patch: status %d: %s", resp.StatusCode, truncate(string(respBody), 400))
	}
	slog.InfoContext(ctx, "PATCH /api/v0/list ok",
		"status", resp.StatusCode, "lid", info.ListLid, "version", info.Version, "entries", len(entries))
	return nil
}

func (c *Client) saveCookies() error {
	if c.cookiePath == "" {
		return nil
	}
	u, _ := url.Parse(BaseURL)
	cookies := c.http.Jar.Cookies(u)
	data, err := json.Marshal(cookies)
	if err != nil {
		return fmt.Errorf("marshal cookies: %w", err)
	}
	return os.WriteFile(c.cookiePath, data, 0600)
}

func (c *Client) loadCookies() error {
	data, err := os.ReadFile(c.cookiePath)
	if err != nil {
		return err
	}
	var cookies []*http.Cookie
	if err := json.Unmarshal(data, &cookies); err != nil {
		return fmt.Errorf("unmarshal cookies: %w", err)
	}
	u, _ := url.Parse(BaseURL)
	c.http.Jar.SetCookies(u, cookies)
	return nil
}

func (c *Client) dumpHTML(tag string, body []byte) {
	if c.dumpDir == "" {
		return
	}
	path := fmt.Sprintf("%s/letterboxd_%s.html", strings.TrimRight(c.dumpDir, "/"), tag)
	if err := os.WriteFile(path, body, 0600); err != nil {
		slog.Warn("dumpHTML: write failed", "path", path, "err", err)
		return
	}
	slog.Info("dumpHTML: wrote", "path", path, "bytes", len(body))
}

func parseListURL(raw string) (user, slug string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", err
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 3 || parts[1] != "list" {
		return "", "", fmt.Errorf("expected /<user>/list/<slug>, got %q", u.Path)
	}
	return parts[0], parts[2], nil
}

// findInputValue locates the `value` of an <input> whose `name` matches the
// given argument, tolerating either attribute order. The returned value is
// HTML-unescaped.
func findInputValue(src, name string) string {
	q := regexp.QuoteMeta(name)
	for _, pattern := range []string{
		`<input\b[^>]*\bname="` + q + `"[^>]*\bvalue="([^"]*)"`,
		`<input\b[^>]*\bvalue="([^"]*)"[^>]*\bname="` + q + `"\b`,
	} {
		re := regexp.MustCompile(pattern)
		if m := re.FindStringSubmatch(src); len(m) > 1 {
			return html.UnescapeString(m[1])
		}
	}
	return ""
}

// hasCheckedAttr reports whether the named <input> element carries the
// `checked` attribute.
func hasCheckedAttr(src, name string) bool {
	q := regexp.QuoteMeta(name)
	re := regexp.MustCompile(`<input\b[^>]*\bname="` + q + `"[^>]*\bchecked\b`)
	return re.MatchString(src)
}

var reSelectBody = regexp.MustCompile(`(?s)<select\b[^>]*\bname="%s"[^>]*>(.*?)</select>`)
var reSelectedOption = regexp.MustCompile(`<option\b[^>]*\bvalue="([^"]*)"[^>]*\bselected\b`)

// findSelectedOption returns the `value` of the <option selected> inside
// the first <select> with the given name. Empty if none selected.
func findSelectedOption(src, selectName string) string {
	q := regexp.QuoteMeta(selectName)
	re := regexp.MustCompile(`(?s)<select\b[^>]*\bname="` + q + `"[^>]*>(.*?)</select>`)
	m := re.FindStringSubmatch(src)
	if len(m) < 2 {
		return ""
	}
	if sm := reSelectedOption.FindStringSubmatch(m[1]); len(sm) > 1 {
		return html.UnescapeString(sm[1])
	}
	return ""
}

// findTextareaValue returns the textual content of the first <textarea>
// with the given name (HTML-unescaped).
func findTextareaValue(src, name string) string {
	q := regexp.QuoteMeta(name)
	re := regexp.MustCompile(`(?s)<textarea\b[^>]*\bname="` + q + `"[^>]*>(.*?)</textarea>`)
	m := re.FindStringSubmatch(src)
	if len(m) < 2 {
		return ""
	}
	return html.UnescapeString(m[1])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
