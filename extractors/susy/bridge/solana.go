package bridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"math/big"

	"github.com/Gravity-Tech/gateway/abi/ethereum/luport"
	"github.com/Gravity-Tech/gravity-node-data-extractor/v2/extractors"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/mr-tron/base58"

	// solcommand "github.com/Gravity-Tech/solanoid/commands"
	solexecutor "github.com/Gravity-Tech/solanoid/commands/executor"
	solclient "github.com/portto/solana-go-sdk/client"
)

type SolanaExtractionProvider struct{}

type EthereumToSolanaExtractionBridge struct {
	config     ConfigureCommand
	configured   bool


	ethClient      *ethclient.Client

	luPortContract *luport.LUPort
	
	solanaClient   *solclient.Client
	solanaCtx       context.Context
	// solanaExecutor *solexecutor.GenericExecutor

	IBPortDataAccount string
}

func (provider *EthereumToSolanaExtractionBridge) Configure(config ConfigureCommand) error {
	if provider.configured {
		return fmt.Errorf("bridge is configured already")
	}

	provider.config = config

	// Node clients instantiation
	var err error
	provider.ethClient, err = ethclient.DialContext(context.Background(), config.SourceNodeUrl)
	if err != nil {
		return err
	}
	provider.luPortContract, err = luport.NewLUPort(common.HexToAddress(config.LUPortAddress), provider.ethClient)
	if err != nil {
		return err
	}

	provider.solanaClient = solclient.NewClient(config.DestinationNodeUrl)
	provider.solanaCtx = context.Background()

	provider.IBPortDataAccount = config.IBPortAddress

	// provider.solanaExecutor, err = solcommand.InitGenericExecutor(

	// )

	return nil
}

func (provider *EthereumToSolanaExtractionBridge) rqBytesToBigInt(rqId [32]byte) *big.Int {
	id := big.NewInt(0)
	id.SetBytes(rqId[:])
	return id
}


func (provider *EthereumToSolanaExtractionBridge) pickRequestFromQueue(luState *luport.LUPort, firstRqId []byte) (SwapID, *big.Int, error) {
	first := *byte32(firstRqId)

	if luState == nil || first == [32]byte{} {
		return *new(SwapID), nil, fmt.Errorf("invalid input")
	}

	ibState, err := provider.IBPortState()
	if err != nil {
		return *new(SwapID), nil, err
	}

	var rqIdInt *big.Int

	for rqIdInt = provider.rqBytesToBigInt(first); rqIdInt != nil; rqIdInt, _ = luState.NextRq(nil, rqIdInt) {
		/**
		 * Due to a fact, that current gateway implementation
		 * on smart contracts (ports) does not have additional
		 * confirmation tx, we should check just for the existence of the swap with that id
		 */
		//if ibRequest := ibState.Request(wavesRequestId); ibRequest != nil && Status(ibRequest.Status) == CompletedStatus {
		// if ibRequest := ibState.Request(wavesRequestId); ibRequest != nil && Status(ibRequest.Status) != CompletedStatus {
		// 	continue
		// }
		var requestIdFixed SwapID
		copy(requestIdFixed[:], rqIdInt.Bytes()[0:16]) 
		
		fmt.Printf("ibState: %v \n", ibState)

		ibRequestStatus := ibState.SwapStatusDict[requestIdFixed]
		
		fmt.Printf("status: %v \n", ibRequestStatus)
		if ibRequestStatus != nil && *ibRequestStatus != EthereumRequestStatusSuccess {
			continue
		}

		// validate target address
		luRequest, err := luState.Requests(nil, rqIdInt)
		if err != nil {
			continue
		}

		fmt.Printf("Solana Address: %v \n", base58.Encode(luRequest.ForeignAddress[0:32]))
		if !ValidateSolanaAddress(base58.Encode(luRequest.ForeignAddress[0:32])) {
			continue
		}

		isTokenDataAccountPassed, tokenDataErr := ValidateSolanaTokenAccountOwnershipByTokenProgram(
			provider.solanaClient, 
			base58.Encode(luRequest.ForeignAddress[0:32]),
			provider.config.Meta,
		)
		if tokenDataErr != nil || !isTokenDataAccountPassed {
			continue
		}

		if luRequest.Amount.Uint64() == 0 {
			continue
		}

		fmt.Printf("rqID last: %v \n", rqIdInt.Bytes()[0:16])

		break
	}

	fmt.Printf("rqID on input: %v \n", rqIdInt.Bytes()[0:16])

	if rqIdInt == nil {
		return *new(SwapID), nil, extractors.NotFoundErr
	}

	var swapID SwapID
	copy(swapID[:], rqIdInt.Bytes()[0:16])

	return swapID, rqIdInt, nil
}

func (provider *EthereumToSolanaExtractionBridge) IBPortState() (*IBPortContractState, error) {
	ibportStateResult, err := provider.solanaClient.GetAccountInfo(provider.solanaCtx, provider.IBPortDataAccount, solclient.GetAccountInfoConfig{
		Encoding: "base64",
	})
	if err != nil {
		return nil, err
	}

	ibportState := ibportStateResult.Data.([]interface{})[0].(string)

	stateDecoded, err := base64.StdEncoding.DecodeString(ibportState)
	if err != nil {
		return nil, err
	}

	return DecodeIBPortState(stateDecoded), nil
}

func (provider *EthereumToSolanaExtractionBridge) requestSerializer(array []byte) string {
	return base64.StdEncoding.EncodeToString(array)
}

func (provider *EthereumToSolanaExtractionBridge) ExtractDirectTransferRequest(ctx context.Context) (*extractors.Data, error) {
	
	// // pick up unprocessed request
	// for swapId, requestStatus := range ibportContractState.SwapStatusDict {
	// 	if requestStatus == EthereumRequestStatusNew {
	// 		currentSwapId = swapId
	// 	}
	// }
	luRequestIds, err := provider.luPortContract.RequestsQueue(nil)
	
	if bytes.Equal(luRequestIds.First[:], make([]byte, 32)) {
		return nil, extractors.NotFoundErr
	}

	if err != nil {
		return nil, err
	}

	rqId, rqIdInt, err := provider.pickRequestFromQueue(provider.luPortContract, luRequestIds.First[:])
	if err != nil {
		return nil, err
	}
	if rqIdInt == nil {
		return nil, extractors.NotFoundErr
	}

	luRequest, err := provider.luPortContract.Requests(nil, rqIdInt)

	decimalsDiff := big.NewInt(provider.config.SourceDecimals - provider.config.DestinationDecimals)

	divideBy := big.NewInt(0).Exp(big.NewInt(10), decimalsDiff, nil)

	targetAmount := luRequest.Amount.Div(luRequest.Amount, divideBy).Uint64()

	solanaDecimals := big.NewInt(0).
		Exp(big.NewInt(10), big.NewInt(provider.config.DestinationDecimals), nil)

	targetAmountCasted := float64(targetAmount) / float64(solanaDecimals.Uint64())

	fmt.Printf("rqId[:]: %v \n", rqId[:])
	fmt.Printf("luRequest.ForeignAddress: %v \n", luRequest.ForeignAddress)
	fmt.Printf("luRequest.ForeignAddress: %v \n", base58.Encode(luRequest.ForeignAddress[:]))
	fmt.Printf("targetAmount: %v \n", targetAmount)
	fmt.Printf("targetAmountCasted: %v \n", targetAmountCasted)

	var resultByteVector [64]byte
	copy(resultByteVector[:], solexecutor.BuildCrossChainMintByteVector(rqId[:], luRequest.ForeignAddress, targetAmountCasted))

	fmt.Printf("result byte string: %v \n", provider.requestSerializer(resultByteVector[:]))
	fmt.Printf("result byte string(len): %v \n", len(resultByteVector))

	return &extractors.Data{
		Type:  extractors.Base64,
		Value: provider.requestSerializer(resultByteVector[:]),
	}, err
}

func (provider *EthereumToSolanaExtractionBridge) ExtractReverseTransferRequest(ctx context.Context) (*extractors.Data, error) {
	ibState, err := provider.IBPortState()
	if err != nil {
		return nil, err
	}

	var rqSwapID SwapID
	var reverseRequest *IBPortContractUnwrapRequest

	for swapID, burnRequest := range ibState.RequestsDict {
		status := ibState.SwapStatusDict[swapID]

		if status == nil {
			continue
		}

		if *status != EthereumRequestStatusNew {
			continue
		}

		luRequest, err := provider.luPortContract.Requests(nil, swapID.AsBigInt())
		if err != nil {
			return nil, err
		}

		// fmt.Printf("luRequest: %+v \n", luRequest)

		_ = luRequest
		// TODO: Bring back
		if luRequest.Status != EthereumRequestStatusNone {
			continue
		}

		fmt.Printf("EVM Address: %v \n", hexutil.Encode(luRequest.ForeignAddress[0:20]))

		if !ValidateEthereumBasedAddress(hexutil.Encode(luRequest.ForeignAddress[0:20])) {
			continue
		}

		if burnRequest.Amount == 0 {
			continue
		}

		reverseRequest = burnRequest
		rqSwapID = swapID
		break
	}

	if reverseRequest == nil {
		return nil, extractors.NotFoundErr
	}

	// fmt.Println("request info")
	// fmt.Printf("amount: %v; \norigin: %v; \ndest: %v; \n", reverseRequest.Amount, solcommon.PublicKeyFromBytes(reverseRequest.OriginAddress[0:32]).ToBase58(), hexutil.Encode(reverseRequest.ForeignAddress[0:20]))

	amount := MapAmount(int64(reverseRequest.Amount), provider.config.DestinationDecimals, provider.config.SourceDecimals)

	fmt.Printf("amount mapped: %v \n", amount)

	result := []byte{'u'} // means 'unlock'
	result = append(result, rqSwapID[:]...)

	var amountBytes [32]byte
	amount.FillBytes(amountBytes[:])

	result = append(result, amountBytes[:]...)
	result = append(result, reverseRequest.ForeignAddress[0:20]...)

	var resultByteVector [64]byte
	copy(resultByteVector[:], result)

	fmt.Printf("swapID: %v \n", rqSwapID[:])
	fmt.Printf("receiver: %v \n", hexutil.Encode(reverseRequest.ForeignAddress[0:20]))
	
	fmt.Printf("amount: %v \n", amount.String())
	fmt.Printf("amount(bytes): %v \n", amount.Bytes())
	fmt.Printf("amountBytes: %v \n", amountBytes)

	fmt.Printf("result byte string: %v \n", provider.requestSerializer(resultByteVector[:]))
	fmt.Printf("result byte string(len): %v \n", len(resultByteVector))

	return &extractors.Data{
		Type:  extractors.Base64,
		Value: provider.requestSerializer(resultByteVector[:]),
	}, err
}
