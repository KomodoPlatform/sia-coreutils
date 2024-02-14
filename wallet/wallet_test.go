package wallet_test

import (
	"errors"
	"fmt"
	"testing"

	"go.sia.tech/core/types"
	"go.sia.tech/coreutils/chain"
	"go.sia.tech/coreutils/testutil"
	"go.sia.tech/coreutils/wallet"
	"go.uber.org/zap/zaptest"
)

// check balance is a helper function that compares the wallet's balance to
// the expected values.
func checkBalance(w *wallet.SingleAddressWallet, spendable, confirmed, immature, unconfirmed types.Currency) error {
	balance, err := w.Balance()
	if err != nil {
		return fmt.Errorf("failed to get balance: %w", err)
	} else if !balance.Confirmed.Equals(confirmed) {
		return fmt.Errorf("expected %v confirmed balance, got %v", confirmed, balance.Confirmed)
	} else if !balance.Spendable.Equals(spendable) {
		return fmt.Errorf("expected %v spendable balance, got %v", spendable, balance.Spendable)
	} else if !balance.Unconfirmed.Equals(unconfirmed) {
		return fmt.Errorf("expected %v unconfirmed balance, got %v", unconfirmed, balance.Unconfirmed)
	} else if !balance.Immature.Equals(immature) {
		return fmt.Errorf("expected %v immature balance, got %v", immature, balance.Immature)
	}
	return nil
}

func TestWallet(t *testing.T) {
	log := zaptest.NewLogger(t)

	network, genesis := testutil.Network()

	cs, tipState, err := chain.NewDBStore(chain.NewMemDB(), network, genesis)
	if err != nil {
		t.Fatal(err)
	}

	cm := chain.NewManager(cs, tipState)

	pk := types.GeneratePrivateKey()
	ws := testutil.NewEphemeralWalletStore(pk)

	if err := cm.AddSubscriber(ws, types.ChainIndex{}); err != nil {
		t.Fatal(err)
	}

	w, err := wallet.NewSingleAddressWallet(pk, cm, ws, wallet.WithLogger(log.Named("wallet")))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if err := checkBalance(w, types.ZeroCurrency, types.ZeroCurrency, types.ZeroCurrency, types.ZeroCurrency); err != nil {
		t.Fatal(err)
	}

	initialReward := cm.TipState().BlockReward()
	// mine a block to fund the wallet
	b := testutil.MineBlock(cm, w.Address())
	if err := cm.AddBlocks([]types.Block{b}); err != nil {
		t.Fatal(err)
	}

	maturityHeight := cm.TipState().MaturityHeight()
	// check that the wallet has a single event
	if events, err := w.Events(100, 0); err != nil {
		t.Fatal(err)
	} else if len(events) != 1 {
		t.Fatalf("expected 1 event, got %v", len(events))
	} else if events[0].Source != wallet.EventSourceMinerPayout {
		t.Fatalf("expected miner payout, got %v", events[0].Source)
	} else if events[0].MaturityHeight != maturityHeight {
		t.Fatalf("expected maturity height %v, got %v", maturityHeight, events[0].MaturityHeight)
	}

	// check that the wallet has an immature balance
	if checkBalance(w, types.ZeroCurrency, types.ZeroCurrency, initialReward, types.ZeroCurrency); err != nil {
		t.Fatal(err)
	}

	// mine until the payout matures
	tip := cm.TipState()
	target := tip.MaturityHeight() + 1
	for i := tip.Index.Height; i < target; i++ {
		b := testutil.MineBlock(cm, types.VoidAddress)
		if err := cm.AddBlocks([]types.Block{b}); err != nil {
			t.Fatal(err)
		}
	}

	// check that one payout has matured
	if checkBalance(w, initialReward, initialReward, types.ZeroCurrency, types.ZeroCurrency); err != nil {
		t.Fatal(err)
	}

	// check that the wallet has a single event
	count, err := w.EventCount()
	if err != nil {
		t.Fatal(err)
	} else if count != 1 {
		t.Fatalf("expected 1 transaction, got %v", count)
	}

	// check that the payout transaction was created
	events, err := w.Events(100, 0)
	if err != nil {
		t.Fatal(err)
	} else if len(events) != 1 {
		t.Fatalf("expected 1 transaction, got %v", len(events))
	} else if events[0].Source != wallet.EventSourceMinerPayout {
		t.Fatalf("expected miner payout, got %v", events[0].Source)
	}

	// split the wallet's balance into 20 outputs
	txn := types.Transaction{
		SiacoinOutputs: make([]types.SiacoinOutput, 20),
	}
	for i := range txn.SiacoinOutputs {
		txn.SiacoinOutputs[i] = types.SiacoinOutput{
			Value:   initialReward.Div64(20),
			Address: w.Address(),
		}
	}

	// fund and sign the transaction
	toSign, err := w.FundTransaction(&txn, initialReward, false)
	if err != nil {
		t.Fatal(err)
	}
	w.SignTransaction(&txn, toSign, types.CoveredFields{WholeTransaction: true})

	// check that wallet now has no spendable balance
	if err := checkBalance(w, types.ZeroCurrency, initialReward, types.ZeroCurrency, types.ZeroCurrency); err != nil {
		t.Fatal(err)
	}

	// check the wallet has no unconfirmed transactions
	poolTxns, err := w.UnconfirmedTransactions()
	if err != nil {
		t.Fatal(err)
	} else if len(poolTxns) != 0 {
		t.Fatalf("expected 0 unconfirmed transaction, got %v", len(poolTxns))
	}

	// add the transaction to the pool
	if _, err := cm.AddPoolTransactions([]types.Transaction{txn}); err != nil {
		t.Fatal(err)
	}

	// check that the wallet has one unconfirmed transaction
	poolTxns, err = w.UnconfirmedTransactions()
	if err != nil {
		t.Fatal(err)
	} else if len(poolTxns) != 1 {
		t.Fatalf("expected 1 unconfirmed transaction, got %v", len(poolTxns))
	} else if poolTxns[0].Transaction.ID() != txn.ID() {
		t.Fatalf("expected transaction %v, got %v", txn.ID(), poolTxns[0].Transaction.ID())
	} else if poolTxns[0].Source != wallet.EventSourceTransaction {
		t.Fatalf("expected wallet source, got %v", poolTxns[0].Source)
	} else if !poolTxns[0].Inflow.Equals(initialReward) {
		t.Fatalf("expected %v inflow, got %v", initialReward, poolTxns[0].Inflow)
	} else if !poolTxns[0].Outflow.Equals(initialReward) {
		t.Fatalf("expected %v outflow, got %v", types.ZeroCurrency, poolTxns[0].Outflow)
	}

	// check that the wallet now has an unconfirmed balance
	// note: the wallet should still have a "confirmed" balance since the pool
	// transaction is not yet confirmed.
	if err := checkBalance(w, types.ZeroCurrency, initialReward, types.ZeroCurrency, initialReward); err != nil {
		t.Fatal(err)
	}

	// mine a block to confirm the transaction
	b = testutil.MineBlock(cm, types.VoidAddress)
	if err := cm.AddBlocks([]types.Block{b}); err != nil {
		t.Fatal(err)
	}

	// check that the balance was confirmed and the other values reset
	if err := checkBalance(w, initialReward, initialReward, types.ZeroCurrency, types.ZeroCurrency); err != nil {
		t.Fatal(err)
	}

	// check that the wallet has two events
	count, err = w.EventCount()
	if err != nil {
		t.Fatal(err)
	} else if count != 2 {
		t.Fatalf("expected 2 transactions, got %v", count)
	}

	// check that the paginated transactions are in the proper order
	events, err = w.Events(100, 0)
	if err != nil {
		t.Fatal(err)
	} else if len(events) != 2 {
		t.Fatalf("expected 2 transactions, got %v", len(events))
	} else if events[0].ID != types.Hash256(txn.ID()) {
		t.Fatalf("expected transaction %v, got %v", txn.ID(), events[1].ID)
	} else if len(events[0].Transaction.SiacoinOutputs) != 20 {
		t.Fatalf("expected 20 outputs, got %v", len(events[1].Transaction.SiacoinOutputs))
	}

	// send all the outputs to the burn address individually
	sent := make([]types.Transaction, 20)
	sendAmount := initialReward.Div64(20)
	for i := range sent {
		sent[i].SiacoinOutputs = []types.SiacoinOutput{
			{Address: types.VoidAddress, Value: sendAmount},
		}

		toSign, err := w.FundTransaction(&sent[i], sendAmount, false)
		if err != nil {
			t.Fatal(err)
		}
		w.SignTransaction(&sent[i], toSign, types.CoveredFields{WholeTransaction: true})
	}

	// add the transactions to the pool
	if _, err := cm.AddPoolTransactions(sent); err != nil {
		t.Fatal(err)
	}

	b = testutil.MineBlock(cm, types.VoidAddress)
	if err := cm.AddBlocks([]types.Block{b}); err != nil {
		t.Fatal(err)
	}

	// check that the wallet now has 22 transactions, the initial payout
	// transaction, the split transaction, and 20 void transactions
	count, err = w.EventCount()
	if err != nil {
		t.Fatal(err)
	} else if count != 22 {
		t.Fatalf("expected 22 transactions, got %v", count)
	}

	// check that all the wallet balances have reset
	if err := checkBalance(w, types.ZeroCurrency, types.ZeroCurrency, types.ZeroCurrency, types.ZeroCurrency); err != nil {
		t.Fatal(err)
	}

	// check that the paginated transactions are in the proper order
	events, err = w.Events(20, 0) // limit of 20 so the original two transactions are not included
	if err != nil {
		t.Fatal(err)
	} else if len(events) != 20 {
		t.Fatalf("expected 20 transactions, got %v", len(events))
	}
	for i := range sent {
		// events should be chronologically ordered, reverse the order they
		// were added to the transaction pool
		j := len(events) - i - 1
		if events[j].ID != types.Hash256(sent[i].ID()) {
			t.Fatalf("expected transaction %v, got %v", sent[i].ID(), events[i].ID)
		}
	}
}

func TestWalletUnconfirmed(t *testing.T) {
	log := zaptest.NewLogger(t)

	network, genesis := testutil.Network()

	cs, tipState, err := chain.NewDBStore(chain.NewMemDB(), network, genesis)
	if err != nil {
		t.Fatal(err)
	}

	cm := chain.NewManager(cs, tipState)

	pk := types.GeneratePrivateKey()
	ws := testutil.NewEphemeralWalletStore(pk)

	if err := cm.AddSubscriber(ws, types.ChainIndex{}); err != nil {
		t.Fatal(err)
	}

	w, err := wallet.NewSingleAddressWallet(pk, cm, ws, wallet.WithLogger(log.Named("wallet")))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// check that the wallet has no balance
	if err := checkBalance(w, types.ZeroCurrency, types.ZeroCurrency, types.ZeroCurrency, types.ZeroCurrency); err != nil {
		t.Fatal(err)
	}

	initialReward := cm.TipState().BlockReward()
	// mine a block to fund the wallet
	b := testutil.MineBlock(cm, w.Address())
	if err := cm.AddBlocks([]types.Block{b}); err != nil {
		t.Fatal(err)
	}

	// check that the wallet has an immature balance
	if err := checkBalance(w, types.ZeroCurrency, types.ZeroCurrency, initialReward, types.ZeroCurrency); err != nil {
		t.Fatal(err)
	}

	// mine until the payout matures
	tip := cm.TipState()
	target := tip.MaturityHeight() + 1
	for i := tip.Index.Height; i < target; i++ {
		b := testutil.MineBlock(cm, types.VoidAddress)
		if err := cm.AddBlocks([]types.Block{b}); err != nil {
			t.Fatal(err)
		}
	}

	// check that one payout has matured
	if err := checkBalance(w, initialReward, initialReward, types.ZeroCurrency, types.ZeroCurrency); err != nil {
		t.Fatal(err)
	}

	// fund and sign a transaction sending half the balance to the burn address
	txn := types.Transaction{
		SiacoinOutputs: []types.SiacoinOutput{
			{Address: types.VoidAddress, Value: initialReward.Div64(2)},
			{Address: w.Address(), Value: initialReward.Div64(2)},
		},
	}

	toSign, err := w.FundTransaction(&txn, initialReward, false)
	if err != nil {
		t.Fatal(err)
	}
	w.SignTransaction(&txn, toSign, types.CoveredFields{WholeTransaction: true})

	// check that wallet now has no spendable balance
	if err := checkBalance(w, types.ZeroCurrency, initialReward, types.ZeroCurrency, types.ZeroCurrency); err != nil {
		t.Fatal(err)
	}
	// add the transaction to the pool
	if _, err := cm.AddPoolTransactions([]types.Transaction{txn}); err != nil {
		t.Fatal(err)
	}

	// check that the wallet has one unconfirmed transaction
	poolTxns, err := w.UnconfirmedTransactions()
	if err != nil {
		t.Fatal(err)
	} else if len(poolTxns) != 1 {
		t.Fatal("expected 1 unconfirmed transaction")
	}

	txn2 := types.Transaction{
		SiacoinOutputs: []types.SiacoinOutput{
			{Address: types.VoidAddress, Value: initialReward.Div64(2)},
		},
	}

	// try to send a new transaction without using the unconfirmed output
	_, err = w.FundTransaction(&txn2, initialReward.Div64(2), false)
	if !errors.Is(err, wallet.ErrNotEnoughFunds) {
		t.Fatalf("expected funding error with no usable utxos, got %v", err)
	}

	toSign, err = w.FundTransaction(&txn2, initialReward.Div64(2), true)
	if err != nil {
		t.Fatal(err)
	}
	w.SignTransaction(&txn2, toSign, types.CoveredFields{WholeTransaction: true})

	// broadcast the transaction
	if _, err := cm.AddPoolTransactions([]types.Transaction{txn, txn2}); err != nil {
		t.Fatal(err)
	}
}
