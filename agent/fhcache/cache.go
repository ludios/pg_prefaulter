// Copyright © 2017 Joyent, Inc.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package fhcache

import (
	"context"
	"sync"
	"time"

	"github.com/bluele/gcache"
	"github.com/bschofield/pg_prefaulter/agent/structs"
	"github.com/bschofield/pg_prefaulter/config"
	"github.com/bschofield/pg_prefaulter/lib"
	"github.com/bschofield/pg_prefaulter/pg"
	"github.com/pkg/errors"
	log "github.com/rs/zerolog/log"
)

var (
	// Add a few counters to verify the system is behaving as expected.
	openLock    sync.RWMutex
	openFDCount uint64

	closeLock    sync.RWMutex
	closeFDCount uint64
)

// FileHandleCache is a file descriptor cache to prevent re-open(2)'ing files
// continually.
type FileHandleCache struct {
	ctx context.Context
	cfg *config.FHCacheConfig

	purgeLock sync.Mutex
	c         gcache.Cache
}

// New creates a new FileHandleCache
func New(ctx context.Context, cfg *config.Config) (*FileHandleCache, error) {
	fhc := &FileHandleCache{
		ctx: ctx,
		cfg: &cfg.FHCacheConfig,
	}

	fhc.c = gcache.New(int(fhc.cfg.Size)).
		ARC().
		LoaderExpireFunc(func(fhCacheKeyRaw interface{}) (interface{}, *time.Duration, error) {
			fhCacheKey, ok := fhCacheKeyRaw.(_Key)
			if !ok {
				log.Panic().Msgf("unable to type assert key in file handle cache: %T %+v", fhCacheKeyRaw, fhCacheKeyRaw)
			}

			fhCacheVal := _Value{
				_Key: fhCacheKey,

				lock: &sync.RWMutex{},
				f:    nil,
			}
			return &fhCacheVal, &fhc.cfg.TTL, nil
		}).
		EvictedFunc(func(fhCacheKeyRaw, fhCacheValueRaw interface{}) {
			fhCacheValue, ok := fhCacheValueRaw.(*_Value)
			if !ok {
				log.Panic().Msgf("bad, evicting something not a file handle: %+v", fhCacheValue)
			}
			defer fhCacheValue.close()
		}).
		PurgeVisitorFunc(func(fhCacheKeyRaw, fhCacheValueRaw interface{}) {
			fhCacheValue, ok := fhCacheValueRaw.(*_Value)
			if !ok {
				log.Panic().Msgf("bad, purging something not a file handle: %+v", fhCacheValue)
			}
			defer fhCacheValue.close()
		}).
		Build()

	go lib.LogCacheStats(fhc.ctx, fhc.c, "filehandle-stats")

	log.Debug().
		Uint("rlimit-nofile", fhc.cfg.MaxOpenFiles).
		Uint("filehandle-cache-size", fhc.cfg.Size).
		Dur("filehandle-cache-ttl", fhc.cfg.TTL).
		Msg("filehandle cache initialized")
	return fhc, nil
}

var (
	numConcurrentReadLock sync.Mutex
	numConcurrentReads    int64
)

// PrefaultPage uses the given IOCacheKey to:
//
// 1) open a relation's segment, if necessary
// 2) pre-fault a given heap page into the OS's filesystem cache using pread(2)
func (fhc *FileHandleCache) PrefaultPage(ioCacheKey structs.IOCacheKey) error {
	fhcValue, err := fhc.getLocked(ioCacheKey)
	if err != nil {
		return errors.Wrap(err, "unable to obtain file handle")
	}
	defer fhcValue.lock.RUnlock()

	numConcurrentReadLock.Lock()
	numConcurrentReads++
	numConcurrentReadLock.Unlock()
	defer func() {
		numConcurrentReadLock.Lock()
		numConcurrentReads--
		numConcurrentReadLock.Unlock()
	}()

	pageNum := pg.HeapSegmentPageNum(ioCacheKey.Block)
	Fadvise(int(fhcValue.f.Fd()), int64(uint64(pageNum)*uint64(pg.HeapPageSize)), int64(pg.HeapPageSize))

	return nil
}

// getLocked returns a read-locked _Value.  Upon success, callers MUST call
// RUnlock().  On error _Value will return nil and the caller will not have to
// release any outstanding locks.
func (fhc *FileHandleCache) getLocked(ioReq structs.IOCacheKey) (*_Value, error) {
	key := _NewKey(ioReq)

	valueRaw, err := fhc.c.Get(key)
	if err != nil {
		return nil, errors.Wrap(err, "unable to open cache file")
	}

	value, ok := valueRaw.(*_Value)
	if !ok {
		log.Panic().Msgf("unable to type assert file handle in IO Cache: %+v", valueRaw)
	}

	// Loop until we exit this with an error or the read lock held.
	for {
		value.lock.RLock()
		if value.f != nil {
			return value, nil
		}

		value.lock.RUnlock()
		value.lock.Lock()
		// Revalidate lock predicate with exclusive lock held
		if value.f != nil {
			value.lock.Unlock()

			// loop to acquire the readlock
			continue
		}

		f, err := value.open(fhc.cfg.PGDataPath)
		if err != nil {
			log.Warn().Err(err).Msgf("unable to open relation file: %+v", key)
			value.lock.Unlock()
			return nil, errors.Wrapf(err, "unable to re-open file: %+v", value._Key)
		}
		value.f = f
		value.lock.Unlock()
	}
}

// Purge purges the FileHandleCache of its cache (and all downstream caches)
func (fhc *FileHandleCache) Purge() {
	fhc.purgeLock.Lock()
	defer fhc.purgeLock.Unlock()

	fhc.c.Purge()

	openLock.RLock()
	defer openLock.RUnlock()
	closeLock.RLock()
	defer closeLock.RUnlock()
	if openFDCount != closeFDCount {
		// Open vs close accountancy errors are considered fatal
		log.Panic().
			Uint64("close-count", closeFDCount).Uint64("open-count", openFDCount).
			Msgf("bad, open vs close count not the same after purge")
	}
}
