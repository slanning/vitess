/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package vreplication

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"golang.org/x/net/context"

	"vitess.io/vitess/go/mysql"
	"vitess.io/vitess/go/sqltypes"
	"vitess.io/vitess/go/vt/binlog/binlogplayer"
	"vitess.io/vitess/go/vt/grpcclient"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/mysqlctl"
	"vitess.io/vitess/go/vt/vttablet/tabletconn"

	binlogdatapb "vitess.io/vitess/go/vt/proto/binlogdata"
	querypb "vitess.io/vitess/go/vt/proto/query"
	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
)

var (
	// idleTimeout is set to slightly above 1s, compared to heartbeatTime
	// set by VStreamer at slightly below 1s. This minimizes conflicts
	// between the two timeouts.
	idleTimeout      = 1100 * time.Millisecond
	dbLockRetryDelay = 1 * time.Second
	relayLogMaxSize  = 10000
	relayLogMaxItems = 1000

	// CreateCopyState is the list of statements to execute for creating
	// the _vt.copy_state table
	CreateCopyState = []string{
		`create table if not exists _vt.copy_state (
  vrepl_id int,
  table_name varbinary(128),
  last_pk varbinary(2000),
  primary key (vrepl_id, table_name))`}
)

type vplayer struct {
	id           uint32
	source       *binlogdatapb.BinlogSource
	sourceTablet *topodatapb.Tablet
	stats        *binlogplayer.Stats
	dbClient     *vdbClient
	// mysqld is used to fetch the local schema.
	mysqld mysqlctl.MysqlDaemon

	pos mysql.Position
	// unsavedGTID when we receive a GTID event and reset
	// if it gets saved. If Fetch returns on idleTimeout,
	// we save the last unsavedGTID.
	unsavedGTID *binlogdatapb.VEvent
	// timeLastSaved is set every time a GTID is saved.
	timeLastSaved time.Time
	// lastTimestampNs is the last timestamp seen so far.
	lastTimestampNs int64
	// timeOffsetNs keeps track of the clock difference with respect to source tablet.
	timeOffsetNs int64
	stopPos      mysql.Position

	tableKeys map[string][]string

	// pplan is built based on the source Filter at the beginning.
	pplan *PlayerPlan
	// tplans[table] is built for each table based on pplan and schema info
	// about the table.
	tplans map[string]*TablePlan
}

func newVPlayer(id uint32, source *binlogdatapb.BinlogSource, sourceTablet *topodatapb.Tablet, stats *binlogplayer.Stats, dbClient binlogplayer.DBClient, mysqld mysqlctl.MysqlDaemon) *vplayer {
	return &vplayer{
		id:            id,
		source:        source,
		sourceTablet:  sourceTablet,
		stats:         stats,
		dbClient:      newVDBClient(dbClient, stats),
		mysqld:        mysqld,
		timeLastSaved: time.Now(),
		tplans:        make(map[string]*TablePlan),
	}
}

func (vp *vplayer) Play(ctx context.Context) error {
	tableKeys, err := vp.buildTableKeys()
	if err != nil {
		return err
	}
	vp.tableKeys = tableKeys
	plan, err := buildPlayerPlan(vp.source.Filter, tableKeys)
	if err != nil {
		return err
	}
	vp.pplan = plan

	settings, err := binlogplayer.ReadVRSettings(vp.dbClient, vp.id)
	if err != nil {
		return fmt.Errorf("error reading VReplication settings: %v", err)
	}
	if settings.State == binlogplayer.VReplicationInit {
		if err := vp.initTablesForCopy(ctx); err != nil {
			return err
		}
		return nil
	}
	return vp.replicate(ctx, settings)
}

func (vp *vplayer) initTablesForCopy(ctx context.Context) error {
	defer vp.dbClient.Rollback()

	// Check if table exists.
	if _, err := vp.dbClient.ExecuteFetch("select * from _vt.copy_state limit 1", 10); err != nil {
		// If it's a not found error, create it.
		merr, isSQLErr := err.(*mysql.SQLError)
		if !isSQLErr || !(merr.Num == mysql.ERNoSuchTable || merr.Num == mysql.ERBadDb) {
			return err
		}
		for _, query := range CreateCopyState {
			if _, merr := vp.dbClient.ExecuteFetch(query, 0); merr != nil {
				log.Errorf("Failed to ensure _vt.copy_state table exists: %v", merr)
				return err
			}
		}
	}
	if err := vp.dbClient.Begin(); err != nil {
		return err
	}
	// Insert the table list only if at least one table matches.
	if len(vp.pplan.TargetTables) != 0 {
		var buf strings.Builder
		buf.WriteString("insert into _vt.copy_state(vrepl_id, table_name) values ")
		prefix := ""
		for name := range vp.pplan.TargetTables {
			fmt.Fprintf(&buf, "%s(%d, %s)", prefix, vp.id, encodeString(name))
			prefix = ", "
		}
		if _, err := vp.dbClient.ExecuteFetch(buf.String(), 1); err != nil {
			return err
		}
	}
	if err := vp.setState(binlogplayer.VReplicationCopying, ""); err != nil {
		return err
	}
	return vp.dbClient.Commit()
}

func (vp *vplayer) replicate(ctx context.Context, settings binlogplayer.VRSettings) error {
	if err := vp.setState(binlogplayer.BlpRunning, ""); err != nil {
		return err
	}
	if err := vp.play(ctx, settings); err != nil {
		msg := err.Error()
		vp.stats.History.Add(&binlogplayer.StatsHistoryRecord{
			Time:    time.Now(),
			Message: msg,
		})
		if err := vp.setState(binlogplayer.BlpError, msg); err != nil {
			log.Errorf("Failed to set error state: %v", err)
		}
		return err
	}
	return nil
}

func (vp *vplayer) play(ctx context.Context, settings binlogplayer.VRSettings) error {
	var err error
	vp.pos, err = mysql.DecodePosition(settings.StartPos)
	if err != nil {
		return vp.setState(binlogplayer.BlpStopped, fmt.Sprintf("error decoding start position %v: %v", settings.StartPos, err))
	}
	if settings.StopPos != "" {
		vp.stopPos, err = mysql.DecodePosition(settings.StopPos)
		if err != nil {
			return vp.setState(binlogplayer.BlpStopped, fmt.Sprintf("error decoding stop position %v: %v", settings.StopPos, err))
		}
	}
	if !vp.stopPos.IsZero() {
		if vp.pos.AtLeast(vp.stopPos) {
			return vp.setState(binlogplayer.BlpStopped, fmt.Sprintf("Stop position %v already reached: %v", vp.pos, vp.stopPos))
		}
	}
	log.Infof("Starting VReplication player id: %v, startPos: %v, stop: %v, source: %v, filter: %v", vp.id, settings.StartPos, vp.stopPos, vp.sourceTablet, vp.source)

	vsClient, err := tabletconn.GetDialer()(vp.sourceTablet, grpcclient.FailFast(false))
	if err != nil {
		return fmt.Errorf("error dialing tablet: %v", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	relay := newRelayLog(ctx, relayLogMaxItems, relayLogMaxSize)

	target := &querypb.Target{
		Keyspace:   vp.sourceTablet.Keyspace,
		Shard:      vp.sourceTablet.Shard,
		TabletType: vp.sourceTablet.Type,
	}
	log.Infof("Sending vstream command: %v", vp.pplan.VStreamFilter)
	streamErr := make(chan error, 1)
	go func() {
		streamErr <- vsClient.VStream(ctx, target, settings.StartPos, vp.pplan.VStreamFilter, func(events []*binlogdatapb.VEvent) error {
			return relay.Send(events)
		})
	}()

	applyErr := make(chan error, 1)
	go func() {
		applyErr <- vp.applyEvents(ctx, relay)
	}()

	select {
	case err := <-applyErr:
		defer func() {
			// cancel and wait for the other thread to finish.
			cancel()
			<-streamErr
		}()
		// If the apply thread ends with io.EOF, it means either the Engine
		// is shutting down and canceled the context, or stop position was reached.
		// If so, we return nil which will cause the controller to not retry.
		if err == io.EOF {
			return nil
		}
		return err
	case err := <-streamErr:
		defer func() {
			// cancel and wait for the other thread to finish.
			cancel()
			<-applyErr
		}()
		// If context is done, don't return an error.
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		// If the stream ends normally we have to return an error indicating
		// that the controller has to retry a different vttablet.
		if err == nil || err == io.EOF {
			return errors.New("vstream ended")
		}
		return err
	}
}

func (vp *vplayer) buildTableKeys() (map[string][]string, error) {
	schema, err := vp.mysqld.GetSchema(vp.dbClient.DBName(), []string{"/.*/"}, nil, false)
	if err != nil {
		return nil, err
	}
	tableKeys := make(map[string][]string)
	for _, td := range schema.TableDefinitions {
		if len(td.PrimaryKeyColumns) != 0 {
			tableKeys[td.Name] = td.PrimaryKeyColumns
		} else {
			tableKeys[td.Name] = td.Columns
		}
	}
	return tableKeys, nil
}

func (vp *vplayer) applyEvents(ctx context.Context, relay *relayLog) error {
	for {
		items, err := relay.Fetch()
		if err != nil {
			return err
		}
		// No events were received. This likely means that there's a network partition.
		// So, we should assume we're falling behind.
		if len(items) == 0 {
			behind := time.Now().UnixNano() - vp.lastTimestampNs - vp.timeOffsetNs
			vp.stats.SecondsBehindMaster.Set(behind / 1e9)
		}
		// Filtered replication often ends up receiving a large number of empty transactions.
		// This is required because the player needs to know the latest position of the source.
		// This allows it to stop at that position if requested.
		// This position also needs to be saved, which will allow an external request
		// to check if a required position has been reached.
		// However, this leads to a large number of empty commits which not only slow
		// down the replay, but also generate binlog bloat on the target.
		// In order to mitigate this problem, empty transactions are saved at most
		// once every idleTimeout.
		// This covers two situations:
		// 1. Fetch was idle for idleTimeout.
		// 2. We've been receiving empty events for longer than idleTimeout.
		// In both cases, now > timeLastSaved. If so, any unsaved GTID should be saved.
		if time.Since(vp.timeLastSaved) >= idleTimeout && vp.unsavedGTID != nil {
			// Although unlikely, we should not save if a transaction is still open.
			// This can happen if a large transaction is split as multiple events.
			if !vp.dbClient.InTransaction {
				if err := vp.updatePos(vp.unsavedGTID.Timestamp); err != nil {
					return err
				}
			}
		}
		for i, events := range items {
			for j, event := range events {
				if event.Timestamp != 0 {
					vp.lastTimestampNs = event.Timestamp * 1e9
					vp.timeOffsetNs = time.Now().UnixNano() - event.CurrentTime
					vp.stats.SecondsBehindMaster.Set(event.CurrentTime/1e9 - event.Timestamp)
				}
				mustSave := false
				switch event.Type {
				case binlogdatapb.VEventType_COMMIT:
					if vp.pos.Equal(vp.stopPos) {
						mustSave = true
						break
					}
					if hasAnotherCommit(items, i, j+1) {
						continue
					}
				}
				if err := vp.applyEvent(ctx, event, mustSave); err != nil {
					return err
				}
			}
		}
	}
}

func hasAnotherCommit(items [][]*binlogdatapb.VEvent, i, j int) bool {
	for i < len(items) {
		for j < len(items[i]) {
			// We ignore GTID, BEGIN, FIELD and ROW.
			switch items[i][j].Type {
			case binlogdatapb.VEventType_COMMIT:
				return true
			case binlogdatapb.VEventType_DDL:
				return false
			}
			j++
		}
		j = 0
		i++
	}
	return false
}

func (vp *vplayer) applyEvent(ctx context.Context, event *binlogdatapb.VEvent, mustSave bool) error {
	switch event.Type {
	case binlogdatapb.VEventType_GTID:
		pos, err := mysql.DecodePosition(event.Gtid)
		if err != nil {
			return err
		}
		vp.pos = pos
		vp.unsavedGTID = event
		if vp.stopPos.IsZero() {
			return nil
		}
		if !vp.pos.Equal(vp.stopPos) && vp.pos.AtLeast(vp.stopPos) {
			// Code is unreachable, but bad data can cause this to happen.
			if err := vp.setState(binlogplayer.BlpStopped, fmt.Sprintf("next event position %v exceeds stop pos %v, exiting without applying", vp.pos, vp.stopPos)); err != nil {
				return err
			}
			return io.EOF
		}
	case binlogdatapb.VEventType_BEGIN:
		// No-op: begin is called as needed.
	case binlogdatapb.VEventType_COMMIT:
		if mustSave {
			if err := vp.dbClient.Begin(); err != nil {
				return err
			}
		}

		if !vp.dbClient.InTransaction {
			return nil
		}
		if err := vp.updatePos(event.Timestamp); err != nil {
			return err
		}
		posReached := !vp.stopPos.IsZero() && vp.pos.Equal(vp.stopPos)
		if posReached {
			if err := vp.setState(binlogplayer.BlpStopped, fmt.Sprintf("Stopped at position %v", vp.stopPos)); err != nil {
				return err
			}
		}
		if err := vp.dbClient.Commit(); err != nil {
			return err
		}
		if posReached {
			return io.EOF
		}
	case binlogdatapb.VEventType_FIELD:
		if err := vp.dbClient.Begin(); err != nil {
			return err
		}
		if err := vp.updatePlan(event.FieldEvent); err != nil {
			return err
		}
	case binlogdatapb.VEventType_ROW:
		if err := vp.dbClient.Begin(); err != nil {
			return err
		}
		if err := vp.applyRowEvent(ctx, event.RowEvent); err != nil {
			return err
		}
	case binlogdatapb.VEventType_DDL:
		if vp.dbClient.InTransaction {
			return fmt.Errorf("unexpected state: DDL encountered in the middle of a transaction: %v", event.Ddl)
		}
		switch vp.source.OnDdl {
		case binlogdatapb.OnDDLAction_IGNORE:
			// no-op
		case binlogdatapb.OnDDLAction_STOP:
			if err := vp.dbClient.Begin(); err != nil {
				return err
			}
			if err := vp.updatePos(event.Timestamp); err != nil {
				return err
			}
			if err := vp.setState(binlogplayer.BlpStopped, fmt.Sprintf("Stopped at DDL %s", event.Ddl)); err != nil {
				return err
			}
			if err := vp.dbClient.Commit(); err != nil {
				return err
			}
			return io.EOF
		case binlogdatapb.OnDDLAction_EXEC:
			if err := vp.exec(ctx, event.Ddl); err != nil {
				return err
			}
			if err := vp.updatePos(event.Timestamp); err != nil {
				return err
			}
		case binlogdatapb.OnDDLAction_EXEC_IGNORE:
			if err := vp.exec(ctx, event.Ddl); err != nil {
				log.Infof("Ignoring error: %v for DDL: %s", err, event.Ddl)
			}
			if err := vp.updatePos(event.Timestamp); err != nil {
				return err
			}
		}
	case binlogdatapb.VEventType_HEARTBEAT:
		// No-op: heartbeat timings are calculated in outer loop.
	}
	return nil
}

func (vp *vplayer) setState(state, message string) error {
	return binlogplayer.SetVReplicationState(vp.dbClient, vp.id, state, message)
}

func (vp *vplayer) updatePlan(fieldEvent *binlogdatapb.FieldEvent) error {
	prelim := vp.pplan.TablePlans[fieldEvent.TableName]
	if prelim == nil {
		prelim = &TablePlan{
			Name: fieldEvent.TableName,
		}
	}
	if prelim.Insert != nil {
		prelim.Fields = fieldEvent.Fields
		vp.tplans[fieldEvent.TableName] = prelim
		return nil
	}
	tplan, err := buildTablePlanFromFields(prelim.Name, fieldEvent.Fields, vp.tableKeys)
	if err != nil {
		return err
	}
	tplan.Fields = fieldEvent.Fields
	vp.tplans[fieldEvent.TableName] = tplan
	return nil
}

func (vp *vplayer) applyRowEvent(ctx context.Context, rowEvent *binlogdatapb.RowEvent) error {
	tplan := vp.tplans[rowEvent.TableName]
	if tplan == nil {
		return fmt.Errorf("unexpected event on table %s", rowEvent.TableName)
	}
	for _, change := range rowEvent.RowChanges {
		queries, err := tplan.generateStatements(change)
		if err != nil {
			return err
		}
		for _, query := range queries {
			if err := vp.exec(ctx, query); err != nil {
				return err
			}
		}
	}
	return nil
}

func (vp *vplayer) updatePos(ts int64) error {
	updatePos := binlogplayer.GenerateUpdatePos(vp.id, vp.pos, time.Now().Unix(), ts)
	if _, err := vp.dbClient.ExecuteFetch(updatePos, 0); err != nil {
		vp.dbClient.Rollback()
		return fmt.Errorf("error %v updating position", err)
	}
	vp.unsavedGTID = nil
	vp.timeLastSaved = time.Now()
	vp.stats.SetLastPosition(vp.pos)
	return nil
}

func (vp *vplayer) exec(ctx context.Context, sql string) error {
	vp.stats.Timings.Record("query", time.Now())
	_, err := vp.dbClient.ExecuteFetch(sql, 0)
	for err != nil {
		if sqlErr, ok := err.(*mysql.SQLError); ok && sqlErr.Number() == mysql.ERLockDeadlock || sqlErr.Number() == mysql.ERLockWaitTimeout {
			log.Infof("retryable error: %v, waiting for %v and retrying", sqlErr, dbLockRetryDelay)
			if err := vp.dbClient.Rollback(); err != nil {
				return err
			}
			time.Sleep(dbLockRetryDelay)
			// Check context here. Otherwise this can become an infinite loop.
			select {
			case <-ctx.Done():
				return io.EOF
			default:
			}
			err = vp.dbClient.Retry()
			continue
		}
		return err
	}
	return nil
}

func encodeString(in string) string {
	var buf strings.Builder
	sqltypes.NewVarChar(in).EncodeSQL(&buf)
	return buf.String()
}
