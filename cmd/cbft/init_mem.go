// Copyright 2018-Present Couchbase, Inc.
//
// Use of this software is governed by the Business Source License included
// in the file licenses/BSL-Couchbase.txt.  As of the Change Date specified
// in that file, in accordance with the Business Source License, use of this
// software will be governed by the Apache License, Version 2.0, included in
// the file licenses/APL2.txt.

package main

import (
	"fmt"
	"strconv"
	"time"

	"github.com/couchbase/cbft"
	log "github.com/couchbase/clog"
)

var (
	ftsHerder              *appHerder
	SIGAR_CGROUP_SUPPORTED uint8 = 1
)

func initMemOptions(options map[string]string) (err error) {
	if options == nil {
		return nil
	}

	var memQuota uint64
	v, exists := options["ftsMemoryQuota"] // In bytes.
	if exists {
		fmq, err2 := strconv.Atoi(v)
		if err2 != nil {
			return fmt.Errorf("init_mem:"+
				" parsing ftsMemoryQuota: %q, err: %v", v, err2)
		}
		memQuota = uint64(fmq)
	}
	memoryLimit, err := getTotalMemStats()
	if memoryLimit < memQuota {
		memQuota = memoryLimit
	}
	// run the above check periodically to allow for memory limit updates?

	var memCheckInterval time.Duration
	v, exists = options["memCheckInterval"] // In Go duration format.
	if exists {
		var err2 error
		memCheckInterval, err2 = time.ParseDuration(v)
		if err2 != nil {
			return fmt.Errorf("init_mem:"+
				" parsing memCheckInterval: %q, err: %v", v, err2)
		}
	}

	var goverseerKickCh chan struct{}
	if memCheckInterval > 0 && memQuota > 0 {
		g := NewGoverseer(memCheckInterval, memQuota)
		go g.Run()
		goverseerKickCh = g.kickCh
	}

	ftsApplicationFraction, err := parseFTSMemApplicationFraction(options)
	if err != nil {
		return err
	}
	ftsIndexingFraction, err := parseFTSMemIndexingFraction(options)
	if err != nil {
		return err
	}
	ftsQueryingFraction, err := parseFTSMemQueryingFraction(options)
	if err != nil {
		return err
	}

	ftsHerder = newAppHerder(memQuota, ftsApplicationFraction,
		ftsIndexingFraction, ftsQueryingFraction, goverseerKickCh)

	cbft.RegistryQueryEventCallback = ftsHerder.queryHerderOnEvent()

	cbft.OnMemoryUsedDropped = func(curMemoryUsed, prevMemoryUsed uint64) {
		ftsHerder.onMemoryUsedDropped(curMemoryUsed, prevMemoryUsed)
	}

	return nil
}

// Returns total memory based on cgroup limits, if possible.
func getTotalMemStats() (uint64, error) {
	stats, err := NewSystemStats()
	if err != nil {
		log.Printf("error getting new stats: %+v", err)
		return 0, err
	}
	memTotal, err := stats.SystemTotalMem()
	if err != nil {
		log.Printf("error getting total mem: %+v", err)
		return 0, err
	}

	cgroupInfo := stats.GetControlGroupInfo()
	if cgroupInfo.Supported == SIGAR_CGROUP_SUPPORTED {
		log.Printf("init_mem: cgroups are supported")
		cGroupTotal := cgroupInfo.MemoryMax
		// cGroupTotal is with-in valid system limits
		if cGroupTotal > 0 && cGroupTotal <= memTotal {
			return cGroupTotal, nil
		}
	}

	return memTotal, nil
}

// defaultFTSApplicationFraction is default ratio for the
// memApplicationFraction of the mem quota (default 100%)
var defaultFTSApplicationFraction = 1.0

func parseFTSMemApplicationFraction(options map[string]string) (float64,
	error) {
	return parseFraction("memApplicationFraction", defaultFTSApplicationFraction,
		options)
}

// defaultFTSMemIndexingFraction is the ratio of the application quota
// to use for indexing (default 75%)
var defaultFTSMemIndexingFraction = 0.75

func parseFTSMemIndexingFraction(options map[string]string) (float64, error) {
	return parseFraction("memIndexingFraction", defaultFTSMemIndexingFraction,
		options)
}

// defaultFTSMemQueryingFraction is the ratio of the application quota
// to use for querying (default 90%)
var defaultFTSMemQueryingFraction = 0.90

func parseFTSMemQueryingFraction(options map[string]string) (float64, error) {
	return parseFraction("memQueryingFraction", defaultFTSMemQueryingFraction,
		options)
}

func parseFraction(name string, defaultValue float64,
	options map[string]string) (float64, error) {
	v, exists := options[name]
	if exists {
		p, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, fmt.Errorf("init_mem: %s, err: %v", name, err)
		}
		return p, nil
	}
	return defaultValue, nil
}
