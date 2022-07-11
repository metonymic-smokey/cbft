//  Copyright 2021-Present Couchbase, Inc.
//
//  Use of this software is governed by the Business Source License included
//  in the file licenses/BSL-Couchbase.txt.  As of the Change Date specified
//  in that file, in accordance with the Business Source License, use of this
//  software will be governed by the Apache License, Version 2.0, included in
//  the file licenses/APL2.txt.

package cbft

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/blevesearch/bleve/v2"

	"github.com/couchbase/cbft/http"
	"github.com/couchbase/cbgt"
	"github.com/couchbase/cbgt/rest"
	log "github.com/couchbase/clog"
	"github.com/couchbase/tools-common/objstore/objcli"
	"github.com/couchbase/tools-common/objstore/objcli/objaws"
	"github.com/couchbase/tools-common/objstore/objutil"
	"github.com/couchbase/tools-common/objstore/objval"
)

// CopyPartition is an overridable implementation
// for copying the remote partition contents.
var CopyPartition func(mgr *cbgt.Manager,
	moveReq *CopyPartitionRequest) error

// IsCopyPartitionPreferred is an overridable implementation
// for deciding whether to build a partition from scratch over DCP
// or by copying index partition files from potential remote nodes
// during a rebalance operation.
var IsCopyPartitionPreferred func(mgr *cbgt.Manager,
	pindexName, path, sourceParams string) bool

// IsFeedable is an implementation of cbgt.Feedable interface.
// It returns true if the dest is ready for feed ingestion.
func (t *BleveDest) IsFeedable() (bool, error) {
	t.m.RLock()
	defer t.m.RUnlock()

	if _, ok := t.bindex.(*noopBleveIndex); ok {
		return false, nil
	} else if _, ok := t.bindex.(bleve.Index); ok {
		return true, nil
	}
	return false, fmt.Errorf("pindex_bleve_copy: failed creating bleve"+
		" index for pindex: %s", cbgt.PIndexNameFromPath(t.path))
}

func downloadFromBucket(c objcli.Client, bucket, prefix, pindexPath string) error {
	err := os.MkdirAll(pindexPath+"/store", os.ModePerm) // change to fsutil.
	if err != nil {
		return fmt.Errorf("pindex bleve copy: failed to create folder '%s': %w", pindexPath, err)
	}

	fn := func(attrs *objval.ObjectAttrs) error {
		if strings.HasPrefix(attrs.Key, prefix) {
			split := strings.Split(attrs.Key, "/")
			fileKey := strings.Join(split[1:], "/") // skip the pindex folder name.
			filename := pindexPath + "/" + fileKey
			log.Printf("pindex bleve copy: downloading file: %s", filename)
			file, err := os.Create(filename)
			if err != nil {
				return fmt.Errorf("pindex bleve copy: failed create file: %+v", err)
			}
			defer file.Close()

			options := objutil.DownloadOptions{
				Client: c,
				Bucket: bucket,
				Key:    attrs.Key,
				Writer: file,
			}
			err = objutil.Download(options)
			if err != nil {
				log.Printf("pindex bleve copy: error downloading file at '%s': %w", filename, err)
			}
		}
		return nil
	}

	err = c.IterateObjects(bucket, prefix, "", nil, nil, fn)
	if err != nil {
		return fmt.Errorf("pindex bleve copy: failed to iterate objects: %w", err)
	}

	return nil
}

func getKeyPrefixFromPIndex(pindexName string) string {
	prefix := ""
	split := strings.Split(pindexName, "_")
	prefix = split[0] + "_" + split[2]
	return prefix
}

func getS3Client() (*objaws.Client, error) {
	session, err := session.NewSession(&aws.Config{
		Region: aws.String("us-east-1"),
	})
	if err != nil {
		log.Printf("pindex bleve copy: error creating session: %e", err)
		return nil, err
	}

	client := objaws.NewClient(s3.New(session))
	return client, nil
}

func uploadToBucket(c objcli.Client, bucket, pindexPath string) error {
	err := filepath.Walk(pindexPath,
		func(path string, info os.FileInfo, err error) error {
			if info != nil && !info.IsDir() {
				file, err := ioutil.ReadFile(path)
				if err != nil {
					fmt.Println("pindex bleve copy: unable to read file " + path)
					return err
				}
				client, err := getS3Client()
				dir, filename := filepath.Split(path)
				pindexName := cbgt.PIndexNameFromPath(pindexPath)
				split := strings.Split(pindexName, "_")
				key_prefix := split[0] + "_" + split[2]
				key := key_prefix + "/"
				if filepath.Base(dir) == "store" {
					key += filepath.Base(dir) + "/"
				}
				log.Printf("pindex bleve copy: uploading file: %s", path)
				options := objutil.UploadOptions{
					Client: client,
					Bucket: bucket,
					Key:    key + filename,
					Body:   bytes.NewReader(file),
				}

				// Todo - concurrent uploads for the files in a dir?
				err = objutil.Upload(options)
				if err != nil {
					if awsErr, ok := err.(awserr.Error); ok {
						log.Printf("pindex bleve copy: upload error is :%s", awsErr.Message())
					}
				}
			}

			return nil
		})
	return err
}

// return no op bleve pindex here based on hib status.
func NewNoOpBlevePIndexImplEx(indexType, indexParams, sourceParams, path string,
	mgr *cbgt.Manager, restart func()) (cbgt.PIndexImpl, cbgt.Dest, error) {
	pindexName := cbgt.PIndexNameFromPath(path)
	// validate the index params and exit early on errors.
	bleveParams, kvConfig, _, _, err := parseIndexParams(indexParams)
	if err != nil {
		return nil, nil, err
	}

	destChan := make(chan *BleveDest, 1)

	bucket := "beer-sample-default-default-test5"
	var hp rest.HibernateParam
	err = json.Unmarshal([]byte(indexParams), &hp)
	if hp.HibernateStatus == cbgt.Hot { // only download and add feeds for a hot pindex
		go func(mgr *cbgt.Manager, kvConfig map[string]interface{}, destChan chan *BleveDest) {
			client, _ := getS3Client()
			prefix := getKeyPrefixFromPIndex(pindexName)
			err := downloadFromBucket(client, bucket, prefix, path)
			if err != nil {
				log.Printf("pindex bleve copy: error while downloading: %#v", err)
			}
			// P1 - Use error channel.
			var bindex bleve.Index
			bindex, err = openBleveIndex(path, kvConfig)
			if err != nil {
				log.Printf("pindex bleve copy: error while opening bleve index: %#v", err)
			}

			// read from dest channel - blocks until there is data to receive
			// i.e. till no-op dest has been formed.
			dest := <-destChan
			updateBleveIndex(pindexName, mgr, bindex, dest)
		}(mgr, kvConfig, destChan)
	} else { // for a cold pindex, upload files to the new location.
		log.Printf("pindex bleve copy: uploading path %s", path)
		go func() {
			client, _ := getS3Client()
			err = uploadToBucket(client, bucket, path)
			if err != nil {
				log.Printf("pindex bleve copy: error while uploading files: %#v", err)
			}
			err = os.RemoveAll(path)
			if err != nil {
				log.Printf("pindex bleve copy: error cleaning path %s: %e", path, err)
			}
		}()
	}

	// create a noop index and wrap that inside the dest.
	noopImpl := &noopBleveIndex{name: pindexName}
	dest := &BleveDest{
		path:           path,
		bleveDocConfig: bleveParams.DocConfig,
		restart:        restart,
		bindex:         noopImpl,
		partitions:     make(map[string]*BleveDestPartition),
		stats:          cbgt.NewPIndexStoreStats(),
		copyStats:      &CopyPartitionStats{},
		stopCh:         make(chan struct{}),
	}
	dest.batchReqChs = make([]chan *batchRequest, asyncBatchWorkerCount)
	destChan <- dest
	destfwd := &cbgt.DestForwarder{DestProvider: dest}

	return nil, destfwd, nil
}

func NewBlevePIndexImplEx(indexType, indexParams, sourceParams, path string,
	mgr *cbgt.Manager, restart func()) (cbgt.PIndexImpl, cbgt.Dest, error) {
	var hp rest.HibernateParam
	err := json.Unmarshal([]byte(indexParams), &hp)
	if hp.HibernateStatus != 0 {
		return NewNoOpBlevePIndexImplEx(indexType, indexParams, sourceParams, path,
			mgr, restart)
	}

	pindexName := cbgt.PIndexNameFromPath(path)
	// validate the index params and exit early on errors.
	bleveParams, kvConfig, bleveIndexType, _, err := parseIndexParams(indexParams)
	if err != nil {
		return nil, nil, err
	}

	if CopyPartition == nil || IsCopyPartitionPreferred == nil ||
		bleveIndexType == "upside_down" ||
		!IsCopyPartitionPreferred(mgr, pindexName, path, sourceParams) {
		return NewBlevePIndexImpl(indexType, indexParams, path, restart)
	}

	if path != "" {
		err := os.MkdirAll(path, 0700)
		if err != nil {
			return nil, nil, err
		}
	}

	pathMeta := filepath.Join(path, "PINDEX_BLEVE_META")
	err = ioutil.WriteFile(pathMeta, []byte(indexParams), 0600)
	if err != nil {
		return nil, nil, err
	}

	// create a noop index and wrap that inside the dest.
	noopImpl := &noopBleveIndex{name: pindexName}
	dest := &BleveDest{
		path:           path,
		bleveDocConfig: bleveParams.DocConfig,
		restart:        restart,
		bindex:         noopImpl,
		partitions:     make(map[string]*BleveDestPartition),
		stats:          cbgt.NewPIndexStoreStats(),
		copyStats:      &CopyPartitionStats{},
		stopCh:         make(chan struct{}),
	}
	dest.batchReqChs = make([]chan *batchRequest, asyncBatchWorkerCount)
	destfwd := &cbgt.DestForwarder{DestProvider: dest}

	go tryCopyBleveIndex(indexType, indexParams, path, kvConfig,
		restart, dest, mgr)

	return nil, destfwd, nil
}

// tryCopyBleveIndex tries to copy the pindex files and open it,
// and upon errors falls back to a fresh index creation.
func tryCopyBleveIndex(indexType, indexParams, path string,
	kvConfig map[string]interface{}, restart func(),
	dest *BleveDest, mgr *cbgt.Manager) (err error) {
	pindexName := cbgt.PIndexNameFromPath(path)

	defer func() {
		// fallback to fresh new creation upon errors.
		if err != nil {
			createNewBleveIndex(indexType, indexParams,
				path, restart, dest, mgr)
			return
		}
	}()

	err = copyBleveIndex(pindexName, path, dest, mgr)
	if err != nil {
		return err
	}

	// check whether the dest is already closed.
	if isClosed(dest.stopCh) {
		log.Printf("pindex_bleve_copy: tryCopyBleveIndex pindex: %s"+
			" has already closed", pindexName)
		// It is cleaner to remove the path as there could be corner/racy
		// cases where it is still desirable, for eg: when the copy partition
		// operation performs a rename of the temp download dir to pindex path
		// that might have missed the clean up performed during the dest closure.
		_ = os.RemoveAll(path)
		return nil
	}

	var bindex bleve.Index
	bindex, err = openBleveIndex(path, kvConfig)
	if err != nil {
		return err
	}

	updateBleveIndex(pindexName, mgr, bindex, dest)
	return nil
}

func copyBleveIndex(pindexName, path string, dest *BleveDest,
	mgr *cbgt.Manager) error {
	startTime := time.Now()

	log.Printf("pindex_bleve_copy: pindex: %s, CopyPartition"+
		" started", pindexName)

	// build the remote partition copy request.
	req, err := buildCopyPartitionRequest(pindexName, dest.copyStats,
		mgr, dest.stopCh)
	if req == nil || err != nil {
		log.Printf("pindex_bleve_copy: buildCopyPartitionRequest,"+
			" no source nodes found for partition: %s, err: %v",
			pindexName, err)
		return err
	}

	err = CopyPartition(mgr, req)
	if err != nil {
		log.Printf("pindex_bleve_copy: CopyPartition failed, err: %v", err)
		return err
	}

	log.Printf("pindex_bleve_copy: pindex: %s, CopyPartition"+
		" finished, took: %s", pindexName, time.Since(startTime).String())

	return err
}

func openBleveIndex(path string, kvConfig map[string]interface{}) (
	bleve.Index, error) {
	startTime := time.Now()

	log.Printf("pindex_bleve_copy: start open using: %s", path)
	bindex, err := bleve.OpenUsing(path, kvConfig)
	if err != nil {
		log.Printf("pindex_bleve_copy: bleve.OpenUsing,"+
			" err: %v", err)
		return nil, err
	}

	log.Printf("pindex_bleve_copy: finished open using: %s"+
		" took: %s", path, time.Since(startTime).String())

	return bindex, nil
}

func createNewBleveIndex(indexType, indexParams, path string,
	restart func(), dest *BleveDest, mgr *cbgt.Manager) {
	pindexName := cbgt.PIndexNameFromPath(path)
	// check whether the dest is already closed.
	if isClosed(dest.stopCh) {
		log.Printf("pindex_bleve_copy: createNewBleveIndex pindex: %s"+
			" has already closed", pindexName)
		// It is cleaner to remove the path as there could be corner/racy
		// cases where it is still desirable, for eg: when the copy partition
		// operation performs a rename of the temp download dir to pindex path
		// that might have missed the clean up performed during the dest closure.
		_ = os.RemoveAll(path)
		return
	}

	impl, destWrapper, err := NewBlevePIndexImpl(indexType, indexParams, path, restart)
	if err != nil {
		var ok bool
		var pi *cbgt.PIndex

		// fetch the pindex again to ensure that pindex is still live.
		_, pindexes := mgr.CurrentMaps()
		if pi, ok = pindexes[pindexName]; !ok {
			log.Printf("pindex_bleve_copy: pindex: %s"+
				" no longer exists", pindexName)
			return
		}

		// remove the currently registered noop powered pindex.
		_ = mgr.RemovePIndex(pi)
		mgr.JanitorKick(fmt.Sprintf("restart kick for pindex: %s",
			pindexName))

		return
	}

	// stop the old workers.
	if fwder, ok := destWrapper.(*cbgt.DestForwarder); ok {
		if bdest, ok := fwder.DestProvider.(*BleveDest); ok {
			bdest.stopBatchWorkers()
		}
	}

	// update the new index into the pindex.
	if bindex, ok := impl.(bleve.Index); ok {
		updateBleveIndex(pindexName, mgr, bindex, dest)
	} else {
		log.Errorf("pindex_bleve_copy: no bleve.Index implementation"+
			"found: %s", pindexName)
	}
}

func updateBleveIndex(pindexName string, mgr *cbgt.Manager,
	index bleve.Index, dest *BleveDest) {
	dest.resetBIndex(index)
	dest.startBatchWorkers()
	http.RegisterIndexName(pindexName, index)

	mgr.JanitorKick(fmt.Sprintf("feed init kick for pindex: %s", pindexName))
}

func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
