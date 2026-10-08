package utils

import (
	"testing"

	altshiftTestingCmp "github.com/altshiftab/utils_go/pkg/testing/cmp"
	"github.com/vphpersson/letterboxd_list_updater/api/types"
)

func TestImportEntriesToCSV(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		entries  []*types.ImportEntry
		expected string
	}{
		{
			name:     "only the columns with values",
			entries:  []*types.ImportEntry{{ImdbID: "tt6751668"}, nil, {Title: "Naza", Directors: "A, B"}},
			expected: "imdbID,Title,Directors\ntt6751668,,\n,Naza,\"A, B\"\n",
		},
		{
			name:     "quotes and backslashes escaped with a backslash",
			entries:  []*types.ImportEntry{{ImdbID: "tt1", Review: "# NYT\n\nA \"great\" film \\ yes"}},
			expected: "imdbID,Review\ntt1,\"# NYT\n\nA \\\"great\\\" film \\\\ yes\"\n",
		},
		{name: "nothing", expected: "\n"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := string(ImportEntriesToCSV(testCase.entries)); got != testCase.expected {
				t.Errorf("unexpected csv: %q", got)
			}
		})
	}
}

func TestParseImportCSV(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		data     string
		expected []*types.ImportEntry
		wantErr  bool
	}{
		{
			name:     "the url alias and crlf",
			data:     "url,Title\r\nhttps://boxd.it/x,Naza\r\n",
			expected: []*types.ImportEntry{{LetterboxdURI: "https://boxd.it/x", Title: "Naza"}},
		},
		{
			name:     "a quoted field with escapes and a newline",
			data:     "imdbID,Review\ntt1,\"# NYT\n\nA \\\"great\\\" film\"",
			expected: []*types.ImportEntry{{ImdbID: "tt1", Review: "# NYT\n\nA \"great\" film"}},
		},
		{name: "short rows", data: "imdbID,Title\ntt1\n", expected: []*types.ImportEntry{{ImdbID: "tt1"}}},
		{name: "empty", data: ""},
		{name: "unterminated quote", data: "imdbID\n\"tt1\n", wantErr: true},
		{name: "stray quote", data: "imdbID\ntt\"1\n", wantErr: true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			entries, err := ParseImportCSV([]byte(testCase.data))
			if testCase.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if diff := altshiftTestingCmp.Diff(testCase.expected, entries); diff != "" {
				t.Errorf("entries mismatch (-expected +got):\n%s", diff)
			}
		})
	}
}

// What is encoded parses back to the same entries.
func TestRoundTrip(t *testing.T) {
	t.Parallel()

	entries := []*types.ImportEntry{
		{ImdbID: "tt33562917", Review: "# NYT\n\nA girlfriend’s pregnancy, \"upends\" a life \\ in the Bronx.\n\nhttps://www.nytimes.com/x.html"},
		{Title: "Lady", Directors: "Olive Nwosu"},
	}
	parsed, err := ParseImportCSV(ImportEntriesToCSV(entries))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if diff := altshiftTestingCmp.Diff(entries, parsed); diff != "" {
		t.Errorf("round trip mismatch (-expected +got):\n%s", diff)
	}
}
