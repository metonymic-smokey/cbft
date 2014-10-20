//  Copyright (c) 2014 Couchbase, Inc.
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the
//  License. You may obtain a copy of the License at
//    http://www.apache.org/licenses/LICENSE-2.0
//  Unless required by applicable law or agreed to in writing,
//  software distributed under the License is distributed on an "AS
//  IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
//  express or implied. See the License for the specific language
//  governing permissions and limitations under the License.

package main

import (
	"fmt"

	log "github.com/couchbaselabs/clog"
)

// A janitor maintains feeds, creating and deleting as necessary.
func (mgr *Manager) JanitorLoop() {
	for reason := range mgr.janitorCh {
		log.Printf("janitor awakes, reason: %s", reason)

		if mgr.cfg == nil { // Can occur during testing.
			log.Printf("janitor skipped due to nil cfg")
			continue
		}

		planPIndexes, _, err := CfgGetPlanPIndexes(mgr.cfg)
		if err != nil {
			log.Printf("janitor skipped due to CfgGetPlanPIndexes err: %v", err)
			continue
		}
		if planPIndexes == nil {
			log.Printf("janitor skipped due to nil planPIndexes")
			continue
		}

		startFeeds, startPIndexes := mgr.CurrentMaps()

		neededPIndexes, unneededPIndexes, err :=
			CalcPIndexesDelta(startPIndexes, planPIndexes)
		if err != nil {
			log.Printf("janitor skipped due to CalcPIndexesDelta, err: %v", err)
			continue
		}
		log.Printf("janitor pindexes needed: %v, unneeded: %v",
			neededPIndexes, unneededPIndexes)

		neededFeeds, unneededFeeds, err :=
			CalcFeedsDelta(startFeeds, startPIndexes)
		if err != nil {
			log.Printf("janitor skipped due to CalcFeedsDelta, err: %v", err)
			continue
		}
		log.Printf("janitor feeds needed: %v, unneeded: %v",
			neededFeeds, unneededFeeds)

		// Create feeds that we're missing.
		for _, targetPindexes := range neededFeeds {
			mgr.StartFeed(targetPindexes)
		}

		// Teardown unneeded feeds.
		for _, unneededFeed := range unneededFeeds {
			mgr.StopFeed(unneededFeed)
		}
	}
}

// Functionally determine the delta of which pindexes need creation
// and which should be shut down.
func CalcPIndexesDelta(pindexes map[string]*PIndex, planPIndexes *PlanPIndexes) (
	neededPlanPIndex []*PlanPIndex, unneededPIndex []*PIndex, err error) {
	neededPlanPIndex = make([]*PlanPIndex, 0)
	unneededPIndex = make([]*PIndex, 0)

	// TODO.

	return neededPlanPIndex, unneededPIndex, nil
}

// Functionally determine the delta of which feeds need creation and
// which should be shut down.
func CalcFeedsDelta(feeds map[string]Feed, pindexes map[string]*PIndex) (
	neededFeeds [][]*PIndex, unneededFeeds []Feed, err error) {
	neededFeeds = make([][]*PIndex, 0)
	unneededFeeds = make([]Feed, 0)

	for _, pindex := range pindexes {
		neededFeedName := FeedName("default", pindex.Name(), "")
		if _, ok := feeds[neededFeedName]; !ok {
			neededFeeds = append(neededFeeds, []*PIndex{pindex})
		}
	}

	return neededFeeds, unneededFeeds, nil
}

func (mgr *Manager) StartFeed(pindexes []*PIndex) error {
	// TODO: Need to create a fan-out feed.
	for _, pindex := range pindexes {
		// TODO: Need bucket UUID.
		err := mgr.StartSimpleFeed(pindex) // TODO: err handling.
		if err != nil {
			log.Printf("error: could not start feed for pindex: %s, err: %v",
				pindex.Name(), err)
		}
	}
	return nil
}

func (mgr *Manager) StopFeed(feed Feed) error {
	// TODO.
	return nil
}

func (mgr *Manager) StartSimpleFeed(pindex *PIndex) error {
	indexName := pindex.Name() // TODO: bad assumption of 1-to-1 pindex.name to indexName

	bucketName := indexName // TODO: read bucketName out of bleve storage.
	bucketUUID := ""        // TODO: read bucketUUID and vbucket list out of bleve storage.
	feed, err := NewTAPFeed(mgr.server, "default", bucketName, bucketUUID,
		pindex.Stream())
	if err != nil {
		return fmt.Errorf("error: could not prepare TAP stream to server: %s,"+
			" bucketName: %s, indexName: %s, err: %v",
			mgr.server, bucketName, indexName, err)
		// TODO: need a way to collect these errors so REST api
		// can show them to user ("hey, perhaps you deleted a bucket
		// and should delete these related full-text indexes?
		// or the couchbase cluster is just down.");
		// perhaps as specialized clog writer?
		// TODO: cleanup on error?
	}

	if err = feed.Start(); err != nil {
		// TODO: need way to track dead cows (non-beef)
		// TODO: cleanup?
		return fmt.Errorf("error: could not start feed, server: %s, err: %v",
			mgr.server, err)
	}

	if err = mgr.RegisterFeed(feed); err != nil {
		// TODO: cleanup?
		return err
	}

	return nil
}
