package cbft

import (
	"bytes"
	"io/ioutil"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestStartIndexActivityMonitor(t *testing.T) {
	url := "url0"
	httpGets := 0
	sampleCh := make(chan indexActivityStats)
	options := indexActivityStatsOptions{
		indexActivityStatsSampleInterval: 10 * time.Second,
	}

	options.Do = func(*http.Client, *http.Request) (*http.Response, error) {
		httpGets++
		return &http.Response{
			StatusCode: 200,
			Body:       ioutil.NopCloser(bytes.NewBuffer([]byte("{}"))),
		}, nil
	}

	indexActivityStats, err := startIndexActivityMonitor(url, sampleCh, options)
	if err != nil {
		t.Errorf("unexpected error")
	}

	go func() {
		for r := range indexActivityStats.SampleCh {
			if r.Url != "url0" || r.Error != nil || r.Data == nil {
				t.Errorf("unexpected error")
			}
		}
	}()
}

func TestStartIndexActivityMonitorErrors(t *testing.T) {
	url := "url0"
	httpGets := 0
	var mut sync.Mutex
	sampleCh := make(chan indexActivityStats)
	options := indexActivityStatsOptions{
		indexActivityStatsSampleInterval: 1 * time.Second,
	}

	options.Do = func(*http.Client, *http.Request) (*http.Response, error) {
		mut.Lock()
		httpGets++
		mut.Unlock()
		return &http.Response{
			StatusCode: 500,
		}, nil
	}

	indexActivityStats, err := startIndexActivityMonitor(url, sampleCh, options)
	if err != nil {
		t.Errorf("unexpected error")
	}

	go func() {
		for chResp := range indexActivityStats.SampleCh {
			if chResp.Url != "url0" {
				t.Errorf("unexpected error")
			}
			if chResp.Error == nil {
				t.Errorf("expected an error")
			}
		}
	}()

	time.Sleep(10 * time.Second)
	indexActivityStats.Stop()
	mut.Lock()
	if httpGets < 10 {
		t.Errorf("expected httpGets to be at least 10") //since it runs uninterrupted for
		// atleast 10 seconds, with a run interval of 1 second.
	}
	mut.Unlock()
}
