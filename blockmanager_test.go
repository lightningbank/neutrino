package neutrino

import (
	"encoding/binary"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil/gcs/builder"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightninglabs/neutrino/headerfs"
)

// maxHeight is the height we will generate filter headers up to.
const maxHeight = 20 * uint32(wire.CFCheckptInterval)

// setupBlockManager initialises a blockManager to be used in tests.
func setupBlockManager() (*blockManager, headerfs.BlockHeaderStore,
	*headerfs.FilterHeaderStore, func(), error) {

	// Set up the block and filter header stores.
	tempDir, err := ioutil.TempDir("", "neutrino")
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("Failed to create "+
			"temporary directory: %s", err)
	}

	db, err := walletdb.Create("bdb", tempDir+"/weks.db")
	if err != nil {
		os.RemoveAll(tempDir)
		return nil, nil, nil, nil, fmt.Errorf("Error opening DB: %s",
			err)
	}

	hdrStore, err := headerfs.NewBlockHeaderStore(
		tempDir, db, &chaincfg.SimNetParams,
	)
	if err != nil {
		db.Close()
		return nil, nil, nil, nil, fmt.Errorf("Error creating block "+
			"header store: %s", err)
	}

	cleanUp := func() {
		defer os.RemoveAll(tempDir)
		defer db.Close()
	}

	cfStore, err := headerfs.NewFilterHeaderStore(
		tempDir, db, headerfs.RegularFilter,
		&chaincfg.SimNetParams,
	)
	if err != nil {
		cleanUp()
		return nil, nil, nil, nil, fmt.Errorf("Error creating filter "+
			"header store: %s", err)
	}

	// Set up a chain service for the block manager. Each test should set
	// custom query methods on this chain service.
	cs := &ChainService{
		BlockHeaders: hdrStore,
	}

	// Set up a blockManager with the chain service we defined.
	bm := &blockManager{
		server: cs,
		blkHeaderProgressLogger: newBlockProgressLogger(
			"Processed", "block", log,
		),
		fltrHeaderProgessLogger: newBlockProgressLogger(
			"Verified", "filter header", log,
		),
	}
	bm.newHeadersSignal = sync.NewCond(&bm.newHeadersMtx)
	bm.newFilterHeadersSignal = sync.NewCond(&bm.newFilterHeadersMtx)

	return bm, hdrStore, cfStore, cleanUp, nil
}

// headers wraps the different headers and filters used throughout the tests.
type headers struct {
	blockHeaders []headerfs.BlockHeader
	cfHeaders    []headerfs.FilterHeader
	checkpoints  []*chainhash.Hash
	filterHashes []chainhash.Hash
}

// generateHeaders generates block headers, filter header and hashes, and
// checkpoints from the given genesis. The onCheckpoint method will be called
// with the current cf header on each checkpoint to modify the derivation of
// the next interval
func generateHeaders(genesisBlockHeader *wire.BlockHeader,
	genesisFilterHeader *chainhash.Hash,
	onCheckpoint func(*chainhash.Hash)) (*headers, error) {

	var blockHeaders []headerfs.BlockHeader
	blockHeaders = append(blockHeaders, headerfs.BlockHeader{
		BlockHeader: genesisBlockHeader,
		Height:      0,
	})

	var cfHeaders []headerfs.FilterHeader
	cfHeaders = append(cfHeaders, headerfs.FilterHeader{
		HeaderHash: genesisBlockHeader.BlockHash(),
		FilterHash: *genesisFilterHeader,
		Height:     0,
	})

	// The filter hashes (not the filter headers!) will be sent as
	// part of the CFHeaders response, so we also keep track of
	// them.
	genesisFilter, err := builder.BuildBasicFilter(
		chaincfg.SimNetParams.GenesisBlock, nil,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to build genesis filter: %v",
			err)
	}

	genesisFilterHash, err := builder.GetFilterHash(genesisFilter)
	if err != nil {
		return nil, fmt.Errorf("unable to get genesis filter hash: %v",
			err)
	}

	var filterHashes []chainhash.Hash
	filterHashes = append(filterHashes, genesisFilterHash)

	// Also keep track of the current filter header. We use this to
	// calculate the next filter header, as it commits to the
	// previous.
	currentCFHeader := *genesisFilterHeader

	// checkpoints will be the checkpoints passed to
	// getCheckpointedCFHeaders.
	var checkpoints []*chainhash.Hash

	for height := uint32(1); height <= maxHeight; height++ {
		header := heightToHeader(height)
		blockHeader := headerfs.BlockHeader{
			BlockHeader: header,
			Height:      height,
		}

		blockHeaders = append(blockHeaders, blockHeader)

		// It doesn't really matter what filter the filter
		// header commit to, so just use the height as a nonce
		// for the filters.
		filterHash := chainhash.Hash{}
		binary.BigEndian.PutUint32(filterHash[:], height)
		filterHashes = append(filterHashes, filterHash)

		// Calculate the current filter header, and add to our
		// slice.
		currentCFHeader = chainhash.DoubleHashH(
			append(filterHash[:], currentCFHeader[:]...),
		)
		cfHeaders = append(cfHeaders, headerfs.FilterHeader{
			HeaderHash: header.BlockHash(),
			FilterHash: currentCFHeader,
			Height:     height,
		})

		// Each interval we must record a checkpoint.
		if height%wire.CFCheckptInterval == 0 {
			// We must make a copy of the current header to
			// avoid mutation.
			cfh := currentCFHeader
			checkpoints = append(checkpoints, &cfh)
			onCheckpoint(&currentCFHeader)
		}
	}

	return &headers{
		blockHeaders: blockHeaders,
		cfHeaders:    cfHeaders,
		checkpoints:  checkpoints,
		filterHashes: filterHashes,
	}, nil
}

// generateResponses generates the MsgCFHeaders messages from the given queries
// and headers.
func generateResponses(msgs []wire.Message,
	headers *headers) ([]*wire.MsgCFHeaders, error) {

	// Craft a response for each message.
	var responses []*wire.MsgCFHeaders
	for _, msg := range msgs {
		// Only GetCFHeaders expected.
		q, ok := msg.(*wire.MsgGetCFHeaders)
		if !ok {
			return nil, fmt.Errorf("got unexpected message %T",
				msg)
		}

		// The start height must be set to a checkpoint height+1.
		if q.StartHeight%wire.CFCheckptInterval != 1 {
			return nil, fmt.Errorf("unexpexted start height %v",
				q.StartHeight)
		}

		var prevFilterHeader chainhash.Hash
		switch q.StartHeight {

		// If the start height is 1 the prevFilterHeader is set to the
		// genesis header.
		case 1:
			genesisFilterHeader := headers.cfHeaders[0].FilterHash
			prevFilterHeader = genesisFilterHeader

			// Otherwise we use one of the created checkpoints.
		default:
			j := q.StartHeight/wire.CFCheckptInterval - 1
			prevFilterHeader = *headers.checkpoints[j]
		}

		resp := &wire.MsgCFHeaders{
			FilterType:       q.FilterType,
			StopHash:         q.StopHash,
			PrevFilterHeader: prevFilterHeader,
		}

		// Keep adding filter hashes until we reach the stop hash.
		for h := q.StartHeight; ; h++ {
			resp.FilterHashes = append(
				resp.FilterHashes, &headers.filterHashes[h],
			)

			blockHash := headers.blockHeaders[h].BlockHash()
			if blockHash == q.StopHash {
				break
			}
		}

		responses = append(responses, resp)
	}

	return responses, nil
}

// TestBlockManagerInitialInterval tests that the block manager is able to
// handle checkpointed filter header query responses in out of order, and when
// a partial interval is already written to the store.
func TestBlockManagerInitialInterval(t *testing.T) {
	t.Parallel()

	type testCase struct {
		// permute indicates whether responses should be permutated.
		permute bool

		// partialInterval indicates whether we should write parts of
		// the first checkpoint interval to the filter header store
		// before starting the test.
		partialInterval bool

		// repeat indicates whether responses should be repeated.
		repeat bool
	}

	// Generate all combinations of testcases.
	var testCases []testCase
	b := []bool{false, true}
	for _, perm := range b {
		for _, part := range b {
			for _, rep := range b {
				testCases = append(testCases, testCase{
					permute:         perm,
					partialInterval: part,
					repeat:          rep,
				})
			}
		}
	}

	for _, test := range testCases {

		bm, hdrStore, cfStore, cleanUp, err := setupBlockManager()
		if err != nil {
			t.Fatalf("unable to set up ChainService: %v", err)
		}
		defer cleanUp()

		// Keep track of the filter headers and block headers. Since
		// the genesis headers are written automatically when the store
		// is created, we query it to add to the slices.
		genesisBlockHeader, _, err := hdrStore.ChainTip()
		if err != nil {
			t.Fatal(err)
		}

		genesisFilterHeader, _, err := cfStore.ChainTip()
		if err != nil {
			t.Fatal(err)
		}

		headers, err := generateHeaders(genesisBlockHeader,
			genesisFilterHeader, func(*chainhash.Hash) {})
		if err != nil {
			t.Fatalf("unable to generate headers;: %v", err)
		}

		// Write all block headers but the genesis, since it is already
		// in the store.
		if err = hdrStore.WriteHeaders(headers.blockHeaders[1:]...); err != nil {
			t.Fatalf("Error writing batch of headers: %s", err)
		}

		// We emulate the case where a few filter headers are already
		// written to the store by writing 1/3 of the first interval.
		if test.partialInterval {
			err = cfStore.WriteHeaders(
				headers.cfHeaders[1 : wire.CFCheckptInterval/3]...,
			)
			if err != nil {
				t.Fatalf("Error writing batch of headers: %s",
					err)
			}
		}

		// We set up a custom query batch method for this test, as we
		// will use this to feed the blockmanager with our crafted
		// responses.
		bm.server.queryBatch = func(msgs []wire.Message,
			f func(*ServerPeer, wire.Message, wire.Message) bool,
			q <-chan struct{}, qo ...QueryOption) {

			responses, err := generateResponses(msgs, headers)
			if err != nil {
				t.Fatalf("unable to generate responses: %v",
					err)
			}

			// We permute the response order if the test signals
			// that.
			perm := rand.Perm(len(responses))
			for i, v := range perm {
				index := i
				if test.permute {
					index = v
				}
				if !f(nil, msgs[index], responses[index]) {
					t.Fatalf("got response false")
				}

				// If we are not testing repeated responses, go
				// on to the next response.
				if !test.repeat {
					continue
				}

				// Otherwise resend the response we just sent.
				if !f(nil, msgs[index], responses[index]) {
					t.Fatalf("got response false")
				}

			}
		}

		// Call the get checkpointed cf headers method with the
		// checkpoints we created to start the test.
		bm.getCheckpointedCFHeaders(
			headers.checkpoints, cfStore, wire.GCSFilterRegular,
		)

		// Finally make sure the filter header tip is what we expect.
		tip, tipHeight, err := cfStore.ChainTip()
		if err != nil {
			t.Fatal(err)
		}

		if tipHeight != maxHeight {
			t.Fatalf("expected tip height to be %v, was %v",
				maxHeight, tipHeight)
		}

		lastCheckpoint := headers.checkpoints[len(headers.checkpoints)-1]
		if *tip != *lastCheckpoint {
			t.Fatalf("expected tip to be %v, was %v",
				lastCheckpoint, tip)
		}
	}
}
