package update_list_endpoint

import (
	"fmt"
	"log/slog"
	"net/http"

	motmedelErrors "github.com/Motmedel/utils_go/pkg/errors"
	"github.com/Motmedel/utils_go/pkg/http/mux/types/body_loader"
	bodyParserAdapter "github.com/Motmedel/utils_go/pkg/http/mux/types/body_parser/adapter"
	jsonSchemaBodyParser "github.com/Motmedel/utils_go/pkg/http/mux/types/body_parser/json_schema_body_parser"
	"github.com/Motmedel/utils_go/pkg/http/mux/types/endpoint"
	"github.com/Motmedel/utils_go/pkg/http/mux/types/endpoint/initialization_endpoint"
	"github.com/Motmedel/utils_go/pkg/http/mux/types/response"
	"github.com/Motmedel/utils_go/pkg/http/mux/types/response_error"
	muxUtils "github.com/Motmedel/utils_go/pkg/http/mux/utils"
	motmedelReflect "github.com/Motmedel/utils_go/pkg/reflect"
	"github.com/vphpersson/letterboxd_list_updater/api/types"
)

type Endpoint struct {
	*initialization_endpoint.Endpoint
}

type BodyInput = types.UpdateList

var bodyParser *jsonSchemaBodyParser.Parser[*BodyInput]

func (e *Endpoint) Initialize() error {
	e.Handler = func(request *http.Request, _ []byte) (*response.Response, *response_error.ResponseError) {
		ctx := request.Context()

		input, responseError := muxUtils.GetServerNonZeroParsedRequestBody[*BodyInput](ctx)
		if responseError != nil {
			return nil, responseError
		}

		// TODO: merge CSV notes with existing list entries and push to Letterboxd.

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
					Parser:      bodyParserAdapter.New(bodyParser),
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
