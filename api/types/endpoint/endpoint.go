package endpoint

import (
	"github.com/Motmedel/utils_go/pkg/http/mux/types/endpoint/initialization_endpoint"
	"github.com/vphpersson/letterboxd_list_updater/api/types/endpoint/update_list_endpoint"
)

type Overview struct {
	UpdateList *update_list_endpoint.Endpoint
}

func (overview *Overview) Endpoints() []*initialization_endpoint.Endpoint {
	return []*initialization_endpoint.Endpoint{
		overview.UpdateList.Endpoint,
	}
}

func NewOverview() *Overview {
	return &Overview{
		UpdateList: update_list_endpoint.New(),
	}
}
