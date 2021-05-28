package service

import (
	"encoding/hex"
	"fmt"
	"github.com/joeqian10/neo3-gogogo/block"
	"github.com/joeqian10/neo3-gogogo/crypto"
	"github.com/joeqian10/neo3-gogogo/helper"
	"github.com/joeqian10/neo3-gogogo/io"
	"github.com/joeqian10/neo3-gogogo/mpt"
	"github.com/polynetwork/neo3-relayer/common"
	"github.com/polynetwork/neo3-relayer/db"
	"github.com/polynetwork/neo3-relayer/log"
	pCommon "github.com/polynetwork/poly/common"
	hsCommon "github.com/polynetwork/poly/native/service/header_sync/common"
	"github.com/polynetwork/poly/native/service/header_sync/neo"
	relayUtils "github.com/polynetwork/poly/native/service/utils"
	"strconv"
	"strings"
	"time"
)

// GetCurrentRelayChainSyncHeight :get the synced NEO blockHeight from Relay Chain
func (this *SyncService) GetCurrentRelayChainSyncHeight(neoChainID uint64) (uint32, error) {
	contractAddress := relayUtils.HeaderSyncContractAddress
	neoChainIDBytes := common.GetUint64Bytes(neoChainID)
	key := common.ConcatKey([]byte(hsCommon.CONSENSUS_PEER), neoChainIDBytes)
	value, err := this.relaySdk.ClientMgr.GetStorage(contractAddress.ToHexString(), key)
	if err != nil {
		return 0, fmt.Errorf("getStorage error: %s", err)
	}
	neoConsensusPeer := new(neo.NeoConsensus)
	if err := neoConsensusPeer.Deserialization(pCommon.NewZeroCopySource(value)); err != nil {
		return 0, fmt.Errorf("neoconsensus peer deserialize err: %s", err)
	}

	height := neoConsensusPeer.Height
	height++
	return height, nil
}

//syncHeaderToRelay : Sync NEO block head to Relay Chain
func (this *SyncService) syncHeaderToRelay(height uint32) error {
	chainIDBytes := relayUtils.GetUint64Bytes(this.config.NeoChainID)
	heightBytes := relayUtils.GetUint32Bytes(height)
	v, err := this.relaySdk.GetStorage(relayUtils.HeaderSyncContractAddress.ToHexString(), common.ConcatKey([]byte(hsCommon.HEADER_INDEX), chainIDBytes, heightBytes))
	if len(v) != 0 {
		return nil
	}

	//Get NEO BlockHeader for syncing
	response := this.neoSdk.GetBlockHeader(strconv.Itoa(int(height)))
	if response.HasError() {
		return fmt.Errorf("[syncHeaderToRelay] neoSdk.GetBlockByIndex error: %s", response.Error.Message)
	}
	rpcBH := response.Result
	blockHeader, err := block.NewBlockHeaderFromRPC(&rpcBH)
	if err != nil {
		return err
	}
	buff := io.NewBufBinaryWriter()
	blockHeader.Serialize(buff.BinaryWriter)
	header := buff.Bytes()
	log.Infof(helper.BytesToHex(header))

	var txHash pCommon.Uint256
	var txErr error
	//Sending transaction to Relay Chain
	txHash, txErr = this.relaySdk.Native.Hs.SyncBlockHeader(this.config.NeoChainID, this.relayAccount.Address, [][]byte{header}, this.relayAccount)

	if txErr != nil {
		return fmt.Errorf("[syncHeaderToRelay] relaySdk.SyncBlockHeader error: %s, neo header: %s", txErr, helper.BytesToHex(header))
	}
	log.Infof("[syncHeaderToRelay] txHash is: %s", txHash.ToHexString())
	this.waitForRelayBlock()
	return nil
}

//syncProofToRelay : send StateRoot Proof to Relay Chain
func (this *SyncService) syncProofToRelay(key string, height uint32) error {
	retry := &db.Retry{
		Height: height,
		Key:    key,
	}
	sink := pCommon.NewZeroCopySink(nil)
	retry.Serialization(sink)

	//get current state height
	var stateHeight uint32 = 0
	for stateHeight < height {
		res := this.neoSdk.GetStateHeight()
		if res.HasError() {
			this.db.PutRetry(sink.Bytes())
			return fmt.Errorf("[syncProofToRelay] neoSdk.GetStateHeight error: %s", res.Error.Message)
		}
		stateHeight = res.Result.ValidateRootIndex
	}

	// get state root
	srGot := false
	stateRoot := mpt.StateRoot{}
	height2 := height
	for !srGot {
		res2 := this.neoSdk.GetStateRoot(height2)
		if res2.HasError() {
			this.db.PutRetry(sink.Bytes())
			return fmt.Errorf("[syncProofToRelay] neoSdk.GetStateRootByIndex error: %s", res2.Error.Message)
		}
		stateRoot = res2.Result
		if len(stateRoot.Witnesses) == 0 {
			height2++
		} else {
			srGot = true
		}
	}
	buff := io.NewBufBinaryWriter()
	stateRoot.Serialize(buff.BinaryWriter)
	crossChainMsg := buff.Bytes()
	log.Infof("stateroot: %s", helper.BytesToHex(crossChainMsg))

	//msg := this.GetStateRootMsg(stateRoot)
	//log.Infof("message: %s", helper.BytesToHex(msg))
	//
	//log.Infof("invocation: %s", stateRoot.Witnesses[0].Invocation)
	//log.Infof("verification: %s", stateRoot.Witnesses[0].Verification)

	// get proof
	res3 := this.neoSdk.GetProof(stateRoot.RootHash, this.config.NeoCCMC, crypto.Base64Encode(helper.HexToBytes(key)))
	if res3.HasError() {
		return fmt.Errorf("[syncProofToRelay] neoSdk.GetProof error: %s", res3.Error.Message)
	}
	proof, err := crypto.Base64Decode(res3.Result)
	if err != nil {
		return fmt.Errorf("[syncProofToRelay] decode proof error: %s", err)
	}
	log.Info("proof: %s", helper.BytesToHex(proof))

	//sending SyncProof transaction to Relay Chain
	txHash, err := this.relaySdk.Native.Ccm.ImportOuterTransfer(this.config.NeoChainID, nil, height, proof, this.relayAccount.Address[:], crossChainMsg, this.relayAccount)
	if err != nil {
		if strings.Contains(err.Error(), "chooseUtxos, current utxo is not enough") {
			log.Infof("[syncProofToRelay] invokeNativeContract error: %s", err)
			err = this.db.PutRetry(sink.Bytes())
			if err != nil {
				return fmt.Errorf("[syncProofToRelay] this.db.PutRetry error: %s", err)
			}
			log.Infof("[syncProofToRelay] put tx into retry db, height %d, key %s", height, key)
			return nil
		} else if strings.Contains(err.Error(), "checkDoneTx, tx already done") {
			return fmt.Errorf("[syncProofToRelay] invokeNativeContract error: %s", err)
		} else {
			return fmt.Errorf("[syncProofToRelay] invokeNativeContract error: %s, crossChainMsg: %s, proof: %s", err, helper.BytesToHex(crossChainMsg), helper.BytesToHex(proof))
		}
	}

	err = this.db.PutCheck(txHash.ToHexString(), sink.Bytes())
	if err != nil {
		return fmt.Errorf("[syncProofToRelay] this.db.PutCheck error: %s", err)
	}

	log.Infof("[syncProofToRelay] polyTxHash is: %s", txHash.ToHexString())
	return nil
}

func (this *SyncService) retrySyncProofToRelay(v []byte) error {
	retry := new(db.Retry)
	err := retry.Deserialization(pCommon.NewZeroCopySource(v))
	if err != nil {
		return fmt.Errorf("[retrySyncProofToRelay] retry.Deserialization error: %s", err)
	}

	// get state root
	res2 := this.neoSdk.GetStateRoot(retry.Height)
	if res2.HasError() {
		return fmt.Errorf("[retrySyncProofToRelay] neoSdk.GetStateRootByIndex error: %s", res2.Error.Message)
	}
	stateRoot := res2.Result
	buff := io.NewBufBinaryWriter()
	stateRoot.Serialize(buff.BinaryWriter)
	crossChainMsg := buff.Bytes()

	// get proof
	res3 := this.neoSdk.GetProof(stateRoot.RootHash, "0x"+helper.ReverseString(this.config.NeoCCMC), retry.Key)
	if res3.HasError() {
		return fmt.Errorf("[retrySyncProofToRelay] neoSdk.GetProof error: %s", res3.Error.Message)
	}
	proof, err := hex.DecodeString(res3.Result)
	if err != nil {
		return fmt.Errorf("[retrySyncProofToRelay] decode proof error: %s", err)
	}

	txHash, err := this.relaySdk.Native.Ccm.ImportOuterTransfer(this.config.NeoChainID, nil, retry.Height, proof, this.relayAccount.Address[:], crossChainMsg, this.relayAccount)
	if err != nil {
		if strings.Contains(err.Error(), "chooseUtxos, current utxo is not enough") {
			log.Infof("[retrySyncProofToRelay] invokeNativeContract error: %s", err)
			return nil
		} else {
			if err := this.db.DeleteRetry(v); err != nil {
				return fmt.Errorf("[retrySyncProofToRelay] this.db.DeleteRetry error: %s", err)
			}
			return fmt.Errorf("[retrySyncProofToRelay] invokeNativeContract error: %s", err)
		}
	}

	err = this.db.PutCheck(txHash.ToHexString(), v)
	if err != nil {
		return fmt.Errorf("[retrySyncProofToRelay] this.db.PutCheck error: %s", err)
	}
	err = this.db.DeleteRetry(v)
	if err != nil {
		return fmt.Errorf("[retrySyncProofToRelay] this.db.DeleteRetry error: %s", err)
	}

	log.Infof("[retrySyncProofToRelay] syncProofToAlia txHash is :", txHash.ToHexString())
	return nil
}

func (this *SyncService) waitForRelayBlock() {
	_, err := this.relaySdk.WaitForGenerateBlock(30*time.Second, 3)
	if err != nil {
		log.Errorf("[waitForRelayBlock] error: %s", err)
	}
}

func (this *SyncService) GetStateRootMsg(sr mpt.StateRoot) []byte {
	buff2 := io.NewBufBinaryWriter()
	sr.SerializeUnsigned(buff2.BinaryWriter)
	hash := helper.UInt256FromBytes(crypto.Sha256(buff2.Bytes()))

	buf := io.NewBufBinaryWriter()
	buf.BinaryWriter.WriteLE(this.config.NeoMagic)
	buf.BinaryWriter.WriteLE(hash)
	if buf.Err != nil {
		return nil
	}
	return buf.Bytes()
}

func (this *SyncService) checkDoneTx() error {
	checkMap, err := this.db.GetAllCheck()
	if err != nil {
		return fmt.Errorf("[checkDoneTx] this.db.GetAllCheck error: %s", err)
	}
	for k, v := range checkMap {
		event, err := this.relaySdk.GetSmartContractEvent(k)
		if err != nil {
			return fmt.Errorf("[checkDoneTx] this.aliaSdk.GetSmartContractEvent error: %s", err)
		}
		if event == nil {
			log.Infof("[checkDoneTx] can not find event of hash %s", k)
			continue
		}
		if event.State != 1 {
			log.Infof("[checkDoneTx] state of tx %s is not success", k)
			err := this.db.PutRetry(v)
			if err != nil {
				log.Errorf("[checkDoneTx] this.db.PutRetry error:%s", err)
			}
		}
		err = this.db.DeleteCheck(k)
		if err != nil {
			log.Errorf("[checkDoneTx] this.db.DeleteCheck error:%s", err)
		}
	}

	return nil
}

func (this *SyncService) retryTx() error {
	retryList, err := this.db.GetAllRetry()
	if err != nil {
		return fmt.Errorf("[retryTx] this.db.GetAllRetry error: %s", err)
	}
	for _, v := range retryList {
		err = this.retrySyncProofToRelay(v)
		if err != nil {
			log.Errorf("[retryTx] this.retrySyncProofToRelay error:%s", err)
		}
		time.Sleep(time.Duration(this.config.RetryInterval) * time.Second)
	}

	return nil
}
