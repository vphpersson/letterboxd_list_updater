package update_list_endpoint

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	motmedelErrors "github.com/Motmedel/utils_go/pkg/errors"
	"github.com/Motmedel/utils_go/pkg/errors/types/nil_error"
	"github.com/Motmedel/utils_go/pkg/http/mux/types/body_loader"
	"github.com/Motmedel/utils_go/pkg/http/mux/types/body_parser"
	bodyParserAdapter "github.com/Motmedel/utils_go/pkg/http/mux/types/body_parser/adapter"
	jsonSchemaBodyParser "github.com/Motmedel/utils_go/pkg/http/mux/types/body_parser/json_schema_body_parser"
	"github.com/Motmedel/utils_go/pkg/http/mux/types/endpoint"
	"github.com/Motmedel/utils_go/pkg/http/mux/types/endpoint/initialization_endpoint"
	"github.com/Motmedel/utils_go/pkg/http/mux/types/processor"
	"github.com/Motmedel/utils_go/pkg/http/mux/types/response"
	"github.com/Motmedel/utils_go/pkg/http/mux/types/response_error"
	muxUtils "github.com/Motmedel/utils_go/pkg/http/mux/utils"
	"github.com/Motmedel/utils_go/pkg/http/types/problem_detail"
	"github.com/Motmedel/utils_go/pkg/http/types/problem_detail/problem_detail_config"
	motmedelReflect "github.com/Motmedel/utils_go/pkg/reflect"
	"github.com/vphpersson/letterboxd_list_updater/api/letterboxd"
	"github.com/vphpersson/letterboxd_list_updater/api/types"
	"github.com/vphpersson/letterboxd_list_updater/api/utils"
)

type Endpoint struct {
	*initialization_endpoint.Endpoint
}

type BodyInput = types.UpdateList
type Stored = *types.ParsedUpdate

var bodyParser *jsonSchemaBodyParser.Parser[*BodyInput]

func parseAndValidate(_ context.Context, input *BodyInput) (Stored, *response_error.ResponseError) {
	entries, err := utils.ParseImportCSV([]byte(input.Data))
	if err != nil {
		return nil, &response_error.ResponseError{
			ProblemDetail: problem_detail.New(
				http.StatusBadRequest,
				problem_detail_config.WithDetail(fmt.Sprintf("Invalid CSV: %v", err)),
			),
			ClientError: motmedelErrors.New(fmt.Errorf("parse import csv: %w", err)),
		}
	}
	return &types.ParsedUpdate{List: input.List, Entries: entries}, nil
}

func (e *Endpoint) Initialize(client *letterboxd.Client) error {
	if client == nil {
		return motmedelErrors.NewWithTrace(nil_error.New("letterboxd client"))
	}

	e.Handler = func(request *http.Request, _ []byte) (*response.Response, *response_error.ResponseError) {
		ctx := request.Context()

		parsed, responseError := muxUtils.GetServerNonZeroParsedRequestBody[Stored](ctx)
		if responseError != nil {
			return nil, responseError
		}

		listPath := strings.Trim(parsed.List, "/")
		if listPath == "" || !strings.Contains(listPath, "/") {
			return nil, &response_error.ResponseError{
				ProblemDetail: problem_detail.New(
					http.StatusBadRequest,
					problem_detail_config.WithDetail("list must be in the form \"user/slug\"."),
				),
			}
		}
		listURL := letterboxd.BaseURL + "/" + strings.Replace(listPath, "/", "/list/", 1)

		csv := utils.ImportEntriesToCSV(parsed.Entries)

		if err := client.UpdateList(ctx, listURL, csv); err != nil {
			return nil, &response_error.ResponseError{
				ServerError: motmedelErrors.New(fmt.Errorf("update list: %w", err), parsed.List),
			}
		}

		return nil, nil
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
					ContentType: "application/json",
					MaxBytes:    2 << 20,
				},
				Hint: &endpoint.Hint{
					InputType:         motmedelReflect.TypeOf[BodyInput](),
					OutputContentType: "text/plain",
				},
			},
		},
	}
}

func init() {
	var err error
	bodyParser, err = jsonSchemaBodyParser.New[*BodyInput]()
	if err != nil {
		panic(motmedelErrors.NewWithTrace(fmt.Errorf("json schema body parser: %w", err)))
	}
}

const DefaultPath = "/api/list"
