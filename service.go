package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	altshiftEnv "github.com/altshiftab/utils_go/pkg/env"
	altshiftErrors "github.com/altshiftab/utils_go/pkg/errors"
	"github.com/altshiftab/utils_go/pkg/errors/types/empty_error"
	altshiftMux "github.com/altshiftab/utils_go/pkg/http/mux"
	altshiftErrorLogger "github.com/altshiftab/utils_go/pkg/log/error_logger"
	"github.com/vphpersson/letterboxd_list_updater/api"
	letterboxdEndpoint "github.com/vphpersson/letterboxd_list_updater/api/types/endpoint"
)

// readPasswordFile reads the password from the file systemd hands the sealed
// credential over in, rather than from the environment, which any process of
// the same user can read through /proc.
func readPasswordFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", altshiftErrors.NewWithTrace(fmt.Errorf("os read file: %w", err), path)
	}

	password := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if password == "" {
		return "", altshiftErrors.NewWithTrace(empty_error.New("password"), path)
	}
	return password, nil
}

func main() {
	logger := altshiftErrorLogger.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger.Logger)

	port := altshiftEnv.GetEnvWithDefault("PORT", "8080")
	username := altshiftEnv.ReadEnvFatal("LETTERBOXD_USERNAME")
	passwordFile := altshiftEnv.ReadEnvFatal("LETTERBOXD_PASSWORD_FILE")

	password, err := readPasswordFile(passwordFile)
	if err != nil {
		logger.FatalWithExitingMessage("An error occurred when reading the Letterboxd password.", err)
	}

	client, err := api.NewClient(
		&api.Options{
			Username:         username,
			Password:         password,
			ChromePath:       altshiftEnv.GetEnvWithDefault("LETTERBOXD_CHROME_EXEC_PATH", ""),
			ProfileDirectory: altshiftEnv.GetEnvWithDefault("LETTERBOXD_CHROME_USER_DATA_DIR", ""),
		},
	)
	if err != nil {
		logger.FatalWithExitingMessage("An error occurred when creating the Letterboxd client.", err)
	}

	overview := letterboxdEndpoint.NewOverview()
	if err := overview.UpdateList.Initialize(client); err != nil {
		logger.FatalWithExitingMessage("An error occurred when initializing the update list endpoint.", err)
	}

	mux := altshiftMux.New()
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

	httpServer := &http.Server{
		Addr:              fmt.Sprintf(":%s", port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	slog.Info("The Letterboxd list updater is starting.", slog.String("port", port))

	if err := httpServer.ListenAndServe(); err != nil {
		logger.FatalWithExitingMessage(
			"An error occurred when listening and serving.",
			altshiftErrors.NewWithTrace(fmt.Errorf("http server listen and serve: %w", err), httpServer),
		)
	}
}
