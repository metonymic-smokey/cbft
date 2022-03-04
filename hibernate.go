package cbft

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"github.com/couchbase/cbgt"
	"github.com/couchbase/cbgt/rest"
	log "github.com/couchbase/clog"

	jp "github.com/buger/jsonparser"
	"github.com/pkg/errors"
)

const DefaultIndexActivityStatsSampleIntervalSecs = 30
const LastAccessedTime = "last_access_time"

var DefaultDoFunc = func(client *http.Client, req *http.Request) (*http.Response, error) {
	resp, err := client.Do(req)
	if err != nil {
		return &http.Response{}, err
	}
	return resp, nil
}

// IndexActivityStats is a struct holding details about
// search indexes.
type indexActivityDetails struct {
	Url      string        // Ex: "http://10.0.0.1:8095".
	Start    time.Time     // When we started to get this sample.
	Duration time.Duration // How long it took to get this sample.
	Error    error
	Data     []byte // response from the endpoint.
}

type doFunc func(*http.Client, *http.Request) (*http.Response, error)

// indexActivityStatsOptions holds options about configuring
// the polling of search indexes' stats.
type indexActivityStatsOptions struct {
	indexActivityStatsSampleInterval time.Duration
	// The Do field is a function that will execute a HTTP request.
	// Added for unit testing.
	Do doFunc
}

// MonitorIndexActivityStats is used to monitor index activity
// stats for a specific URL.
type MonitorIndexActivityStats struct {
	URL      string // REST URL to monitor
	SampleCh chan indexActivityDetails
	Options  indexActivityStatsOptions
	StopCh   chan struct{}
}

type MonitorIndexActivityStatsBase struct {
	URL     string // REST URL to monitor
	Options indexActivityStatsOptions
}

// IndividualIndexActivityStats holds acitivity stats
// ns each individual index.
type individualIndexActivityStats struct {
	LastAccessedTime time.Time `json:"last_accessed_time"`
}

// separate struct for hibernation policy - last access time, and actions to take
// buffered channel - push Result into channel?
// analyse stats from results and apply hibernation policies
func updateHibernationStatus(mgr *cbgt.Manager,
	results map[string]individualIndexActivityStats, indexDefs *cbgt.IndexDefs) error {
	indexUUID := cbgt.NewUUID()
	var changed, anyChange bool
	layout := "2006-01-02 15:04:05 -0700 MST"

	if indexDefs != nil {
		for _, index := range indexDefs.IndexDefs {
			if index.PlanParams.NodePlanParams == nil {
				index.PlanParams.NodePlanParams =
					map[string]map[string]*cbgt.NodePlanParam{}
			}
			if index.PlanParams.NodePlanParams[""] == nil {
				index.PlanParams.NodePlanParams[""] =
					map[string]*cbgt.NodePlanParam{}
			}
			if index.PlanParams.NodePlanParams[""][""] == nil {
				index.PlanParams.NodePlanParams[""][""] = &cbgt.NodePlanParam{
					CanRead:  true,
					CanWrite: true,
				}
			}

			indexName := index.Name
			changed = false
			var policy hibernationPolicy
			var hibernationStatus int
			indexLastAccessTime := results[index.Name].LastAccessedTime
			t, err := time.Parse(layout, "0001-01-01 00:00:00 +0000 UTC")
			if err != nil {
				log.Printf("hibernate: error parsing time: %e", err)
				continue
			}
			if t.Equal(indexLastAccessTime) {
				// log.Printf("nil last access time for %s", mgr.BindHttp())
				continue
			}
			// log.Printf("index last accessed time for url %s: %s", mgr.BindHttp(),
			//	indexLastAccessTime)
			if time.Since(indexLastAccessTime) >
				DefaultColdPolicy.HibernationCriteria[0].Criteria["last_accessed_time"] {
				if index.HibernateStatus != cbgt.Cold {
					log.Printf("index %s being set to cold", indexName)
					changed = true
					hibernationStatus = cbgt.Cold
					policy = DefaultColdPolicy
				}
			} else if time.Since(indexLastAccessTime) >
				DefaultWarmPolicy.HibernationCriteria[0].Criteria["last_accessed_time"] {
				if index.HibernateStatus != cbgt.Warm {
					log.Printf("index %s being set to warm", indexName)
					changed = true
					hibernationStatus = cbgt.Warm
					policy = DefaultWarmPolicy
				}
			} else if index.HibernateStatus != cbgt.Hot {
				log.Printf("index %s being set to hot", indexName)
				changed = true
				hibernationStatus = cbgt.Hot
				policy = DefaultHotPolicy
			}

			if changed {
				anyChange = true
				updateIndexParams(index, indexUUID, hibernationStatus, policy)
				indexDefs.IndexDefs[indexName] = index
			}
		}
	}

	// only if there is change, update indexDefs
	if anyChange {
		indexDefs.UUID = indexUUID
		_, err := cbgt.CfgSetIndexDefs(mgr.Cfg(), indexDefs, cbgt.CFG_CAS_FORCE)
		if err != nil {
			return errors.Wrapf(err, "hibernate: error setting index defs: %s",
				err.Error())
		}
		// locks not required here since Set uses locks internally.
	}
	return nil
}

// Updates hibernation related parameters for indexDef.
func updateIndexParams(indexDef *cbgt.IndexDef, uuid string, hibernateStatus int,
	policy hibernationPolicy) {
	acceptPlanning := policy.HibernationCriteria[0].Actions[0].AcceptPlanning
	acceptMutation := policy.HibernationCriteria[0].Actions[0].AcceptIndexing
	indexDef.UUID = uuid
	indexDef.HibernateStatus = hibernateStatus
	indexDef.PlanParams.NodePlanParams[""][""].CanWrite = acceptMutation
	indexDef.PlanParams.PlanFrozen = !acceptPlanning
}

// Unmarshals /nsstats and returns the relevant stats in a struct
// Currently, only returns the last_access_time for each index.
func extractIndexActivityStats(data []byte, mgr *cbgt.Manager, indexDefs *cbgt.IndexDefs) (
	map[string]individualIndexActivityStats, error) {
	result := make(map[string]individualIndexActivityStats)

	if indexDefs != nil {
		for _, index := range indexDefs.IndexDefs {
			bucketName := index.SourceName
			indexName := index.Name
			path := bucketName + ":" + indexName + ":" + LastAccessedTime
			resByte, _, _, err := jp.Get(data, path)
			if err != nil {
				return result, errors.Wrapf(err, "hibernate: error extracting data "+
					"from response: %s", err.Error())
			}
			stringRes := string(resByte)
			if stringRes != "" {
				resTime, err := time.Parse(time.RFC3339, stringRes)
				if err != nil {
					log.Printf("hibernate: error parsing result into time format: %s",
						err.Error())
					continue
				}
				result[indexName] = individualIndexActivityStats{
					LastAccessedTime: resTime,
				}
			}
		}
	}
	return result, nil
}

// Returns a FQDN URL to the /nsstats endpoint.
func getNsStatsURL(mgr *cbgt.Manager) (string, error) {
	nodeDefs, err := mgr.GetNodeDefs(cbgt.NODE_DEFS_KNOWN, false)
	if err != nil {
		return "", errors.Wrapf(err, "hibernate: error getting nodedefs: %s",
			err.Error())
	}
	if nodeDefs == nil {
		return "", fmt.Errorf("hibernate: empty nodedefs: %s", err.Error())
	}

	url := "http://" + mgr.BindHttp() + "/api/nsstats"
	url = strings.Trim(url, "\n")
	return url, nil
}

func (h *IndexMonitoringHandler) ServeHTTP(
	w http.ResponseWriter, req *http.Request) {
	options := h.mgr.Options()
	newOptions := map[string]string{}
	for k, v := range options {
		newOptions[k] = v
	}
	op := rest.RequestVariableLookup(req, "op")
	if op == "start" {
		intervalString := rest.RequestVariableLookup(req, "interval")
		newOptions["monitoring"] = "true"
		newOptions["monitoringInterval"] = intervalString
		h.mgr.SetOptions(newOptions)
	} else if op == "stop" {
		newOptions["monitoring"] = "false"
		h.mgr.SetOptions(newOptions)
	} else {
		rest.ShowError(w, req, fmt.Sprintf("hibernate: error invalid op: %s",
			op), http.StatusBadRequest)
		return
	}
	rv := struct {
		Status string `json:"status"`
	}{
		Status: op,
	}
	rest.MustEncode(w, rv)
}

func (h *IndexHibernationHandler) ServeHTTP(
	w http.ResponseWriter, req *http.Request) {
	indexName := rest.IndexNameLookup(req)
	if indexName == "" {
		rest.ShowError(w, req, "index name is required", http.StatusBadRequest)
		return
	}

	indexUUID := cbgt.NewUUID()
	status := rest.RequestVariableLookup(req, "status")
	if _, ok := h.allowedStatuses[status]; !ok {
		rest.ShowError(w, req, fmt.Sprintf("hibernate: invalid status: %s",
			status), http.StatusBadRequest)
		return
	}

	indexDefs, indexDefsMap, err := h.mgr.GetIndexDefs(false)
	if err != nil {
		rest.ShowError(w, req, fmt.Sprintf("hibernate: error getting index defs: %e",
			err), http.StatusBadRequest)
		return
	}
	indexDef := indexDefsMap[indexName]

	// need to change default policy to custom
	if status == "cold" && indexDef.HibernateStatus != cbgt.Cold {
		updateIndexParams(indexDef, indexUUID, cbgt.Cold, DefaultColdPolicy)
		indexDefs.IndexDefs[indexName] = indexDef
		indexDefs.UUID = indexUUID
	} else if status == "warm" && indexDef.HibernateStatus != cbgt.Warm {
		updateIndexParams(indexDef, indexUUID, cbgt.Warm, DefaultWarmPolicy)
		indexDefs.IndexDefs[indexName] = indexDef
		indexDefs.UUID = indexUUID
	} else if status == "hot" && indexDef.HibernateStatus != cbgt.Hot {
		updateIndexParams(indexDef, indexUUID, cbgt.Hot, DefaultHotPolicy)
		indexDefs.IndexDefs[indexName] = indexDef
		indexDefs.UUID = indexUUID
	}

	_, err = cbgt.CfgSetIndexDefs(h.mgr.Cfg(), indexDefs, cbgt.CFG_CAS_FORCE)
	if err != nil {
		rest.ShowError(w, req, fmt.Sprintf("hibernate: error setting index defs: %e",
			err), http.StatusBadRequest)
		return
	}
	rv := struct {
		Status string `json:"status"`
	}{
		Status: status,
	}
	rest.MustEncode(w, rv)
}

func NewMonitorIndexActivityStats(url string, indexActivityStatsSample chan indexActivityDetails,
	options *indexActivityStatsOptions) *MonitorIndexActivityStats {
	return &MonitorIndexActivityStats{
		URL:      url,
		SampleCh: indexActivityStatsSample,
		Options:  *options,
		StopCh:   make(chan struct{}),
	}
}

func InitMonitorIndexActivityStats(mgr *cbgt.Manager) (*MonitorIndexActivityStats, error) {
	url, err := getNsStatsURL(mgr)
	if err != nil || url == "" {
		return nil, errors.Wrapf(err, "hibernate: error getting /nsstats endpoint URL: %s",
			err.Error())
	}

	intervalString, ok := mgr.Options()["monitoringInterval"]
	var interval time.Duration
	if ok && intervalString != "" {
		interval, err = time.ParseDuration(intervalString)
		if err != nil {
			log.Printf("hibernate: unable to parse interval: %e, reverting to default", err)
		}
		log.Printf("interval duration: %s", interval.String())
	}
	indexActivityStatsSample := make(chan indexActivityDetails)
	options := &indexActivityStatsOptions{
		indexActivityStatsSampleInterval: interval,
	}

	return NewMonitorIndexActivityStats(url, indexActivityStatsSample, options), nil
}

// IndexHibernateProbe monitors an index's activity.
func IndexHibernateProbe(mgr *cbgt.Manager, resCh *MonitorIndexActivityStats) error {
	err := startIndexActivityMonitor(resCh)
	if err != nil {
		return errors.Wrapf(err, "hibernate: error starting NSMonitor: %s",
			err.Error())
	}
	go func() {
		for r := range resCh.SampleCh {
			processIndexActivityStats(mgr, r)
		}
	}()

	return err
}

// Processes index activity stats from extracting the relevant fields to updating
// index defs based on the stats.
func processIndexActivityStats(mgr *cbgt.Manager, stats indexActivityDetails) {
	indexDefs, _, err := mgr.GetIndexDefs(false)
	if err != nil {
		log.Printf("hibernate: error getting index defs: %s", err.Error())
		return
	}
	result, err := extractIndexActivityStats(stats.Data, mgr, indexDefs)
	if err != nil {
		log.Printf("hibernate: error unmarshalling stats: %s", err.Error())
		return
		// should continue even if unmarshalling does not
		// work for one endpoint.
	}
	updateHibernationStatus(mgr, result, indexDefs)
}

// startIndexActivityMonitor starts a goroutine to monitor a URL and returns
// the stats from polling that URL.
func startIndexActivityMonitor(n *MonitorIndexActivityStats) error {
	go n.pollURL(n.URL)

	return nil
}

// pollURL polls a specific URL at given intervals, gathering
// stats for a specific node.
func (n *MonitorIndexActivityStats) pollURL(url string) {
	IndexActivityStatsSampleInterval := n.Options.indexActivityStatsSampleInterval
	if IndexActivityStatsSampleInterval <= 0 {
		IndexActivityStatsSampleInterval =
			DefaultIndexActivityStatsSampleIntervalSecs * time.Second
	}
	if n.Options.Do == nil {
		n.Options.Do = DefaultDoFunc
	}

	indexActivityStatsTicker := time.NewTicker(IndexActivityStatsSampleInterval)
	// want this to be continuously polled, hence not stopped.

	for {
		select {
		case <-n.StopCh:
			log.Printf("stopping poll url")
			return

		case t, ok := <-indexActivityStatsTicker.C:
			if !ok {
				return
			}
			n.sample(url, t)
		}
	}
}

func (n *MonitorIndexActivityStats) Stop() {
	n.StopCh <- struct{}{}
	close(n.StopCh)
}

// sample makes a GET request to a URL and populates a
// channel with the raw byte data.
func (n *MonitorIndexActivityStats) sample(url string, start time.Time) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		log.Printf("hibernate: Request error: %s", err.Error())
		return
	}

	// res, err := http.Get(url) //don't use http.Get  - no default timeout
	client := cbgt.HttpClient()
	res, err := n.Options.Do(client, req)
	if err != nil {
		log.Printf("hibernate: Response error: %s", err.Error())
		// do not need to close res.Body here since already closed on non-nil err.
		// Ref: https://github.com/mholt/curl-to-go/issues/6#issuecomment-283641312
		return
	}
	// removed not-nil check for res.Body since if err == nil, body = not nil
	// Ref: https://medium.com/@KeithAlpichi/go-gotcha-closing-a-nil-http-response-body-with-defer-9b7a3eb30e8c
	defer res.Body.Close()
	duration := time.Since(start)

	data := []byte{}
	if err == nil && res != nil {
		if res.StatusCode == 200 {
			var dataErr error

			data, dataErr = ioutil.ReadAll(res.Body)
			if err == nil && dataErr != nil {
				err = dataErr
			}
		} else {
			err = fmt.Errorf("hibernate: sample res.StatusCode not 200,"+
				" res: %#v, url: %#v, err: %v",
				res, url, err)
		}
	} else {
		err = fmt.Errorf("hibernate: sample,"+
			" res: %#v, url: %#v, err: %v",
			res, url, err)
	}

	finalIndexActivityStatsSample := indexActivityDetails{
		Url:      url,
		Duration: duration,
		Error:    err,
		Data:     data,
		Start:    start,
	}

	select {
	case <-n.StopCh:
		close(n.SampleCh) // close sample ch
		return
	case n.SampleCh <- finalIndexActivityStatsSample:
	}
}
