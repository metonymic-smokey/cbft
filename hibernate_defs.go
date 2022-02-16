package cbft

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/buger/jsonparser"
	jp "github.com/buger/jsonparser"
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

var (
	// Metrics considered in the hibernation policy
	metrics = []string{
		"last_accessed_time",
	}
	// Permitted actions.
	actions = []string{
		"planFreeze",
		"ingestControl",
		"close",
	}
)

// have an Actions map: planFreeze: func(indexName,mgr)

func updateIndexStatus(mgr *cbgt.Manager, indexDefs *cbgt.IndexDefs, results map[string]individualIndexActivityStats) bool {
	indexUUID := cbgt.NewUUID()
	var changed bool

	if indexDefs != nil {
		for _, index := range indexDefs.IndexDefs {
			indexName := index.Name
			changed = false
			indexLastAccessTime := results[index.Name].LastAccessedTime
			if time.Since(indexLastAccessTime) > DefaultColdPolicy.HibernationCriteria[0].Criteria["last_accessed_time"] {
				if index.HibernateStatus != cbgt.Cold {
					changed = true
					index.UUID = indexUUID
					index.HibernateStatus = cbgt.Cold
					// invoking idxControl here did not change the indexDefs, hence same plan
					log.Printf("index %s being set to cold", indexName)
				}
			} else if time.Since(indexLastAccessTime) > DefaultWarmPolicy.HibernationCriteria[0].Criteria["last_accessed_time"] {
				if index.HibernateStatus != cbgt.Warm {
					changed = true
					index.UUID = indexUUID
					index.HibernateStatus = cbgt.Warm
					log.Printf("index %s being set to warm", indexName)
				}
			} else {
				if index.HibernateStatus != cbgt.Hot {
					changed = true
					index.UUID = indexUUID
					index.HibernateStatus = cbgt.Hot
					log.Printf("index %s being set to hot", indexName)
				}
			}
			if changed {
				indexDefs.IndexDefs[indexName] = index // only if there is change only then update indexDefs
				indexDefs.UUID = indexUUID
			}
		}
	}

	return changed
}


