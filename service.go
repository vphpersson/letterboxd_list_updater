package main

import (
	"fmt"
	"log/slog"
	"net/http"

	motmedelEnv "github.com/Motmedel/utils_go/pkg/env"
	motmedelErrors "github.com/Motmedel/utils_go/pkg/errors"
	motmedelMux "github.com/Motmedel/utils_go/pkg/http/mux"
	"github.com/Motmedel/utils_go/pkg/utils"
	gcpUtilsLogger "github.com/altshiftab/gcp_utils/pkg/types/logger"
	"github.com/altshiftab/gcp_utils/pkg/types/logger/logger_config"
	"github.com/vphpersson/letterboxd_list_updater/api"
	letterboxdEndpoint "github.com/vphpersson/letterboxd_list_updater/api/types/endpoint"
)

func main() {
	logger := gcpUtilsLogger.New(logger_config.WithLogLevel(slog.LevelDebug))
	slog.SetDefault(logger.Logger)

	port := motmedelEnv.GetEnvWithDefault("PORT", "8080")

	username := motmedelEnv.ReadEnvFatal("LETTERBOXD_USERNAME")
	password := motmedelEnv.ReadEnvFatal("LETTERBOXD_PASSWORD")
	cookiePath := motmedelEnv.GetEnvWithDefault("LETTERBOXD_COOKIE_PATH", "")
	dumpDir := motmedelEnv.GetEnvWithDefault("LETTERBOXD_DUMP_DIR", "/tmp")

	client, err := api.NewClient(
		api.Options{
			Username:   username,
			Password:   password,
			CookiePath: cookiePath,
			DumpDir:    dumpDir,
		},
	)
	if err != nil {
		logger.FatalWithExitingMessage("An error occurred when creating the Letterboxd client.", err)
	}
	defer client.Close()

	overview := letterboxdEndpoint.NewOverview()

	utils.Must(overview.UpdateList.Initialize(client), "update list initialize")

	mux := motmedelMux.New()

	for _, endpoint := range overview.Endpoints() {
		if endpoint == nil {
			continue
		}
		if !endpoint.Initialized {
			logger.FatalWithExitingMessage(
				fmt.Sprintf("Endpoint \"%s %s\" is not initialized.", endpoint.Method, endpoint.Path),
				nil,
			)
		}
		mux.Add(endpoint.Endpoint)
	}

	httpServer := &http.Server{Addr: fmt.Sprintf(":%s", port), Handler: mux}

	if err := httpServer.ListenAndServe(); err != nil {
		logger.FatalWithExitingMessage(
			"An error occurred when listening and serving.",
			motmedelErrors.NewWithTrace(fmt.Errorf("http server listen and serve: %w", err), httpServer),
		)
	}
}
