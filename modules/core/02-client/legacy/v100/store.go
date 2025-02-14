package v100

import (
	"fmt"
	"strings"

	"github.com/cosmos/cosmos-sdk/codec"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	"github.com/cosmos/cosmos-sdk/store/prefix"
	storetypes "github.com/cosmos/cosmos-sdk/store/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"

	clienttypes "github.com/cosmos/ibc-go/v6/modules/core/02-client/types"
	host "github.com/cosmos/ibc-go/v6/modules/core/24-host"
	"github.com/cosmos/ibc-go/v6/modules/core/exported"
	solomachine "github.com/cosmos/ibc-go/v6/modules/light-clients/06-solomachine"
	ibctm "github.com/cosmos/ibc-go/v6/modules/light-clients/07-tendermint"
)

// MigrateStore performs in-place store migrations from SDK v0.40 of the IBC module to v1.0.0 of ibc-go.
// The migration includes:
//
// - Migrating solo machine client states from v1 to v2 protobuf definition
// - Pruning all solo machine consensus states
// - Pruning expired tendermint consensus states
// - Adds ProcessedHeight and Iteration keys for unexpired tendermint consensus states
func MigrateStore(ctx sdk.Context, storeKey storetypes.StoreKey, cdc codec.BinaryCodec) (err error) {
	store := ctx.KVStore(storeKey)
	iterator := sdk.KVStorePrefixIterator(store, host.KeyClientStorePrefix)

	var clients []string

	// collect all clients
	defer iterator.Close()
	for ; iterator.Valid(); iterator.Next() {
		keySplit := strings.Split(string(iterator.Key()), "/")
		if keySplit[len(keySplit)-1] != host.KeyClientState {
			continue
		}

		// key is clients/{clientid}/clientState
		// Thus, keySplit[1] is clientID
		clients = append(clients, keySplit[1])
	}

	for _, clientID := range clients {
		clientType, _, err := clienttypes.ParseClientIdentifier(clientID)
		if err != nil {
			return err
		}

		clientPrefix := []byte(fmt.Sprintf("%s/%s/", host.KeyClientStorePrefix, clientID))
		clientStore := prefix.NewStore(ctx.KVStore(storeKey), clientPrefix)

		bz := clientStore.Get(host.ClientStateKey())
		if bz == nil {
			return clienttypes.ErrClientNotFound
		}

		switch clientType {
		case exported.Solomachine:
			any := &codectypes.Any{}
			if err := cdc.Unmarshal(bz, any); err != nil {
				return sdkerrors.Wrap(err, "failed to unmarshal client state bytes into solo machine client state")
			}

			clientState := &ClientState{}
			if err := cdc.Unmarshal(any.Value, clientState); err != nil {
				return sdkerrors.Wrap(err, "failed to unmarshal client state bytes into solo machine client state")
			}

			updatedClientState := migrateSolomachine(clientState)

			bz, err := clienttypes.MarshalClientState(cdc, updatedClientState)
			if err != nil {
				return sdkerrors.Wrap(err, "failed to unmarshal client state bytes into solo machine client state")
			}

			// update solomachine in store
			clientStore.Set(host.ClientStateKey(), bz)

			pruneSolomachineConsensusStates(clientStore)

		case exported.Tendermint:
			var clientState exported.ClientState
			if err := cdc.UnmarshalInterface(bz, &clientState); err != nil {
				return sdkerrors.Wrap(err, "failed to unmarshal client state bytes into tendermint client state")
			}

			tmClientState, ok := clientState.(*ibctm.ClientState)
			if !ok {
				return sdkerrors.Wrap(clienttypes.ErrInvalidClient, "client state is not tendermint even though client id contains 07-tendermint")
			}

			// add iteration keys so pruning will be successful
			addConsensusMetadata(ctx, clientStore)

			ibctm.PruneAllExpiredConsensusStates(ctx, clientStore, cdc, tmClientState)
		default:
			continue
		}
	}

	return nil
}

// migrateSolomachine migrates the solomachine from v1 to v2 solo machine protobuf definition.
func migrateSolomachine(clientState *ClientState) *solomachine.ClientState {
	isFrozen := clientState.FrozenSequence != 0
	consensusState := &solomachine.ConsensusState{
		PublicKey:   clientState.ConsensusState.PublicKey,
		Diversifier: clientState.ConsensusState.Diversifier,
		Timestamp:   clientState.ConsensusState.Timestamp,
	}

	return &solomachine.ClientState{
		Sequence:       clientState.Sequence,
		IsFrozen:       isFrozen,
		ConsensusState: consensusState,
	}
}

// pruneSolomachineConsensusStates removes all solomachine consensus states from the
// client store.
func pruneSolomachineConsensusStates(clientStore sdk.KVStore) {
	iterator := sdk.KVStorePrefixIterator(clientStore, []byte(host.KeyConsensusStatePrefix))
	var heights []exported.Height

	defer iterator.Close()
	for ; iterator.Valid(); iterator.Next() {
		keySplit := strings.Split(string(iterator.Key()), "/")
		// key is in the format "consensusStates/<height>"
		if len(keySplit) != 2 || keySplit[0] != string(host.KeyConsensusStatePrefix) {
			continue
		}

		// collect consensus states to be pruned
		heights = append(heights, clienttypes.MustParseHeight(keySplit[1]))
	}

	// delete all consensus states
	for _, height := range heights {
		clientStore.Delete(host.ConsensusStateKey(height))
	}
}

// addConsensusMetadata adds the iteration key and processed height for all tendermint consensus states
// These keys were not included in the previous release of the IBC module. Adding the iteration keys allows
// for pruning iteration.
func addConsensusMetadata(ctx sdk.Context, clientStore sdk.KVStore) {
	var heights []exported.Height
	iterator := sdk.KVStorePrefixIterator(clientStore, []byte(host.KeyConsensusStatePrefix))

	defer iterator.Close()
	for ; iterator.Valid(); iterator.Next() {
		keySplit := strings.Split(string(iterator.Key()), "/")
		// consensus key is in the format "consensusStates/<height>"
		if len(keySplit) != 2 {
			continue
		}

		heights = append(heights, clienttypes.MustParseHeight(keySplit[1]))
	}

	for _, height := range heights {
		// set the iteration key and processed height
		// these keys were not included in the SDK v0.42.0 release
		ibctm.SetProcessedHeight(clientStore, height, clienttypes.GetSelfHeight(ctx))
		ibctm.SetIterationKey(clientStore, height)
	}
}
