package cbft

import (
	"encoding/json"
	"time"

	"github.com/couchbase/cbgt"
	"github.com/pkg/errors"
)

// currently, the accepted value for criteria string is
// "last_accessed_time"
type condition struct {
	Criteria map[string]time.Duration
	Actions  []lifecycleAction
}

// struct fields exported for JSON marshalling

type hibernationPolicy struct {
	HibernationCriteria []condition
}

type lifecycleAction struct {
	AcceptIndexing bool `json:"ingestControl",omitempty`
	AcceptPlanning bool `json:"planFreeze",omitempty`
	Hibernate      bool `json:"hibernate",omitempty`
}

var defaultHotCondition = condition{
	Actions: []lifecycleAction{{
		AcceptPlanning: true,
		AcceptIndexing: true,
		Hibernate:      false},
	},
}

var defaultWarmCondition = condition{
	Criteria: map[string]time.Duration{
		"last_accessed_time": 1 * time.Minute,
	},
	Actions: []lifecycleAction{{
		AcceptPlanning: false,
		AcceptIndexing: true},
	},
}

var defaultColdCondition = condition{
	Criteria: map[string]time.Duration{
		"last_accessed_time": 3 * time.Minute,
	},
	Actions: []lifecycleAction{{
		AcceptPlanning: false,
		AcceptIndexing: false,
		Hibernate:      true},
	},
}

var DefaultHotPolicy = hibernationPolicy{
	HibernationCriteria: []condition{defaultHotCondition},
}

var DefaultWarmPolicy = hibernationPolicy{
	HibernationCriteria: []condition{defaultWarmCondition},
}

var DefaultColdPolicy = hibernationPolicy{
	HibernationCriteria: []condition{defaultColdCondition},
}

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
