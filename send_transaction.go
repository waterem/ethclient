// Copyright 2016-2017 Hyperchain Corp.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"errors"
	"math/big"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/rjl493456442/ethclient/client"
	"gopkg.in/urfave/cli.v1"
)

var (
	errInvalidArguments  = errors.New("invalid transaction or call arguments")
	errWaitTimeout       = errors.New("wait transaction mined timeout")
	errInvalidBatchIndex = errors.New("invalid batch index")
)

var commandSend = cli.Command{
	Name:        "send",
	Usage:       "Send transaction to ethereum network",
	Description: "Send transaction to connected ethereum node with specified arguments",
	Flags: []cli.Flag{
		passphraseFlag,
		passphraseFileFlag,
		keystoreFlag,
		clientFlag,
		senderFlag,
		receiverFlag,
		valueFlag,
		dataFlag,
		syncFlag,
	},
	Action: Send,
}

var commandSendBatch = cli.Command{
	Name:        "sendBatch",
	Usage:       "Send batch of transactions to ethereum network",
	Description: "Send a batch of transactions to specified ethereum server",
	Flags: []cli.Flag{
		passphraseFlag,
		passphraseFileFlag,
		keystoreFlag,
		clientFlag,
		batchFileFlag,
		batchIndexBeginFlag,
		batchIndexEndFlag,
		tokenfileFlag,
	},
	Action: SendBatch,
}

// Send sends a transaction with specified fields.
func Send(ctx *cli.Context) error {
	var (
		sender   = ctx.String(senderFlag.Name)
		receiver = ctx.String(receiverFlag.Name)
		value    = ctx.Int(valueFlag.Name)
		data     = ctx.String(dataFlag.Name)
	)
	// Construct call message
	if !CheckArguments(sender, receiver, value, common.FromHex(data)) {
		return errInvalidArguments
	}
	to := common.HexToAddress(receiver)
	callMsg := &ethereum.CallMsg{
		From:  common.HexToAddress(sender),
		To:    &to,
		Value: big.NewInt(int64(value)),
		Data:  common.FromHex(data),
	}
	if receiver == "" {
		callMsg.To = nil
	}
	// Extract password
	passphrase := getPassphrase(ctx, false)

	// Setup rpc client
	client, err := getClient(ctx)
	if err != nil {
		return err
	}
	keystore := getKeystore(ctx)

	_, err = sendTransaction(client, callMsg, passphrase, keystore, ctx.Bool(syncFlag.Name))
	return err
}

// SendBatch sends a batch of specified transactions to ethereum server.
func SendBatch(ctx *cli.Context) error {
	var (
		batchfile = getBatchFile(ctx)
		rw        RWriter
		err       error
		begin     int
		end       int
	)
	if _, err := os.Stat(batchfile); os.IsNotExist(err) {
		return err
	}

	switch strings.HasSuffix(batchfile, ".xlsx") {
	case true:
		rw, err = NewExcelRWriter(batchfile, getSheetId(ctx))
	default:
		rw, err = NewRawTextRWriter(batchfile)
	}
	if err != nil {
		return err
	}
	entries, err := rw.ReadAll()
	if err != nil {
		return err
	}
	// Read begin, end index for batch file
	begin, end = ctx.Int(batchIndexBeginFlag.Name), ctx.Int(batchIndexEndFlag.Name)
	if end == 0 {
		end = len(entries)
	}

	if begin >= end {
		return errInvalidBatchIndex
	}

	entries = entries[begin:end]
	// Setup rpc client
	client, err := getClient(ctx)
	if err != nil {
		return err
	}
	keystore := getKeystore(ctx)

	mp, err := getMacroParser(client, ctx.String(tokenfileFlag.Name))
	if err != nil {
		return err
	}

	for idx, entry := range entries {
		// Construct call message
		if !CheckArguments(entry.From.Hex(), entry.To.Hex(), int(entry.Value), []byte(entry.Data)) {
			return errInvalidArguments
		}
		var data string	= entry.Data
		var to common.Address = entry.To
		if mp.isMacroDefinition(data) {
			to, data, _, err = mp.Parse(data, entry.From.Hex(), entry.To.Hex())
			if err != nil {
				logger.Error(err)
				continue
			}
		}

		callMsg := &ethereum.CallMsg{
			From:  entry.From,
			To:    &to,
			Value: big.NewInt(entry.Value),
			Data:  common.FromHex(data),
		}

		if entry.To.Hex() == "" {
			callMsg.To = nil
		}
		if entry.Passphrase == "" {
			entry.Passphrase = getPassphrase(ctx, false)
		}
		// Never wait during the batch sending
		if hash, err := sendTransaction(client, callMsg, entry.Passphrase, keystore, false); err != nil {
			logger.Error(err)
			continue
		} else {
			// Record the hash to batch file
			var (
				actualIdx = idx + begin
				axis      string
			)
			switch rw.(type) {
			case *ExcelRWriter:
				axis = "F" + strconv.Itoa(actualIdx+2)
			case *RawTextRWriter:
				axis = strconv.Itoa(actualIdx)
			}
			err = rw.WriteString(axis, hash.Hex())
			if err != nil {
				logger.Error(err)
			}
		}
	}
	rw.Flush()
	return nil
}

// sendTransaction sends a transaction with given call message and fill with sufficient fields like account nonce.
func sendTransaction(client *client.Client, callMsg *ethereum.CallMsg, passphrase string, keystore *keystore.KeyStore, wait bool) (common.Hash, error) {
	gasPrice, gasLimit, nonce, chainId, err := fetchParams(client, callMsg)
	if err != nil {
		return common.Hash{}, err
	}
	var tx *types.Transaction
	callMsg.Gas = gasLimit
	callMsg.GasPrice = gasPrice

	if callMsg.To == nil {
		tx = types.NewContractCreation(nonce, callMsg.Value, callMsg.Gas, callMsg.GasPrice, callMsg.Data)
	} else {
		tx = types.NewTransaction(nonce, *callMsg.To, callMsg.Value, callMsg.Gas, callMsg.GasPrice, callMsg.Data)
	}
	// Sign transaction
	tx, err = keystore.SignTxWithPassphrase(accounts.Account{Address: callMsg.From}, passphrase, tx, chainId)
	if err != nil {
		return common.Hash{}, err
	}

	// Send transaction
	timeoutContext, _ := makeTimeoutContext(5 * time.Second)
	if err := client.Cli.SendTransaction(timeoutContext, tx); err != nil {
		return common.Hash{}, err
	}
	logger.Noticef("sendTransaction, hash=%s", tx.Hash().Hex())

	// Wait for the mining
	if wait {
		timeoutContext, _ := makeTimeoutContext(60 * time.Second)
		receipt, err := waitMined(timeoutContext, client, tx.Hash())
		if err != nil {
			logger.Notice("wait transaction receipt failed")
		} else {
			logger.Noticef("transaction receipt=%s", receipt.String())
		}
	}
	return tx.Hash(), nil
}

// fetchParams returns estimated gas limit, suggested gas price and sender pending nonce.
func fetchParams(client *client.Client, callMsg *ethereum.CallMsg) (*big.Int, uint64, uint64, *big.Int, error) {
	timeoutContext, _ := makeTimeoutContext(5 * time.Second)
	// Gas estimation
	gasLimit, err := client.Cli.EstimateGas(timeoutContext, *callMsg)
	if err != nil {
		return nil, 0, 0, nil, err
	}

	// Suggestion gas price
	timeoutContext, _ = makeTimeoutContext(5 * time.Second)
	gasPrice, err := client.Cli.SuggestGasPrice(timeoutContext)
	if err != nil {
		return nil, 0, 0, nil, err
	}

	// Account Nonce
	timeoutContext, _ = makeTimeoutContext(5 * time.Second)
	nonce, err := client.Cli.PendingNonceAt(timeoutContext, callMsg.From)
	if err != nil {
		return nil, 0, 0, nil, err
	}

	// Chain Id
	timeoutContext, _ = makeTimeoutContext(5 * time.Second)
	chainId, err := client.Cli.NetworkID(timeoutContext)
	if err != nil {
		return nil, 0, 0, nil, err
	}
	// TODO Use cache to improve query efficiency
	return gasPrice, gasLimit, nonce, chainId, nil
}

// waitMined waits the transaction been mined and fetch the receipt.
// An error will been returned if waiting exceeds the given timeout
func waitMined(ctx context.Context, client *client.Client, txHash common.Hash) (*types.Receipt, error) {
	for {
		receipt, err := client.Cli.TransactionReceipt(ctx, txHash)
		if receipt == nil || err != nil {
			time.Sleep(1 * time.Second)
		} else {
			return receipt, nil
		}
		select {
		case <-ctx.Done():
			return nil, errWaitTimeout
		default:
		}
	}
}
