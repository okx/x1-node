package service

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/0xPolygonHermez/zkevm-node/etherman"
	"github.com/0xPolygonHermez/zkevm-node/etherman/types"
	ethmanTypes "github.com/0xPolygonHermez/zkevm-node/etherman/types"
	"github.com/0xPolygonHermez/zkevm-node/hex"
	"github.com/0xPolygonHermez/zkevm-node/log"
	"github.com/0xPolygonHermez/zkevm-node/tools/sign/config"
	"github.com/ethereum/go-ethereum/common"
	ethTypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// Server is an API backend to handle RPC requests
type Server struct {
	ethCfg etherman.Config
	l1Cfg  etherman.L1Config
	ctx    context.Context

	seqPrivateKey *ecdsa.PrivateKey
	aggPrivateKey *ecdsa.PrivateKey
	ethClient     *etherman.Client

	seqAddress common.Address
	aggAddress common.Address

	result map[string]string
}

// NewServer creates a new server
func NewServer(cfg *config.Config, ctx context.Context) *Server {
	srv := &Server{
		ctx: ctx,
	}

	srv.ethCfg = etherman.Config{
		URL:              cfg.L1.RPC,
		ForkIDChunkSize:  20000, //nolint:gomnd
		MultiGasProvider: false,
	}

	srv.l1Cfg = etherman.L1Config{
		L1ChainID:                 cfg.L1.ChainId,
		ZkEVMAddr:                 cfg.L1.PolygonZkEVMAddress,
		MaticAddr:                 cfg.L1.PolygonMaticAddress,
		GlobalExitRootManagerAddr: cfg.L1.GlobalExitRootManagerAddr,
		DataCommitteeAddr:         cfg.L1.DataCommitteeAddr,
	}

	var err error
	srv.ethClient, err = etherman.NewClient(srv.ethCfg, srv.l1Cfg)
	if err != nil {
		log.Fatal("error creating etherman client. Error: %v", err)
	}

	_, srv.seqPrivateKey, err = srv.ethClient.LoadAuthFromKeyStore(cfg.L1.SeqPrivateKey.Path, cfg.L1.SeqPrivateKey.Password)
	if err != nil {
		log.Fatal("error loading sequencer private key. Error: %v", err)
	}

	srv.seqAddress = crypto.PubkeyToAddress(srv.seqPrivateKey.PublicKey)
	log.Infof("Sequencer address: %s", srv.seqAddress.String())

	_, srv.aggPrivateKey, err = srv.ethClient.LoadAuthFromKeyStore(cfg.L1.AggPrivateKey.Path, cfg.L1.AggPrivateKey.Password)
	if err != nil {
		log.Fatal("error loading aggregator private key. Error: %v", err)
	}

	srv.aggAddress = crypto.PubkeyToAddress(srv.aggPrivateKey.PublicKey)
	log.Infof("Sequencer address: %s", srv.seqAddress.String())

	srv.result = make(map[string]string)

	return srv
}

// Response is the response struct
func sendJSONResponse(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(data) //nolint:errcheck
}

// PostSignDataByOrderNo is the handler for the /priapi/v1/assetonchain/ecology/ecologyOperate endpoint
func (s *Server) PostSignDataByOrderNo(w http.ResponseWriter, r *http.Request) {
	log.Infof("PostSignDataByOrderNo start")
	response := Response{Code: CodeFail, Data: "", DetailMsg: "", Msg: "", Status: 200, Success: false} //nolint:gomnd
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Error reading request body", http.StatusBadRequest)
		response.DetailMsg = err.Error()
		sendJSONResponse(w, response)
		return
	}

	var requestData Request
	err = json.Unmarshal(body, &requestData)
	if err != nil {
		http.Error(w, "Error decoding JSON", http.StatusBadRequest)
		response.DetailMsg = err.Error()
		sendJSONResponse(w, response)
		return
	}

	log.Infof("Request: %v,%v,%v,%v,%v,%v,%v,%v", requestData.OperateType, requestData.OperateAddress, requestData.Symbol, requestData.ProjectSymbol, requestData.RefOrderId, requestData.OperateSymbol, requestData.OperateAmount, requestData.SysFrom)
	if value, ok := s.result[requestData.RefOrderId]; ok {
		response.DetailMsg = "already exist"
		log.Infof("already exist, key:%v, value:%v", requestData.RefOrderId, value)
		sendJSONResponse(w, response)
		return
	}

	if requestData.OperateType == OperateTypeSeq {
		err, data := s.signSeq(requestData)
		if err != nil {
			response.DetailMsg = err.Error()
			log.Errorf("error signSeq: %v", err)
		} else {
			response.Code = CodeSuccess
			response.Success = true
			s.result[requestData.RefOrderId] = data
		}
	} else if requestData.OperateType == OperateTypeAgg {
		err, data := s.signAgg(requestData)
		if err != nil {
			response.DetailMsg = err.Error()
			log.Errorf("error signAgg: %v", err)
		} else {
			response.Code = CodeSuccess
			response.Success = true
			s.result[requestData.RefOrderId] = data
		}
	} else {
		log.Error("error operateType")
		response.DetailMsg = "error operateType"
	}
	sendJSONResponse(w, response)
}

// signSeq is the handler for the /priapi/v1/assetonchain/ecology/ecologyOperate endpoint
func (s *Server) signSeq(requestData Request) (error, string) {
	var seqData SeqData
	err := json.Unmarshal([]byte(requestData.OtherInfo), &seqData)
	if err != nil {
		log.Errorf("error Unmarshal: %v", err)
		return err, ""
	}

	var sequences []types.Sequence
	for _, batch := range seqData.Batches {
		var txsBytes []byte
		txsBytes, err := hex.DecodeHex(batch.Transactions)
		if err != nil {
			return err, ""
		}
		sequences = append(sequences, types.Sequence{
			BatchL2Data:          txsBytes,
			GlobalExitRoot:       common.HexToHash(batch.GlobalExitRoot),
			Timestamp:            batch.Timestamp,
			ForcedBatchTimestamp: batch.MinForcedTimestamp,
		})
	}

	var signData []byte
	signData, err = hex.DecodeHex(seqData.SignaturesAndAddrs)
	if err != nil {
		signData = nil
	}

	to, data, err := s.ethClient.BuildSequenceBatchesTxData(s.seqAddress, sequences, common.HexToAddress(seqData.L2Coinbase), signData)
	if err != nil {
		log.Errorf("error BuildSequenceBatchesTxData: %v", err)
		return err, ""
	}

	nonce, err := s.ethClient.CurrentNonce(s.ctx, s.seqAddress)
	if err != nil {
		log.Errorf("error CurrentNonce: %v", err)
		return err, ""
	}
	log.Infof("CurrentNonce: %v", nonce)
	tx := ethTypes.NewTx(&ethTypes.DynamicFeeTx{
		To:   to,
		Data: data,
	})
	signedTx, err := s.ethClient.SignTx(s.ctx, s.seqAddress, tx) //nolint:staticcheck
	if err != nil {
		log.Errorf("error SignTx: %v", err)
		return err, ""
	}

	gas := uint64(2000000) //nolint:gomnd

	// get gas price
	gasPrice, err := s.ethClient.SuggestedGasPrice(s.ctx)
	if err != nil {
		err := fmt.Errorf("failed to get suggested gas price: %w", err)
		log.Error(err.Error())
		return err, ""
	}
	tx = ethTypes.NewTx(&ethTypes.DynamicFeeTx{
		Nonce:     nonce,
		GasTipCap: gasPrice,
		GasFeeCap: gasPrice,
		Gas:       gas,
		To:        to,
		Data:      data,
	})
	signedTx, err = s.ethClient.SignTx(s.ctx, s.seqAddress, tx)
	if err != nil {
		log.Errorf("error SignTx: %v", err)
		return err, ""
	}

	txBin, err := signedTx.MarshalBinary()
	if err != nil {
		log.Errorf("error MarshalBinary: %v", err)
		return err, ""
	}

	log.Infof("TxHash: %v", signedTx.Hash().String())
	return nil, hex.EncodeToString(txBin)
}

// signAgg is the handler for the /priapi/v1/assetonchain/ecology/ecologyOperate endpoint
func (s *Server) signAgg(requestData Request) (error, string) {
	var aggData AggData
	err := json.Unmarshal([]byte(requestData.OtherInfo), &aggData)
	if err != nil {
		log.Errorf("error Unmarshal: %v", err)
		return err, ""
	}

	newLocal, err := hex.DecodeHex(aggData.NewLocalExitRoot)
	if err != nil {
		log.Errorf("error DecodeHex: %v", err)
		return err, ""
	}

	newStateRoot, err := hex.DecodeHex(aggData.NewStateRoot)
	if err != nil {
		log.Errorf("error DecodeHex: %v", err)
		return err, ""
	}

	var inputs = &ethmanTypes.FinalProofInputs{
		NewLocalExitRoot: newLocal,
		NewStateRoot:     newStateRoot,
	}

	to, data, err := s.ethClient.BuildTrustedVerifyBatchesTxData(aggData.InitNumBatch, aggData.FinalNewBatch, inputs)
	if err != nil {
		log.Errorf("error BuildTrustedVerifyBatchesTxData: %v", err)
		return err, ""
	}

	nonce, err := s.ethClient.CurrentNonce(s.ctx, s.seqAddress)
	if err != nil {
		log.Errorf("error CurrentNonce: %v", err)
		return err, ""
	}

	tx := ethTypes.NewTx(&ethTypes.DynamicFeeTx{
		To:   to,
		Data: data,
	})
	signedTx, err := s.ethClient.SignTx(s.ctx, s.seqAddress, tx) //nolint:staticcheck
	if err != nil {
		log.Errorf("error SignTx: %v", err)
		return err, ""
	}

	gas := uint64(2000000) //nolint:gomnd

	// get gas price
	gasPrice, err := s.ethClient.SuggestedGasPrice(s.ctx)
	if err != nil {
		err := fmt.Errorf("failed to get suggested gas price: %w", err)
		log.Error(err.Error())
		return err, ""
	}

	tx = ethTypes.NewTx(&ethTypes.DynamicFeeTx{
		Nonce:     nonce,
		GasTipCap: gasPrice,
		GasFeeCap: gasPrice,
		Gas:       gas,
		To:        to,
		Data:      data,
	})
	signedTx, err = s.ethClient.SignTx(s.ctx, s.seqAddress, tx)
	if err != nil {
		log.Errorf("error SignTx: %v", err)
		return err, ""
	}

	txBin, err := signedTx.MarshalBinary()
	if err != nil {
		log.Errorf("error MarshalBinary: %v", err)
		return err, ""
	}

	log.Infof("TxHash: %v", signedTx.Hash().String())
	return nil, hex.EncodeToString(txBin)
}

// GetSignDataByOrderNo is the handler for the /priapi/v1/assetonchain/ecology/ecologyOperate endpoint
func (s *Server) GetSignDataByOrderNo(w http.ResponseWriter, r *http.Request) {
	response := Response{Code: CodeFail, Data: "", DetailMsg: "", Msg: "", Status: 200, Success: false} //nolint:gomnd

	orderID := r.URL.Query().Get("orderId")
	projectSymbol := r.URL.Query().Get("projectSymbol")
	log.Infof("GetSignDataByOrderNo: %v,%v", orderID, projectSymbol)
	if value, ok := s.result[orderID]; ok {
		response.Code = CodeSuccess
		response.Success = true
		response.Data = value
	} else {
		response.DetailMsg = "not exist"
	}

	sendJSONResponse(w, response)
}