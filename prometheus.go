//  Copyright 2020-Present Couchbase, Inc.
//
//  Use of this software is governed by the Business Source License included
//  in the file licenses/BSL-Couchbase.txt.  As of the Change Date specified
//  in that file, in accordance with the Business Source License, use of this
//  software will be governed by the Apache License, Version 2.0, included in
//  the file licenses/APL2.txt.

package cbft

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/couchbase/cbgt"
	"github.com/couchbase/cbgt/rest"
)

// TODO finalise the high/low cardinality stats.
// highCardinalityStats enumerates a minimum essential subset of index
//  level stats for ns_server/prometheus uses.
var prometheusStats = map[string]string{
	"doc_count":                      "counter",
	"total_grpc_internal_queries":    "counter",
	"total_term_searchers":           "counter",
	"total_queries_timeout":          "counter",
	"total_grpc_queries":             "counter",
	"total_grpc_queries_slow":        "counter",
	"total_bytes_indexed":            "counter",
	"total_compaction_written_bytes": "counter",
	"total_queries":                  "counter",
	"total_queries_slow":             "counter",
	"total_request_time":             "counter",
	"total_grpc_queries_timeout":     "counter",
	"total_bytes_query_results":      "counter",
	"total_internal_queries":         "counter",
	"total_queries_error":            "counter",
	"total_grpc_queries_error":       "counter",
	"total_term_searchers_finished":  "counter",

	"tot_batches_flushed_on_maxops":  "counter",
	"tot_batches_flushed_on_timer":   "counter",
	"tot_bleve_dest_opened":          "counter",
	"tot_bleve_dest_closed":          "counter",
	"tot_http_limitlisteners_opened": "counter",
	"tot_http_limitlisteners_closed": "counter",
	"tot_grpc_listeners_opened":      "counter",
	"tot_grpc_listeners_closed":      "counter",
	"tot_grpcs_listeners_opened":     "counter",
	"tot_grpcs_listeners_closed":     "counter",

	"tot_remote_http2":                 "counter",
	"tot_remote_grpc":                  "counter",
	"tot_remote_grpc_tls":              "counter",
	"tot_queryreject_on_memquota":      "counter",
	"tot_https_limitlisteners_opened":  "counter",
	"tot_https_limitlisteners_closed":  "counter",
	"tot_grpc_queryreject_on_memquota": "counter",

	"tot_remote_http":                  "counter",
	"total_queries_rejected_by_herder": "counter",
	"total_gc":                         "counter",
	"batch_bytes_added":                "counter",
	"batch_bytes_removed":              "counter",

	"pct_cpu_gc":                     "gauge",
	"num_bytes_used_ram":             "gauge",
	"avg_grpc_queries_latency":       "gauge",
	"num_files_on_disk":              "gauge",
	"num_pindexes_actual":            "gauge",
	"num_pindexes_target":            "gauge",
	"num_mutations_to_index":         "gauge",
	"num_recs_to_persist":            "gauge",
	"num_bytes_used_disk":            "gauge",
	"avg_queries_latency":            "gauge",
	"avg_internal_queries_latency":   "gauge",
	"num_bytes_used_disk_by_root":    "gauge",
	"num_root_filesegments":          "gauge",
	"num_root_memorysegments":        "gauge",
	"curr_batches_blocked_by_herder": "gauge",
}

var bline = []byte("\n")

// PrometheusHighMetricsHandler is a REST handler that provides high
// cardinality stats/metrics for consumption by ns_server/prometheus.
type PrometheusHighMetricsHandler struct {
	statsCount int64
	mgr        *cbgt.Manager
}

func NewPrometheusHighMetricsHandler(mgr *cbgt.Manager) *PrometheusHighMetricsHandler {
	return &PrometheusHighMetricsHandler{mgr: mgr}
}

func (h *PrometheusHighMetricsHandler) ServeHTTP(w http.ResponseWriter,
	req *http.Request) {
	initNsServerCaching(h.mgr)

	rd := getRecentInfo()
	if rd.err != nil {
		rest.ShowError(w, req, fmt.Sprintf("could not retrieve defs: %v", rd.err),
			http.StatusInternalServerError)
		return
	}

	nsIndexStats, err := gatherIndexStats(h.mgr, rd, true)
	if err != nil {
		rest.ShowError(w, req, fmt.Sprintf("error in retrieving defs: %v", err),
			http.StatusInternalServerError)
		return
	}

	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	for k, nsis := range nsIndexStats {
		for nsik, nsiv := range nsis {
			b, err := json.Marshal(nsiv)
			if err != nil {
				rest.ShowError(w, req, fmt.Sprintf("json marshal err: %v", err),
					http.StatusInternalServerError)
				return
			}
			if typ, ok := prometheusStats[nsik]; ok {
				w.Write([]byte(fmt.Sprintf("# TYPE fts_%s %s\n", nsik, typ)))
				w.Write(append([]byte("fts_"+nsik+k+" "), b...))
				w.Write(bline)
			}
		}
	}
}

func scopeCollNames(params, sourceName string) (string, []string) {
	tmp := struct {
		Mapping mapping.IndexMapping `json:"mapping"`
	}{Mapping: bleve.NewIndexMapping()}

	err := json.Unmarshal([]byte(params), &tmp)
	if err != nil {
		return "", nil
	}

	if im, ok := tmp.Mapping.(*mapping.IndexMappingImpl); ok {
		scope, err := validateScopeCollFromMappings(sourceName,
			im, true)
		if err != nil {
			return "", nil
		}

		if scope != nil && len(scope.Collections) > 0 {
			rv := make([]string, len(scope.Collections))
			for i, coll := range scope.Collections {
				rv[i] = coll.Name
			}
			return scope.Name, rv
		}
	}

	return "", nil
}

func getNsIndexStatsKey(indexName, sourceName, params string) string {
	var colNames string
	// look up the scope/collection details from the cache.
	sname, cnames := metaFieldValCache.getScopeCollectionNames(indexName)
	// compute afresh for a cache miss.
	if len(sname) == 0 && len(cnames) == 0 {
		sname, cnames = scopeCollNames(params, sourceName)
		if sname == "" && len(cnames) == 0 {
			sname = "_default"
			cnames = []string{"_default"}
		}
	}

	for i, colName := range cnames {
		if i > 0 {
			colName = "," + colName
		}
		colNames += colName
	}

	return ` {bucket="` + sourceName + `",scope="` + sname + `"` +
		`,collection="` + colNames + `",index="` + indexName + `"}`
}

// PrometheusMetricsHandler is a REST handler that provides low
// cardinality stats/metrics for consumption by ns_server/prometheus.
type PrometheusMetricsHandler struct {
	statsCount int64
	mgr        *cbgt.Manager
}

func NewPrometheusMetricsHandler(mgr *cbgt.Manager) *PrometheusMetricsHandler {
	return &PrometheusMetricsHandler{mgr: mgr}
}

func (h *PrometheusMetricsHandler) ServeHTTP(w http.ResponseWriter,
	req *http.Request) {
	initNsServerCaching(h.mgr)

	rd := getRecentInfo()
	if rd.err != nil {
		rest.ShowError(w, req, fmt.Sprintf("could not retrieve defs: %v", rd.err),
			http.StatusInternalServerError)
		return
	}

	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	stats := gatherTopLevelStats(rd)
	for k, v := range stats {
		b, err := json.Marshal(v)
		if err != nil {
			rest.ShowError(w, req, fmt.Sprintf("json marshal err: %v", err),
				http.StatusInternalServerError)
			return
		}
		if typ, ok := prometheusStats[k]; ok {
			w.Write([]byte(fmt.Sprintf("# TYPE fts_%s %s\n", k, typ)))
			w.Write(append([]byte("fts_"+k+" "), b...))
			w.Write(bline)
		}
	}
}
