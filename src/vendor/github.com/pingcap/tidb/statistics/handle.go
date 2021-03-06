// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package statistics

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/juju/errors"
	"github.com/pingcap/tidb/ddl/util"
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/model"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/util/sqlexec"
	log "github.com/sirupsen/logrus"
)

type statsCache map[int64]*Table

// Handle can update stats info periodically.
type Handle struct {
	ctx sessionctx.Context
	// LastVersion is the latest update version before last lease. Exported for test.
	LastVersion uint64
	// PrevLastVersion is the latest update version before two lease. Exported for test.
	// We need this because for two tables, the smaller version may write later than the one with larger version.
	// We can read the version with lastTwoVersion if the diff between commit time and version is less than one lease.
	// PrevLastVersion will be assigned by LastVersion every time Update is called.
	PrevLastVersion uint64
	statsCache      atomic.Value
	// ddlEventCh is a channel to notify a ddl operation has happened.
	// It is sent only by owner or the drop stats executor, and read by stats handle.
	ddlEventCh chan *util.Event
	// analyzeResultCh is a channel to notify an analyze index or column operation has ended.
	// We need this to avoid updating the stats simultaneously.
	analyzeResultCh chan *AnalyzeResult
	// listHead contains all the stats collector required by session.
	listHead *SessionStatsCollector
	// globalMap contains all the delta map from collectors when we dump them to KV.
	globalMap tableDeltaMap
	// rateMap contains the error rate delta from feedback.
	rateMap errorRateDeltaMap
	// feedback is used to store query feedback info.
	feedback []*QueryFeedback

	Lease time.Duration
	// loadMetaCh is a channel to notify a load stats operation has done.
	loadMetaCh chan *LoadMeta
}

// Clear the statsCache, only for test.
func (h *Handle) Clear() {
	h.statsCache.Store(statsCache{})
	h.LastVersion = 0
	h.PrevLastVersion = 0
	for len(h.ddlEventCh) > 0 {
		<-h.ddlEventCh
	}
	for len(h.analyzeResultCh) > 0 {
		<-h.analyzeResultCh
	}
	h.ctx.GetSessionVars().MaxChunkSize = 1
	h.listHead = &SessionStatsCollector{mapper: make(tableDeltaMap), rateMap: make(errorRateDeltaMap)}
	h.globalMap = make(tableDeltaMap)
	h.rateMap = make(errorRateDeltaMap)
}

// MaxQueryFeedbackCount is the max number of feedback that cache in memory.
var MaxQueryFeedbackCount = 1 << 10

// NewHandle creates a Handle for update stats.
func NewHandle(ctx sessionctx.Context, lease time.Duration) *Handle {
	handle := &Handle{
		ctx:             ctx,
		ddlEventCh:      make(chan *util.Event, 100),
		analyzeResultCh: make(chan *AnalyzeResult, 100),
		listHead:        &SessionStatsCollector{mapper: make(tableDeltaMap), rateMap: make(errorRateDeltaMap)},
		globalMap:       make(tableDeltaMap),
		Lease:           lease,
		feedback:        make([]*QueryFeedback, 0, MaxQueryFeedbackCount),
		loadMetaCh:      make(chan *LoadMeta, 1),
		rateMap:         make(errorRateDeltaMap),
	}
	handle.statsCache.Store(statsCache{})
	return handle
}

// GetQueryFeedback gets the query feedback. It is only use in test.
func (h *Handle) GetQueryFeedback() []*QueryFeedback {
	defer func() {
		h.feedback = h.feedback[:0]
	}()
	return h.feedback
}

// AnalyzeResultCh returns analyze result channel in handle.
func (h *Handle) AnalyzeResultCh() chan *AnalyzeResult {
	return h.analyzeResultCh
}

// Update reads stats meta from store and updates the stats map.
func (h *Handle) Update(is infoschema.InfoSchema) error {
	sql := fmt.Sprintf("SELECT version, table_id, modify_count, count from mysql.stats_meta where version > %d order by version", h.PrevLastVersion)
	rows, _, err := h.ctx.(sqlexec.RestrictedSQLExecutor).ExecRestrictedSQL(h.ctx, sql)
	if err != nil {
		return errors.Trace(err)
	}
	h.PrevLastVersion = h.LastVersion
	tables := make([]*Table, 0, len(rows))
	deletedTableIDs := make([]int64, 0, len(rows))
	for _, row := range rows {
		version := row.GetUint64(0)
		tableID := row.GetInt64(1)
		modifyCount := row.GetInt64(2)
		count := row.GetInt64(3)
		h.LastVersion = version
		table, ok := is.TableByID(tableID)
		if !ok {
			log.Debugf("Unknown table ID %d in stats meta table, maybe it has been dropped", tableID)
			deletedTableIDs = append(deletedTableIDs, tableID)
			continue
		}
		tableInfo := table.Meta()
		tbl, err := h.tableStatsFromStorage(tableInfo, false)
		// Error is not nil may mean that there are some ddl changes on this table, we will not update it.
		if err != nil {
			log.Debugf("Error occurred when read table stats for table %s. The error message is %s.", tableInfo.Name.O, errors.ErrorStack(err))
			continue
		}
		if tbl == nil {
			deletedTableIDs = append(deletedTableIDs, tableID)
			continue
		}
		tbl.Version = version
		tbl.Count = count
		tbl.ModifyCount = modifyCount
		tables = append(tables, tbl)
	}
	h.UpdateTableStats(tables, deletedTableIDs)
	return nil
}

// GetTableStats retrieves the statistics table from cache, and the cache will be updated by a goroutine.
func (h *Handle) GetTableStats(tblInfo *model.TableInfo) *Table {
	tbl, ok := h.statsCache.Load().(statsCache)[tblInfo.ID]
	if !ok {
		tbl = PseudoTable(tblInfo)
		h.UpdateTableStats([]*Table{tbl}, nil)
		return tbl
	}
	return tbl
}

func (h *Handle) copyFromOldCache() statsCache {
	newCache := statsCache{}
	oldCache := h.statsCache.Load().(statsCache)
	for k, v := range oldCache {
		newCache[k] = v
	}
	return newCache
}

// UpdateTableStats updates the statistics table cache using copy on write.
func (h *Handle) UpdateTableStats(tables []*Table, deletedIDs []int64) {
	newCache := h.copyFromOldCache()
	for _, tbl := range tables {
		id := tbl.TableID
		newCache[id] = tbl
	}
	for _, id := range deletedIDs {
		delete(newCache, id)
	}
	h.statsCache.Store(newCache)
}

// LoadNeededHistograms will load histograms for those needed columns.
func (h *Handle) LoadNeededHistograms() error {
	cols := histogramNeededColumns.allCols()
	for _, col := range cols {
		tbl, ok := h.statsCache.Load().(statsCache)[col.tableID]
		if !ok {
			continue
		}
		tbl = tbl.copy()
		c, ok := tbl.Columns[col.columnID]
		if !ok || c.Len() > 0 {
			histogramNeededColumns.delete(col)
			continue
		}
		hg, err := histogramFromStorage(h.ctx, col.tableID, c.ID, &c.Info.FieldType, c.NDV, 0, c.LastUpdateVersion, c.NullCount, c.TotColSize)
		if err != nil {
			return errors.Trace(err)
		}
		cms, err := h.cmSketchFromStorage(col.tableID, 0, col.columnID)
		if err != nil {
			return errors.Trace(err)
		}
		tbl.Columns[c.ID] = &Column{Histogram: *hg, Info: c.Info, CMSketch: cms, Count: int64(hg.totalRowCount())}
		h.UpdateTableStats([]*Table{tbl}, nil)
		histogramNeededColumns.delete(col)
	}
	return nil
}

// LoadMetaCh returns loaded statistic meta channel in handle.
func (h *Handle) LoadMetaCh() chan *LoadMeta {
	return h.loadMetaCh
}

// FlushStats flushes the cached stats update into store.
func (h *Handle) FlushStats() {
	for len(h.ddlEventCh) > 0 {
		e := <-h.ddlEventCh
		if err := h.HandleDDLEvent(e); err != nil {
			log.Debug("[stats] handle ddl event fail: ", errors.ErrorStack(err))
		}
	}
	if err := h.DumpStatsDeltaToKV(DumpAll); err != nil {
		log.Debug("[stats] dump stats delta fail: ", errors.ErrorStack(err))
	}
	for len(h.analyzeResultCh) > 0 {
		t := <-h.analyzeResultCh
		for i, hg := range t.Hist {
			if err := SaveStatsToStorage(h.ctx, t.TableID, t.Count, t.IsIndex, hg, t.Cms[i], 1); err != nil {
				log.Debug("[stats] save histogram to storage fail: ", errors.ErrorStack(err))
			}
		}
	}
	if err := h.DumpStatsFeedbackToKV(); err != nil {
		log.Debug("[stats] dump stats feedback fail: ", errors.ErrorStack(err))
	}
}
