package api

import (
	"html"
	"regexp"
	"slices"
	"strings"
)

// The pages are read with regular expressions over start tags rather than a
// parser: what is wanted is a handful of attributes on elements the server
// renders, never anything a script builds.

var (
	reStartTag  = regexp.MustCompile(`(?is)<([a-z][a-z0-9-]*)((?:\s(?:[^>"']|"[^"]*"|'[^']*')*)?)/?>`)
	reAttribute = regexp.MustCompile(`([^\s"'<>/=]+)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'=<>` + "`" + `]+)))?`)
	// reCsrf is the page's own CSRF token, which its scripts send as
	// X-CSRF-TOKEN and __csrf; the first assignment declares it empty.
	reCsrf = regexp.MustCompile(`supermodelCSRF\s*=\s*['"]([^'"]+)['"]`)
	// reUnrendered is markup that is not rendered as it stands: comments, and
	// scripts and templates, whose bodies may hold markup of their own.
	reUnrendered = regexp.MustCompile(`(?is)<!--.*?-->|<script\b[^>]*>.*?</script\s*>|<template\b[^>]*>.*?</template\s*>`)
	// reItemTag is a div start or end tag, or an input, for following which
	// div an input sits in.
	reItemTag = regexp.MustCompile(`(?is)<(/?)(div|input)\b((?:[^>"']|"[^"]*"|'[^']*')*)>`)
)

// startTag is an element's start tag: its name and its attributes, with their
// values unescaped. An attribute without a value maps to "".
type startTag struct {
	Name       string
	Attributes map[string]string
}

func (t *startTag) hasClass(class string) bool {
	return slices.Contains(strings.Fields(t.Attributes["class"]), class)
}

// parseAttributes reads the attributes of a start tag, unescaping their values.
func parseAttributes(src string) map[string]string {
	attributes := make(map[string]string)
	for _, attribute := range reAttribute.FindAllStringSubmatch(src, -1) {
		key := strings.ToLower(attribute[1])
		if _, seen := attributes[key]; seen {
			// The first of a repeated attribute wins, as in a browser.
			continue
		}
		attributes[key] = html.UnescapeString(attribute[2] + attribute[3] + attribute[4])
	}
	return attributes
}

// startTags returns the start tags of the named elements, or of every element
// when none is named, in document order, leaving out what is not rendered.
func startTags(src string, names ...string) []*startTag {
	var tags []*startTag
	for _, match := range reStartTag.FindAllStringSubmatch(reUnrendered.ReplaceAllString(src, ""), -1) {
		name := strings.ToLower(match[1])
		if len(names) > 0 && !slices.Contains(names, name) {
			continue
		}
		tags = append(tags, &startTag{Name: name, Attributes: parseAttributes(match[2])})
	}
	return tags
}

// extractFormById returns the start tag of <form id="<id>"> and its contents,
// or nothing when there is no such form.
func extractFormById(src string, id string) (*startTag, string) {
	re := regexp.MustCompile(`(?is)(<form\b[^>]*\bid\s*=\s*["']` + regexp.QuoteMeta(id) + `["'][^>]*>)(.*?)</form>`)
	match := re.FindStringSubmatch(src)
	if len(match) < 3 {
		return nil, ""
	}
	tags := startTags(match[1], "form")
	if len(tags) == 0 {
		return nil, ""
	}
	return tags[0], match[2]
}

// csrfToken returns the token the page's scripts post with: the last value
// assigned, as the scripts run in order.
func csrfToken(src string) string {
	matches := reCsrf.FindAllStringSubmatch(src, -1)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1][1]
}

// listEditorTag returns the element the list editor is rendered into, whose
// attributes carry the list.
func listEditorTag(src string) *startTag {
	for _, tag := range startTags(src, "div") {
		if tag.hasClass("list-editor-wrapper") || tag.Attributes["data-component-class"] == "ListEditor" {
			return tag
		}
	}
	return nil
}

// importFilmObjects returns the rows the importer understood from the CSV, as
// the JSON objects it embeds on each <li class="import-film" data-json="...">.
func importFilmObjects(src string) []string {
	var objects []string
	for _, tag := range startTags(src, "li") {
		if !tag.hasClass("import-film") {
			continue
		}
		if object := strings.TrimSpace(tag.Attributes["data-json"]); object != "" {
			objects = append(objects, object)
		}
	}
	return objects
}

// matchedProductions returns the LIDs of the films the rows were matched to
// and that are to be imported, each once, in order: the importProductionId of
// every div.matched-item whose shouldImportProduction is "true", as the
// site's importer collects them. Only inputs inside an item count.
func matchedProductions(src string) []string {
	type item struct {
		id       string
		included bool
	}

	var (
		items []*item
		// open is the item being read and depth how many divs deep it is.
		open        *item
		depth       int
		openedDepth int
	)
	for _, match := range reItemTag.FindAllStringSubmatch(reUnrendered.ReplaceAllString(src, ""), -1) {
		closing, name := match[1] == "/", strings.ToLower(match[2])

		switch {
		case name == "div" && closing:
			depth--
			if open != nil && depth < openedDepth {
				open = nil
			}
		case name == "div":
			depth++
			if open == nil && slices.Contains(strings.Fields(parseAttributes(match[3])["class"]), "matched-item") {
				open = &item{}
				openedDepth = depth
				items = append(items, open)
			}
		case name == "input" && open != nil:
			attributes := parseAttributes(match[3])
			switch attributes["name"] {
			case "importProductionId":
				open.id = strings.TrimSpace(attributes["value"])
			case "shouldImportProduction":
				open.included = attributes["value"] == "true"
			}
		}
	}

	var ids []string
	for _, item := range items {
		if item.id != "" && item.included && !slices.Contains(ids, item.id) {
			ids = append(ids, item.id)
		}
	}
	return ids
}
