package cbft

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/couchbase/cbgt"
	"github.com/couchbase/cbgt/rest"
	log "github.com/couchbase/clog"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
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

			indexName := index.Name
			changed = false
			var policy cbgt.HibernationPolicy
			var hibernationStatus int
			if index.PlanParams.NodePlanParams[""][""] == nil {
				index.PlanParams.NodePlanParams[""][""] = &cbgt.NodePlanParam{
					CanRead:  true,
					CanWrite: true,
				}
			}
			indexLastAccessTime := results[index.Name].LastAccessedTime
			t, err := time.Parse(layout, "0001-01-01 00:00:00 +0000 UTC")
			if err != nil {
				log.Printf("hibernate: error parsing time: %e", err)
				continue
			}
			if t.Equal(indexLastAccessTime) {
				continue
			}
			if time.Since(indexLastAccessTime) >
				cbgt.DefaultColdPolicy.HibernationCriteria[0].Criteria["last_accessed_time"] {
				if index.HibernateStatus != cbgt.Cold {
					log.Printf("hibernate: index %s being set to cold", indexName)
					changed = true
					hibernationStatus = cbgt.Cold
					policy = cbgt.DefaultColdPolicy
				}
			} else if time.Since(indexLastAccessTime) >
				cbgt.DefaultWarmPolicy.HibernationCriteria[0].Criteria["last_accessed_time"] {
				if index.HibernateStatus != cbgt.Warm {
					log.Printf("hibernate: index %s being set to warm", indexName)
					changed = true
					hibernationStatus = cbgt.Warm
					policy = cbgt.DefaultWarmPolicy
				}
			} else if index.HibernateStatus != cbgt.Hot {
				log.Printf("hibernate: index %s being set to hot", indexName)
				changed = true
				hibernationStatus = cbgt.Hot
				policy = cbgt.DefaultHotPolicy
			}

			if changed {
				if index.PlanParams.NodePlanParams[""][""] == nil {
					index.PlanParams.NodePlanParams[""][""] = &cbgt.NodePlanParam{
						CanRead:  true,
						CanWrite: true,
					}
				}
				anyChange = true
				UpdateIndexParams(index, indexUUID, hibernationStatus, policy)
				log.Printf("hibernate: index %s can write after update: %s", indexName,
					index.PlanParams.NodePlanParams[""][""].CanWrite)
				indexDefs.IndexDefs[indexName] = index
			}
		}
	}

	// only if there is change, update indexDefs
	if anyChange {
		log.Printf("hibernate: inside anychange...")
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
func UpdateIndexParams(indexDef *cbgt.IndexDef, uuid string, hibernateStatus int,
	policy cbgt.HibernationPolicy) {
	acceptPlanning := policy.HibernationCriteria[0].Actions[0].AcceptPlanning
	acceptMutation := policy.HibernationCriteria[0].Actions[0].AcceptIndexing
	indexDef.UUID = uuid
	indexDef.HibernateStatus = hibernateStatus
	if indexDef.PlanParams.NodePlanParams == nil {
		indexDef.PlanParams.NodePlanParams =
			map[string]map[string]*cbgt.NodePlanParam{}
	}
	if indexDef.PlanParams.NodePlanParams[""] == nil {
		indexDef.PlanParams.NodePlanParams[""] =
			map[string]*cbgt.NodePlanParam{}
	}
	if indexDef.PlanParams.NodePlanParams[""][""] == nil {
		indexDef.PlanParams.NodePlanParams[""][""] = &cbgt.NodePlanParam{
			CanRead:  true,
			CanWrite: true,
		}
	}
	indexDef.PlanParams.NodePlanParams[""][""].CanWrite = acceptMutation
	indexDef.PlanParams.PlanFrozen = !acceptPlanning
	indexDef.Params, _ = updateIndexParams(indexDef.Params, hibernateStatus)
}

// change the OG index params func name!
func updateIndexParams(indexParams string, newHibStatus int) (string, error) {
	currHibStatus, _ := getHibernateStatusFromIndexParams(indexParams)
	log.Printf("hibernate: the current status in params is: %d", currHibStatus)
	var hs rest.HibernateParam
	hs.HibernateStatus = newHibStatus

	bleveParams := NewBleveParams()
	err := json.Unmarshal([]byte(indexParams), bleveParams)
	if err != nil {
		return "",
			fmt.Errorf(": parse params, err: %v", err)
	}
	startPos := len(indexParams) - 2
	newStr := indexParams[0:startPos+1] + ",\"hibernate\":" +
		strconv.Itoa(newHibStatus) + "}"

	log.Printf("hibernate: the updated status in params is: %s", newStr)

	return newStr, nil
}

// Unmarshals /nsstats and returns the relevant stats in a struct
// Currently, only returns the last_access_time for each index.
func extractIndexActivityStats(data []byte, mgr *cbgt.Manager, indexDefs *cbgt.IndexDefs) (
	map[string]individualIndexActivityStats, error) {
	result := make(map[string]individualIndexActivityStats)

	if indexDefs != nil {
		for _, index := range indexDefs.IndexDefs {
			indexName := index.Name
			path := indexName + ":" + LastAccessedTime
			lastAccessTime, _, _, err := jp.Get(data, path)
			if err != nil {
				return result, errors.Wrapf(err, "hibernate: error extracting data "+
					"from response: %s", err.Error())
			}
			stringLAT := string(lastAccessTime)
			if stringLAT != "" {
				resTime, err := time.Parse(time.RFC3339, stringLAT)
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
		// handle case for empty interval string
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

func getS3BucketName(indexDef *cbgt.IndexDef) (string, error) {
	bucketName := indexDef.SourceName + "-"
	scope, collections, err := GetScopeCollectionsFromIndexDef(indexDef)
	if err != nil {
		return "", err
	}
	bucketName += scope + "-"
	for i := 0; i < len(collections)-1; i++ {
		bucketName += collections[i] + "-"
	}
	bucketName += collections[len(collections)-1]
	bucketName = strings.ReplaceAll(bucketName, "_", "")
	return bucketName, nil
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
		UpdateIndexParams(indexDef, indexUUID, cbgt.Cold, cbgt.DefaultColdPolicy)
		indexDefs.IndexDefs[indexName] = indexDef
		indexDefs.UUID = indexUUID

		bucket, err := getS3BucketName(indexDef)
		if err != nil {
			return
		}
		cfg, err := config.LoadDefaultConfig(context.TODO(),
			config.WithRegion("us-east-1"),
		)
		if err != nil {
			log.Printf("%+v\n", err)
			return
		}
		svc := s3.NewFromConfig(cfg)

		bucketOutput, err := svc.CreateBucket(context.TODO(), &s3.CreateBucketInput{
			Bucket: &bucket,
		})
		if err != nil {
			log.Printf("hibernate: bucket create error: %+v", err)
		}
		log.Printf("bucket creation output: %+v", bucketOutput)
	} else if status == "warm" && indexDef.HibernateStatus != cbgt.Warm {
		UpdateIndexParams(indexDef, indexUUID, cbgt.Warm, cbgt.DefaultWarmPolicy)
		indexDefs.IndexDefs[indexName] = indexDef
		indexDefs.UUID = indexUUID
	} else if status == "hot" && indexDef.HibernateStatus != cbgt.Hot {
		UpdateIndexParams(indexDef, indexUUID, cbgt.Hot, cbgt.DefaultHotPolicy)
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
	err := startIndexActivityMonitor(resCh, mgr)
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
func startIndexActivityMonitor(n *MonitorIndexActivityStats, mgr *cbgt.Manager) error {
	go n.pollURL(n.URL, mgr)

	return nil
}

// pollURL polls a specific URL at given intervals, gathering
// stats for a specific node.
func (n *MonitorIndexActivityStats) pollURL(url string, mgr *cbgt.Manager) {
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
			close(n.SampleCh)
			log.Printf("stopping poll url")
			return

		case t, ok := <-indexActivityStatsTicker.C:
			if !ok {
				return
			}
			n.sample(url, t, mgr)
		}
	}
}

func (n *MonitorIndexActivityStats) Stop() {
	n.StopCh <- struct{}{}
	n.StopCh <- struct{}{}
}

// sample makes a GET request to a URL and populates a
// channel with the raw byte data.
func (n *MonitorIndexActivityStats) sample(url string, start time.Time,
	mgr *cbgt.Manager) {
	indexDefs, _, err := mgr.GetIndexDefs(false)
	if err != nil {
		log.Printf("hibernate: error getting index defs: %s", err.Error())
		return
	}

	res := make(NSIndexStats, 0)
	for _, index := range indexDefs.IndexDefs {
		indexName := index.Name
		lat := querySupervisor.GetLastAccessTimeForIndex(indexName)
		if _, exists := res[indexName]; !exists {
			res[indexName] = make(map[string]interface{}, 0)
		}
		res[indexName][LastAccessedTime] = lat
	}
	data, err := json.Marshal(res)
	if err != nil {
		log.Printf("hibernate: error marshalling JSON: %s", err.Error())
		return
	}
	duration := time.Since(start)

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
