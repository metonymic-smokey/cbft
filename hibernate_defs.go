package cbft

import (
	"encoding/json"

	"github.com/couchbase/cbgt"
	"github.com/pkg/errors"
)

type IndexMonitoringHandler struct {
	mgr *cbgt.Manager
}

func NewIndexMonitoringHandler(mgr *cbgt.Manager) *IndexMonitoringHandler {
	return &IndexMonitoringHandler{
		mgr: mgr,
	}
}

type IndexHibernationHandler struct {
	mgr             *cbgt.Manager
	allowedStatuses map[string]struct{}
}

func NewIndexHibernationHandler(
	mgr *cbgt.Manager, allowedStatuses map[string]struct{}) *IndexHibernationHandler {
	return &IndexHibernationHandler{
		mgr:             mgr,
		allowedStatuses: allowedStatuses,
	}
}

// Function to marshal results(stats) map into custom JSON
// returns []byte for easy parsing.
func marshalResults(results map[string]individualIndexActivityStats) ([]byte, error) {
	resBytes, err := json.Marshal(results)
	if err != nil {
		return []byte{}, errors.Wrapf(err, "error marshaling results: ")
	}

	return resBytes, nil
}
