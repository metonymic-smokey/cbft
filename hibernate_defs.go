package cbft

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/buger/jsonparser"
	jp "github.com/buger/jsonparser"
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
	Action transitionAction
	// runInterval time.Duration
}

type transitionAction struct {
	AcceptMutation bool `json:"ingestControl",omitempty`
	AcceptPlanning bool `json:"planFreeze",omitempty`
	Close          bool `json:"close",omitempty`
}

var defaultWarmCondition = condition{
	Criteria: map[string]time.Duration{
		"last_accessed_time": 1 * time.Minute,
	},
	Actions: []lifecycleAction{{Action: transitionAction{AcceptPlanning: false}}},
}

var defaultColdCondition = condition{
	Criteria: map[string]time.Duration{
		"last_accessed_time": 3 * time.Minute,
	},
	Actions: []lifecycleAction{{Action: transitionAction{
		AcceptPlanning: false, AcceptMutation: false, Close: true}, //find better name for close? - active?
	//planning instead of acceptplan
	// plan, index, active
	}},
}

var DefaultWarmPolicy = hibernationPolicy{
	HibernationCriteria: []condition{defaultWarmCondition},
}

var DefaultColdPolicy = hibernationPolicy{
	HibernationCriteria: []condition{defaultColdCondition},
}

// have an Actions map: planFreeze: func(indexName,mgr)

// Function to marshal results(stats) map into custom JSON
// returns []byte for easy parsing.
func marshalResults(results map[string]individualIndexActivityStats) ([]byte, error) {
	resBytes, err := json.Marshal(results)
	if err != nil {
		return []byte{}, errors.Wrapf(err, "error marshaling results: ")
	}

	return resBytes, nil
}
