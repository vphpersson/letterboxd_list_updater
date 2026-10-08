package endpoint

import (
	"testing"

	"github.com/vphpersson/letterboxd_list_updater/api/types/endpoint/update_list_endpoint"
)

func TestOverview(t *testing.T) {
	t.Parallel()

	overview := NewOverview()
	endpoints := overview.Endpoints()
	if len(endpoints) != 1 || endpoints[0] != overview.UpdateList.Endpoint {
		t.Fatalf("unexpected endpoints: %v", endpoints)
	}
	if endpoints[0].Path != update_list_endpoint.DefaultPath {
		t.Errorf("unexpected path: %q", endpoints[0].Path)
	}
}
