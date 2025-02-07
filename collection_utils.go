//  Copyright 2020-Present Couchbase, Inc.
//
// Use of this software is governed by the Business Source License included
// in the file licenses/BSL-Couchbase.txt.  As of the Change Date specified
// in that file, in accordance with the Business Source License, use of this
// software will be governed by the Apache License, Version 2.0, included in
// the file licenses/APL2.txt.

package cbft

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/couchbase/cbgt"
	log "github.com/couchbase/clog"
)

const defaultScopeName = "_default"

const defaultCollName = "_default"

type sourceDetails struct {
	scopeName      string
	collUIDNameMap map[uint32]string // coll uid => coll name
}

type collMetaFieldCache struct {
	m                sync.RWMutex
	cache            map[string]string         // indexName$collName => _$suid_$cuid
	sourceDetailsMap map[string]*sourceDetails // indexName => sourceDetails
}

// metaFieldValCache holds a runtime volatile cache
// for collection meta field look ups during the collection
// specific query in a multi collection index.
var metaFieldValCache *collMetaFieldCache

func init() {
	metaFieldValCache = &collMetaFieldCache{
		cache:            make(map[string]string),
		sourceDetailsMap: make(map[string]*sourceDetails),
	}
}

// refreshMetaFieldValCache updates the metaFieldValCache with
// the latest index definitions.
func refreshMetaFieldValCache(indexDefs *cbgt.IndexDefs) {
	if indexDefs == nil || metaFieldValCache == nil {
		return
	}

	bleveParams := NewBleveParams()
	for _, indexDef := range indexDefs.IndexDefs {
		err := json.Unmarshal([]byte(indexDef.Params), bleveParams)
		if err != nil {
			log.Printf("collection_utils: refreshMetaFieldValCache,"+
				" indexName: %v, err: %v", indexDef.Name, err)
			continue
		}
		if strings.HasPrefix(bleveParams.DocConfig.Mode, ConfigModeCollPrefix) {
			if im, ok := bleveParams.Mapping.(*mapping.IndexMappingImpl); ok {
				_ = initMetaFieldValCache(indexDef.Name, indexDef.SourceName, im)
			}
		}
	}
}

func (c *collMetaFieldCache) getMetaFieldValue(indexName, collName string) string {
	c.m.RLock()
	rv := c.cache[indexName+"$"+collName]
	c.m.RUnlock()
	return rv
}

func encodeCollMetaFieldValue(suid, cuid int64) string {
	return fmt.Sprintf("_$%d_$%d", suid, cuid)
}

func (c *collMetaFieldCache) setValue(indexName, scopeName string,
	suid int64, collName string, cuid int64, multiCollIndex bool) {
	key := indexName + "$" + collName
	c.m.Lock()
	c.cache[key] = encodeCollMetaFieldValue(suid, cuid)

	if multiCollIndex {
		var sd *sourceDetails
		var ok bool
		if sd, ok = c.sourceDetailsMap[indexName]; !ok {
			sd = &sourceDetails{scopeName: scopeName,
				collUIDNameMap: make(map[uint32]string)}
			c.sourceDetailsMap[indexName] = sd
		}
		sd.collUIDNameMap[uint32(cuid)] = collName
	}

	c.m.Unlock()
}

func (c *collMetaFieldCache) reset(indexName string) {
	prefix := indexName + "$"
	c.m.Lock()
	delete(c.sourceDetailsMap, indexName)

	for k := range c.cache {
		if strings.HasPrefix(k, prefix) {
			delete(c.cache, k)
		}
	}
	c.m.Unlock()
}

func (c *collMetaFieldCache) getSourceDetailsMap(indexName string) (
	*sourceDetails, bool) {
	c.m.RLock()
	sdm, multiCollIndex := c.sourceDetailsMap[indexName]
	var ret *sourceDetails
	if sdm != nil {
		// obtain a copy of the sourceDetails
		ret = &sourceDetails{
			scopeName:      sdm.scopeName,
			collUIDNameMap: make(map[uint32]string),
		}

		for k, v := range sdm.collUIDNameMap {
			ret.collUIDNameMap[k] = v
		}
	}
	c.m.RUnlock()
	return ret, multiCollIndex
}

func (c *collMetaFieldCache) getScopeCollectionNames(indexName string) (
	scopeName string, collectionNames []string) {
	c.m.RLock()
	sdm, _ := c.sourceDetailsMap[indexName]
	if sdm != nil {
		scopeName = sdm.scopeName
		collectionNames = make([]string, len(sdm.collUIDNameMap))
		i := 0
		for _, v := range sdm.collUIDNameMap {
			collectionNames[i] = v
			i++
		}
	}
	c.m.RUnlock()
	return
}

func scopeCollTypeMapping(in string) (string, string, string) {
	vals := strings.SplitN(in, ".", 3)
	scope := "_default"
	collection := "_default"
	typeMapping := ""
	if len(vals) == 1 {
		typeMapping = vals[0]
	} else if len(vals) == 2 {
		scope = vals[0]
		collection = vals[1]
	} else {
		scope = vals[0]
		collection = vals[1]
		typeMapping = vals[2]
	}

	return scope, collection, typeMapping
}

// getScopeCollTypeMappings will return a deduplicated
// list of collection names when skipMappings is enabled.
func getScopeCollTypeMappings(im *mapping.IndexMappingImpl,
	skipMappings bool) (scope string,
	cols []string, typeMappings []string, err error) {
	// index the _default/_default scope and collection when
	// default mapping is enabled.
	if im.DefaultMapping.Enabled {
		scope = defaultScopeName
		cols = []string{defaultCollName}
		typeMappings = []string{""}
	}

	hash := make(map[string]struct{}, len(im.TypeMapping))
	for tp, dm := range im.TypeMapping {
		if !dm.Enabled {
			continue
		}
		s, c, t := scopeCollTypeMapping(tp)
		key := c
		if !skipMappings {
			key += t
		}
		if _, exists := hash[key]; !exists {
			hash[key] = struct{}{}
			cols = append(cols, c)
			if !skipMappings {
				typeMappings = append(typeMappings, t)
			}
		}
		if scope == "" {
			scope = s
			continue
		}
		if scope != s {
			return "", nil, nil, fmt.Errorf("collection_utils: multiple scopes"+
				" found: '%s', '%s'; index can only span collections on a single"+
				" scope", scope, s)
		}
	}

	return scope, cols, typeMappings, nil
}

// validateScopeCollFromMappings performs the $scope.$collection
// validations in the type mappings defined. It also performs
// - single scope validation across collections
// - verify scope to collection mapping with kv manifest
func validateScopeCollFromMappings(bucket string,
	im *mapping.IndexMappingImpl, ignoreCollNotFoundErrs bool) (*Scope, error) {
	sName, collNames, typeMappings, err := getScopeCollTypeMappings(im, false)
	if err != nil {
		return nil, err
	}

	manifest, err := GetBucketManifest(bucket)
	if err != nil {
		return nil, err
	}

	rv := &Scope{Collections: make([]Collection, 0, len(collNames))}
	for _, scope := range manifest.Scopes {
		if scope.Name == sName {
			rv.Name = scope.Name
			rv.Uid = scope.Uid
		OUTER:
			for i := range collNames {
				for _, collection := range scope.Collections {
					if collection.Name == collNames[i] {
						rv.Collections = append(rv.Collections, Collection{
							Uid:         collection.Uid,
							Name:        collection.Name,
							typeMapping: typeMappings[i],
						})
						continue OUTER
					}
				}
				if !ignoreCollNotFoundErrs {
					return nil, fmt.Errorf("collection_utils: collection:"+
						" '%s' doesn't belong to scope: '%s' in bucket: '%s'",
						collNames[i], sName, bucket)
				}
			}
			break
		}
	}

	if rv.Name == "" {
		return nil, fmt.Errorf("collection_utils: scope:"+
			" '%s' not found in bucket: '%s' ",
			sName, bucket)
	}

	return rv, nil
}

func enhanceMappingsWithCollMetaField(tm map[string]*mapping.DocumentMapping) error {
OUTER:
	for _, mp := range tm {
		fm := metaFieldMapping()
		for fname := range mp.Properties {
			if fname == CollMetaFieldName {
				continue OUTER
			}
		}
		mp.AddFieldMappingsAt(CollMetaFieldName, fm)
	}
	return nil
}

func metaFieldMapping() *mapping.FieldMapping {
	fm := mapping.NewTextFieldMapping()
	fm.Store = false
	fm.DocValues = false
	fm.IncludeInAll = false
	fm.IncludeTermVectors = false
	fm.Name = CollMetaFieldName
	fm.Analyzer = "keyword"
	return fm
}

// -----------------------------------------------------------------------------

// API to retrieve the scope & unique list of collection names from the
// provided index definition.
func GetScopeCollectionsFromIndexDef(indexDef *cbgt.IndexDef) (
	scope string, collections []string, err error) {
	if indexDef == nil {
		return "", nil, fmt.Errorf("no index-def provided")
	}

	bp := NewBleveParams()
	if len(indexDef.Params) > 0 {
		if err = json.Unmarshal([]byte(indexDef.Params), bp); err != nil {
			return "", nil, err
		}

		if strings.HasPrefix(bp.DocConfig.Mode, ConfigModeCollPrefix) {
			if im, ok := bp.Mapping.(*mapping.IndexMappingImpl); ok {
				var collectionNames []string
				scope, collectionNames, _, err := getScopeCollTypeMappings(im, false)
				if err != nil {
					return "", nil, err
				}

				uniqueCollections := map[string]struct{}{}
				for _, coll := range collectionNames {
					if _, exists := uniqueCollections[coll]; !exists {
						uniqueCollections[coll] = struct{}{}
						collections = append(collections, coll)
					}
				}

				return scope, collections, nil
			}
		}
	}

	return "_default", []string{"_default"}, nil
}

func multiCollection(colls []Collection) bool {
	hash := make(map[string]struct{}, 1)
	for _, c := range colls {
		if _, ok := hash[c.Name]; !ok {
			hash[c.Name] = struct{}{}
		}
	}
	return len(hash) > 1
}
