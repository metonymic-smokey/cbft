//  Copyright 2016-Present Couchbase, Inc.
//
//  Use of this software is governed by the Business Source License included
//  in the file licenses/BSL-Couchbase.txt.  As of the Change Date specified
//  in that file, in accordance with the Business Source License, use of this
//  software will be governed by the Apache License, Version 2.0, included in
//  the file licenses/APL2.txt.

package cbft

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"github.com/couchbase/cbgt"
)

var ConfigModeCollPrefix = "scope.collection"
var ConfigModeCollPrefixLen = len(ConfigModeCollPrefix)

var CollMetaFieldName = "_$scope_$collection"

type BleveInterface interface{}

type BleveDocument struct {
	typ            string
	BleveInterface `json:""`
}

func (c *BleveDocument) Type() string {
	return c.typ
}

func (c *BleveDocument) SetType(nTyp string) {
	c.typ = nTyp
}

type collMetaField struct {
	scopeDotColl string
	typeMappings []string // for multiple mappings for a collection
	value        string   // _$<scope_id>_$<collection_id>
}

type BleveDocumentConfig struct {
	Mode             string         `json:"mode"`
	TypeField        string         `json:"type_field"`
	DocIDPrefixDelim string         `json:"docid_prefix_delim"`
	DocIDRegexp      *regexp.Regexp `json:"docid_regexp"`
	CollPrefixLookup map[uint32]*collMetaField
	legacyMode       bool
}

func (b *BleveDocumentConfig) UnmarshalJSON(data []byte) error {
	docIDRegexp := ""
	if b.DocIDRegexp != nil {
		docIDRegexp = b.DocIDRegexp.String()
	}
	if b.CollPrefixLookup == nil {
		b.CollPrefixLookup = make(map[uint32]*collMetaField, 1)
	}
	tmp := struct {
		Mode             string `json:"mode"`
		TypeField        string `json:"type_field"`
		DocIDPrefixDelim string `json:"docid_prefix_delim"`
		DocIDRegexp      string `json:"docid_regexp"`
		CollPrefixLookup map[uint32]*collMetaField
	}{
		Mode:             b.Mode,
		TypeField:        b.TypeField,
		DocIDPrefixDelim: b.DocIDPrefixDelim,
		DocIDRegexp:      docIDRegexp,
		CollPrefixLookup: b.CollPrefixLookup,
	}
	err := json.Unmarshal(data, &tmp)
	if err != nil {
		return err
	}
	b.Mode = tmp.Mode
	switch tmp.Mode {
	case "scope.collection":
		return nil
	case "scope.collection.type_field":
		fallthrough
	case "type_field":
		b.TypeField = tmp.TypeField
		if b.TypeField == "" {
			return fmt.Errorf("with mode type_field, type_field cannot be empty")
		}
	case "scope.collection.docid_prefix":
		fallthrough
	case "docid_prefix":
		b.DocIDPrefixDelim = tmp.DocIDPrefixDelim
		if b.DocIDPrefixDelim == "" {
			return fmt.Errorf("with mode docid_prefix, docid_prefix_delim cannot be empty")
		}
	case "scope.collection.docid_regexp":
		fallthrough
	case "docid_regexp":
		if tmp.DocIDRegexp != "" {
			b.DocIDRegexp, err = regexp.Compile(tmp.DocIDRegexp)
			if err != nil {
				return err
			}
		} else {
			return fmt.Errorf("with mode docid_regexp, docid_regexp cannot be empty")
		}
	default:
		return fmt.Errorf("unknown mode: %s", tmp.Mode)
	}

	return nil
}

func (b *BleveDocumentConfig) MarshalJSON() ([]byte, error) {
	docIDRegexp := ""
	if b.DocIDRegexp != nil {
		docIDRegexp = b.DocIDRegexp.String()
	}
	tmp := struct {
		Mode             string `json:"mode"`
		TypeField        string `json:"type_field"`
		DocIDPrefixDelim string `json:"docid_prefix_delim"`
		DocIDRegexp      string `json:"docid_regexp"`
	}{
		Mode:             b.Mode,
		TypeField:        b.TypeField,
		DocIDPrefixDelim: b.DocIDPrefixDelim,
		DocIDRegexp:      docIDRegexp,
	}
	return json.Marshal(&tmp)
}

func (b *BleveDocumentConfig) multiCollection() bool {
	return len(b.CollPrefixLookup) > 1
}

func (b *BleveDocumentConfig) BuildDocumentEx(key, val []byte,
	defaultType string, extrasType cbgt.DestExtrasType,
	extras []byte) (*BleveDocument, []byte, error) {
	var cmf *collMetaField
	if len(extras) >= 8 {
		cmf = b.CollPrefixLookup[binary.LittleEndian.Uint32(extras[4:])]
	}

	var v map[string]interface{}
	err := json.Unmarshal(val, &v)
	if err != nil || v == nil {
		v = map[string]interface{}{}
	}

	if cmf != nil && len(b.CollPrefixLookup) > 1 {
		// more than 1 collection indexed
		key = append(extras[4:8], key...)
		v[CollMetaFieldName] = cmf.value
	}

	bdoc := b.BuildDocumentFromObj(key, v, defaultType)
	if !b.legacyMode && cmf != nil {
		typ := cmf.scopeDotColl
		// there could be multiple type mappings under a single collection.
		for _, t := range cmf.typeMappings {
			if len(t) > 0 && bdoc.Type() == t {
				// append type information only if the type mapping specifies a
				// 'type' and the document's matches it.
				typ += "." + t
				break
			}
		}
		bdoc.SetType(typ)
	}

	return bdoc, key, err
}

// BuildDocument returns a BleveDocument for the k/v pair
// NOTE: err may be non-nil AND a document is returned
// this allows the error to be logged, but a stub document to be indexed
func (b *BleveDocumentConfig) BuildDocument(key, val []byte,
	defaultType string) (*BleveDocument, error) {
	var v interface{}
	err := json.Unmarshal(val, &v)
	if err != nil {
		v = map[string]interface{}{}
	}

	return b.BuildDocumentFromObj(key, v, defaultType), err
}

func (b *BleveDocumentConfig) BuildDocumentFromObj(key []byte, v interface{},
	defaultType string) *BleveDocument {
	return &BleveDocument{
		typ:            b.DetermineType(key, v, defaultType),
		BleveInterface: v,
	}
}

func (b *BleveDocumentConfig) DetermineType(key []byte, v interface{}, defaultType string) string {
	i := strings.LastIndex(b.Mode, ConfigModeCollPrefix)
	if i == -1 {
		i = 0
		b.legacyMode = true
	} else {
		i += ConfigModeCollPrefixLen
		if len(b.Mode) > ConfigModeCollPrefixLen {
			// consider the "." that follows scope.collection
			i++
		}
	}

	return b.findTypeUtil(key, v, defaultType, b.Mode[i:])
}

func (b *BleveDocumentConfig) findTypeUtil(key []byte, v interface{},
	defaultType, mode string) string {
	switch mode {
	case "type_field":
		typ, ok := mustString(lookupPropertyPath(v, b.TypeField))
		if ok {
			return typ
		}
	case "docid_prefix":
		index := bytes.Index(key, []byte(b.DocIDPrefixDelim))
		if index > 0 {
			return string(key[0:index])
		}
	case "docid_regexp":
		typ := b.DocIDRegexp.Find(key)
		if typ != nil {
			return string(typ)
		}
	}
	return defaultType
}

// utility functions copied from bleve/reflect.go
func lookupPropertyPath(data interface{}, path string) interface{} {
	pathParts := decodePath(path)

	current := data
	for _, part := range pathParts {
		current = lookupPropertyPathPart(current, part)
		if current == nil {
			break
		}
	}

	return current
}

func lookupPropertyPathPart(data interface{}, part string) interface{} {
	val := reflect.ValueOf(data)
	if !val.IsValid() {
		return nil
	}
	typ := val.Type()
	switch typ.Kind() {
	case reflect.Map:
		// TODO can add support for other map keys in the future
		if typ.Key().Kind() == reflect.String {
			if key := reflect.ValueOf(part); key.IsValid() {
				entry := val.MapIndex(key)
				if entry.IsValid() {
					return entry.Interface()
				}
			}
		}
	case reflect.Struct:
		field := val.FieldByName(part)
		if field.IsValid() && field.CanInterface() {
			return field.Interface()
		}
	}
	return nil
}

const pathSeparator = "."

func decodePath(path string) []string {
	return strings.Split(path, pathSeparator)
}

func mustString(data interface{}) (string, bool) {
	if data != nil {
		str, ok := data.(string)
		if ok {
			return str, true
		}
	}
	return "", false
}
