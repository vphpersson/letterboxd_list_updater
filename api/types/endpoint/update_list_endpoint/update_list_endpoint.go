package update_list_endpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	altshiftErrors "github.com/altshiftab/utils_go/pkg/errors"
	"github.com/altshiftab/utils_go/pkg/errors/types/nil_error"
	"github.com/altshiftab/utils_go/pkg/http/mux/types/body_loader"
	"github.com/altshiftab/utils_go/pkg/http/mux/types/body_parser"
	bodyParserAdapter "github.com/altshiftab/utils_go/pkg/http/mux/types/body_parser/adapter"
	jsonSchemaBodyParser "github.com/altshiftab/utils_go/pkg/http/mux/types/body_parser/json_schema_body_parser"
	"github.com/altshiftab/utils_go/pkg/http/mux/types/endpoint"
	"github.com/altshiftab/utils_go/pkg/http/mux/types/endpoint/initialization_endpoint"
	"github.com/altshiftab/utils_go/pkg/http/mux/types/processor"
	"github.com/altshiftab/utils_go/pkg/http/mux/types/response"
	"github.com/altshiftab/utils_go/pkg/http/mux/types/response_error"
	muxUtils "github.com/altshiftab/utils_go/pkg/http/mux/utils"
	"github.com/altshiftab/utils_go/pkg/http/types/problem_detail"
	"github.com/altshiftab/utils_go/pkg/http/types/problem_detail/problem_detail_config"
	altshiftReflect "github.com/altshiftab/utils_go/pkg/reflect"
	"github.com/vphpersson/letterboxd_list_updater/api"
	"github.com/vphpersson/letterboxd_list_updater/api/types"
	"github.com/vphpersson/letterboxd_list_updater/api/utils"
)

const (
	DefaultPath = "/api/list"

	jsonContentType = "application/json"

	// updateTimeout bounds one update: a Chrome started, a challenge, perhaps
	// a sign-in, and five requests.
	updateTimeout = 5 * time.Minute
)

// Updater updates a list; *api.Client is one.
type Updater interface {
	UpdateList(ctx context.Context, listPath string, csv []byte, dryRun bool) (*types.UpdateResult, error)
}

type Endpoint struct {
	*initialization_endpoint.Endpoint
}

type BodyInput = types.UpdateList
type Stored = *types.ParsedUpdate

var bodyParser *jsonSchemaBodyParser.Parser[*BodyInput]

func parseAndValidate(_ context.Context, input *BodyInput) (Stored, *response_error.ResponseError) {
	if _, _, err := api.ParseListPath(input.List); err != nil {
		return nil, &response_error.ResponseError{
			ProblemDetail: problem_detail.New(
				http.StatusBadRequest,
				problem_detail_config.WithDetail("list must be in the form \"user/slug\"."),
			),
			ClientError: err,
		}
	}

	entries, err := utils.ParseImportCSV([]byte(input.Data))
	if err != nil {
		return nil, &response_error.ResponseError{
			ProblemDetail: problem_detail.New(
				http.StatusBadRequest,
				problem_detail_config.WithDetail(fmt.Sprintf("Invalid CSV: %v", err)),
			),
			ClientError: altshiftErrors.New(fmt.Errorf("%w: parse import csv: %w", altshiftErrors.ErrParseError, err)),
		}
	}
	if len(entries) == 0 {
		return nil, &response_error.ResponseError{
			ProblemDetail: problem_detail.New(
				http.StatusBadRequest,
				problem_detail_config.WithDetail("The CSV has no rows."),
			),
		}
	}

	return &types.ParsedUpdate{List: input.List, Entries: entries, DryRun: input.DryRun}, nil
}

func (e *Endpoint) Initialize(updater Updater) error {
	if updater == nil {
		return altshiftErrors.NewWithTrace(nil_error.New("updater"))
	}

	e.Handler = func(request *http.Request, _ []byte) (*response.Response, *response_error.ResponseError) {
		parsed, responseError := muxUtils.GetServerNonZeroParsedRequestBody[Stored](request.Context())
		if responseError != nil {
			return nil, responseError
		}

		// The update runs to the end even when the caller stops waiting: one
		// cut short between staging and committing leaves the work half done.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(request.Context()), updateTimeout)
		defer cancel()

		csv := utils.ImportEntriesToCSV(parsed.Entries)
		result, err := updater.UpdateList(ctx, parsed.List, csv, parsed.DryRun)
		if err != nil {
			status := http.StatusBadGateway
			switch {
			case errors.Is(err, altshiftErrors.ErrValidationError):
				status = http.StatusBadRequest
			case errors.Is(err, api.ErrBusy):
				status = http.StatusServiceUnavailable
			}
			return nil, &response_error.ResponseError{
				ProblemDetail: problem_detail.New(status),
				ServerError:   altshiftErrors.New(fmt.Errorf("update list: %w", err), parsed.List),
			}
		}

		data, err := json.Marshal(result)
		if err != nil {
			return nil, &response_error.ResponseError{
				ServerError: altshiftErrors.NewWithTrace(fmt.Errorf("json marshal: %w", err), result),
			}
		}

		return &response.Response{
			Headers: []*response.HeaderEntry{{Name: "Content-Type", Value: jsonContentType}},
			Body:    data,
		}, nil
	}

	e.Initialized = true
	return nil
}

func New() *Endpoint {
	return &Endpoint{
		Endpoint: &initialization_endpoint.Endpoint{
			Endpoint: &endpoint.Endpoint{
				Path:   DefaultPath,
				Method: http.MethodPatch,
				BodyLoader: &body_loader.Loader{
					Parser: bodyParserAdapter.New[Stored](
						body_parser.NewWithProcessor(
							bodyParser,
							processor.New(parseAndValidate),
						),
					),
					ContentType: jsonContentType,
					MaxBytes:    2 << 20,
				},
				Hint: &endpoint.Hint{
					InputType:         altshiftReflect.TypeOf[BodyInput](),
					OutputContentType: jsonContentType,
				},
			},
		},
	}
}

func init() {
	var err error
	bodyParser, err = jsonSchemaBodyParser.New[*BodyInput]()
	if err != nil {
		panic(altshiftErrors.NewWithTrace(fmt.Errorf("json schema body parser: %w", err)))
	}
}
