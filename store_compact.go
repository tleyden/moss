//  Copyright (c) 2016 Couchbase, Inc.
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the
//  License. You may obtain a copy of the License at
//    http://www.apache.org/licenses/LICENSE-2.0
//  Unless required by applicable law or agreed to in writing,
//  software distributed under the License is distributed on an "AS
//  IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
//  express or implied. See the License for the specific language
//  governing permissions and limitations under the License.

package moss

import (
	"fmt"
	"os"
	"path"
)

type CompactionConcern int // See StorePersistOptions.CompactionConcern.

// CompactionDisable means no compaction.
var CompactionDisable = CompactionConcern(0)

// CompactionAllow means compaction decision is automated and based
// on the configed policy and parameters.
var CompactionAllow = CompactionConcern(1)

// CompactionForce means compaction should be performed immediately.
var CompactionForce = CompactionConcern(2)

// --------------------------------------------------------

func (s *Store) compactMaybe(higher Snapshot, persistOptions StorePersistOptions) (
	bool, error) {
	compactionConcern := persistOptions.CompactionConcern
	if compactionConcern <= 0 {
		return false, nil
	}

	footer, err := s.snapshot()
	if err != nil {
		return false, err
	}

	defer footer.DecRef()

	slocs, ss := footer.SegmentStack()

	defer footer.DecRef()

	if compactionConcern == CompactionAllow {
		totUpperLen := 0
		if ss != nil && len(ss.a) >= 2 {
			for i := 1; i < len(ss.a); i++ {
				totUpperLen += ss.a[i].Len()
			}
		}

		if higher != nil {
			higherSS, ok := higher.(*segmentStack)
			if ok {
				higherStats := higherSS.Stats()
				if higherStats != nil {
					totUpperLen += int(higherStats.CurOps)
				}
			}
		}

		if totUpperLen > 0 {
			var pct float64
			if ss != nil && len(ss.a) > 0 && ss.a[0].Len() > 0 {
				pct = float64(totUpperLen) / float64(ss.a[0].Len())
			}

			if pct >= s.options.CompactionPercentage {
				compactionConcern = CompactionForce
			}
		}
	}

	if compactionConcern != CompactionForce {
		return false, nil
	}

	err = s.compact(footer, higher, persistOptions)
	if err != nil {
		return false, err
	}

	if len(slocs) > 0 {
		mref := slocs[0].mref
		if mref != nil && mref.fref != nil {
			finfo, err := mref.fref.file.Stat()
			if err == nil && len(finfo.Name()) > 0 {
				mref.fref.OnAfterClose(func() {
					os.Remove(path.Join(s.dir, finfo.Name()))
				})
			}
		}
	}

	return true, nil
}

func (s *Store) compact(footer *Footer, higher Snapshot,
	persistOptions StorePersistOptions) error {
	_, ss := footer.SegmentStack()

	defer footer.DecRef()

	if higher != nil {
		ssHigher, ok := higher.(*segmentStack)
		if !ok {
			return fmt.Errorf("store: can only compact higher that's a segmentStack")
		}
		ssHigher.ensureSorted(0, len(ssHigher.a)-1)

		ssOrig := ss

		ss = &segmentStack{
			options: ssOrig.options,
			a:       make([]Segment, 0, len(ssOrig.a)+len(ssHigher.a)),
		}
		ss.a = append(ss.a, ssOrig.a...)
		ss.a = append(ss.a, ssHigher.a...)
	}

	s.m.Lock()
	frefCompact, fileCompact, err := s.startFileLOCKED()
	s.m.Unlock()
	if err != nil {
		return err
	}

	stats := ss.Stats()

	kvsBegPos := pageAlign(int64(STORE_PAGE_SIZE))
	bufBegPos := pageAlign(kvsBegPos + 1 + (int64(8+8) * int64(stats.CurOps)))

	compactionBufferPages := 0
	if s.options != nil {
		compactionBufferPages = s.options.CompactionBufferPages
	}
	if compactionBufferPages <= 0 {
		compactionBufferPages = DefaultStoreOptions.CompactionBufferPages
	}
	compactionBufferSize := STORE_PAGE_SIZE * compactionBufferPages

	compactWriter := &compactWriter{
		kvsWriter: NewBufferedSectionWriter(fileCompact, kvsBegPos, 0, compactionBufferSize),
		bufWriter: NewBufferedSectionWriter(fileCompact, bufBegPos, 0, compactionBufferSize),
	}
	onError := func(err error) error {
		compactWriter.kvsWriter.Stop()
		compactWriter.bufWriter.Stop()
		frefCompact.DecRef()
		return err
	}

	err = ss.mergeInto(0, len(ss.a), compactWriter, nil, false, false, nil)
	if err != nil {
		return onError(err)
	}

	if err = compactWriter.kvsWriter.Flush(); err != nil {
		return onError(err)
	}
	if err = compactWriter.bufWriter.Flush(); err != nil {
		return onError(err)
	}

	if err = compactWriter.kvsWriter.Stop(); err != nil {
		return onError(err)
	}
	if err = compactWriter.bufWriter.Stop(); err != nil {
		return onError(err)
	}

	compactFooter := &Footer{
		refs: 1,
		SegmentLocs: []SegmentLoc{
			SegmentLoc{
				Kind:       BASIC_SEGMENT_KIND,
				KvsOffset:  uint64(kvsBegPos),
				KvsBytes:   uint64(compactWriter.kvsWriter.Offset() - kvsBegPos),
				BufOffset:  uint64(bufBegPos),
				BufBytes:   uint64(compactWriter.bufWriter.Offset() - bufBegPos),
				TotOpsSet:  compactWriter.totOperationSet,
				TotOpsDel:  compactWriter.totOperationDel,
				TotKeyByte: compactWriter.totKeyByte,
				TotValByte: compactWriter.totValByte,
			},
		},
	}

	sync := !persistOptions.NoSync
	if !sync {
		sync = s.options != nil && s.options.CompactionSync
	}

	err = s.persistFooter(fileCompact, compactFooter, sync)
	if err != nil {
		return onError(err)
	}

	footerReady, err := ReadFooter(s.options, fileCompact)
	if err != nil {
		return onError(err)
	}

	s.m.Lock()
	footerPrev := s.footer
	s.footer = footerReady // Owns the frefCompact ref-count.
	s.totCompactions++
	s.m.Unlock()

	if footerPrev != nil {
		footerPrev.DecRef()
	}

	return nil
}

type compactWriter struct {
	file      File
	kvsWriter *bufferedSectionWriter
	bufWriter *bufferedSectionWriter

	totOperationSet   uint64
	totOperationDel   uint64
	totOperationMerge uint64
	totKeyByte        uint64
	totValByte        uint64
}

func (cw *compactWriter) Mutate(operation uint64, key, val []byte) error {
	keyStart := cw.bufWriter.Written()

	_, err := cw.bufWriter.Write(key)
	if err != nil {
		return err
	}

	_, err = cw.bufWriter.Write(val)
	if err != nil {
		return err
	}

	keyLen := len(key)
	valLen := len(val)

	opKlVl := encodeOpKeyLenValLen(operation, keyLen, valLen)

	if keyLen <= 0 && valLen <= 0 {
		keyStart = 0
	}

	pair := []uint64{opKlVl, uint64(keyStart)}
	kvsBuf, err := Uint64SliceToByteSlice(pair)
	if err != nil {
		return err
	}

	_, err = cw.kvsWriter.Write(kvsBuf)
	if err != nil {
		return err
	}

	switch operation {
	case OperationSet:
		cw.totOperationSet++
	case OperationDel:
		cw.totOperationDel++
	case OperationMerge:
		cw.totOperationMerge++
	default:
	}

	cw.totKeyByte += uint64(keyLen)
	cw.totValByte += uint64(valLen)

	return nil
}
