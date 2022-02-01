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
	LastAccessedTime time.Time
}

// Unmarshals /nsstats and returns the relevant stats in a struct
// Currently, only returns the last_access_time for each index.
func unmarshalIndexActivityStats(data []byte, mgr *cbgt.Manager) (map[string]individualIndexActivityStats, error) {
	result := make(map[string]individualIndexActivityStats)
	indexDefs, _, err := mgr.GetIndexDefs(true)
	if err != nil {
		return result, errors.Wrapf(err, "error getting index defs")
	}
	for _, index := range indexDefs.IndexDefs {
		bucketName := index.SourceName
		indexName := index.Name
		path := bucketName + ":" + indexName + ":" + LastAccessedTime
		resByte, _, _, err := jp.Get(data, path)
		if err != nil {
			return result, errors.Wrapf(err, "error extracting data from response")
		}
		stringRes := string(resByte)
		if stringRes != "" {
			resTime, err := time.Parse(time.RFC3339, stringRes)
			if err != nil {
				log.Printf("Hibernate: error parsing result into time format: %e", err)
			} else {
				result[indexName] = individualIndexActivityStats{
					LastAccessedTime: resTime,
				}
			}
		}
	}
	return result, nil
}

// Returns a FQDN URL to the /nsstats endpoint.
func getNsStatsURL(mgr *cbgt.Manager, bindHTTP string) (string, error) {
	cfg := mgr.Cfg()

	nodeDefs, _, err := cbgt.CfgGetNodeDefs(cfg, cbgt.NODE_DEFS_KNOWN)
	if err != nil {
		return "", errors.Wrapf(err, "Error getting nodedefs")
	}
	if nodeDefs == nil {
		return "", fmt.Errorf("empty nodedefs")
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
		indexActivityStatsSampleInterval: 10 * time.Second,
	}

	resCh, err := startIndexActivityMonitor(url, indexActivityStatsSample, *options)
	if err != nil {
		return errors.Wrapf(err, "Error starting NSMonitor")
	}
	go func() {
		for r := range resCh.SampleCh {
			result, err := unmarshalIndexActivityStats(r.Data, mgr)
			if err != nil {
				log.Printf("Hibernate: Error unmarshalling stats: %e", err)
				continue
				// should continue even if unmarshalling does not
				// work for one endpoint.
			}
			log.Printf("Hibernate: Result: %+v", result)
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
		log.Printf("Hibernate: Request error: %s", err.Error())
		return
	}

	// res, err := http.Get(url) //don't use http.Get  - no default timeout
	client := cbgt.HttpClient()
	res, err := n.Options.Do(client, req)
	if err != nil {
		log.Printf("Hibernate: Response error: %s", err.Error())
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
