// Copyright IBM Corp. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0
package blockprocessor

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	"github.com/IBM-Blockchain/bcdb-server/internal/blockstore"
	"github.com/IBM-Blockchain/bcdb-server/internal/identity"
	mptrieStore "github.com/IBM-Blockchain/bcdb-server/internal/mptrie/store"
	"github.com/IBM-Blockchain/bcdb-server/internal/provenance"
	"github.com/IBM-Blockchain/bcdb-server/internal/worldstate"
	"github.com/IBM-Blockchain/bcdb-server/internal/worldstate/leveldb"
	"github.com/IBM-Blockchain/bcdb-server/pkg/logger"
	"github.com/IBM-Blockchain/bcdb-server/pkg/types"
	"github.com/golang/protobuf/proto"
	"github.com/stretchr/testify/require"
)

type committerTestEnv struct {
	db              *leveldb.LevelDB
	dbPath          string
	blockStore      *blockstore.Store
	blockStorePath  string
	identityQuerier *identity.Querier
	committer       *committer
	cleanup         func()
}

func newCommitterTestEnv(t *testing.T) *committerTestEnv {
	lc := &logger.Config{
		Level:         "debug",
		OutputPath:    []string{"stdout"},
		ErrOutputPath: []string{"stderr"},
		Encoding:      "console",
	}
	logger, err := logger.New(lc)
	require.NoError(t, err)

	dir, err := ioutil.TempDir("/tmp", "committer")
	require.NoError(t, err)

	dbPath := filepath.Join(dir, "leveldb")
	db, err := leveldb.Open(
		&leveldb.Config{
			DBRootDir: dbPath,
			Logger:    logger,
		},
	)
	if err != nil {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			t.Errorf("error while removing directory %s, %v", dir, rmErr)
		}
		t.Fatalf("error while creating leveldb, %v", err)
	}

	blockStorePath := filepath.Join(dir, "blockstore")
	blockStore, err := blockstore.Open(
		&blockstore.Config{
			StoreDir: blockStorePath,
			Logger:   logger,
		},
	)
	if err != nil {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			t.Errorf("error while removing directory %s, %v", dir, rmErr)
		}
		t.Fatalf("error while creating blockstore, %v", err)
	}

	provenanceStorePath := filepath.Join(dir, "provenancestore")
	provenanceStore, err := provenance.Open(
		&provenance.Config{
			StoreDir: provenanceStorePath,
			Logger:   logger,
		},
	)

	mptrieStorePath := filepath.Join(dir, "statetriestore")
	mptrieStore, err := mptrieStore.Open(
		&mptrieStore.Config{
			StoreDir: mptrieStorePath,
			Logger:   logger,
		},
	)

	if err != nil {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			t.Errorf("error while removing directory %s, %v", dir, err)
		}
		t.Fatalf("error while creating the block store, %v", err)
	}

	cleanup := func() {
		if err := provenanceStore.Close(); err != nil {
			t.Errorf("error while closing the provenance store, %v", err)
		}

		if err := db.Close(); err != nil {
			t.Errorf("error while closing the db instance, %v", err)
		}

		if err := blockStore.Close(); err != nil {
			t.Errorf("error while closing blockstore, %v", err)
		}

		if err := os.RemoveAll(dir); err != nil {
			t.Fatalf("error while removing directory %s, %v", dir, err)
		}
	}

	c := &Config{
		BlockStore:      blockStore,
		DB:              db,
		ProvenanceStore: provenanceStore,
		StateTrieStore:  mptrieStore,
		Logger:          logger,
	}
	env := &committerTestEnv{
		db:              db,
		dbPath:          dbPath,
		blockStore:      blockStore,
		blockStorePath:  blockStorePath,
		identityQuerier: identity.NewQuerier(db),
		committer:       newCommitter(c),
		cleanup:         cleanup,
	}
	_, _, env.committer.stateTrie, err = loadStateTrie(mptrieStore, blockStore)
	require.NoError(t, err)
	return env
}

func TestCommitter(t *testing.T) {
	t.Parallel()

	t.Run("commit block to block store and state db", func(t *testing.T) {
		t.Parallel()

		env := newCommitterTestEnv(t)
		defer env.cleanup()

		createDB := map[string]*worldstate.DBUpdates{
			worldstate.DatabasesDBName: {
				Writes: []*worldstate.KVWithMetadata{
					{
						Key: "db1",
					},
					{
						Key: "db2",
					},
					{
						Key: "db3",
					},
				},
			},
		}
		require.NoError(t, env.db.Commit(createDB, 1))

		block1 := &types.Block{
			Header: &types.BlockHeader{
				BaseHeader: &types.BlockHeaderBase{
					Number: 1,
				},
				ValidationInfo: []*types.ValidationInfo{
					{
						Flag: types.Flag_VALID,
					},
				},
			},
			Payload: &types.Block_DataTxEnvelopes{
				DataTxEnvelopes: &types.DataTxEnvelopes{
					Envelopes: []*types.DataTxEnvelope{
						{
							Payload: &types.DataTx{
								MustSignUserIds: []string{"testUser"},
								DbOperations: []*types.DBOperation{
									{
										DbName: "db1",
										DataWrites: []*types.DataWrite{
											{
												Key:   "db1-key1",
												Value: []byte("value-1"),
											},
										},
									},
									{
										DbName: "db2",
										DataWrites: []*types.DataWrite{
											{
												Key:   "db2-key1",
												Value: []byte("value-1"),
											},
										},
									},
									{
										DbName: "db3",
										DataWrites: []*types.DataWrite{
											{
												Key:   "db3-key1",
												Value: []byte("value-1"),
											},
										},
									},
								},
							},
						},
					},
				},
			},
		}

		err := env.committer.commitBlock(block1)
		require.NoError(t, err)

		height, err := env.blockStore.Height()
		require.NoError(t, err)
		require.Equal(t, uint64(1), height)

		block, err := env.blockStore.Get(1)
		require.NoError(t, err)
		require.True(t, proto.Equal(block, block1))

		for _, db := range []string{"db1", "db2", "db3"} {
			val, metadata, err := env.db.Get(db, db+"-key1")
			require.NoError(t, err)

			expectedMetadata := &types.Metadata{
				Version: &types.Version{
					BlockNum: 1,
					TxNum:    0,
				},
			}
			require.True(t, proto.Equal(expectedMetadata, metadata))
			require.Equal(t, val, []byte("value-1"))
		}
		stateTrieHash, err := env.committer.stateTrie.Hash()
		require.NoError(t, err)
		require.Equal(t, block.GetHeader().GetStateMerkelTreeRootHash(), stateTrieHash)
	})
}

func TestBlockStoreCommitter(t *testing.T) {
	t.Parallel()

	getSampleBlock := func(number uint64) *types.Block {
		return &types.Block{
			Header: &types.BlockHeader{
				BaseHeader: &types.BlockHeaderBase{
					Number: number,
				},
				ValidationInfo: []*types.ValidationInfo{
					{
						Flag: types.Flag_VALID,
					},
					{
						Flag: types.Flag_VALID,
					},
				},
			},
			Payload: &types.Block_DataTxEnvelopes{
				DataTxEnvelopes: &types.DataTxEnvelopes{
					Envelopes: []*types.DataTxEnvelope{
						{
							Payload: &types.DataTx{
								DbOperations: []*types.DBOperation{
									{
										DbName: "db1",
										DataWrites: []*types.DataWrite{
											{
												Key:   fmt.Sprintf("db1-key%d", number),
												Value: []byte(fmt.Sprintf("value-%d", number)),
											},
										},
									},
								},
							},
						},
						{
							Payload: &types.DataTx{
								DbOperations: []*types.DBOperation{
									{
										DbName: "db2",
										DataWrites: []*types.DataWrite{
											{
												Key:   fmt.Sprintf("db2-key%d", number),
												Value: []byte(fmt.Sprintf("value-%d", number)),
											},
										},
									},
								},
							},
						},
					},
				},
			},
		}
	}

	t.Run("commit multiple blocks to the block store and query the same", func(t *testing.T) {
		t.Parallel()

		env := newCommitterTestEnv(t)
		defer env.cleanup()

		var expectedBlocks []*types.Block

		for blockNumber := uint64(1); blockNumber <= 100; blockNumber++ {
			block := getSampleBlock(blockNumber)
			require.NoError(t, env.committer.commitToBlockStore(block))
			expectedBlocks = append(expectedBlocks, block)
		}

		for blockNumber := uint64(1); blockNumber <= 100; blockNumber++ {
			block, err := env.blockStore.Get(blockNumber)
			require.NoError(t, err)
			require.True(t, proto.Equal(expectedBlocks[blockNumber-1], block))
		}

		height, err := env.blockStore.Height()
		require.NoError(t, err)
		require.Equal(t, uint64(100), height)
	})

	t.Run("commit unexpected block to the block store", func(t *testing.T) {
		t.Parallel()

		env := newCommitterTestEnv(t)
		defer env.cleanup()

		block := getSampleBlock(10)
		err := env.committer.commitToBlockStore(block)
		require.EqualError(t, err, "failed to commit block 10 to block store: expected block number [1] but received [10]")
	})
}

func TestStateDBCommitterForDataBlock(t *testing.T) {
	t.Parallel()

	setup := func(db worldstate.DB) {
		createDB := map[string]*worldstate.DBUpdates{
			worldstate.DatabasesDBName: {
				Writes: []*worldstate.KVWithMetadata{
					{
						Key: "db1",
					},
				},
			},
		}
		require.NoError(t, db.Commit(createDB, 1))

		data := map[string]*worldstate.DBUpdates{
			worldstate.DefaultDBName: {
				Writes: []*worldstate.KVWithMetadata{
					constructDataEntryForTest("key1", []byte("value1"), &types.Metadata{
						Version: &types.Version{
							BlockNum: 1,
							TxNum:    1,
						},
						AccessControl: &types.AccessControl{
							ReadWriteUsers: map[string]bool{
								"user1": true,
							},
						},
					}),
					constructDataEntryForTest("key2", []byte("value2"), nil),
					constructDataEntryForTest("key3", []byte("value3"), nil),
				},
			},
			"db1": {
				Writes: []*worldstate.KVWithMetadata{
					constructDataEntryForTest("key1", []byte("value1"), &types.Metadata{
						Version: &types.Version{
							BlockNum: 1,
							TxNum:    1,
						},
						AccessControl: &types.AccessControl{
							ReadWriteUsers: map[string]bool{
								"user1": true,
							},
						},
					}),
					constructDataEntryForTest("key2", []byte("value2"), nil),
					constructDataEntryForTest("key3", []byte("value3"), nil),
				},
			},
		}

		require.NoError(t, db.Commit(data, 1))
	}

	expectedDataBefore := []*worldstate.KVWithMetadata{
		constructDataEntryForTest("key1", []byte("value1"), &types.Metadata{
			Version: &types.Version{
				BlockNum: 1,
				TxNum:    1,
			},
			AccessControl: &types.AccessControl{
				ReadWriteUsers: map[string]bool{
					"user1": true,
				},
			},
		}),
		constructDataEntryForTest("key2", []byte("value2"), nil),
		constructDataEntryForTest("key3", []byte("value3"), nil),
	}

	tests := []struct {
		name              string
		txs               []*types.DataTxEnvelope
		valInfo           []*types.ValidationInfo
		expectedDataAfter []*worldstate.KVWithMetadata
	}{
		{
			name: "update existing, add new, and delete existing",
			txs: []*types.DataTxEnvelope{
				{
					Payload: &types.DataTx{
						MustSignUserIds: []string{"testUser"},
						DbOperations: []*types.DBOperation{
							{
								DbName: worldstate.DefaultDBName,
								DataWrites: []*types.DataWrite{
									{
										Key:   "key2",
										Value: []byte("new-value2"),
										Acl: &types.AccessControl{
											ReadUsers: map[string]bool{
												"user1": true,
											},
											ReadWriteUsers: map[string]bool{
												"user2": true,
											},
										},
									},
								},
							},
							{
								DbName: "db1",
								DataWrites: []*types.DataWrite{
									{
										Key:   "key2",
										Value: []byte("new-value2"),
										Acl: &types.AccessControl{
											ReadUsers: map[string]bool{
												"user1": true,
											},
											ReadWriteUsers: map[string]bool{
												"user2": true,
											},
										},
									},
								},
							},
						},
					},
				},
				{
					Payload: &types.DataTx{
						MustSignUserIds: []string{"testUser"},
						DbOperations: []*types.DBOperation{
							{
								DbName: worldstate.DefaultDBName,
								DataWrites: []*types.DataWrite{
									{
										Key:   "key3",
										Value: []byte("new-value3"),
									},
								},
							},
							{
								DbName: "db1",
								DataWrites: []*types.DataWrite{
									{
										Key:   "key3",
										Value: []byte("new-value3"),
									},
								},
							},
						},
					},
				},
				{
					Payload: &types.DataTx{
						MustSignUserIds: []string{"testUser"},
						DbOperations: []*types.DBOperation{
							{
								DbName: worldstate.DefaultDBName,
								DataWrites: []*types.DataWrite{
									{
										Key:   "key4",
										Value: []byte("value4"),
									},
								},
							},
							{
								DbName: "db1",
								DataWrites: []*types.DataWrite{
									{
										Key:   "key4",
										Value: []byte("value4"),
									},
								},
							},
						},
					},
				},
				{
					Payload: &types.DataTx{
						MustSignUserIds: []string{"testUser"},
						DbOperations: []*types.DBOperation{
							{
								DbName: worldstate.DefaultDBName,
								DataDeletes: []*types.DataDelete{
									{
										Key: "key1",
									},
								},
							},
							{
								DbName: "db1",
								DataDeletes: []*types.DataDelete{
									{
										Key: "key1",
									},
								},
							},
						},
					},
				},
				{
					Payload: &types.DataTx{
						MustSignUserIds: []string{"testUser"},
						DbOperations: []*types.DBOperation{
							{
								DbName: worldstate.DefaultDBName,
								DataDeletes: []*types.DataDelete{
									{
										Key: "key2",
									},
								},
							},
						},
					},
				},
			},
			valInfo: []*types.ValidationInfo{
				{
					Flag: types.Flag_VALID,
				},
				{
					Flag: types.Flag_VALID,
				},
				{
					Flag: types.Flag_VALID,
				},
				{
					Flag: types.Flag_VALID,
				},
				{
					Flag: types.Flag_INVALID_MVCC_CONFLICT_WITHIN_BLOCK,
				},
			},
			expectedDataAfter: []*worldstate.KVWithMetadata{
				constructDataEntryForTest("key2", []byte("new-value2"), &types.Metadata{
					Version: &types.Version{
						BlockNum: 2,
						TxNum:    0,
					},
					AccessControl: &types.AccessControl{
						ReadUsers: map[string]bool{
							"user1": true,
						},
						ReadWriteUsers: map[string]bool{
							"user2": true,
						},
					},
				}),
				constructDataEntryForTest("key3", []byte("new-value3"), &types.Metadata{
					Version: &types.Version{
						BlockNum: 2,
						TxNum:    1,
					},
				}),
				constructDataEntryForTest("key4", []byte("value4"), &types.Metadata{
					Version: &types.Version{
						BlockNum: 2,
						TxNum:    2,
					},
				}),
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			env := newCommitterTestEnv(t)
			defer env.cleanup()

			setup(env.db)

			for _, db := range []string{worldstate.DefaultDBName, "db1"} {
				for _, kv := range expectedDataBefore {
					val, meta, err := env.db.Get(db, kv.Key)
					require.NoError(t, err)
					require.Equal(t, kv.Value, val)
					require.Equal(t, kv.Metadata, meta)
				}
			}

			block := &types.Block{
				Header: &types.BlockHeader{
					BaseHeader: &types.BlockHeaderBase{
						Number: 2,
					},
					ValidationInfo: tt.valInfo,
				},
				Payload: &types.Block_DataTxEnvelopes{
					DataTxEnvelopes: &types.DataTxEnvelopes{
						Envelopes: tt.txs,
					},
				},
			}

			dbsUpdates, _, err := env.committer.constructDBAndProvenanceEntries(block)
			require.NoError(t, err)
			require.NoError(t, env.committer.commitToStateDB(2, dbsUpdates))

			for _, db := range []string{worldstate.DefaultDBName, "db1"} {
				for _, kv := range tt.expectedDataAfter {
					val, meta, err := env.db.Get(db, kv.Key)
					require.NoError(t, err)
					require.Equal(t, kv.Value, val)
					require.Equal(t, kv.Metadata, meta)
				}
			}
		})
	}
}

func TestStateDBCommitterForUserBlock(t *testing.T) {
	t.Parallel()

	sampleVersion := &types.Version{
		BlockNum: 1,
		TxNum:    1,
	}

	tests := []struct {
		name                string
		setup               func(env *committerTestEnv)
		expectedUsersBefore []string
		tx                  *types.UserAdministrationTx
		valInfo             []*types.ValidationInfo
		expectedUsersAfter  []string
	}{
		{
			name: "add and delete users",
			setup: func(env *committerTestEnv) {
				user1 := constructUserForTest(t, "user1", nil, nil, sampleVersion, nil)
				user2 := constructUserForTest(t, "user2", nil, nil, sampleVersion, nil)
				user3 := constructUserForTest(t, "user3", nil, nil, sampleVersion, nil)
				user4 := constructUserForTest(t, "user4", nil, nil, sampleVersion, nil)
				users := map[string]*worldstate.DBUpdates{
					worldstate.UsersDBName: {
						Writes: []*worldstate.KVWithMetadata{
							user1,
							user2,
							user3,
							user4,
						},
					},
				}
				require.NoError(t, env.db.Commit(users, 1))

				txsData := []*provenance.TxDataForProvenance{
					{
						IsValid: true,
						DBName:  worldstate.UsersDBName,
						UserID:  "user0",
						TxID:    "tx0",
						Writes: []*types.KVWithMetadata{
							{
								Key:      "user1",
								Value:    user1.Value,
								Metadata: user1.Metadata,
							},
							{
								Key:      "user2",
								Value:    user2.Value,
								Metadata: user2.Metadata,
							},
							{
								Key:      "user3",
								Value:    user3.Value,
								Metadata: user3.Metadata,
							},
							{
								Key:      "user4",
								Value:    user4.Value,
								Metadata: user4.Metadata,
							},
						},
					},
				}
				require.NoError(t, env.committer.provenanceStore.Commit(1, txsData))
			},
			expectedUsersBefore: []string{"user1", "user2", "user3", "user4"},
			tx: &types.UserAdministrationTx{
				UserId: "user0",
				TxId:   "tx1",
				UserWrites: []*types.UserWrite{
					{
						User: &types.User{
							Id:          "user4",
							Certificate: []byte("new-certificate~user4"),
						},
					},
					{
						User: &types.User{
							Id:          "user5",
							Certificate: []byte("certificate~user5"),
						},
					},
					{
						User: &types.User{
							Id:          "user6",
							Certificate: []byte("certificate~user6"),
						},
					},
				},
				UserDeletes: []*types.UserDelete{
					{
						UserId: "user1",
					},
					{
						UserId: "user2",
					},
				},
			},
			valInfo: []*types.ValidationInfo{
				{
					Flag: types.Flag_VALID,
				},
			},
			expectedUsersAfter: []string{"user3", "user4", "user5", "user6"},
		},
		{
			name: "tx is marked invalid",
			setup: func(env *committerTestEnv) {
				users := map[string]*worldstate.DBUpdates{
					worldstate.UsersDBName: {
						Writes: []*worldstate.KVWithMetadata{
							constructUserForTest(t, "user1", nil, nil, sampleVersion, nil),
						},
					},
				}
				require.NoError(t, env.db.Commit(users, 1))
			},
			expectedUsersBefore: []string{"user1"},
			tx: &types.UserAdministrationTx{
				UserId: "user0",
				TxId:   "tx1",
				UserWrites: []*types.UserWrite{
					{
						User: &types.User{
							Id:          "user2",
							Certificate: []byte("new-certificate~user2"),
						},
					},
				},
				UserDeletes: []*types.UserDelete{
					{
						UserId: "user1",
					},
				},
			},
			valInfo: []*types.ValidationInfo{
				{
					Flag: types.Flag_INVALID_NO_PERMISSION,
				},
			},
			expectedUsersAfter: []string{"user1"},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			env := newCommitterTestEnv(t)
			defer env.cleanup()

			tt.setup(env)

			for _, user := range tt.expectedUsersBefore {
				exist, err := env.identityQuerier.DoesUserExist(user)
				require.NoError(t, err)
				require.True(t, exist)
			}

			block := &types.Block{
				Header: &types.BlockHeader{
					BaseHeader: &types.BlockHeaderBase{
						Number: 2,
					},
					ValidationInfo: tt.valInfo,
				},
				Payload: &types.Block_UserAdministrationTxEnvelope{
					UserAdministrationTxEnvelope: &types.UserAdministrationTxEnvelope{
						Payload: tt.tx,
					},
				},
			}

			dbsUpdates, provenanceData, err := env.committer.constructDBAndProvenanceEntries(block)
			require.NoError(t, err)
			require.NoError(t, env.committer.commitToDBs(dbsUpdates, provenanceData, block))

			for _, user := range tt.expectedUsersAfter {
				exist, err := env.identityQuerier.DoesUserExist(user)
				require.NoError(t, err)
				require.True(t, exist)
			}
		})
	}
}

func TestStateDBCommitterForDBBlock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		setup             func(db worldstate.DB)
		expectedDBsBefore []string
		dbAdminTx         *types.DBAdministrationTx
		expectedDBsAfter  []string
		expectedIndex     map[string]*types.DBIndex
		valInfo           []*types.ValidationInfo
	}{
		{
			name: "create new DBss and delete existing DBs",
			setup: func(db worldstate.DB) {
				createDBs := map[string]*worldstate.DBUpdates{
					worldstate.DatabasesDBName: {
						Writes: []*worldstate.KVWithMetadata{
							{
								Key: "db1",
							},
							{
								Key: "db2",
							},
						},
					},
				}

				require.NoError(t, db.Commit(createDBs, 1))
			},
			expectedDBsBefore: []string{"db1", "db2"},
			dbAdminTx: &types.DBAdministrationTx{
				CreateDbs: []string{"db3", "db4"},
				DeleteDbs: []string{"db1", "db2"},
				DbsIndex: map[string]*types.DBIndex{
					"db3": {
						AttributeAndType: map[string]types.Type{
							"attr1": types.Type_BOOLEAN,
							"attr2": types.Type_NUMBER,
						},
					},
				},
			},
			expectedDBsAfter: []string{"db3", "db4"},
			expectedIndex: map[string]*types.DBIndex{
				"db3": {
					AttributeAndType: map[string]types.Type{
						"attr1": types.Type_BOOLEAN,
						"attr2": types.Type_NUMBER,
					},
				},
				"db4": nil,
			},
			valInfo: []*types.ValidationInfo{
				{
					Flag: types.Flag_VALID,
				},
			},
		},
		{
			name: "change index of an existing DB and create index for new DB",
			setup: func(db worldstate.DB) {
				indexDB1, err := json.Marshal(
					map[string]types.Type{
						"attr1-db1": types.Type_NUMBER,
						"attr2-db1": types.Type_BOOLEAN,
						"attr3-db1": types.Type_STRING,
					},
				)
				require.NoError(t, err)

				indexDB2, err := json.Marshal(
					map[string]types.Type{
						"attr1-db2": types.Type_NUMBER,
						"attr2-db2": types.Type_BOOLEAN,
						"attr3-db2": types.Type_STRING,
					},
				)
				require.NoError(t, err)

				createDBs := map[string]*worldstate.DBUpdates{
					worldstate.DatabasesDBName: {
						Writes: []*worldstate.KVWithMetadata{
							{
								Key:   "db1",
								Value: indexDB1,
							},
							{
								Key:   "db2",
								Value: indexDB2,
							},
						},
					},
				}

				require.NoError(t, db.Commit(createDBs, 1))
			},
			expectedDBsBefore: []string{"db1", "db2"},
			dbAdminTx: &types.DBAdministrationTx{
				CreateDbs: []string{"db3", "db4"},
				DbsIndex: map[string]*types.DBIndex{
					"db1": {
						AttributeAndType: map[string]types.Type{
							"attr1": types.Type_BOOLEAN,
							"attr2": types.Type_NUMBER,
						},
					},
					"db2": nil,
					"db3": {
						AttributeAndType: map[string]types.Type{
							"attr1": types.Type_STRING,
						},
					},
					"db4": {
						AttributeAndType: map[string]types.Type{
							"attr1": types.Type_NUMBER,
						},
					},
				},
			},
			expectedDBsAfter: []string{"db1", "db2", "db3", "db4"},
			expectedIndex: map[string]*types.DBIndex{
				"db1": {
					AttributeAndType: map[string]types.Type{
						"attr1": types.Type_BOOLEAN,
						"attr2": types.Type_NUMBER,
					},
				},
				"db2": nil,
				"db3": {
					AttributeAndType: map[string]types.Type{
						"attr1": types.Type_STRING,
					},
				},
				"db4": {
					AttributeAndType: map[string]types.Type{
						"attr1": types.Type_NUMBER,
					},
				},
			},
			valInfo: []*types.ValidationInfo{
				{
					Flag: types.Flag_VALID,
				},
			},
		},
		{
			name: "tx marked invalid",
			setup: func(db worldstate.DB) {
				createDBs := map[string]*worldstate.DBUpdates{
					worldstate.DatabasesDBName: {
						Writes: []*worldstate.KVWithMetadata{
							{
								Key: "db1",
							},
							{
								Key: "db2",
							},
						},
					},
				}

				require.NoError(t, db.Commit(createDBs, 1))
			},
			expectedDBsBefore: []string{"db1", "db2"},
			dbAdminTx: &types.DBAdministrationTx{
				DeleteDbs: []string{"db1", "db2"},
			},
			expectedDBsAfter: []string{"db1", "db2"},
			expectedIndex: map[string]*types.DBIndex{
				"db1": nil,
				"db2": nil,
			},
			valInfo: []*types.ValidationInfo{
				{
					Flag: types.Flag_INVALID_NO_PERMISSION,
				},
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			env := newCommitterTestEnv(t)
			defer env.cleanup()

			tt.setup(env.db)

			for _, dbName := range tt.expectedDBsBefore {
				require.True(t, env.db.Exist(dbName))
			}

			block := &types.Block{
				Header: &types.BlockHeader{
					BaseHeader: &types.BlockHeaderBase{
						Number: 2,
					},
					ValidationInfo: tt.valInfo,
				},
				Payload: &types.Block_DbAdministrationTxEnvelope{
					DbAdministrationTxEnvelope: &types.DBAdministrationTxEnvelope{
						Payload: tt.dbAdminTx,
					},
				},
			}

			dbsUpdates, provenanceData, err := env.committer.constructDBAndProvenanceEntries(block)
			require.NoError(t, err)
			require.NoError(t, env.committer.commitToDBs(dbsUpdates, provenanceData, block))

			for _, dbName := range tt.expectedDBsAfter {
				require.True(t, env.db.Exist(dbName))
				index, _, err := env.db.Get(worldstate.DatabasesDBName, dbName)
				require.NoError(t, err)

				var expectedIndex []byte
				if tt.expectedIndex[dbName] != nil {
					expectedIndex, err = json.Marshal(tt.expectedIndex[dbName].GetAttributeAndType())
				}
				require.NoError(t, err)
				require.Equal(t, expectedIndex, index)
			}
		})
	}
}

func TestStateDBCommitterForConfigBlock(t *testing.T) {
	t.Parallel()

	generateSampleConfigBlock := func(number uint64, adminsID []string, nodeIDs []string, valInfo []*types.ValidationInfo) *types.Block {
		var admins []*types.Admin
		for _, id := range adminsID {
			admins = append(admins, &types.Admin{
				Id:          id,
				Certificate: []byte("certificate~" + id),
			})
		}

		var nodes []*types.NodeConfig
		for _, id := range nodeIDs {
			nodes = append(nodes, constructNodeEntryForTest(id))
		}

		clusterConfig := &types.ClusterConfig{
			Nodes:  nodes,
			Admins: admins,
			CertAuthConfig: &types.CAConfig{
				Roots: [][]byte{[]byte("root-ca")},
			},
		}

		configBlock := &types.Block{
			Header: &types.BlockHeader{
				BaseHeader: &types.BlockHeaderBase{
					Number: number,
				},
				ValidationInfo: valInfo,
			},
			Payload: &types.Block_ConfigTxEnvelope{
				ConfigTxEnvelope: &types.ConfigTxEnvelope{
					Payload: &types.ConfigTx{
						NewConfig: clusterConfig,
					},
				},
			},
		}

		return configBlock
	}

	assertExpectedUsers := func(t *testing.T, q *identity.Querier, expectedUsers []*types.User) {
		for _, expectedUser := range expectedUsers {
			user, _, err := q.GetUser(expectedUser.Id)
			require.NoError(t, err)
			require.True(t, proto.Equal(expectedUser, user))
		}
	}

	assertExpectedNodes := func(t *testing.T, q *identity.Querier, expectedNodes []*types.NodeConfig) {
		for _, expectedNode := range expectedNodes {
			node, _, err := q.GetNode(expectedNode.Id)
			require.NoError(t, err)
			require.True(t, proto.Equal(expectedNode, node))
		}
	}

	tests := []struct {
		name                        string
		adminsInCommittedConfigTx   []string
		nodesInCommittedConfigTx    []string
		adminsInNewConfigTx         []string
		nodesInNewConfigTx          []string
		expectedClusterAdminsBefore []*types.User
		expectedNodesBefore         []*types.NodeConfig
		expectedClusterAdminsAfter  []*types.User
		expectedNodesAfter          []*types.NodeConfig
		valInfo                     []*types.ValidationInfo
	}{
		{
			name:                      "no change in the set of admins and nodes",
			adminsInCommittedConfigTx: []string{"admin1", "admin2"},
			adminsInNewConfigTx:       []string{"admin1", "admin2"},
			nodesInCommittedConfigTx:  []string{"node1", "node2"},
			nodesInNewConfigTx:        []string{"node1", "node2"},
			expectedClusterAdminsBefore: []*types.User{
				constructAdminEntryForTest("admin1"),
				constructAdminEntryForTest("admin2"),
			},
			expectedNodesBefore: []*types.NodeConfig{
				constructNodeEntryForTest("node1"),
				constructNodeEntryForTest("node2"),
			},
			expectedClusterAdminsAfter: []*types.User{
				constructAdminEntryForTest("admin1"),
				constructAdminEntryForTest("admin2"),
			},
			expectedNodesAfter: []*types.NodeConfig{
				constructNodeEntryForTest("node1"),
				constructNodeEntryForTest("node2"),
			},
			valInfo: []*types.ValidationInfo{
				{
					Flag: types.Flag_VALID,
				},
			},
		},
		{
			name:                      "add and delete admins and nodes",
			adminsInCommittedConfigTx: []string{"admin1", "admin2", "admin3"},
			adminsInNewConfigTx:       []string{"admin3", "admin4", "admin5"},
			nodesInCommittedConfigTx:  []string{"node1", "node2", "node3"},
			nodesInNewConfigTx:        []string{"node3", "node4", "node5"},
			expectedClusterAdminsBefore: []*types.User{
				constructAdminEntryForTest("admin1"),
				constructAdminEntryForTest("admin2"),
				constructAdminEntryForTest("admin3"),
			},
			expectedNodesBefore: []*types.NodeConfig{
				constructNodeEntryForTest("node1"),
				constructNodeEntryForTest("node2"),
				constructNodeEntryForTest("node3"),
			},
			expectedClusterAdminsAfter: []*types.User{
				constructAdminEntryForTest("admin3"),
				constructAdminEntryForTest("admin4"),
				constructAdminEntryForTest("admin5"),
			},
			expectedNodesAfter: []*types.NodeConfig{
				constructNodeEntryForTest("node3"),
				constructNodeEntryForTest("node4"),
				constructNodeEntryForTest("node5"),
			},
			valInfo: []*types.ValidationInfo{
				{
					Flag: types.Flag_VALID,
				},
			},
		},
		{
			name:                      "tx is marked invalid",
			adminsInCommittedConfigTx: []string{"admin1"},
			expectedClusterAdminsBefore: []*types.User{
				constructAdminEntryForTest("admin1"),
			},
			adminsInNewConfigTx: []string{"admin1", "admin2"},
			expectedClusterAdminsAfter: []*types.User{
				constructAdminEntryForTest("admin1"),
			},
			valInfo: []*types.ValidationInfo{
				{
					Flag: types.Flag_INVALID_NO_PERMISSION,
				},
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			// t.Parallel()

			env := newCommitterTestEnv(t)
			defer env.cleanup()

			var blockNumber uint64

			validationInfo := []*types.ValidationInfo{
				{
					Flag: types.Flag_VALID,
				},
			}
			blockNumber = 1
			configBlock := generateSampleConfigBlock(blockNumber, tt.adminsInCommittedConfigTx, tt.nodesInCommittedConfigTx, validationInfo)
			dbsUpdates, provenanceData, err := env.committer.constructDBAndProvenanceEntries(configBlock)
			require.NoError(t, err)
			require.NoError(t, env.committer.commitToDBs(dbsUpdates, provenanceData, configBlock))
			assertExpectedUsers(t, env.identityQuerier, tt.expectedClusterAdminsBefore)
			assertExpectedNodes(t, env.identityQuerier, tt.expectedNodesBefore)

			blockNumber++
			configBlock = generateSampleConfigBlock(blockNumber, tt.adminsInNewConfigTx, tt.nodesInNewConfigTx, tt.valInfo)
			dbsUpdates, provenanceData, err = env.committer.constructDBAndProvenanceEntries(configBlock)
			require.NoError(t, err)
			require.NoError(t, env.committer.commitToDBs(dbsUpdates, provenanceData, configBlock))
			assertExpectedUsers(t, env.identityQuerier, tt.expectedClusterAdminsAfter)
			assertExpectedNodes(t, env.identityQuerier, tt.expectedNodesAfter)
		})
	}
}
func TestProvenanceStoreCommitterForDataBlockWithValidTxs(t *testing.T) {
	t.Parallel()

	setup := func(env *committerTestEnv) {
		createDB := map[string]*worldstate.DBUpdates{
			worldstate.DatabasesDBName: {
				Writes: []*worldstate.KVWithMetadata{
					{
						Key: "db1",
					},
				},
			},
		}
		require.NoError(t, env.db.Commit(createDB, 1))

		data := map[string]*worldstate.DBUpdates{
			worldstate.DefaultDBName: {
				Writes: []*worldstate.KVWithMetadata{
					constructDataEntryForTest("key0", []byte("value0"), &types.Metadata{
						Version: &types.Version{
							BlockNum: 1,
							TxNum:    0,
						},
					}),
				},
			},
			"db1": {
				Writes: []*worldstate.KVWithMetadata{
					constructDataEntryForTest("key0", []byte("value0"), &types.Metadata{
						Version: &types.Version{
							BlockNum: 1,
							TxNum:    0,
						},
					}),
				},
			},
		}
		require.NoError(t, env.committer.db.Commit(data, 1))

		txsData := []*provenance.TxDataForProvenance{
			{
				IsValid: true,
				DBName:  worldstate.DefaultDBName,
				UserID:  "user1",
				TxID:    "tx0",
				Writes: []*types.KVWithMetadata{
					{
						Key:   "key0",
						Value: []byte("value0"),
						Metadata: &types.Metadata{
							Version: &types.Version{
								BlockNum: 1,
								TxNum:    0,
							},
						},
					},
				},
			},
			{
				IsValid: true,
				DBName:  "db1",
				UserID:  "user1",
				TxID:    "tx0",
				Writes: []*types.KVWithMetadata{
					{
						Key:   "key0",
						Value: []byte("value0"),
						Metadata: &types.Metadata{
							Version: &types.Version{
								BlockNum: 1,
								TxNum:    0,
							},
						},
					},
				},
			},
		}
		require.NoError(t, env.committer.provenanceStore.Commit(1, txsData))
	}

	tests := []struct {
		name         string
		txs          []*types.DataTxEnvelope
		valInfo      []*types.ValidationInfo
		query        func(s *provenance.Store, dbName string) ([]*types.ValueWithMetadata, error)
		expectedData []*types.ValueWithMetadata
	}{
		{
			name: "previous value link within the block",
			txs: []*types.DataTxEnvelope{
				{
					Payload: &types.DataTx{
						MustSignUserIds: []string{"user1"},
						TxId:            "tx1",
						DbOperations: []*types.DBOperation{
							{
								DbName: worldstate.DefaultDBName,
								DataWrites: []*types.DataWrite{
									{
										Key:   "key1",
										Value: []byte("value1"),
									},
								},
							},
							{
								DbName: "db1",
								DataWrites: []*types.DataWrite{
									{
										Key:   "key1",
										Value: []byte("value1"),
									},
								},
							},
						},
					},
				},
				{
					Payload: &types.DataTx{
						MustSignUserIds: []string{"user1"},
						TxId:            "tx2",
						DbOperations: []*types.DBOperation{
							{
								DbName: worldstate.DefaultDBName,
								DataWrites: []*types.DataWrite{
									{
										Key:   "key1",
										Value: []byte("value2"),
									},
								},
							},
							{
								DbName: "db1",
								DataWrites: []*types.DataWrite{
									{
										Key:   "key1",
										Value: []byte("value2"),
									},
								},
							},
						},
					},
				},
			},
			valInfo: []*types.ValidationInfo{
				{
					Flag: types.Flag_VALID,
				},
				{
					Flag: types.Flag_VALID,
				},
			},
			query: func(s *provenance.Store, dbName string) ([]*types.ValueWithMetadata, error) {
				return s.GetPreviousValues(
					dbName,
					"key1",
					&types.Version{
						BlockNum: 2,
						TxNum:    1,
					},
					-1,
				)
			},
			expectedData: []*types.ValueWithMetadata{
				{
					Value: []byte("value1"),
					Metadata: &types.Metadata{
						Version: &types.Version{
							BlockNum: 2,
							TxNum:    0,
						},
					},
				},
			},
		},
		{
			name: "previous value link with the already committed value",
			txs: []*types.DataTxEnvelope{
				{
					Payload: &types.DataTx{
						MustSignUserIds: []string{"user1"},
						TxId:            "tx1",
						DbOperations: []*types.DBOperation{
							{
								DbName: worldstate.DefaultDBName,
								DataWrites: []*types.DataWrite{
									{
										Key:   "key0",
										Value: []byte("value1"),
									},
								},
							},
							{
								DbName: "db1",
								DataWrites: []*types.DataWrite{
									{
										Key:   "key0",
										Value: []byte("value1"),
									},
								},
							},
						},
					},
				},
			},
			valInfo: []*types.ValidationInfo{
				{
					Flag: types.Flag_VALID,
				},
			},
			query: func(s *provenance.Store, dbName string) ([]*types.ValueWithMetadata, error) {
				return s.GetPreviousValues(
					dbName,
					"key0",
					&types.Version{
						BlockNum: 2,
						TxNum:    0,
					},
					-1,
				)
			},
			expectedData: []*types.ValueWithMetadata{
				{
					Value: []byte("value0"),
					Metadata: &types.Metadata{
						Version: &types.Version{
							BlockNum: 1,
							TxNum:    0,
						},
					},
				},
			},
		},
		{
			name: "tx with read set",
			txs: []*types.DataTxEnvelope{
				{
					Payload: &types.DataTx{
						MustSignUserIds: []string{"user1"},
						TxId:            "tx1",
						DbOperations: []*types.DBOperation{
							{
								DbName: worldstate.DefaultDBName,
								DataReads: []*types.DataRead{
									{
										Key: "key0",
										Version: &types.Version{
											BlockNum: 1,
											TxNum:    0,
										},
									},
								},
								DataWrites: []*types.DataWrite{
									{
										Key:   "key0",
										Value: []byte("value1"),
									},
								},
							},
						},
					},
				},
			},
			valInfo: []*types.ValidationInfo{
				{
					Flag: types.Flag_VALID,
				},
			},
			query: func(s *provenance.Store, _ string) ([]*types.ValueWithMetadata, error) {
				kvs, err := s.GetValuesReadByUser("user1")
				if err != nil {
					return nil, err
				}

				var values []*types.ValueWithMetadata
				for _, kv := range kvs {
					values = append(
						values,
						&types.ValueWithMetadata{
							Value:    kv.Value,
							Metadata: kv.Metadata,
						},
					)
				}

				return values, nil
			},
			expectedData: []*types.ValueWithMetadata{
				{
					Value: []byte("value0"),
					Metadata: &types.Metadata{
						Version: &types.Version{
							BlockNum: 1,
							TxNum:    0,
						},
					},
				},
			},
		},
		{
			name: "delete key0",
			txs: []*types.DataTxEnvelope{
				{
					Payload: &types.DataTx{
						MustSignUserIds: []string{"user1"},
						TxId:            "tx1",
						DbOperations: []*types.DBOperation{
							{
								DbName: worldstate.DefaultDBName,
								DataDeletes: []*types.DataDelete{
									{
										Key: "key0",
									},
								},
							},
							{
								DbName: "db1",
								DataDeletes: []*types.DataDelete{
									{
										Key: "key0",
									},
								},
							},
						},
					},
				},
			},
			valInfo: []*types.ValidationInfo{
				{
					Flag: types.Flag_VALID,
				},
			},
			query: func(s *provenance.Store, dbName string) ([]*types.ValueWithMetadata, error) {
				return s.GetDeletedValues(
					dbName,
					"key0",
				)
			},
			expectedData: []*types.ValueWithMetadata{
				{
					Value: []byte("value0"),
					Metadata: &types.Metadata{
						Version: &types.Version{
							BlockNum: 1,
							TxNum:    0,
						},
					},
				},
			},
		},
		{
			name: "blind write and blind delete key0",
			txs: []*types.DataTxEnvelope{
				{
					Payload: &types.DataTx{
						MustSignUserIds: []string{"user1"},
						TxId:            "tx1",
						DbOperations: []*types.DBOperation{
							{
								DbName: worldstate.DefaultDBName,
								DataWrites: []*types.DataWrite{
									{
										Key:   "key0",
										Value: []byte("value1"),
									},
								},
							},
							{
								DbName: "db1",
								DataWrites: []*types.DataWrite{
									{
										Key:   "key0",
										Value: []byte("value1"),
									},
								},
							},
						},
					},
				},
				{
					Payload: &types.DataTx{
						MustSignUserIds: []string{"user1"},
						TxId:            "tx2",
						DbOperations: []*types.DBOperation{
							{
								DbName: worldstate.DefaultDBName,
								DataDeletes: []*types.DataDelete{
									{
										Key: "key0",
									},
								},
							},
							{
								DbName: "db1",
								DataDeletes: []*types.DataDelete{
									{
										Key: "key0",
									},
								},
							},
						},
					},
				},
				{
					Payload: &types.DataTx{
						MustSignUserIds: []string{"user1"},
						TxId:            "tx3",
						DbOperations: []*types.DBOperation{
							{
								DbName: worldstate.DefaultDBName,
								DataWrites: []*types.DataWrite{
									{
										Key:   "key0",
										Value: []byte("value2"),
									},
								},
							},
							{
								DbName: "db1",
								DataWrites: []*types.DataWrite{
									{
										Key:   "key0",
										Value: []byte("value2"),
									},
								},
							},
						},
					},
				},
			},
			valInfo: []*types.ValidationInfo{
				{
					Flag: types.Flag_VALID,
				},
				{
					Flag: types.Flag_VALID,
				},
				{
					Flag: types.Flag_VALID,
				},
			},
			query: func(s *provenance.Store, dbName string) ([]*types.ValueWithMetadata, error) {
				return s.GetValues(
					worldstate.DefaultDBName,
					"key0",
				)
			},
			expectedData: []*types.ValueWithMetadata{
				{
					Value: []byte("value0"),
					Metadata: &types.Metadata{
						Version: &types.Version{
							BlockNum: 1,
							TxNum:    0,
						},
					},
				},
				{
					Value: []byte("value1"),
					Metadata: &types.Metadata{
						Version: &types.Version{
							BlockNum: 2,
							TxNum:    0,
						},
					},
				},
				{
					Value: []byte("value2"),
					Metadata: &types.Metadata{
						Version: &types.Version{
							BlockNum: 2,
							TxNum:    2,
						},
					},
				},
			},
		},
		{
			name: "invalid tx",
			txs: []*types.DataTxEnvelope{
				{
					Payload: &types.DataTx{
						MustSignUserIds: []string{"user1"},
						TxId:            "tx1",
						DbOperations: []*types.DBOperation{
							{
								DbName: worldstate.DefaultDBName,
								DataReads: []*types.DataRead{
									{
										Key: "key0",
										Version: &types.Version{
											BlockNum: 1,
											TxNum:    0,
										},
									},
								},
							},
						},
					},
				},
			},
			valInfo: []*types.ValidationInfo{
				{
					Flag: types.Flag_INVALID_INCORRECT_ENTRIES,
				},
			},
			query: func(s *provenance.Store, _ string) ([]*types.ValueWithMetadata, error) {
				kvs, err := s.GetValuesReadByUser("user1")
				if err != nil {
					return nil, err
				}

				var values []*types.ValueWithMetadata
				for _, kv := range kvs {
					values = append(
						values,
						&types.ValueWithMetadata{
							Value:    kv.Value,
							Metadata: kv.Metadata,
						},
					)
				}

				return values, nil
			},
			expectedData: nil,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			env := newCommitterTestEnv(t)
			defer env.cleanup()
			setup(env)

			block := &types.Block{
				Header: &types.BlockHeader{
					BaseHeader: &types.BlockHeaderBase{
						Number: 2,
					},
					ValidationInfo: tt.valInfo,
				},
				Payload: &types.Block_DataTxEnvelopes{
					DataTxEnvelopes: &types.DataTxEnvelopes{
						Envelopes: tt.txs,
					},
				},
			}

			_, provenanceData, err := env.committer.constructDBAndProvenanceEntries(block)
			require.NoError(t, err)
			require.NoError(t, env.committer.commitToProvenanceStore(2, provenanceData))

			for _, dbName := range []string{worldstate.DefaultDBName, "db1"} {
				actualData, err := tt.query(env.committer.provenanceStore, dbName)
				require.NoError(t, err)
				require.ElementsMatch(t, tt.expectedData, actualData)
			}
		})
	}
}

func TestProvenanceStoreCommitterForUserBlockWithValidTxs(t *testing.T) {
	t.Parallel()

	sampleVersion := &types.Version{
		BlockNum: 1,
		TxNum:    0,
	}
	user1 := constructUserForTest(t, "user1", []byte("rawcert-user1"), nil, sampleVersion, nil)
	rawUser1New := &types.User{
		Id:          "user1",
		Certificate: []byte("rawcert-user1-new"),
	}

	setup := func(env *committerTestEnv) {
		data := map[string]*worldstate.DBUpdates{
			worldstate.UsersDBName: {
				Writes: []*worldstate.KVWithMetadata{
					user1,
				},
			},
		}
		require.NoError(t, env.committer.db.Commit(data, 1))

		txsData := []*provenance.TxDataForProvenance{
			{
				IsValid: true,
				DBName:  worldstate.UsersDBName,
				UserID:  "user1",
				TxID:    "tx0",
				Writes: []*types.KVWithMetadata{
					{
						Key:      "user1",
						Value:    user1.Value,
						Metadata: user1.Metadata,
					},
				},
			},
		}
		require.NoError(t, env.committer.provenanceStore.Commit(1, txsData))
	}

	tests := []struct {
		name         string
		tx           *types.UserAdministrationTxEnvelope
		valInfo      *types.ValidationInfo
		query        func(s *provenance.Store) ([]*types.ValueWithMetadata, error)
		expectedData []*types.ValueWithMetadata
	}{
		{
			name: "previous link with the already committed value of user1",
			valInfo: &types.ValidationInfo{
				Flag: types.Flag_VALID,
			},
			tx: &types.UserAdministrationTxEnvelope{
				Payload: &types.UserAdministrationTx{
					UserId: "user1",
					TxId:   "tx1",
					UserWrites: []*types.UserWrite{
						{
							User: rawUser1New,
						},
					},
				},
				Signature: nil,
			},
			query: func(s *provenance.Store) ([]*types.ValueWithMetadata, error) {
				return s.GetPreviousValues(
					worldstate.UsersDBName,
					"user1",
					&types.Version{
						BlockNum: 2,
						TxNum:    0,
					},
					-1,
				)
			},
			expectedData: []*types.ValueWithMetadata{
				{
					Value:    user1.Value,
					Metadata: user1.Metadata,
				},
			},
		},
		{
			name: "delete user1",
			tx: &types.UserAdministrationTxEnvelope{
				Payload: &types.UserAdministrationTx{
					UserId: "user1",
					TxId:   "tx1",
					UserDeletes: []*types.UserDelete{
						{
							UserId: "user1",
						},
					},
				},
				Signature: nil,
			},
			valInfo: &types.ValidationInfo{
				Flag: types.Flag_VALID,
			},
			query: func(s *provenance.Store) ([]*types.ValueWithMetadata, error) {
				return s.GetDeletedValues(
					worldstate.UsersDBName,
					"user1",
				)
			},
			expectedData: []*types.ValueWithMetadata{
				{
					Value:    user1.Value,
					Metadata: user1.Metadata,
				},
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			env := newCommitterTestEnv(t)
			defer env.cleanup()
			setup(env)

			block := &types.Block{
				Header: &types.BlockHeader{
					BaseHeader: &types.BlockHeaderBase{
						Number: 2,
					},
					ValidationInfo: []*types.ValidationInfo{
						tt.valInfo,
					},
				},
				Payload: &types.Block_UserAdministrationTxEnvelope{
					UserAdministrationTxEnvelope: tt.tx,
				},
			}

			_, provenanceData, err := env.committer.constructDBAndProvenanceEntries(block)
			require.NoError(t, err)
			require.NoError(t, env.committer.commitToProvenanceStore(2, provenanceData))

			actualData, err := tt.query(env.committer.provenanceStore)
			require.NoError(t, err)
			require.Len(t, actualData, len(tt.expectedData))
			for i, expected := range tt.expectedData {
				require.True(t, proto.Equal(expected, actualData[i]))
			}
		})
	}
}

func TestProvenanceStoreCommitterForConfigBlockWithValidTxs(t *testing.T) {
	t.Parallel()

	clusterConfigWithOneNode := &types.ClusterConfig{
		Nodes: []*types.NodeConfig{
			{
				Id:          "bdb-node-1",
				Certificate: []byte("node-cert"),
				Address:     "127.0.0.1",
				Port:        0,
			},
		},
		Admins: []*types.Admin{
			{
				Id:          "admin1",
				Certificate: []byte("cert-admin1"),
			},
		},
		CertAuthConfig: &types.CAConfig{
			Roots: [][]byte{[]byte("root-ca")},
		},
	}

	clusterConfigWithTwoNodes := proto.Clone(clusterConfigWithOneNode).(*types.ClusterConfig)
	clusterConfigWithTwoNodes.Nodes = append(clusterConfigWithTwoNodes.Nodes, &types.NodeConfig{
		Id:          "bdb-node-2",
		Certificate: []byte("node-2-cert"),
		Address:     "127.0.0.2",
		Port:        0,
	})
	clusterConfigWithTwoNodes.Admins[0].Certificate = []byte("cert-new-admin1")
	clusterConfigWithTwoNodesSerialized, err := proto.Marshal(clusterConfigWithTwoNodes)
	require.NoError(t, err)

	admin1Serialized, err := proto.Marshal(
		&types.User{
			Id:          "admin1",
			Certificate: []byte("cert-admin1"),
			Privilege: &types.Privilege{
				Admin: true,
			},
		},
	)
	require.NoError(t, err)
	admin1NewSerialized, err := proto.Marshal(
		&types.User{
			Id:          "admin1",
			Certificate: []byte("cert-new-admin1"),
			Privilege: &types.Privilege{
				Admin: true,
			},
		},
	)
	require.NoError(t, err)

	node1Serialized, err := proto.Marshal(
		&types.NodeConfig{
			Id:          "bdb-node-1",
			Certificate: []byte("cert-node"),
			Address:     "127.0.0.1",
			Port:        0,
		},
	)
	require.NoError(t, err)
	node2Serialized, err := proto.Marshal(
		&types.NodeConfig{
			Id:          "bdb-node-2",
			Certificate: []byte("node-2-cert"),
			Address:     "127.0.0.2",
			Port:        0,
		},
	)
	require.NoError(t, err)

	setup := func(env *committerTestEnv) {
		sampleMetadata := &types.Metadata{
			Version: &types.Version{
				BlockNum: 1,
				TxNum:    0,
			},
		}

		configUpdates := map[string]*worldstate.DBUpdates{
			worldstate.ConfigDBName: {
				Writes: []*worldstate.KVWithMetadata{
					{
						Key:      worldstate.ConfigKey,
						Value:    clusterConfigWithTwoNodesSerialized,
						Metadata: sampleMetadata,
					},
					{
						Key:      string(identity.NodeNamespace) + "bdb-node-1",
						Value:    node1Serialized,
						Metadata: sampleMetadata,
					},
					{
						Key:      string(identity.NodeNamespace) + "bdb-node-2",
						Value:    node2Serialized,
						Metadata: sampleMetadata,
					},
				},
			},
			worldstate.UsersDBName: {
				Writes: []*worldstate.KVWithMetadata{
					{
						Key:      string(identity.UserNamespace) + "admin1",
						Value:    admin1NewSerialized,
						Metadata: sampleMetadata,
					},
				},
			},
		}
		require.NoError(t, env.committer.db.Commit(configUpdates, 1))

		provenanceData := []*provenance.TxDataForProvenance{
			{
				IsValid: true,
				DBName:  worldstate.ConfigDBName,
				UserID:  "user1",
				TxID:    "tx1",
				Writes: []*types.KVWithMetadata{
					{
						Key:   worldstate.ConfigKey,
						Value: clusterConfigWithTwoNodesSerialized,
						Metadata: &types.Metadata{
							Version: &types.Version{
								BlockNum: 1,
								TxNum:    0,
							},
						},
					},
				},
				Deletes:            make(map[string]*types.Version),
				OldVersionOfWrites: make(map[string]*types.Version),
			},
			{
				IsValid: true,
				DBName:  worldstate.UsersDBName,
				UserID:  "user1",
				TxID:    "tx1",
				Writes: []*types.KVWithMetadata{
					{
						Key:   "admin1",
						Value: admin1NewSerialized,
						Metadata: &types.Metadata{
							Version: &types.Version{
								BlockNum: 1,
								TxNum:    0,
							},
						},
					},
				},
				Deletes:            make(map[string]*types.Version),
				OldVersionOfWrites: make(map[string]*types.Version),
			},
			{
				IsValid: true,
				DBName:  worldstate.ConfigDBName,
				UserID:  "user1",
				TxID:    "tx1",
				Writes: []*types.KVWithMetadata{
					{
						Key:   "bdb-node-1",
						Value: node1Serialized,
						Metadata: &types.Metadata{
							Version: &types.Version{
								BlockNum: 1,
								TxNum:    0,
							},
						},
					},
					{
						Key:   "bdb-node-2",
						Value: node2Serialized,
						Metadata: &types.Metadata{
							Version: &types.Version{
								BlockNum: 1,
								TxNum:    0,
							},
						},
					},
				},
				Deletes:            make(map[string]*types.Version),
				OldVersionOfWrites: make(map[string]*types.Version),
			},
		}
		require.NoError(t, env.committer.provenanceStore.Commit(1, provenanceData))
	}

	tests := []struct {
		name                     string
		tx                       *types.ConfigTx
		valInfo                  *types.ValidationInfo
		queryAdmin               func(s *provenance.Store) ([]*types.ValueWithMetadata, error)
		expectedAdminQueryResult []*types.ValueWithMetadata
		queryNode                func(s *provenance.Store) ([]*types.ValueWithMetadata, error)
		expectedNodeQueryResult  []*types.ValueWithMetadata
	}{
		{
			name: "previous link with the already committed config tx; get deleted value of bdb-node-2 config",
			tx: &types.ConfigTx{
				UserId: "user1",
				TxId:   "tx1",
				ReadOldConfigVersion: &types.Version{
					BlockNum: 1,
					TxNum:    0,
				},
				NewConfig: clusterConfigWithOneNode,
			},
			valInfo: &types.ValidationInfo{
				Flag: types.Flag_VALID,
			},
			queryAdmin: func(s *provenance.Store) ([]*types.ValueWithMetadata, error) {
				return s.GetPreviousValues(
					worldstate.ConfigDBName,
					worldstate.ConfigKey,
					&types.Version{
						BlockNum: 2,
						TxNum:    0,
					},
					-1,
				)
			},
			expectedAdminQueryResult: []*types.ValueWithMetadata{
				{
					Value: clusterConfigWithTwoNodesSerialized,
					Metadata: &types.Metadata{
						Version: &types.Version{
							BlockNum: 1,
							TxNum:    0,
						},
					},
				},
			},
			queryNode: func(s *provenance.Store) ([]*types.ValueWithMetadata, error) {
				return s.GetDeletedValues(
					worldstate.ConfigDBName,
					"bdb-node-2",
				)
			},
			expectedNodeQueryResult: []*types.ValueWithMetadata{
				{
					Value: node2Serialized,
					Metadata: &types.Metadata{
						Version: &types.Version{
							BlockNum: 1,
							TxNum:    0,
						},
					},
				},
			},
		},
		{
			name: "next link with the admin present in committed config tx; get bdb-node-1 config",
			tx: &types.ConfigTx{
				UserId: "user1",
				TxId:   "tx1",
				ReadOldConfigVersion: &types.Version{
					BlockNum: 1,
					TxNum:    0,
				},
				NewConfig: clusterConfigWithOneNode,
			},
			valInfo: &types.ValidationInfo{
				Flag: types.Flag_VALID,
			},
			queryAdmin: func(s *provenance.Store) ([]*types.ValueWithMetadata, error) {
				return s.GetNextValues(
					worldstate.UsersDBName,
					"admin1",
					&types.Version{
						BlockNum: 1,
						TxNum:    0,
					},
					-1,
				)
			},
			expectedAdminQueryResult: []*types.ValueWithMetadata{
				{
					Value: admin1Serialized,
					Metadata: &types.Metadata{
						Version: &types.Version{
							BlockNum: 2,
							TxNum:    0,
						},
					},
				},
			},
			queryNode: func(s *provenance.Store) ([]*types.ValueWithMetadata, error) {
				return s.GetValues(
					worldstate.ConfigDBName,
					"bdb-node-1",
				)
			},
			expectedNodeQueryResult: []*types.ValueWithMetadata{
				{
					Value: node1Serialized,
					Metadata: &types.Metadata{
						Version: &types.Version{
							BlockNum: 1,
							TxNum:    0,
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			env := newCommitterTestEnv(t)
			defer env.cleanup()
			setup(env)

			block := &types.Block{
				Header: &types.BlockHeader{
					BaseHeader: &types.BlockHeaderBase{
						Number: 2,
					},
					ValidationInfo: []*types.ValidationInfo{
						tt.valInfo,
					},
				},
				Payload: &types.Block_ConfigTxEnvelope{
					ConfigTxEnvelope: &types.ConfigTxEnvelope{
						Payload: tt.tx,
					},
				},
			}

			_, provenanceData, err := env.committer.constructDBAndProvenanceEntries(block)
			require.NoError(t, err)
			require.NoError(t, env.committer.commitToProvenanceStore(2, provenanceData))

			actualData, err := tt.queryAdmin(env.committer.provenanceStore)
			require.NoError(t, err)
			require.Len(t, actualData, len(tt.expectedAdminQueryResult))
			for i, expected := range tt.expectedAdminQueryResult {
				require.True(t, proto.Equal(expected, actualData[i]))
			}

			actualData, err = tt.queryNode(env.committer.provenanceStore)
			require.NoError(t, err)
			require.Len(t, actualData, len(tt.expectedNodeQueryResult))
			for i, expected := range tt.expectedNodeQueryResult {
				require.True(t, proto.Equal(expected, actualData[i]))
			}

			txIDs, err := env.committer.provenanceStore.GetTxIDsSubmittedByUser("user1")
			require.NoError(t, err)
			require.ElementsMatch(t, []string{"tx1"}, txIDs)
		})
	}
}

func TestProvenanceStoreCommitterWithInvalidTxs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		block        *types.Block
		query        func(s *provenance.Store) (*provenance.TxIDLocation, error)
		expectedData *provenance.TxIDLocation
		expectedErr  string
	}{
		{
			name: "invalid data tx",
			block: &types.Block{
				Header: &types.BlockHeader{
					BaseHeader: &types.BlockHeaderBase{
						Number: 2,
					},
					ValidationInfo: []*types.ValidationInfo{
						{
							Flag: types.Flag_VALID,
						},
						{
							Flag: types.Flag_INVALID_INCORRECT_ENTRIES,
						},
					},
				},
				Payload: &types.Block_DataTxEnvelopes{
					DataTxEnvelopes: &types.DataTxEnvelopes{
						Envelopes: []*types.DataTxEnvelope{
							{
								Payload: &types.DataTx{
									MustSignUserIds: []string{"user1"},
									TxId:            "tx1",
									DbOperations: []*types.DBOperation{
										{
											DbName: worldstate.DefaultDBName,
										},
									},
								},
							},
							{
								Payload: &types.DataTx{
									MustSignUserIds: []string{"user1"},
									TxId:            "tx2",
									DbOperations: []*types.DBOperation{
										{
											DbName: worldstate.DefaultDBName,
										},
									},
								},
							},
						},
					},
				},
			},
			query: func(s *provenance.Store) (*provenance.TxIDLocation, error) {
				return s.GetTxIDLocation("tx2")
			},
			expectedData: &provenance.TxIDLocation{
				BlockNum: 2,
				TxIndex:  1,
			},
		},
		{
			name: "invalid user admin tx",
			block: &types.Block{
				Header: &types.BlockHeader{
					BaseHeader: &types.BlockHeaderBase{
						Number: 2,
					},
					ValidationInfo: []*types.ValidationInfo{
						{
							Flag: types.Flag_INVALID_INCORRECT_ENTRIES,
						},
					},
				},
				Payload: &types.Block_UserAdministrationTxEnvelope{
					UserAdministrationTxEnvelope: &types.UserAdministrationTxEnvelope{
						Payload: &types.UserAdministrationTx{
							UserId: "user1",
							TxId:   "tx1",
						},
					},
				},
			},
			query: func(s *provenance.Store) (*provenance.TxIDLocation, error) {
				return s.GetTxIDLocation("tx1")
			},
			expectedData: &provenance.TxIDLocation{
				BlockNum: 2,
				TxIndex:  0,
			},
		},
		{
			name: "invalid config tx",
			block: &types.Block{
				Header: &types.BlockHeader{
					BaseHeader: &types.BlockHeaderBase{
						Number: 2,
					},
					ValidationInfo: []*types.ValidationInfo{
						{
							Flag: types.Flag_INVALID_INCORRECT_ENTRIES,
						},
					},
				},
				Payload: &types.Block_ConfigTxEnvelope{
					ConfigTxEnvelope: &types.ConfigTxEnvelope{
						Payload: &types.ConfigTx{
							UserId: "user1",
							TxId:   "tx1",
						},
					},
				},
			},
			query: func(s *provenance.Store) (*provenance.TxIDLocation, error) {
				return s.GetTxIDLocation("tx1")
			},
			expectedData: &provenance.TxIDLocation{
				BlockNum: 2,
				TxIndex:  0,
			},
		},
		{
			name: "non-existing txID",
			block: &types.Block{
				Header: &types.BlockHeader{
					BaseHeader: &types.BlockHeaderBase{
						Number: 2,
					},
					ValidationInfo: []*types.ValidationInfo{
						{
							Flag: types.Flag_INVALID_INCORRECT_ENTRIES,
						},
					},
				},
				Payload: &types.Block_DataTxEnvelopes{
					DataTxEnvelopes: &types.DataTxEnvelopes{
						Envelopes: []*types.DataTxEnvelope{
							{
								Payload: &types.DataTx{
									MustSignUserIds: []string{"user1"},
									TxId:            "tx1",
									DbOperations: []*types.DBOperation{
										{
											DbName: worldstate.DefaultDBName,
										},
									},
								},
							},
						},
					},
				},
			},
			query: func(s *provenance.Store) (*provenance.TxIDLocation, error) {
				return s.GetTxIDLocation("tx-not-there")
			},
			expectedErr: "TxID not found: tx-not-there",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			env := newCommitterTestEnv(t)
			defer env.cleanup()

			_, provenanceData, err := env.committer.constructDBAndProvenanceEntries(tt.block)
			require.NoError(t, err)
			require.NoError(t, env.committer.commitToProvenanceStore(2, provenanceData))

			actualData, err := tt.query(env.committer.provenanceStore)
			if tt.expectedErr == "" {
				require.NoError(t, err)
				require.Equal(t, tt.expectedData, actualData)
			} else {
				require.Equal(t, tt.expectedErr, err.Error())
			}
		})
	}
}

func TestConstructProvenanceEntriesForDataTx(t *testing.T) {
	tests := []struct {
		name                   string
		tx                     *types.DataTx
		version                *types.Version
		setup                  func(db worldstate.DB)
		dirtyWriteKeyVersion   map[string]*types.Version
		expectedProvenanceData []*provenance.TxDataForProvenance
	}{
		{
			name: "tx with only reads",
			tx: &types.DataTx{
				MustSignUserIds: []string{"user1"},
				TxId:            "tx1",
				DbOperations: []*types.DBOperation{
					{
						DbName: worldstate.DefaultDBName,
						DataReads: []*types.DataRead{
							{
								Key: "key1",
								Version: &types.Version{
									BlockNum: 5,
									TxNum:    10,
								},
							},
							{
								Key: "key2",
								Version: &types.Version{
									BlockNum: 9,
									TxNum:    1,
								},
							},
						},
					},
					{
						DbName: "db1",
						DataReads: []*types.DataRead{
							{
								Key: "key3",
								Version: &types.Version{
									BlockNum: 5,
									TxNum:    10,
								},
							},
						},
					},
				},
			},
			version: &types.Version{
				BlockNum: 10,
				TxNum:    3,
			},
			setup: func(db worldstate.DB) {},
			expectedProvenanceData: []*provenance.TxDataForProvenance{
				{
					IsValid: true,
					DBName:  worldstate.DefaultDBName,
					UserID:  "user1",
					TxID:    "tx1",
					Reads: []*provenance.KeyWithVersion{
						{
							Key: "key1",
							Version: &types.Version{
								BlockNum: 5,
								TxNum:    10,
							},
						},
						{
							Key: "key2",
							Version: &types.Version{
								BlockNum: 9,
								TxNum:    1,
							},
						},
					},
					Deletes:            make(map[string]*types.Version),
					OldVersionOfWrites: make(map[string]*types.Version),
				},
				{
					IsValid: true,
					DBName:  "db1",
					UserID:  "user1",
					TxID:    "tx1",
					Reads: []*provenance.KeyWithVersion{
						{
							Key: "key3",
							Version: &types.Version{
								BlockNum: 5,
								TxNum:    10,
							},
						},
					},
					Deletes:            make(map[string]*types.Version),
					OldVersionOfWrites: make(map[string]*types.Version),
				},
			},
		},
		{
			name: "tx with writes and previous version",
			tx: &types.DataTx{
				MustSignUserIds: []string{"user2"},
				TxId:            "tx2",
				DbOperations: []*types.DBOperation{
					{
						DbName: worldstate.DefaultDBName,
						DataWrites: []*types.DataWrite{
							{
								Key:   "key1",
								Value: []byte("value1"),
							},
							{
								Key:   "key2",
								Value: []byte("value2"),
							},
							{
								Key:   "key3",
								Value: []byte("value3"),
							},
						},
					},
					{
						DbName: "db1",
						DataWrites: []*types.DataWrite{
							{
								Key:   "key1",
								Value: []byte("value1"),
							},
							{
								Key:   "key2",
								Value: []byte("value2"),
							},
							{
								Key:   "key3",
								Value: []byte("value3"),
							},
						},
					},
				},
			},
			version: &types.Version{
				BlockNum: 10,
				TxNum:    3,
			},
			setup: func(db worldstate.DB) {
				createDB := map[string]*worldstate.DBUpdates{
					worldstate.DatabasesDBName: {
						Writes: []*worldstate.KVWithMetadata{
							{
								Key: "db1",
							},
						},
					},
				}
				require.NoError(t, db.Commit(createDB, 1))

				writes := []*worldstate.KVWithMetadata{
					{
						Key:   "key1",
						Value: []byte("value1"),
						Metadata: &types.Metadata{
							Version: &types.Version{
								BlockNum: 3,
								TxNum:    3,
							},
						},
					},
					{
						Key:   "key2",
						Value: []byte("value2"),
						Metadata: &types.Metadata{
							Version: &types.Version{
								BlockNum: 5,
								TxNum:    5,
							},
						},
					},
				}

				update := map[string]*worldstate.DBUpdates{
					worldstate.DefaultDBName: {
						Writes: writes,
					},
					"db1": {
						Writes: writes,
					},
				}
				require.NoError(t, db.Commit(update, 1))
			},
			dirtyWriteKeyVersion: map[string]*types.Version{
				constructCompositeKey(worldstate.DefaultDBName, "key1"): {
					BlockNum: 4,
					TxNum:    4,
				},
			},
			expectedProvenanceData: []*provenance.TxDataForProvenance{
				{
					IsValid: true,
					DBName:  worldstate.DefaultDBName,
					UserID:  "user2",
					TxID:    "tx2",
					Writes: []*types.KVWithMetadata{
						{
							Key:   "key1",
							Value: []byte("value1"),
							Metadata: &types.Metadata{
								Version: &types.Version{
									BlockNum: 10,
									TxNum:    3,
								},
							},
						},
						{
							Key:   "key2",
							Value: []byte("value2"),
							Metadata: &types.Metadata{
								Version: &types.Version{
									BlockNum: 10,
									TxNum:    3,
								},
							},
						},
						{
							Key:   "key3",
							Value: []byte("value3"),
							Metadata: &types.Metadata{
								Version: &types.Version{
									BlockNum: 10,
									TxNum:    3,
								},
							},
						},
					},
					Deletes: make(map[string]*types.Version),
					OldVersionOfWrites: map[string]*types.Version{
						"key1": {
							BlockNum: 4,
							TxNum:    4,
						},
						"key2": {
							BlockNum: 5,
							TxNum:    5,
						},
					},
				},
				{
					IsValid: true,
					DBName:  "db1",
					UserID:  "user2",
					TxID:    "tx2",
					Writes: []*types.KVWithMetadata{
						{
							Key:   "key1",
							Value: []byte("value1"),
							Metadata: &types.Metadata{
								Version: &types.Version{
									BlockNum: 10,
									TxNum:    3,
								},
							},
						},
						{
							Key:   "key2",
							Value: []byte("value2"),
							Metadata: &types.Metadata{
								Version: &types.Version{
									BlockNum: 10,
									TxNum:    3,
								},
							},
						},
						{
							Key:   "key3",
							Value: []byte("value3"),
							Metadata: &types.Metadata{
								Version: &types.Version{
									BlockNum: 10,
									TxNum:    3,
								},
							},
						},
					},
					Deletes: make(map[string]*types.Version),
					OldVersionOfWrites: map[string]*types.Version{
						"key1": {
							BlockNum: 3,
							TxNum:    3,
						},
						"key2": {
							BlockNum: 5,
							TxNum:    5,
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			env := newCommitterTestEnv(t)
			defer env.cleanup()
			tt.setup(env.db)

			provenanceData, err := constructProvenanceEntriesForDataTx(env.db, tt.tx, tt.version, tt.dirtyWriteKeyVersion)
			require.NoError(t, err)
			require.Equal(t, tt.expectedProvenanceData, provenanceData)
		})
	}
}

func TestConstructProvenanceEntriesForConfigTx(t *testing.T) {
	clusterConfig := &types.ClusterConfig{
		Nodes: []*types.NodeConfig{
			{
				Id:          "bdb-node-1",
				Certificate: []byte("node-cert"),
				Address:     "127.0.0.1",
				Port:        0,
			},
		},
		Admins: []*types.Admin{
			{
				Id:          "admin1",
				Certificate: []byte("cert-admin1"),
			},
		},
		CertAuthConfig: &types.CAConfig{
			Roots: [][]byte{[]byte("root-ca")},
		},
	}
	clusterConfigSerialized, err := proto.Marshal(clusterConfig)
	require.NoError(t, err)

	tests := []struct {
		name                   string
		tx                     *types.ConfigTx
		version                *types.Version
		expectedProvenanceData *provenance.TxDataForProvenance
	}{
		{
			name: "first cluster config tx",
			tx: &types.ConfigTx{
				UserId:    "user1",
				TxId:      "tx1",
				NewConfig: clusterConfig,
			},
			version: &types.Version{
				BlockNum: 1,
				TxNum:    0,
			},
			expectedProvenanceData: &provenance.TxDataForProvenance{
				IsValid: true,
				DBName:  worldstate.ConfigDBName,
				UserID:  "user1",
				TxID:    "tx1",
				Writes: []*types.KVWithMetadata{
					{
						Key:   worldstate.ConfigKey,
						Value: clusterConfigSerialized,
						Metadata: &types.Metadata{
							Version: &types.Version{
								BlockNum: 1,
								TxNum:    0,
							},
						},
					},
				},
				OldVersionOfWrites: make(map[string]*types.Version),
			},
		},
		{
			name: "updating existing cluster config",
			tx: &types.ConfigTx{
				UserId: "user1",
				TxId:   "tx1",
				ReadOldConfigVersion: &types.Version{
					BlockNum: 1,
					TxNum:    0,
				},
				NewConfig: clusterConfig,
			},
			version: &types.Version{
				BlockNum: 2,
				TxNum:    0,
			},
			expectedProvenanceData: &provenance.TxDataForProvenance{
				IsValid: true,
				DBName:  worldstate.ConfigDBName,
				UserID:  "user1",
				TxID:    "tx1",
				Writes: []*types.KVWithMetadata{
					{
						Key:   worldstate.ConfigKey,
						Value: clusterConfigSerialized,
						Metadata: &types.Metadata{
							Version: &types.Version{
								BlockNum: 2,
								TxNum:    0,
							},
						},
					},
				},
				OldVersionOfWrites: map[string]*types.Version{
					worldstate.ConfigKey: {
						BlockNum: 1,
						TxNum:    0,
					},
				},
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			provenanceData, err := constructProvenanceEntriesForConfigTx(tt.tx, tt.version, &dbEntriesForConfigTx{}, nil)
			require.NoError(t, err)
			require.Equal(t, tt.expectedProvenanceData, provenanceData[0])
		})
	}
}

func constructDataEntryForTest(key string, value []byte, metadata *types.Metadata) *worldstate.KVWithMetadata {
	return &worldstate.KVWithMetadata{
		Key:      key,
		Value:    value,
		Metadata: metadata,
	}
}

func constructAdminEntryForTest(userID string) *types.User {
	return &types.User{
		Id:          userID,
		Certificate: []byte("certificate~" + userID),
		Privilege: &types.Privilege{
			Admin: true,
		},
	}
}

func constructNodeEntryForTest(nodeID string) *types.NodeConfig {
	return &types.NodeConfig{
		Id:          nodeID,
		Address:     "192.168.0.5",
		Port:        1234,
		Certificate: []byte("certificate~" + nodeID),
	}
}
