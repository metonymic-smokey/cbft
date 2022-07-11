//  Copyright 2016-Present Couchbase, Inc.
//
//  Use of this software is governed by the Business Source License included
//  in the file licenses/BSL-Couchbase.txt.  As of the Change Date specified
//  in that file, in accordance with the Business Source License, use of this
//  software will be governed by the Apache License, Version 2.0, included in
//  the file licenses/APL2.txt.

package cbft

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"strconv"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/mapping"
	index "github.com/blevesearch/bleve_index_api"
	"github.com/couchbase/cbgt"

	log "github.com/couchbase/clog"

	"github.com/pkg/errors"
)

var hibernatedPIndexUnimplementedErr = errors.New("unimplemented for " +
	"hibernated pindexes")

type HibernatedPIndex struct {
	name   string
	pindex *cbgt.PIndex
	mgr    *cbgt.Manager
}

func (m *HibernatedPIndex) Name() string {
	return m.name
}

func (m *HibernatedPIndex) SetName(name string) {
	m.name = name
}

func (m *HibernatedPIndex) Index(id string, data interface{}) error {
	return hibernatedPIndexUnimplementedErr
}

func (m *HibernatedPIndex) Delete(id string) error {
	return hibernatedPIndexUnimplementedErr
}

func (m *HibernatedPIndex) Batch(b *bleve.Batch) error {
	return hibernatedPIndexUnimplementedErr
}

func (m *HibernatedPIndex) Document(id string) (index.Document, error) {
	return nil, hibernatedPIndexUnimplementedErr
}

func docCountUtil(path string) (uint64, error) {
	index, err := bleve.OpenUsing(path, nil)
	if err != nil {
		return 0, fmt.Errorf("hib pindexes: error opening "+
			"bleve path %s: %v", path, err)
	}

	count, err := index.DocCount()
	if err != nil {
		return 0, fmt.Errorf("hib pindexes: error getting "+
			"docCount for path %s: %v", path, err)
	}
	err = index.Close()
	if err != nil {
		return 0, fmt.Errorf("hib pindexes: error closing "+
			"bleve path %s: %v", path, err)
	}
	return count, nil
}

func appendDocCount(path string, buf []byte) ([]byte, error) {
	count, err := docCountUtil(path)
	if err != nil {
		return []byte{}, fmt.Errorf("hib pindexes: error calculating "+
			"docCount for path %s: %v", path, err)
	}
	bufStr := string(buf)
	startPos := len(bufStr) - 2
	newStr := bufStr[0:startPos+1] + ",\"docCount\":" +
		strconv.Itoa(int(count)) + "}"
	buf = []byte(newStr)
	return buf, nil
}

type pindexDocCount struct {
	DocCount int `json:"docCount"`
}

func (m *HibernatedPIndex) DocCount() (uint64, error) {
	buf, err := ioutil.ReadFile(m.pindex.Path +
		string(os.PathSeparator) + cbgt.PINDEX_META_FILENAME)
	if err != nil {
		return 0, fmt.Errorf("hib pindexes: could not load PINDEX_META_FILENAME,"+
			" path: %s, err: %v", m.pindex.Path, err)
	}
	temp := &pindexDocCount{}
	err = json.Unmarshal(buf, &temp)
	if err != nil {
		return 0, fmt.Errorf("hib pindexes: hibernatePIndex could not unmarshal "+
			"file %s, err: %v", m.pindex.Path, err)
	}
	if temp.DocCount == 0 {
		buf, err = appendDocCount(m.pindex.Path, buf)
		if err != nil {
			return 0, fmt.Errorf("hib pindexes: hibernatePIndex could append to "+
				"file %s, err: %v", m.pindex.Path, err)
		}
		err = ioutil.WriteFile(m.pindex.Path+string(os.PathSeparator)+
			cbgt.PINDEX_META_FILENAME, buf, 0600)
		if err != nil {
			return 0, fmt.Errorf("hib pindexes: hibernatePIndex could not save "+
				"PINDEX_META_FILENAME,"+" path: %s, err: %v", m.pindex.Path, err)
		}
		err = json.Unmarshal(buf, &temp)
		if err != nil {
			return 0, fmt.Errorf("hib pindexes: hibernatePIndex could not unmarshal "+
				"file %s, err: %v", m.pindex.Path, err)
		}
	}
	count := uint64(temp.DocCount)
	return count, nil
}

func (m *HibernatedPIndex) Search(req *bleve.SearchRequest) (
	*bleve.SearchResult, error) {
	return m.SearchInContext(context.Background(), req)
}

func (m *HibernatedPIndex) SearchInContext(ctx context.Context,
	req *bleve.SearchRequest) (*bleve.SearchResult, error) {
	kvConfig := make(map[string]interface{})
	kvConfig["read_only"] = true
	log.Printf("hib pindexes: start open using: %s", m.pindex.Path)
	bindex, err := bleve.OpenUsing(m.pindex.Path, kvConfig)
	if err != nil {
		return nil, fmt.Errorf("hib pindexes: err opening pindex path: %s",
			err.Error())
	}
	log.Printf("hib pindexes: finished open using: %s", m.pindex.Path)

	// call search in context using bindex
	searchResult, err := bindex.SearchInContext(ctx, req)
	if err != nil {
		return searchResult, fmt.Errorf("hib pindexes: error searching : %s",
			err.Error())
	}
	// close bleve index
	err = bindex.Close()
	if err != nil {
		return nil, fmt.Errorf("hib pindexes: error closing bleve index: %s",
			err.Error())
	}

	return searchResult, nil
}

func (m *HibernatedPIndex) Fields() ([]string, error) {
	return nil, hibernatedPIndexUnimplementedErr
}

func (m *HibernatedPIndex) FieldDict(field string) (index.FieldDict, error) {
	return nil, hibernatedPIndexUnimplementedErr
}

func (m *HibernatedPIndex) FieldDictRange(field string,
	startTerm []byte, endTerm []byte) (index.FieldDict, error) {
	return nil, hibernatedPIndexUnimplementedErr
}

func (m *HibernatedPIndex) FieldDictPrefix(field string,
	termPrefix []byte) (index.FieldDict, error) {
	return nil, hibernatedPIndexUnimplementedErr
}

func (m *HibernatedPIndex) DumpAll() chan interface{} {
	return nil
}

func (m *HibernatedPIndex) DumpDoc(id string) chan interface{} {
	return nil
}

func (m *HibernatedPIndex) DumpFields() chan interface{} {
	return nil
}

func (m *HibernatedPIndex) Close() error {
	// what is the status on this since already 'closed'?
	return hibernatedPIndexUnimplementedErr
}

func (m *HibernatedPIndex) Mapping() mapping.IndexMapping {
	return nil
}

func (m *HibernatedPIndex) NewBatch() *bleve.Batch {
	return nil
}

func (m *HibernatedPIndex) Stats() *bleve.IndexStat {
	// change stats to IndexStat and return here
	return nil
}

func (m *HibernatedPIndex) StatsMap() map[string]interface{} {
	return nil
}

func (m *HibernatedPIndex) GetInternal(key []byte) ([]byte, error) {
	return nil, hibernatedPIndexUnimplementedErr
}

func (m *HibernatedPIndex) SetInternal(key, val []byte) error {
	return hibernatedPIndexUnimplementedErr
}

func (m *HibernatedPIndex) DeleteInternal(key []byte) error {
	return hibernatedPIndexUnimplementedErr
}

func (m *HibernatedPIndex) Advanced() (index.Index, error) {
	return nil, hibernatedPIndexUnimplementedErr
}
