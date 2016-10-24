// Copyright 2016 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Spencer Kimball (spencer@cockroachlabs.com)

package sql_test

import (
	"bytes"
	"sync/atomic"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/testcluster"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/pkg/errors"
)

// TestAmbiguousCommit verifies that an ambiguous commit error is
// returned from sql.Exec in situations where an EndTransaction is
// part of a batch and the disposition of the batch request is unknown
// after a network failure or timeout. The goal here is to prevent
// spurious transaction retries after the initial transaction actually
// succeeded. In cases where there's an auto-generated primary key,
// this can result in silent duplications. In cases where the primary
// key is specified in advance, it can result in violated uniqueness
// constraints, or duplicate key violations. See #6053, #7604, and #10023.
func TestAmbiguousCommit(t *testing.T) {
	defer leaktest.AfterTest(t)()

	// Create a command filter which prevents EndTransaction from
	// returning a response.
	var responseCount int32
	committed := make(chan struct{})
	wait := make(chan struct{})
	tableStartKey := keys.MakeTablePrefix(51 /* initial table ID */)
	params := base.TestServerArgs{}
	// Prevent the first conditional put on table 51 from returning to
	// waiting client in order to simulate a lost update or slow network
	// link.
	params.Knobs.Store = &storage.StoreTestingKnobs{
		TestingResponseFilter: func(ba roachpb.BatchRequest, br *roachpb.BatchResponse) *roachpb.Error {
			req, ok := ba.GetArg(roachpb.ConditionalPut)
			if !ok || !bytes.HasPrefix(req.Header().Key, tableStartKey) {
				return nil
			}
			// If this is the first write to the table, wait to respond to the
			// client in order to simulate a retry.
			if atomic.AddInt32(&responseCount, 1) == 1 {
				close(committed)
				<-wait
			}
			return nil
		},
	}
	testClusterArgs := base.TestClusterArgs{
		ReplicationMode: base.ReplicationAuto,
		ServerArgs:      params,
	}
	tc := testcluster.StartTestCluster(t, 3, testClusterArgs)
	defer tc.Stopper().Stop()
	if err := tc.WaitForFullReplication(); err != nil {
		t.Error(err)
	}

	sqlDB := sqlutils.MakeSQLRunner(t, tc.Conns[0])

	_ = sqlDB.Exec(`CREATE DATABASE test`)
	_ = sqlDB.Exec(`CREATE TABLE test.t (k SERIAL PRIMARY KEY, v INT)`)

	// Wait for new table to split.
	util.SucceedsSoon(t, func() error {
		desc, err := tc.LookupRange(keys.MakeRowSentinelKey(tableStartKey))
		if err != nil {
			t.Fatal(err)
		}
		if !desc.StartKey.Equal(tableStartKey) {
			return errors.Errorf("expected range start key %s; got %s", tableStartKey, desc.StartKey)
		}
		return nil
	})

	// Lookup the lease.
	tableRangeDesc, err := tc.LookupRange(keys.MakeRowSentinelKey(tableStartKey))
	if err != nil {
		t.Fatal(err)
	}
	leaseHolder, err := tc.FindRangeLeaseHolder(
		&tableRangeDesc,
		&testcluster.ReplicationTarget{
			NodeID:  tc.Servers[0].GetNode().Descriptor.NodeID,
			StoreID: tc.Servers[0].GetFirstStoreID(),
		})
	if err != nil {
		t.Fatal(err)
	}

	// In a goroutine, send an insert which will commit but not return
	// from the leader (due to the command filter we installed on node 0).
	sqlErrCh := make(chan error, 1)
	go func() {
		// Use a connection other than through the node which is the current
		// leaseholder to ensure that we use GRPC instead of the local server.
		_, err := tc.Conns[leaseHolder.NodeID%3].Exec(`INSERT INTO test.t (v) VALUES (1)`)
		sqlErrCh <- err
		close(wait)
	}()
	// Wait until the insert has committed.
	<-committed

	// Find a node other than the current lease holder to transfer the lease to.
	for i, s := range tc.Servers {
		if leaseHolder.StoreID != s.GetFirstStoreID() {
			err = tc.TransferRangeLease(&tableRangeDesc, tc.Target(i))
			if err != nil {
				t.Fatal(err)
			}
			break
		}
	}

	// Close the wait channel and wait for the error from the pending SQL insert.
	if err = <-sqlErrCh; !testutils.IsError(err, "transaction commit result is ambiguous") {
		t.Errorf("expected ambiguous commit error; got %v", err)
	}

	// Verify a single row exists in the table.
	var rowCount int
	sqlDB.QueryRow(`SELECT COUNT(*) FROM test.t`).Scan(&rowCount)
	if rowCount != 1 {
		t.Errorf("expected 1 row but found %d", rowCount)
	}
}