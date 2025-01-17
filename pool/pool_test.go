package pool_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"math"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/0xPolygonHermez/zkevm-node/db"
	"github.com/0xPolygonHermez/zkevm-node/encoding"
	"github.com/0xPolygonHermez/zkevm-node/hex"
	"github.com/0xPolygonHermez/zkevm-node/log"
	"github.com/0xPolygonHermez/zkevm-node/merkletree"
	"github.com/0xPolygonHermez/zkevm-node/pool"
	"github.com/0xPolygonHermez/zkevm-node/pool/pgpoolstorage"
	"github.com/0xPolygonHermez/zkevm-node/state"
	"github.com/0xPolygonHermez/zkevm-node/state/runtime/executor"
	"github.com/0xPolygonHermez/zkevm-node/test/dbutils"
	"github.com/0xPolygonHermez/zkevm-node/test/testutils"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	senderPrivateKey = "0x28b2b0318721be8c8339199172cd7cc8f5e273800a35616ec893083a4b32c02e"
)

var (
	dbCfg = dbutils.NewConfigFromEnv()
)

func TestMain(m *testing.M) {
	log.Init(log.Config{
		Level:   "debug",
		Outputs: []string{"stdout"},
	})

	code := m.Run()
	os.Exit(code)
}

func Test_AddTx(t *testing.T) {
	if err := dbutils.InitOrReset(dbCfg); err != nil {
		panic(err)
	}

	sqlDB, err := db.NewSQLDB(dbCfg)
	if err != nil {
		panic(err)
	}
	defer sqlDB.Close() //nolint:gosec,errcheck

	st := newState(sqlDB)

	genesisBlock := state.Block{
		BlockNumber: 0,
		BlockHash:   state.ZeroHash,
		ParentHash:  state.ZeroHash,
		ReceivedAt:  time.Now(),
	}
	balance, _ := big.NewInt(0).SetString("1000000000000000000000", encoding.Base10)
	genesis := state.Genesis{
		Balances: map[common.Address]*big.Int{
			common.HexToAddress("0xb48cA794d49EeC406A5dD2c547717e37b5952a83"): balance,
		},
	}
	ctx := context.Background()
	dbTx, err := st.BeginStateTransaction(ctx)
	require.NoError(t, err)
	err = st.SetGenesis(ctx, genesisBlock, genesis, dbTx)
	require.NoError(t, err)
	require.NoError(t, dbTx.Commit(ctx))

	s, err := pgpoolstorage.NewPostgresPoolStorage(dbCfg)
	if err != nil {
		t.Error(err)
	}

	p := pool.NewPool(s, st, common.Address{})

	txRLPHash := "0xf86e8212658082520894fd8b27a263e19f0e9592180e61f0f8c9dfeb1ff6880de0b6b3a764000080850133333355a01eac4c2defc7ed767ae36bbd02613c581b8fb87d0e4f579c9ee3a7cfdb16faa7a043ce30f43d952b9d034cf8f04fecb631192a5dbc7ee2a47f1f49c0d022a8849d"
	b, err := hex.DecodeHex(txRLPHash)
	if err != nil {
		t.Error(err)
	}
	tx := new(types.Transaction)
	tx.UnmarshalBinary(b) //nolint:gosec,errcheck

	err = p.AddTx(ctx, *tx)
	if err != nil {
		t.Error(err)
	}

	rows, err := sqlDB.Query(ctx, "SELECT hash, encoded, decoded, state FROM pool.txs")
	defer rows.Close() // nolint:staticcheck
	if err != nil {
		t.Error(err)
	}

	c := 0
	for rows.Next() {
		var hash, encoded, decoded, state string
		err := rows.Scan(&hash, &encoded, &decoded, &state)
		if err != nil {
			t.Error(err)
		}
		b, _ := tx.MarshalJSON()

		assert.Equal(t, "0xa3cff5abdf47d4feb8204a45c0a8c58fc9b9bb9b29c6588c1d206b746815e9cc", hash, "invalid hash")
		assert.Equal(t, txRLPHash, encoded, "invalid encoded")
		assert.JSONEq(t, string(b), decoded, "invalid decoded")
		assert.Equal(t, string(pool.TxStatePending), state, "invalid tx state")
		c++
	}

	assert.Equal(t, 1, c, "invalid number of txs in the pool")
}

func Test_GetPendingTxs(t *testing.T) {
	if err := dbutils.InitOrReset(dbCfg); err != nil {
		panic(err)
	}

	sqlDB, err := db.NewSQLDB(dbCfg)
	if err != nil {
		t.Error(err)
	}
	defer sqlDB.Close() //nolint:gosec,errcheck

	st := newState(sqlDB)

	genesisBlock := state.Block{
		BlockNumber: 0,
		BlockHash:   state.ZeroHash,
		ParentHash:  state.ZeroHash,
		ReceivedAt:  time.Now(),
	}
	balance, _ := big.NewInt(0).SetString("1000000000000000000000", encoding.Base10)
	genesis := state.Genesis{
		Balances: map[common.Address]*big.Int{
			common.HexToAddress("0x617b3a3528F9cDd6630fd3301B9c8911F7Bf063D"): balance,
		},
	}
	ctx := context.Background()
	dbTx, err := st.BeginStateTransaction(ctx)
	require.NoError(t, err)
	err = st.SetGenesis(ctx, genesisBlock, genesis, dbTx)
	require.NoError(t, err)
	require.NoError(t, dbTx.Commit(ctx))

	s, err := pgpoolstorage.NewPostgresPoolStorage(dbCfg)
	if err != nil {
		t.Error(err)
	}

	p := pool.NewPool(s, st, common.Address{})

	const txsCount = 10
	const limit = 5

	privateKey, err := crypto.HexToECDSA(strings.TrimPrefix(senderPrivateKey, "0x"))
	require.NoError(t, err)

	auth, err := bind.NewKeyedTransactorWithChainID(privateKey, big.NewInt(1337))
	require.NoError(t, err)

	// insert pending transactions
	for i := 0; i < txsCount; i++ {
		tx := types.NewTransaction(uint64(i), common.Address{}, big.NewInt(10), uint64(1), big.NewInt(10), []byte{})
		signedTx, err := auth.Signer(auth.From, tx)
		require.NoError(t, err)
		if err := p.AddTx(ctx, *signedTx); err != nil {
			t.Error(err)
		}
	}

	txs, err := p.GetPendingTxs(ctx, false, limit)
	if err != nil {
		t.Error(err)
	}

	assert.Equal(t, limit, len(txs))

	for i := 0; i < txsCount; i++ {
		assert.Equal(t, pool.TxStatePending, txs[0].State)
	}
}

func Test_GetPendingTxsZeroPassed(t *testing.T) {
	if err := dbutils.InitOrReset(dbCfg); err != nil {
		panic(err)
	}

	sqlDB, err := db.NewSQLDB(dbCfg)
	if err != nil {
		t.Error(err)
	}
	defer sqlDB.Close() //nolint:gosec,errcheck

	st := newState(sqlDB)

	genesisBlock := state.Block{
		BlockNumber: 0,
		BlockHash:   state.ZeroHash,
		ParentHash:  state.ZeroHash,
		ReceivedAt:  time.Now(),
	}
	balance, _ := big.NewInt(0).SetString("1000000000000000000000", encoding.Base10)
	genesis := state.Genesis{
		Balances: map[common.Address]*big.Int{
			common.HexToAddress("0x617b3a3528F9cDd6630fd3301B9c8911F7Bf063D"): balance,
		},
	}
	ctx := context.Background()
	dbTx, err := st.BeginStateTransaction(ctx)
	require.NoError(t, err)
	err = st.SetGenesis(ctx, genesisBlock, genesis, dbTx)
	require.NoError(t, err)
	require.NoError(t, dbTx.Commit(ctx))

	s, err := pgpoolstorage.NewPostgresPoolStorage(dbCfg)
	if err != nil {
		t.Error(err)
	}

	p := pool.NewPool(s, st, common.Address{})

	const txsCount = 10
	const limit = 0

	privateKey, err := crypto.HexToECDSA(strings.TrimPrefix(senderPrivateKey, "0x"))
	require.NoError(t, err)

	auth, err := bind.NewKeyedTransactorWithChainID(privateKey, big.NewInt(1337))
	require.NoError(t, err)

	// insert pending transactions
	for i := 0; i < txsCount; i++ {
		tx := types.NewTransaction(uint64(i), common.Address{}, big.NewInt(10), uint64(1), big.NewInt(10), []byte{})
		signedTx, err := auth.Signer(auth.From, tx)
		require.NoError(t, err)
		if err := p.AddTx(ctx, *signedTx); err != nil {
			t.Error(err)
		}
	}

	txs, err := p.GetPendingTxs(ctx, false, limit)
	if err != nil {
		t.Error(err)
	}

	assert.Equal(t, txsCount, len(txs))

	for i := 0; i < txsCount; i++ {
		assert.Equal(t, pool.TxStatePending, txs[0].State)
	}
}

func Test_GetTopPendingTxByProfitabilityAndZkCounters(t *testing.T) {
	ctx := context.Background()
	if err := dbutils.InitOrReset(dbCfg); err != nil {
		panic(err)
	}

	sqlDB, err := db.NewSQLDB(dbCfg)
	if err != nil {
		t.Error(err)
	}
	defer sqlDB.Close()

	st := newState(sqlDB)

	genesisBlock := state.Block{
		BlockNumber: 0,
		BlockHash:   state.ZeroHash,
		ParentHash:  state.ZeroHash,
		ReceivedAt:  time.Now(),
	}
	balance, _ := big.NewInt(0).SetString("1000000000000000000000", encoding.Base10)
	genesis := state.Genesis{
		Balances: map[common.Address]*big.Int{
			common.HexToAddress("0x617b3a3528F9cDd6630fd3301B9c8911F7Bf063D"): balance,
		},
	}
	dbTx, err := st.BeginStateTransaction(ctx)
	require.NoError(t, err)
	err = st.SetGenesis(ctx, genesisBlock, genesis, dbTx)
	require.NoError(t, err)
	require.NoError(t, dbTx.Commit(ctx))

	s, err := pgpoolstorage.NewPostgresPoolStorage(dbCfg)
	if err != nil {
		t.Error(err)
	}

	p := pool.NewPool(s, st, common.Address{})

	const txsCount = 10

	privateKey, err := crypto.HexToECDSA(strings.TrimPrefix(senderPrivateKey, "0x"))
	require.NoError(t, err)

	auth, err := bind.NewKeyedTransactorWithChainID(privateKey, big.NewInt(1337))
	require.NoError(t, err)

	// insert pending transactions
	for i := 0; i < txsCount; i++ {
		tx := types.NewTransaction(uint64(i), common.Address{}, big.NewInt(10), uint64(1), big.NewInt(10+int64(i)), []byte{})
		signedTx, err := auth.Signer(auth.From, tx)
		require.NoError(t, err)
		if err := p.AddTx(ctx, *signedTx); err != nil {
			t.Error(err)
		}
	}

	zkCounters := pool.ZkCounters{
		CumulativeGasUsed:    1000000,
		UsedKeccakHashes:     1,
		UsedPoseidonHashes:   1,
		UsedPoseidonPaddings: 1,
		UsedMemAligns:        1,
		UsedArithmetics:      1,
		UsedBinaries:         1,
		UsedSteps:            1,
	}
	tx, err := p.GetTopPendingTxByProfitabilityAndZkCounters(ctx, zkCounters)
	require.NoError(t, err)
	assert.Equal(t, tx.Transaction.GasPrice().Uint64(), uint64(19))
}

func Test_UpdateTxsState(t *testing.T) {
	ctx := context.Background()

	if err := dbutils.InitOrReset(dbCfg); err != nil {
		panic(err)
	}

	sqlDB, err := db.NewSQLDB(dbCfg)
	if err != nil {
		t.Error(err)
	}
	defer sqlDB.Close() //nolint:gosec,errcheck

	st := newState(sqlDB)

	genesisBlock := state.Block{
		BlockNumber: 0,
		BlockHash:   state.ZeroHash,
		ParentHash:  state.ZeroHash,
		ReceivedAt:  time.Now(),
	}
	balance, _ := big.NewInt(0).SetString("1000000000000000000000", encoding.Base10)
	genesis := state.Genesis{
		Balances: map[common.Address]*big.Int{
			common.HexToAddress("0x617b3a3528F9cDd6630fd3301B9c8911F7Bf063D"): balance,
		},
	}
	dbTx, err := st.BeginStateTransaction(ctx)
	require.NoError(t, err)
	err = st.SetGenesis(ctx, genesisBlock, genesis, dbTx)
	require.NoError(t, err)
	require.NoError(t, dbTx.Commit(ctx))

	s, err := pgpoolstorage.NewPostgresPoolStorage(dbCfg)
	if err != nil {
		t.Error(err)
	}

	p := pool.NewPool(s, st, common.Address{})

	privateKey, err := crypto.HexToECDSA(strings.TrimPrefix(senderPrivateKey, "0x"))
	require.NoError(t, err)

	auth, err := bind.NewKeyedTransactorWithChainID(privateKey, big.NewInt(1337))
	require.NoError(t, err)

	tx1 := types.NewTransaction(uint64(0), common.Address{}, big.NewInt(10), uint64(1), big.NewInt(10), []byte{})
	signedTx1, err := auth.Signer(auth.From, tx1)
	require.NoError(t, err)
	if err := p.AddTx(ctx, *signedTx1); err != nil {
		t.Error(err)
	}

	tx2 := types.NewTransaction(uint64(1), common.Address{}, big.NewInt(10), uint64(1), big.NewInt(10), []byte{})
	signedTx2, err := auth.Signer(auth.From, tx2)
	require.NoError(t, err)
	if err := p.AddTx(ctx, *signedTx2); err != nil {
		t.Error(err)
	}

	err = p.UpdateTxsState(ctx, []common.Hash{signedTx1.Hash(), signedTx2.Hash()}, pool.TxStateInvalid)
	if err != nil {
		t.Error(err)
	}

	var count int
	err = sqlDB.QueryRow(ctx, "SELECT COUNT(*) FROM pool.txs WHERE state = $1", pool.TxStateInvalid).Scan(&count)
	if err != nil {
		t.Error(err)
	}
	assert.Equal(t, 2, count)
}

func Test_UpdateTxState(t *testing.T) {
	ctx := context.Background()

	if err := dbutils.InitOrReset(dbCfg); err != nil {
		panic(err)
	}

	sqlDB, err := db.NewSQLDB(dbCfg)
	if err != nil {
		t.Error(err)
	}
	defer sqlDB.Close() //nolint:gosec,errcheck

	st := newState(sqlDB)

	genesisBlock := state.Block{
		BlockNumber: 0,
		BlockHash:   state.ZeroHash,
		ParentHash:  state.ZeroHash,
		ReceivedAt:  time.Now(),
	}
	balance, _ := big.NewInt(0).SetString("1000000000000000000000", encoding.Base10)
	genesis := state.Genesis{
		Balances: map[common.Address]*big.Int{
			common.HexToAddress("0x617b3a3528F9cDd6630fd3301B9c8911F7Bf063D"): balance,
		},
	}
	dbTx, err := st.BeginStateTransaction(ctx)
	require.NoError(t, err)
	err = st.SetGenesis(ctx, genesisBlock, genesis, dbTx)
	require.NoError(t, err)
	require.NoError(t, dbTx.Commit(ctx))

	s, err := pgpoolstorage.NewPostgresPoolStorage(dbCfg)
	if err != nil {
		t.Error(err)
	}

	p := pool.NewPool(s, st, common.Address{})

	privateKey, err := crypto.HexToECDSA(strings.TrimPrefix(senderPrivateKey, "0x"))
	require.NoError(t, err)

	auth, err := bind.NewKeyedTransactorWithChainID(privateKey, big.NewInt(1337))
	require.NoError(t, err)

	tx := types.NewTransaction(uint64(0), common.Address{}, big.NewInt(10), uint64(1), big.NewInt(10), []byte{})
	signedTx, err := auth.Signer(auth.From, tx)
	require.NoError(t, err)
	if err := p.AddTx(ctx, *signedTx); err != nil {
		t.Error(err)
	}

	err = p.UpdateTxState(ctx, signedTx.Hash(), pool.TxStateInvalid)
	if err != nil {
		t.Error(err)
	}

	rows, err := sqlDB.Query(ctx, "SELECT state FROM pool.txs WHERE hash = $1", signedTx.Hash().Hex())
	defer rows.Close() // nolint:staticcheck
	if err != nil {
		t.Error(err)
	}

	var state string
	rows.Next()
	if err := rows.Scan(&state); err != nil {
		t.Error(err)
	}

	assert.Equal(t, pool.TxStateInvalid, pool.TxState(state))
}

func Test_SetAndGetGasPrice(t *testing.T) {
	if err := dbutils.InitOrReset(dbCfg); err != nil {
		panic(err)
	}

	s, err := pgpoolstorage.NewPostgresPoolStorage(dbCfg)
	if err != nil {
		t.Error(err)
	}

	p := pool.NewPool(s, nil, common.Address{})

	nBig, err := rand.Int(rand.Reader, big.NewInt(0).SetUint64(math.MaxUint64))
	if err != nil {
		t.Error(err)
	}
	expectedGasPrice := nBig.Uint64()

	ctx := context.Background()

	err = p.SetGasPrice(ctx, expectedGasPrice)
	if err != nil {
		t.Error(err)
	}

	gasPrice, err := p.GetGasPrice(ctx)
	if err != nil {
		t.Error(err)
	}

	assert.Equal(t, expectedGasPrice, gasPrice)
}

func TestMarkReorgedTxsAsPending(t *testing.T) {
	if err := dbutils.InitOrReset(dbCfg); err != nil {
		panic(err)
	}
	ctx := context.Background()
	sqlDB, err := db.NewSQLDB(dbCfg)
	if err != nil {
		t.Error(err)
	}
	defer sqlDB.Close() //nolint:gosec,errcheck

	st := newState(sqlDB)

	genesisBlock := state.Block{
		BlockNumber: 0,
		BlockHash:   state.ZeroHash,
		ParentHash:  state.ZeroHash,
		ReceivedAt:  time.Now(),
	}
	balance, _ := big.NewInt(0).SetString("1000000000000000000000", encoding.Base10)
	genesis := state.Genesis{
		Balances: map[common.Address]*big.Int{
			common.HexToAddress("0x617b3a3528F9cDd6630fd3301B9c8911F7Bf063D"): balance,
		},
	}
	dbTx, err := st.BeginStateTransaction(ctx)
	require.NoError(t, err)
	err = st.SetGenesis(ctx, genesisBlock, genesis, dbTx)
	require.NoError(t, err)
	require.NoError(t, dbTx.Commit(ctx))

	s, err := pgpoolstorage.NewPostgresPoolStorage(dbCfg)
	if err != nil {
		t.Error(err)
	}

	p := pool.NewPool(s, st, common.Address{})

	privateKey, err := crypto.HexToECDSA(strings.TrimPrefix(senderPrivateKey, "0x"))
	require.NoError(t, err)

	auth, err := bind.NewKeyedTransactorWithChainID(privateKey, big.NewInt(1337))
	require.NoError(t, err)

	tx1 := types.NewTransaction(uint64(0), common.Address{}, big.NewInt(10), uint64(1), big.NewInt(10), []byte{})
	signedTx1, err := auth.Signer(auth.From, tx1)
	require.NoError(t, err)
	if err := p.AddTx(ctx, *signedTx1); err != nil {
		t.Error(err)
	}

	tx2 := types.NewTransaction(uint64(1), common.Address{}, big.NewInt(10), uint64(1), big.NewInt(10), []byte{})
	signedTx2, err := auth.Signer(auth.From, tx2)
	require.NoError(t, err)
	if err := p.AddTx(ctx, *signedTx2); err != nil {
		t.Error(err)
	}

	err = p.UpdateTxsState(ctx, []common.Hash{signedTx1.Hash(), signedTx2.Hash()}, pool.TxStateSelected)
	if err != nil {
		t.Error(err)
	}

	err = p.MarkReorgedTxsAsPending(ctx)
	require.NoError(t, err)
	txs, err := p.GetPendingTxs(ctx, false, 100)
	require.NoError(t, err)
	require.Equal(t, signedTx1.Hash().Hex(), txs[1].Hash().Hex())
	require.Equal(t, signedTx2.Hash().Hex(), txs[0].Hash().Hex())
}

func TestGetPendingTxSince(t *testing.T) {
	if err := dbutils.InitOrReset(dbCfg); err != nil {
		panic(err)
	}

	sqlDB, err := db.NewSQLDB(dbCfg)
	if err != nil {
		t.Error(err)
	}
	defer sqlDB.Close() //nolint:gosec,errcheck

	st := newState(sqlDB)

	genesisBlock := state.Block{
		BlockNumber: 0,
		BlockHash:   state.ZeroHash,
		ParentHash:  state.ZeroHash,
		ReceivedAt:  time.Now(),
	}
	balance, _ := big.NewInt(0).SetString("1000000000000000000000", encoding.Base10)
	genesis := state.Genesis{
		Balances: map[common.Address]*big.Int{
			common.HexToAddress("0x617b3a3528F9cDd6630fd3301B9c8911F7Bf063D"): balance,
		},
	}
	ctx := context.Background()
	dbTx, err := st.BeginStateTransaction(ctx)
	require.NoError(t, err)
	err = st.SetGenesis(ctx, genesisBlock, genesis, dbTx)
	require.NoError(t, err)
	require.NoError(t, dbTx.Commit(ctx))

	s, err := pgpoolstorage.NewPostgresPoolStorage(dbCfg)
	if err != nil {
		t.Error(err)
	}

	p := pool.NewPool(s, st, common.Address{})

	const txsCount = 10

	privateKey, err := crypto.HexToECDSA(strings.TrimPrefix(senderPrivateKey, "0x"))
	require.NoError(t, err)

	auth, err := bind.NewKeyedTransactorWithChainID(privateKey, big.NewInt(1337))
	require.NoError(t, err)

	txsAddedHashes := []common.Hash{}
	txsAddedTime := []time.Time{}

	timeBeforeTxs := time.Now()
	// insert pending transactions
	for i := 0; i < txsCount; i++ {
		tx := types.NewTransaction(uint64(i), common.Address{}, big.NewInt(10), uint64(1), big.NewInt(10), []byte{})
		signedTx, err := auth.Signer(auth.From, tx)
		require.NoError(t, err)
		txsAddedTime = append(txsAddedTime, time.Now())
		if err := p.AddTx(ctx, *signedTx); err != nil {
			t.Error(err)
		}
		txsAddedHashes = append(txsAddedHashes, signedTx.Hash())
		time.Sleep(1 * time.Second)
	}

	txHashes, err := p.GetPendingTxHashesSince(ctx, timeBeforeTxs)
	if err != nil {
		t.Error(err)
	}
	assert.Equal(t, txsCount, len(txHashes))
	for i, txHash := range txHashes {
		assert.Equal(t, txHash.Hex(), txsAddedHashes[i].Hex())
	}

	txHashes, err = p.GetPendingTxHashesSince(ctx, txsAddedTime[5])
	if err != nil {
		t.Error(err)
	}
	assert.Equal(t, 5, len(txHashes))
	assert.Equal(t, txHashes[0].Hex(), txsAddedHashes[5].Hex())
	assert.Equal(t, txHashes[1].Hex(), txsAddedHashes[6].Hex())
	assert.Equal(t, txHashes[2].Hex(), txsAddedHashes[7].Hex())
	assert.Equal(t, txHashes[3].Hex(), txsAddedHashes[8].Hex())
	assert.Equal(t, txHashes[4].Hex(), txsAddedHashes[9].Hex())

	txHashes, err = p.GetPendingTxHashesSince(ctx, txsAddedTime[8])
	if err != nil {
		t.Error(err)
	}
	assert.Equal(t, 2, len(txHashes))
	assert.Equal(t, txHashes[0].Hex(), txsAddedHashes[8].Hex())
	assert.Equal(t, txHashes[1].Hex(), txsAddedHashes[9].Hex())

	txHashes, err = p.GetPendingTxHashesSince(ctx, txsAddedTime[9])
	if err != nil {
		t.Error(err)
	}
	assert.Equal(t, 1, len(txHashes))
	assert.Equal(t, txHashes[0].Hex(), txsAddedHashes[9].Hex())

	txHashes, err = p.GetPendingTxHashesSince(ctx, txsAddedTime[9].Add(1*time.Second))
	if err != nil {
		t.Error(err)
	}
	assert.Equal(t, 0, len(txHashes))
}

func Test_DeleteTxsByHashes(t *testing.T) {
	ctx := context.Background()
	if err := dbutils.InitOrReset(dbCfg); err != nil {
		panic(err)
	}
	sqlDB, err := db.NewSQLDB(dbCfg)
	if err != nil {
		t.Error(err)
	}
	defer sqlDB.Close() //nolint:gosec,errcheck

	st := newState(sqlDB)

	genesisBlock := state.Block{
		BlockNumber: 0,
		BlockHash:   state.ZeroHash,
		ParentHash:  state.ZeroHash,
		ReceivedAt:  time.Now(),
	}
	balance, _ := big.NewInt(0).SetString("1000000000000000000000", encoding.Base10)
	genesis := state.Genesis{
		Balances: map[common.Address]*big.Int{
			common.HexToAddress("0x617b3a3528F9cDd6630fd3301B9c8911F7Bf063D"): balance,
		},
	}
	dbTx, err := st.BeginStateTransaction(ctx)
	require.NoError(t, err)
	err = st.SetGenesis(ctx, genesisBlock, genesis, dbTx)
	require.NoError(t, err)
	require.NoError(t, dbTx.Commit(ctx))

	s, err := pgpoolstorage.NewPostgresPoolStorage(dbCfg)
	if err != nil {
		t.Error(err)
	}

	p := pool.NewPool(s, st, common.Address{})

	privateKey, err := crypto.HexToECDSA(strings.TrimPrefix(senderPrivateKey, "0x"))
	require.NoError(t, err)

	auth, err := bind.NewKeyedTransactorWithChainID(privateKey, big.NewInt(1337))
	require.NoError(t, err)

	tx1 := types.NewTransaction(uint64(0), common.Address{}, big.NewInt(10), uint64(1), big.NewInt(10), []byte{})
	signedTx1, err := auth.Signer(auth.From, tx1)
	require.NoError(t, err)
	if err := p.AddTx(ctx, *signedTx1); err != nil {
		t.Error(err)
	}

	tx2 := types.NewTransaction(uint64(1), common.Address{}, big.NewInt(10), uint64(1), big.NewInt(10), []byte{})
	signedTx2, err := auth.Signer(auth.From, tx2)
	require.NoError(t, err)
	if err := p.AddTx(ctx, *signedTx2); err != nil {
		t.Error(err)
	}

	err = p.DeleteTxsByHashes(ctx, []common.Hash{signedTx1.Hash(), signedTx2.Hash()})
	if err != nil {
		t.Error(err)
	}

	var count int
	err = sqlDB.QueryRow(ctx, "SELECT COUNT(*) FROM pool.txs").Scan(&count)
	if err != nil {
		t.Error(err)
	}
	assert.Equal(t, 0, count)
}

func newState(sqlDB *pgxpool.Pool) *state.State {
	ctx := context.Background()
	stateDb := state.NewPostgresStorage(sqlDB)
	zkProverURI := testutils.GetEnv("ZKPROVER_URI", "localhost")

	executorServerConfig := executor.Config{URI: fmt.Sprintf("%s:50071", zkProverURI)}
	mtDBServerConfig := merkletree.Config{URI: fmt.Sprintf("%s:50061", zkProverURI)}
	executorClient, _, _ := executor.NewExecutorClient(ctx, executorServerConfig)
	stateDBClient, _, _ := merkletree.NewMTDBServiceClient(ctx, mtDBServerConfig)
	stateTree := merkletree.NewStateTree(stateDBClient)
	st := state.NewState(state.Config{MaxCumulativeGasUsed: 800000}, stateDb, executorClient, stateTree)
	return st
}
