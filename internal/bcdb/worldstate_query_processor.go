// Copyright IBM Corp. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bcdb

import (
	"context"
	"fmt"

	"github.com/golang/protobuf/proto"
	"github.com/hyperledger-labs/orion-server/internal/blockstore"
	"github.com/hyperledger-labs/orion-server/internal/errors"
	ierrors "github.com/hyperledger-labs/orion-server/internal/errors"
	"github.com/hyperledger-labs/orion-server/internal/identity"
	"github.com/hyperledger-labs/orion-server/internal/queryexecutor"
	"github.com/hyperledger-labs/orion-server/internal/stateindex"
	"github.com/hyperledger-labs/orion-server/internal/worldstate"
	"github.com/hyperledger-labs/orion-server/pkg/logger"
	"github.com/hyperledger-labs/orion-server/pkg/types"
)

type worldstateQueryProcessor struct {
	nodeID          string
	db              worldstate.DB
	blockStore      *blockstore.Store
	identityQuerier *identity.Querier
	logger          *logger.SugarLogger
}

type worldstateQueryProcessorConfig struct {
	nodeID          string
	db              worldstate.DB
	blockStore      *blockstore.Store
	identityQuerier *identity.Querier
	logger          *logger.SugarLogger
}

func newWorldstateQueryProcessor(conf *worldstateQueryProcessorConfig) *worldstateQueryProcessor {
	return &worldstateQueryProcessor{
		nodeID:          conf.nodeID,
		db:              conf.db,
		blockStore:      conf.blockStore,
		identityQuerier: conf.identityQuerier,
		logger:          conf.logger,
	}
}

func (q *worldstateQueryProcessor) isDBExists(name string) bool {
	return q.db.Exist(name)
}

// getDBStatus returns the status about a database, i.e., whether a database exist or not
func (q *worldstateQueryProcessor) getDBStatus(dbName string) (*types.GetDBStatusResponse, error) {
	// ACL is meaningless here as this call is to check whether a DB exist. Even with ACL,
	// the user can infer the information.
	return &types.GetDBStatusResponse{
		Exist: q.isDBExists(dbName),
	}, nil
}

// getState return the state associated with a given key
func (q *worldstateQueryProcessor) getData(dbName, querierUserID, key string) (*types.GetDataResponse, error) {
	if worldstate.IsSystemDB(dbName) {
		return nil, &errors.PermissionErr{
			ErrMsg: "no user can directly read from a system database [" + dbName + "]. " +
				"To read from a system database, use /config, /user, /db rest endpoints instead of /data",
		}
	}

	hasPerm, err := q.identityQuerier.HasReadAccessOnDataDB(querierUserID, dbName)
	if err != nil {
		return nil, err
	}
	if !hasPerm {
		return nil, &errors.PermissionErr{
			ErrMsg: "the user [" + querierUserID + "] has no permission to read from database [" + dbName + "]",
		}
	}

	value, metadata, err := q.db.Get(dbName, key)
	if err != nil {
		return nil, err
	}

	acl := metadata.GetAccessControl()
	if acl != nil {
		if !acl.ReadUsers[querierUserID] && !acl.ReadWriteUsers[querierUserID] {
			return nil, &errors.PermissionErr{
				ErrMsg: "the user [" + querierUserID + "] has no permission to read key [" + key + "] from database [" + dbName + "]",
			}
		}
	}

	return &types.GetDataResponse{
		Value:    value,
		Metadata: metadata,
	}, nil
}

func (q *worldstateQueryProcessor) getUser(querierUserID, targetUserID string) (*types.GetUserResponse, error) {
	user, metadata, err := q.identityQuerier.GetUser(targetUserID)
	if err != nil {
		if _, ok := err.(*identity.NotFoundErr); !ok {
			return nil, err
		}
	}

	acl := metadata.GetAccessControl()
	if acl != nil {
		if !acl.ReadUsers[querierUserID] && !acl.ReadWriteUsers[querierUserID] {
			return nil, &errors.PermissionErr{
				ErrMsg: "the user [" + querierUserID + "] has no permission to read info of user [" + targetUserID + "]",
			}
		}
	}

	return &types.GetUserResponse{
		User:     user,
		Metadata: metadata,
	}, nil
}

func (q *worldstateQueryProcessor) getConfig(querierUserID string) (*types.GetConfigResponse, error) {
	// Limited access to admins only. Regular users can use the `GetNodeConfig` or `GetClusterStatus` APIs to discover
	// and fetch the details of nodes that are needed for external cluster access.
	isAdmin, err := q.identityQuerier.HasAdministrationPrivilege(querierUserID)
	if err != nil {
		return nil, err
	}
	if !isAdmin {
		return nil, &errors.PermissionErr{
			ErrMsg: "the user [" + querierUserID + "] has no permission to read a config object",
		}
	}

	config, metadata, err := q.db.GetConfig()
	if err != nil {
		return nil, err
	}

	return &types.GetConfigResponse{
		Config:   config,
		Metadata: metadata,
	}, nil
}

func (q *worldstateQueryProcessor) getNodeConfigAndMetadata() ([]*types.NodeConfig, *types.Metadata, error) {
	config, metadata, err := q.db.GetConfig()
	if err != nil {
		return nil, nil, err
	}

	return config.Nodes, metadata, nil
}

func (q *worldstateQueryProcessor) getNodeConfig(nodeID string) (*types.GetNodeConfigResponse, error) {
	nodeConfig, _, err := q.identityQuerier.GetNode(nodeID)
	if err != nil {
		if _, ok := err.(*identity.NotFoundErr); !ok {
			return nil, err
		}
	}

	c := &types.GetNodeConfigResponse{
		NodeConfig: nodeConfig,
	}

	return c, nil
}

func (q *worldstateQueryProcessor) getConfigBlock(querierUserID string, blockNumber uint64) (*types.GetConfigBlockResponse, error) {
	isAdmin, err := q.identityQuerier.HasAdministrationPrivilege(querierUserID)
	if err != nil {
		return nil, err
	}
	if !isAdmin {
		return nil, &errors.PermissionErr{
			ErrMsg: "the user [" + querierUserID + "] has no permission to read a config block",
		}
	}

	if blockNumber == 0 {
		_, metadata, err := q.db.GetConfig()
		if err != nil {
			return nil, err
		}
		blockNumber = metadata.GetVersion().GetBlockNum()
	}
	block, err := q.blockStore.Get(blockNumber)
	if err != nil {
		return nil, err
	}
	if isConfig := block.GetConfigTxEnvelope(); isConfig == nil {
		return nil, &ierrors.NotFoundErr{Message: fmt.Sprintf("block [%d] is not a config block", blockNumber)}
	}

	blockBytes, err := proto.Marshal(block)
	if err != nil {
		return nil, err
	}
	return &types.GetConfigBlockResponse{
		Block: blockBytes,
	}, nil
}

func (q *worldstateQueryProcessor) executeJSONQuery(ctx context.Context, dbName, querierUserID string, query []byte) (*types.DataQueryResponse, error) {
	if worldstate.IsSystemDB(dbName) {
		return nil, &errors.PermissionErr{
			ErrMsg: "no user can directly read from a system database [" + dbName + "]. " +
				"To read from a system database, use /config, /user, /db rest endpoints instead of /data",
		}
	}

	hasPerm, err := q.identityQuerier.HasReadAccessOnDataDB(querierUserID, dbName)
	if err != nil {
		return nil, err
	}
	if !hasPerm {
		return nil, &errors.PermissionErr{
			ErrMsg: "the user [" + querierUserID + "] has no permission to read from database [" + dbName + "]",
		}
	}

	snapshots, err := q.db.GetDBsSnapshot(
		[]string{
			worldstate.DatabasesDBName,
			dbName,
			stateindex.IndexDB(dbName),
		},
	)
	if err != nil {
		return nil, err
	}
	defer func() {
		snapshots.Release()
	}()

	jsonQueryExecutor := queryexecutor.NewWorldStateJSONQueryExecutor(snapshots, q.logger)
	keys, err := jsonQueryExecutor.ExecuteQuery(ctx, dbName, query)
	select {
	case <-ctx.Done():
		return nil, nil
	default:
		if err != nil {
			return nil, err
		}
	}

	var results []*types.KVWithMetadata

	for k := range keys {
		select {
		case <-ctx.Done():
			return nil, nil
		default:
			value, metadata, err := snapshots.Get(dbName, k)
			if err != nil {
				return nil, err
			}

			// TODO: we can store the ACL as value in the indexEntry. With that, we can avoid reading the whole value
			// to perform the access control - issue #152
			acl := metadata.GetAccessControl()
			if acl != nil {
				if !acl.ReadUsers[querierUserID] && !acl.ReadWriteUsers[querierUserID] {
					continue
				}
			}

			results = append(
				results,
				&types.KVWithMetadata{
					Key:      k,
					Value:    value,
					Metadata: metadata,
				},
			)
		}
	}

	return &types.DataQueryResponse{
		KVs: results,
	}, nil
}
