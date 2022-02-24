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
	"errors"
	"fmt"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/mapping"
	index "github.com/blevesearch/bleve_index_api"
	"github.com/couchbase/cbgt"
	log "github.com/couchbase/clog"
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

func (m *HibernatedPIndex) DocCount() (uint64, error) {
	return 0, nil
}

func (m *HibernatedPIndex) Search(req *bleve.SearchRequest) (
	*bleve.SearchResult, error) {
	return m.SearchInContext(context.Background(), req)
}

func (m *HibernatedPIndex) SearchInContext(ctx context.Context,
	req *bleve.SearchRequest) (*bleve.SearchResult, error) {
	log.Printf("hib pindexes: in searchincontext...")
	// trial - here since existing indexes will be 'bleve opened' on startup.
	// can consider removing this once cold indexes are not 'bleve opened' on start
	err := m.pindex.Close(false)
	if err != nil {
		return nil, fmt.Errorf("hib pindexes: error closing pindex: %s", err.Error())
	}

	restart := func() {} // just for trial
	bleveParams := NewBleveParams()
	kvConfig, _, _ := bleveRuntimeConfigMap(bleveParams)
	kvConfig["read_only"] = true
	log.Printf("hib pindexes: start open using: %s", m.pindex.Path)
	bindex, err := bleve.OpenUsing(m.pindex.Path, kvConfig)
	if err != nil {
		return nil, fmt.Errorf("hib pindexes: err opening pindex path: %s", err.Error())
	}
	// need to initialise the bleve-related parameters to avoid a nil bindex
	m.pindex.Impl = bindex
	m.pindex.Dest = &cbgt.DestForwarder{
		DestProvider: NewBleveDest(m.pindex.Path, bindex, restart, bleveParams.DocConfig),
	}
	log.Printf("hib pindexes: finished open using: %s", m.pindex.Path)

	// call search in context using bindex
	searchResult, err := bindex.SearchInContext(ctx, req)
	if err != nil {
		return searchResult, fmt.Errorf("hib pindexes: error searching : %s", err.Error())
	}
	// works with only stopPIndex() for a new index
	err = m.mgr.StopPIndex(m.pindex, false) // stop feeds
	// do feeds need to be stopped?
	if err != nil {
		return nil, fmt.Errorf("hib pindexes: error stopping pindex: %s", err.Error())
	}
	err = m.pindex.Close(false)
	if err != nil {
		return nil, fmt.Errorf("hib pindexes: error closing pindex: %s", err.Error())
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
