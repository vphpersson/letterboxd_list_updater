package update_list_endpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	altshiftErrors "github.com/altshiftab/utils_go/pkg/errors"
	"github.com/altshiftab/utils_go/pkg/errors/types/nil_error"
	altshiftMux "github.com/altshiftab/utils_go/pkg/http/mux"
	altshiftTestingCmp "github.com/altshiftab/utils_go/pkg/testing/cmp"
	"github.com/vphpersson/letterboxd_list_updater/api"
	"github.com/vphpersson/letterboxd_list_updater/api/types"
)

var errSiteDown = errors.New("site down")

// fakeUpdater keeps what it was asked and answers as told.
type fakeUpdater struct {
	err error

	calls    int
	listPath string
	csv      string
	dryRun   bool
	ctxErr   error
}

func (f *fakeUpdater) UpdateList(ctx context.Context, listPath string, csv []byte, dryRun bool) (*types.UpdateResult, error) {
	f.calls++
	f.listPath, f.csv, f.dryRun = listPath, string(csv), dryRun
	f.ctxErr = ctx.Err()
	if f.err != nil {
		return nil, f.err
	}
	return &types.UpdateResult{List: listPath, Matched: 2, Added: 1, DryRun: dryRun}, nil
}

func serve(t *testing.T, updater Updater, body string) *httptest.ResponseRecorder {
	t.Helper()

	endpoint := New()
	if err := endpoint.Initialize(updater); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	request := httptest.NewRequestWithContext(t.Context(), http.MethodPatch, DefaultPath, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Content-Length", strconv.Itoa(len(body)))
	recorder := httptest.NewRecorder()
	altshiftMux.New(endpoint.Endpoint.Endpoint).ServeHTTP(recorder, request)
	return recorder
}

func TestEndpoint(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name       string
		body       string
		updaterErr error
		wantStatus int
		wantCalls  int
		wantCsv    string
		wantDryRun bool
		expected   *types.UpdateResult
	}{
		{
			name:       "updated",
			body:       `{"list": "vph/collected", "data": "imdbID,Review\ntt6751668,\"# NYT\n\nA \\\"great\\\" film.\"\n"}`,
			wantStatus: http.StatusOK,
			wantCalls:  1,
			wantCsv:    "imdbID,Review\ntt6751668,\"# NYT\n\nA \\\"great\\\" film.\"\n",
			expected:   &types.UpdateResult{List: "vph/collected", Matched: 2, Added: 1},
		},
		{
			name:       "a dry run",
			body:       `{"list": "vph/collected", "dry_run": true, "data": "imdbID\ntt6751668\n"}`,
			wantStatus: http.StatusOK,
			wantCalls:  1,
			wantCsv:    "imdbID\ntt6751668\n",
			wantDryRun: true,
			expected:   &types.UpdateResult{List: "vph/collected", Matched: 2, Added: 1, DryRun: true},
		},
		{name: "a bad list path", body: `{"list": "collected", "data": "imdbID\ntt6751668\n"}`, wantStatus: http.StatusBadRequest},
		{name: "bad csv", body: `{"list": "vph/collected", "data": "imdbID\n\"tt6751668\n"}`, wantStatus: http.StatusBadRequest},
		{name: "no rows", body: `{"list": "vph/collected", "data": "imdbID\n"}`, wantStatus: http.StatusBadRequest},
		{name: "no data", body: `{"list": "vph/collected"}`, wantStatus: http.StatusUnprocessableEntity},
		{
			name:       "the site failing",
			body:       `{"list": "vph/collected", "data": "imdbID\ntt6751668\n"}`,
			updaterErr: errSiteDown,
			wantStatus: http.StatusBadGateway,
			wantCalls:  1,
			wantCsv:    "imdbID\ntt6751668\n",
		},
		{
			name:       "another update running",
			body:       `{"list": "vph/collected", "data": "imdbID\ntt6751668\n"}`,
			updaterErr: fmt.Errorf("update: %w", api.ErrBusy),
			wantStatus: http.StatusServiceUnavailable,
			wantCalls:  1,
			wantCsv:    "imdbID\ntt6751668\n",
		},
		{
			name:       "refused as invalid",
			body:       `{"list": "vph/collected", "data": "imdbID\ntt6751668\n"}`,
			updaterErr: fmt.Errorf("%w: nope", altshiftErrors.ErrValidationError),
			wantStatus: http.StatusBadRequest,
			wantCalls:  1,
			wantCsv:    "imdbID\ntt6751668\n",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			updater := &fakeUpdater{err: testCase.updaterErr}
			recorder := serve(t, updater, testCase.body)

			if recorder.Code != testCase.wantStatus {
				t.Fatalf("unexpected status %d: %s", recorder.Code, recorder.Body.String())
			}
			if updater.calls != testCase.wantCalls {
				t.Fatalf("unexpected calls: %d", updater.calls)
			}
			if testCase.wantCalls == 0 {
				return
			}
			if updater.listPath != "vph/collected" || updater.csv != testCase.wantCsv || updater.dryRun != testCase.wantDryRun {
				t.Errorf("unexpected update: %q %q %v", updater.listPath, updater.csv, updater.dryRun)
			}
			if updater.ctxErr != nil {
				t.Errorf("the update ran on an ended context: %v", updater.ctxErr)
			}
			if testCase.expected == nil {
				return
			}

			var result types.UpdateResult
			if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
				t.Fatalf("json unmarshal: %v", err)
			}
			if diff := altshiftTestingCmp.Diff(testCase.expected, &result); diff != "" {
				t.Errorf("result mismatch (-expected +got):\n%s", diff)
			}
		})
	}
}

func TestInitialize(t *testing.T) {
	t.Parallel()

	endpoint := New()
	err := endpoint.Initialize(nil)
	altshiftTestingCmp.CompareErr(t, err, nil_error.New("updater"))
	if endpoint.Initialized {
		t.Error("initialized without an updater")
	}
}
