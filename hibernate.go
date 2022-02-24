package cbft

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"github.com/couchbase/cbgt"
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

// IndexActivityStats is a struct holding statistics about
// search indexes.
type indexActivityStats struct {
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
	SampleCh chan indexActivityStats
	Options  indexActivityStatsOptions
	StopCh   chan struct{}
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
	results map[string]individualIndexActivityStats) error {
	// indexDefs, cas, err := mgr.GetIndexDefs(true) - cannot do this since cas needed later on
	cfg := mgr.Cfg()
	indexDefs, cas, err := cbgt.CfgGetIndexDefs(cfg)
	if err != nil {
		return errors.Wrapf(err, "hibernate: error getting index defs:")
	}

	indexUUID := cbgt.NewUUID()
	var changed, anyChange bool

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

			npp := index.PlanParams.NodePlanParams[""][""]
			indexName := index.Name
			changed = false
			indexLastAccessTime := results[index.Name].LastAccessedTime
			if time.Since(indexLastAccessTime) > DefaultColdPolicy.HibernationCriteria[0].Criteria["last_accessed_time"] {
				if index.HibernateStatus != cbgt.Cold {
					log.Printf("index %s being set to cold", indexName)
					changed = true
					index.UUID = indexUUID
					index.HibernateStatus = cbgt.Cold
					// invoking idxControl here did not change the indexDefs, hence same plan
					npp.CanWrite = false
					index.PlanParams.PlanFrozen = true
				}
			} else if time.Since(indexLastAccessTime) > DefaultWarmPolicy.HibernationCriteria[0].Criteria["last_accessed_time"] {
				if index.HibernateStatus != cbgt.Warm {
					log.Printf("index %s being set to warm", indexName)
					changed = true
					index.UUID = indexUUID
					index.HibernateStatus = cbgt.Warm
					npp.CanWrite = true
					index.PlanParams.PlanFrozen = true
				}
			} else {
				if index.HibernateStatus != cbgt.Hot {
					log.Printf("index %s being set to hot", indexName)
					changed = true
					index.UUID = indexUUID
					index.HibernateStatus = cbgt.Hot
					npp.CanWrite = true
					index.PlanParams.PlanFrozen = false
				}
			}
			if changed {
				anyChange = true
				indexDefs.IndexDefs[indexName] = index
				// only if there is change, update indexDefs
				indexDefs.UUID = indexUUID
			}
		}
	}

	if anyChange {
		log.Printf("hibernate: update hibernate status %s...", changed)
		_, err = cbgt.CfgSetIndexDefs(cfg, indexDefs, cas)
		if err != nil {
			return errors.Wrapf(err, "hibernate: error setting index defs: %s",
				err.Error())
		}
		// locks not required here since Set uses locks internally.
	}
	return err
}

// Unmarshals /nsstats and returns the relevant stats in a struct
// Currently, only returns the last_access_time for each index.
func unmarshalIndexActivityStats(data []byte, mgr *cbgt.Manager) (map[string]individualIndexActivityStats, error) {
	result := make(map[string]individualIndexActivityStats)
	indexDefs, _, err := mgr.GetIndexDefs(false)
	if err != nil {
		return result, errors.Wrapf(err, "hibernate: error getting index defs: %s",
			err.Error())
	}
	if indexDefs != nil {
		for _, index := range indexDefs.IndexDefs {
			bucketName := index.SourceName
			indexName := index.Name
			path := bucketName + ":" + indexName + ":" + LastAccessedTime
			resByte, _, _, err := jp.Get(data, path)
			if err != nil {
				return result, errors.Wrapf(err, "hibernate: error extracting data from response: %s",
					err.Error())
			}
			stringRes := string(resByte)
			if stringRes != "" {
				resTime, err := time.Parse(time.RFC3339, stringRes)
				if err != nil {
					log.Printf("hibernate: error parsing result into time format: %s", err.Error())
				} else {
					result[indexName] = individualIndexActivityStats{
						LastAccessedTime: resTime,
					}
				}
			}
		}
	}
	return result, nil
}

// Returns a FQDN URL to the /nsstats endpoint.
func getNsStatsURL(mgr *cbgt.Manager, bindHTTP string) (string, error) {
	nodeDefs, err := mgr.GetNodeDefs(cbgt.NODE_DEFS_KNOWN, false)
	if err != nil {
		return "", errors.Wrapf(err, "hibernate: error getting nodedefs: %s",
			err.Error())
	}
	if nodeDefs == nil {
		return "", fmt.Errorf("hibernate: empty nodedefs: %s", err.Error())
	}

	prefix := mgr.Options()["urlPrefix"]
	// polling one node endpoint is enough - avoids duplicate statistics.
	for _, node := range nodeDefs.NodeDefs {
		fqdnURL := "http://" + bindHTTP + prefix + "/api/nsstats"
		// no auth required for /nsstats
		secSettings := cbgt.GetSecuritySetting()
		if secSettings.EncryptionEnabled {
			if httpsURL, err := node.HttpsURL(); err == nil {
				fqdnURL = httpsURL
			}
		}
		// request error: parse 192.168.1.6:9200/api/nsstats: first path segment in URL cannot contain colon
		// Ref: https://stackoverflow.com/questions/54392948/first-path-segment-in-url-cannot-contain-colon
		fqdnURL = strings.Trim(fqdnURL, "\n")
		// possibly check node health here?
		// return only if node is healthy?
		return fqdnURL, nil
	}
	return "", nil
}

// IndexHibernateProbe monitors an index's activity.
func IndexHibernateProbe(mgr *cbgt.Manager, bindHTTP string) error {
	url, err := getNsStatsURL(mgr, bindHTTP)
	if err != nil {
		return errors.Wrapf(err, "Error getting /nsstats endpoint URL")
	}

	indexActivityStatsSample := make(chan indexActivityStats)
	options := &indexActivityStatsOptions{
		indexActivityStatsSampleInterval: 1 * time.Minute,
	}

	resCh, err := startIndexActivityMonitor(url, indexActivityStatsSample, *options)
	if err != nil {
		return errors.Wrapf(err, "hibernate: error starting NSMonitor: %s",
			err.Error())
	}
	go func() {
		for r := range resCh.SampleCh {
			result, err := unmarshalIndexActivityStats(r.Data, mgr)
			if err != nil {
				log.Printf("hibernate: Error unmarshalling stats: %s", err.Error())
				continue
				// should continue even if unmarshalling does not
				// work for one endpoint.
			}
			go updateHibernationStatus(mgr, result)
		}
	}()

	return err
}

// startIndexActivityMonitor starts a goroutine to monitor a URL and returns
// the stats from polling that URL.
func startIndexActivityMonitor(url string, sampleCh chan indexActivityStats,
	options indexActivityStatsOptions) (*MonitorIndexActivityStats, error) {
	n := &MonitorIndexActivityStats{
		URL:      url,
		SampleCh: sampleCh,
		Options:  options,
		StopCh:   make(chan struct{}),
	}

	go n.runNode(url)

	return n, nil
}

// runNode polls a specific URL at given intervals, gathering
// stats for a specific node.
func (n *MonitorIndexActivityStats) runNode(url string) {
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
		if res.Body != nil {
			res.Body.Close()
		}
		// closing here since defer will not apply on a premature return
		return
	}
	if res.Body != nil {
		defer res.Body.Close()
	}
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

	finalIndexActivityStatsSample := indexActivityStats{
		Url:      url,
		Duration: duration,
		Error:    err,
		Data:     data,
		Start:    start,
	}

	select {
	case <-n.StopCh:
	case n.SampleCh <- finalIndexActivityStatsSample:
	}
}
