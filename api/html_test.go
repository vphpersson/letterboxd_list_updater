package api

import (
	"testing"

	altshiftTestingCmp "github.com/altshiftab/utils_go/pkg/testing/cmp"
)

func TestStartTags(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		src      string
		names    []string
		expected []*startTag
	}{
		{
			name:  "attribute forms and entities",
			src:   `<li class="import-film" data-json="{&quot;a&quot;:1}" hidden data-x='y "z"' data-n=5>`,
			names: []string{"li"},
			expected: []*startTag{{Name: "li", Attributes: map[string]string{
				"class": "import-film", "data-json": `{"a":1}`, "hidden": "", "data-x": `y "z"`, "data-n": "5",
			}}},
		},
		{
			name:     "a quoted > does not end the tag",
			src:      `<input value="a>b" name="x"/>`,
			names:    []string{"input"},
			expected: []*startTag{{Name: "input", Attributes: map[string]string{"value": "a>b", "name": "x"}}},
		},
		{
			name:     "the first of a repeated attribute wins",
			src:      `<INPUT NAME="first" name="second">`,
			names:    []string{"input"},
			expected: []*startTag{{Name: "input", Attributes: map[string]string{"name": "first"}}},
		},
		{
			name:  "filtered by name, in order",
			src:   `<div id="a"><span id="b"></span><input id="c"></div>`,
			names: []string{"div", "input"},
			expected: []*startTag{
				{Name: "div", Attributes: map[string]string{"id": "a"}},
				{Name: "input", Attributes: map[string]string{"id": "c"}},
			},
		},
		{
			name: "every element when none is named",
			src:  `<p><br></p>`,
			expected: []*startTag{
				{Name: "p", Attributes: map[string]string{}},
				{Name: "br", Attributes: map[string]string{}},
			},
		},
		{
			name:     "end tags, comments, scripts and templates are not start tags",
			src:      `</div><!-- <div id="old"> --><script type="text/template"><div id="tpl"></div></script><template><div id="t"></div></template><div id="real">`,
			names:    []string{"div"},
			expected: []*startTag{{Name: "div", Attributes: map[string]string{"id": "real"}}},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if diff := altshiftTestingCmp.Diff(testCase.expected, startTags(testCase.src, testCase.names...)); diff != "" {
				t.Errorf("tags mismatch (-expected +got):\n%s", diff)
			}
		})
	}
}

func TestExtractFormById(t *testing.T) {
	t.Parallel()

	src := `<form id="other"><input name="a"></form><form method="POST" action="/list/add-films/" id="importer-matched-form"><input name="b"></form>`

	form, contents := extractFormById(src, "importer-matched-form")
	if form == nil || form.Attributes["action"] != "/list/add-films/" {
		t.Fatalf("unexpected form: %+v", form)
	}
	if contents != `<input name="b">` {
		t.Errorf("unexpected contents: %q", contents)
	}

	if form, contents := extractFormById(src, "missing"); form != nil || contents != "" {
		t.Errorf("found a form that is not there: %+v %q", form, contents)
	}
}

func TestCsrfToken(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		src      string
		expected string
	}{
		{
			name:     "the declaration is skipped",
			src:      "var x = {\n\tsupermodelCSRF = \"\",\n};\nsupermodelCSRF = 'c45ff41db9e6df185c8a'; geolocation.country = 'SE';",
			expected: "c45ff41db9e6df185c8a",
		},
		{name: "double quotes", src: `supermodelCSRF="abc"`, expected: "abc"},
		{name: "the last assignment", src: `supermodelCSRF = 'placeholder'; supermodelCSRF = 'abc';`, expected: "abc"},
		{name: "none", src: `supermodelCSRF = ""`},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := csrfToken(testCase.src); got != testCase.expected {
				t.Errorf("unexpected token: %q", got)
			}
		})
	}
}

func TestImportFilmObjects(t *testing.T) {
	t.Parallel()

	src := `<ul id="import-films">
		<li class="import-film" data-json="{&quot;imdbId&quot;:&quot;tt6751668&quot;}"><input type="hidden" name="importRating" value=""></li>
		<li data-json="{&quot;imdbId&quot;:&quot;tt0081505&quot;}" class="import-film -other">
		<li class="not-import-film" data-json="{}">
		<li class="import-film" data-json="">
	</ul>`

	expected := []string{`{"imdbId":"tt6751668"}`, `{"imdbId":"tt0081505"}`}
	if diff := altshiftTestingCmp.Diff(expected, importFilmObjects(src)); diff != "" {
		t.Errorf("objects mismatch (-expected +got):\n%s", diff)
	}
}

func TestMatchedProductions(t *testing.T) {
	t.Parallel()

	item := func(id string, should string) string {
		html := `<div class="matched-item"><input type="hidden" name="importProductionId" value="` + id + `"/><input type="hidden" name="importViewingId" value=""/><div class="import-film-details has-icon" data-duplicate=""><div class="import-match">`
		if should != "" {
			html += `<input type="hidden" name="shouldImportProduction" value="` + should + `" />`
		}
		return html + `</div></div></div>`
	}

	testCases := []struct {
		name     string
		src      string
		expected []string
	}{
		{name: "matched", src: item("hTha", "true") + item("29Nu", "true"), expected: []string{"hTha", "29Nu"}},
		{name: "not to be imported", src: item("hTha", "false") + item("29Nu", "true"), expected: []string{"29Nu"}},
		{name: "unmatched", src: item("", "") + item("29Nu", "true"), expected: []string{"29Nu"}},
		{name: "no decision", src: item("hTha", ""), expected: nil},
		{name: "once each", src: item("hTha", "true") + item("hTha", "true"), expected: []string{"hTha"}},
		{name: "inputs before any item", src: `<input name="importProductionId" value="x"><input name="shouldImportProduction" value="true">`, expected: nil},
		{
			name:     "an input after the last item",
			src:      `<form>` + item("hTha", "true") + `<input type="hidden" name="shouldImportProduction" value="false"></form>`,
			expected: []string{"hTha"},
		},
		{
			name:     "an item in a commented-out copy",
			src:      `<!--` + item("old", "true") + `-->` + item("hTha", "true"),
			expected: []string{"hTha"},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if diff := altshiftTestingCmp.Diff(testCase.expected, matchedProductions(testCase.src)); diff != "" {
				t.Errorf("productions mismatch (-expected +got):\n%s", diff)
			}
		})
	}
}

// The editor element is the rendered one, not a commented-out copy before it.
func TestListEditorTag(t *testing.T) {
	t.Parallel()

	src := `<!-- <div class="list-editor-wrapper" data-list-lid="OLD"> --><div data-component-class="ListEditor" class="react-component list-editor-wrapper" data-list-lid="TZx9e"></div>`
	tag := listEditorTag(src)
	if tag == nil || tag.Attributes["data-list-lid"] != "TZx9e" {
		t.Fatalf("unexpected editor: %+v", tag)
	}

	if tag := listEditorTag(`<div class="list-editor"></div>`); tag != nil {
		t.Errorf("found an editor that is not there: %+v", tag)
	}
}
